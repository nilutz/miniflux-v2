// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package indexer wires the store, the embedder and the passage pipeline
// together into IndexEntry, the single-entry indexing path both the
// backfill and the near-real-time lanes call.
package indexer // import "miniflux.app/v2/sidecar/internal/indexer"

import (
	"context"
	"fmt"

	"miniflux.app/v2/sidecar/internal/embed"
	"miniflux.app/v2/sidecar/internal/passage"
	"miniflux.app/v2/sidecar/internal/store"
)

// DefaultBatchSize is the number of passages embedded per forward pass. The
// spike (spec §6.7) measured a sweet spot of 8-32 passages per batch, with
// batch 64 slower than batch 8; do not raise this without re-measuring.
const DefaultBatchSize = 16

// Indexer indexes one entry at a time into search.passages.
type Indexer struct {
	store    *store.Store
	embedder embed.Embedder
}

// New builds an Indexer over the given store and embedder.
func New(s *store.Store, e embed.Embedder) *Indexer {
	return &Indexer{store: s, embedder: e}
}

// IndexEntry indexes a single entry: it loads the entry's content, and if
// its content hash matches what is already recorded as successfully
// indexed, returns immediately without doing any work (in particular,
// without calling the embedder — see spec §10, "entry unchanged"). Text is
// extracted from the entry's HTML; an entry with no usable text is recorded
// as skipped and is not treated as an error. Otherwise the text is split
// into passages and embedded in batches of DefaultBatchSize; an embedding
// failure records the entry as failed (retryable later) and IndexEntry
// returns the error. Passages are written, and the entry recorded ok, in a
// single atomic replace — IndexEntry never marks an entry ok on a partial
// result.
func (idx *Indexer) IndexEntry(ctx context.Context, entryID int64) error {
	entry, err := idx.store.EntryForIndexing(entryID)
	if err != nil {
		return fmt.Errorf("indexer: unable to load entry #%d: %w", entryID, err)
	}

	state, err := idx.store.EntryIndexState(entryID)
	if err != nil {
		return fmt.Errorf("indexer: unable to load index state for entry #%d: %w", entryID, err)
	}
	if state != nil && state.Status == "ok" && state.ContentHash == entry.ContentHash {
		return nil
	}

	text := passage.ExtractText(entry.Content)
	if text == "" {
		if err := idx.store.MarkEntrySkipped(entryID, entry.ContentHash, "no usable text extracted from entry content"); err != nil {
			return fmt.Errorf("indexer: unable to mark entry #%d skipped: %w", entryID, err)
		}
		return nil
	}

	passages := passage.Split(text, passage.DefaultSplitOptions())
	if len(passages) == 0 {
		if err := idx.store.MarkEntrySkipped(entryID, entry.ContentHash, "no passages produced from extracted text"); err != nil {
			return fmt.Errorf("indexer: unable to mark entry #%d skipped: %w", entryID, err)
		}
		return nil
	}

	rows := make([]store.PassageRow, len(passages))
	for batchStart := 0; batchStart < len(passages); batchStart += DefaultBatchSize {
		batchEnd := min(batchStart+DefaultBatchSize, len(passages))

		texts := make([]string, batchEnd-batchStart)
		for i := batchStart; i < batchEnd; i++ {
			texts[i-batchStart] = passages[i].Text
		}

		vectors, err := idx.embedder.Embed(ctx, texts)
		if err != nil {
			reason := fmt.Sprintf("embedding failed: %v", err)
			if markErr := idx.store.MarkEntryFailed(entryID, entry.ContentHash, reason); markErr != nil {
				return fmt.Errorf("indexer: entry #%d failed to embed (%w) and could not be marked failed: %v", entryID, err, markErr)
			}
			return fmt.Errorf("indexer: unable to embed entry #%d: %w", entryID, err)
		}

		for i, vector := range vectors {
			p := passages[batchStart+i]
			rows[batchStart+i] = store.PassageRow{
				Ordinal:   batchStart + i,
				Text:      p.Text,
				CharStart: p.CharStart,
				CharEnd:   p.CharEnd,
				Embedding: vector,
			}
		}
	}

	if err := idx.store.ReplacePassages(entryID, entry.ContentHash, rows); err != nil {
		return fmt.Errorf("indexer: unable to replace passages for entry #%d: %w", entryID, err)
	}

	return nil
}
