// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package indexer wires the store, the embedder and the passage pipeline
// together into IndexEntry, the single-entry indexing path both the
// backfill and the near-real-time lanes call.
package indexer // import "miniflux.app/v2/sidecar/internal/indexer"

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"

	"miniflux.app/v2/sidecar/internal/embed"
	"miniflux.app/v2/sidecar/internal/passage"
	"miniflux.app/v2/sidecar/internal/store"
)

// DefaultBatchSize is the number of passages embedded per forward pass. The
// spike (spec §6.7) measured a sweet spot of 8-32 passages per batch, with
// batch 64 slower than batch 8; DefaultBatchSize is the starting value
// SetBatchSize's own clamp (see MinBatchSize/MaxBatchSize) is centred on.
const DefaultBatchSize = 16

// MinBatchSize and MaxBatchSize bound the runtime-configurable embedding
// batch size (spec §9.2's third live-editable knob: "batch size —
// passages per forward pass; the main lever on CPU efficiency"). The
// spike (spec §6.7) measured batch 64 as slower than batch 8, so
// SetBatchSize clamps into this range rather than accepting anything —
// treat a value outside it as a regression, not an optimisation.
const (
	MinBatchSize = 8
	MaxBatchSize = 32
)

// The kinds of failure IndexEntry can return. Every error it produces
// wraps exactly one of these, so a caller can classify an outcome without
// pattern-matching on message text.
//
// This exists because the backfill lane aggregates failures by cause for
// the admin page (spec §9.4, "error and skip counts by cause") and for its
// own retry de-duplication. Keying that on err.Error() made every failing
// entry its own key — the string carries the entry id, and often the
// embedder's own per-call detail — so the map grew without bound across a
// lane meant to run unattended for 41 hours (spec §9.3), recordFailure's
// de-duplication could never fire across entries, and the "Failures by
// cause" table rendered one row per entry, which is not a breakdown by
// cause (whole-branch review, finding 4).
//
// Their message text is the leading clause of the error string they are
// wrapped into, so wrapping costs no change in what an operator reads in
// a log line.
var (
	errLoadEntry      = errors.New("indexer: unable to load entry")
	errLoadIndexState = errors.New("indexer: unable to load index state for entry")
	errMarkSkipped    = errors.New("indexer: unable to mark entry skipped")
	errMarkFailed     = errors.New("indexer: unable to mark entry failed")
	errEmbed          = errors.New("indexer: unable to embed entry")
	errInterrupted    = errors.New("indexer: embedding interrupted for entry")
	errWritePassages  = errors.New("indexer: unable to replace passages for entry")

	// errEmbedderUnavailable is IndexEntry's LANE-level counterpart to
	// errEmbed. It wraps an Embed error that itself wraps embed.ErrUnavailable
	// — see that sentinel's doc comment for the classification rule. Unlike
	// every other error above, this one is deliberately never handed to
	// store.MarkEntryFailed: the entry is not at fault, so IndexEntry leaves
	// its index state exactly as it found it (identical to the ctx-cancelled
	// branch just above it), and the backfill/live lanes classify it via
	// isEmbedderUnavailable to pause themselves instead of counting a
	// per-entry failure (spec §13.1).
	errEmbedderUnavailable = errors.New("indexer: embedder unavailable")
)

// isEmbedderUnavailable reports whether err (as IndexEntry returns it)
// means the embedder itself is currently unable to serve requests — a
// lane-level condition the backfill and live lanes must pause themselves
// for — rather than an ordinary per-entry failure the existing
// retry/backoff machinery already handles correctly. Both lanes call this
// before deciding how to account for an IndexEntry error, so the
// classification rule lives in exactly one place.
func isEmbedderUnavailable(err error) bool {
	return errors.Is(err, errEmbedderUnavailable)
}

// requiresEmbedderRestart reports whether an embedder-unavailable error
// (isEmbedderUnavailable must already be true, or this is meaningless) can
// never clear on its own — the remote reported a different model mid-run
// (embed.ErrRequiresRestart; spec §13.1) — as opposed to an ordinary
// transient outage, which resolves the moment the network or remote
// recovers with no operator action. Both lanes surface this alongside
// EmbedderPaused/EmbedderPauseReason so the admin page can tell "wait,
// this clears itself" from "go restart the sidecar" without an operator
// having to parse the full reason text to find out.
func requiresEmbedderRestart(err error) bool {
	return errors.Is(err, embed.ErrRequiresRestart)
}

// maxCauseLength truncates an unclassified cause label, so that even a
// pathological error string cannot make one map key, one JSON field or one
// admin-page table cell arbitrarily large.
const maxCauseLength = 120

// causeDigits matches the runs of digits a generic cause label must lose
// before it can be used as an aggregation key: an entry id, a row count,
// an offset, a port, a timestamp. Without this, "connection refused on
// 127.0.0.1:5434" and "deadlock detected on relation 41234" are as
// unbounded as the strings they came from.
var causeDigits = regexp.MustCompile(`\d+`)

// failureCause maps an error returned by IndexEntry to a short, stable,
// low-cardinality label suitable for use as an aggregation key. The full
// error, entry id and all, belongs in the log line; this is what the
// counters and the admin page's "Failures by cause" table are keyed on.
func failureCause(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, errEmbed):
		return "embedding failed"
	case errors.Is(err, errEmbedderUnavailable):
		// Defensive only: both lanes classify errEmbedderUnavailable
		// themselves (isEmbedderUnavailable) before ever reaching
		// failureCause, specifically so it is never counted as a
		// per-entry failure. This case exists so that IF one ever did
		// reach here anyway, it still gets a bounded, stable label
		// rather than falling into genericCause's per-error text.
		return "embedder unavailable"
	case errors.Is(err, errInterrupted):
		return "interrupted"
	case errors.Is(err, errLoadEntry):
		return "could not read the entry"
	case errors.Is(err, errLoadIndexState):
		return "could not read the entry's index state"
	case errors.Is(err, errMarkSkipped), errors.Is(err, errMarkFailed):
		return "could not record the entry's index state"
	case errors.Is(err, errWritePassages):
		return "could not write the entry's passages"
	default:
		return genericCause(err)
	}
}

// genericCause is failureCause's fallback for an error that wraps none of
// the known kinds: strip the digit runs that make a message per-entry
// unique and truncate. It is deliberately lossy — an aggregation key is
// not a diagnostic, and the diagnostic is already in the log.
func genericCause(err error) string {
	cause := causeDigits.ReplaceAllString(err.Error(), "N")
	cause = strings.TrimSpace(cause)
	if cause == "" {
		return "unknown"
	}
	if len(cause) > maxCauseLength {
		cause = cause[:maxCauseLength] + "..."
	}
	return cause
}

// Indexer indexes one entry at a time into search.passages.
type Indexer struct {
	store    *store.Store
	embedder embed.Embedder

	mu        sync.Mutex
	batchSize int // live-editable (spec §9.2); guarded by mu, always read via BatchSize()
}

// New builds an Indexer over the given store and embedder, with the
// embedding batch size starting at DefaultBatchSize.
//
// It records e's Identity() with the store (store.SetModelIdentity) before
// returning, so that contentHash folds in the model actually configured
// (spec §13.1) from the first call onward — an Indexer whose embedder's
// identity was never recorded would leave every entry's expected hash
// computed as if no model, or the wrong one, were configured. e may be
// nil for a config-only Backfill built solely to exercise live-editable
// settings (ApplyConfig/RuntimeConfig), which never calls IndexEntry and
// so never needs a real embedder; that path leaves the recorded identity
// untouched rather than overwriting it with one derived from nothing.
func New(s *store.Store, e embed.Embedder) *Indexer {
	if e != nil {
		store.SetModelIdentity(e.Identity())
	}
	return &Indexer{store: s, embedder: e, batchSize: DefaultBatchSize}
}

// SetBatchSize live-edits the number of passages embedded per forward
// pass, clamping to [MinBatchSize, MaxBatchSize] (spec §6.7's measured
// sweet spot) rather than accepting anything outside it. Takes effect on
// the next IndexEntry call to start batching passages; an entry already
// mid-batch keeps the size it started with.
func (idx *Indexer) SetBatchSize(n int) {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	if n < MinBatchSize {
		n = MinBatchSize
	}
	if n > MaxBatchSize {
		n = MaxBatchSize
	}
	idx.batchSize = n
}

// BatchSize returns the current embedding batch size.
func (idx *Indexer) BatchSize() int {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	return idx.batchSize
}

// IndexEntry indexes a single entry: it loads the entry's content, and if
// its content hash matches what is already recorded as successfully
// indexed, returns immediately without doing any work (in particular,
// without calling the embedder — see spec §10, "entry unchanged"). The
// entry's title becomes its own passage (ordinal 0, source="title",
// offsets into the title itself — task 1.5), skipped only if the title is
// empty or all whitespace; text is extracted from the entry's HTML and
// split into body passages (source="content", offsets into that extracted
// plaintext, ordinals following the title). An entry with neither a usable
// title nor usable body text is recorded as skipped and is not treated as
// an error — one with a usable title but no usable body still indexes,
// by its title alone. Every passage, title and body alike, is embedded in
// batches of BatchSize() (DefaultBatchSize unless SetBatchSize has
// live-edited it — spec §9.2); an embedding failure records the entry as
// failed (retryable later) and IndexEntry returns the error — unless the
// failure was ctx being cancelled mid-embed (a caller shutting down or
// interrupting a batch, not a real embedder failure) or the embedder
// reporting itself unavailable (errors.Is against embed.ErrUnavailable —
// a network outage, or a remote that changed identity mid-run; spec
// §13.1), in either of which cases entry_index_state is left untouched
// entirely: a graceful shutdown must never manufacture a spurious
// "failed" row, and neither must an outage that has nothing to do with
// this entry's own content — the backfill and live lanes classify that
// second case (isEmbedderUnavailable) and pause themselves rather than
// treating it as a per-entry failure. Passages are written, and the
// entry recorded ok, in a single atomic replace — IndexEntry never
// marks an entry ok on a partial result.
func (idx *Indexer) IndexEntry(ctx context.Context, entryID int64) error {
	entry, err := idx.store.EntryForIndexing(ctx, entryID)
	if err != nil {
		return fmt.Errorf("%w #%d: %w", errLoadEntry, entryID, err)
	}

	state, err := idx.store.EntryIndexState(ctx, entryID)
	if err != nil {
		return fmt.Errorf("%w #%d: %w", errLoadIndexState, entryID, err)
	}
	if state != nil && state.Status == "ok" && state.ContentHash == entry.ContentHash {
		return nil
	}

	// title's offsets are into the title itself -- a completely different
	// string from bodyText below. Never conflate the two: see PassageRow's
	// doc comment on why that hazard has no error, only wrong highlights.
	title := strings.TrimSpace(entry.Title)

	bodyText := passage.ExtractText(entry.Content)
	var bodyPassages []passage.Passage
	if bodyText != "" {
		bodyPassages = passage.Split(bodyText, passage.DefaultSplitOptions())
	}

	if title == "" && len(bodyPassages) == 0 {
		reason := "no usable text extracted from entry content"
		if bodyText != "" {
			reason = "no passages produced from extracted text"
		}
		if err := idx.store.MarkEntrySkipped(entryID, entry.ContentHash, reason); err != nil {
			return fmt.Errorf("%w #%d: %w", errMarkSkipped, entryID, err)
		}
		return nil
	}

	// combined lays the title passage (if any) at index 0 -- giving it
	// ordinal 0 -- followed by the body passages, so a single batching
	// loop below embeds both kinds uniformly. Embedding the title costs
	// one extra short forward pass per entry, negligible against the
	// entry's own 4-6 body passages.
	type sourcedPassage struct {
		text               string
		charStart, charEnd int
		source             string
	}
	var combined []sourcedPassage
	if title != "" {
		combined = append(combined, sourcedPassage{text: title, charStart: 0, charEnd: len(title), source: "title"})
	}
	for _, p := range bodyPassages {
		combined = append(combined, sourcedPassage{text: p.Text, charStart: p.CharStart, charEnd: p.CharEnd, source: "content"})
	}

	batchSize := idx.BatchSize()
	rows := make([]store.PassageRow, len(combined))
	for batchStart := 0; batchStart < len(combined); batchStart += batchSize {
		batchEnd := min(batchStart+batchSize, len(combined))

		texts := make([]string, batchEnd-batchStart)
		for i := batchStart; i < batchEnd; i++ {
			texts[i-batchStart] = combined[i].text
		}

		vectors, err := idx.embedder.EmbedDocuments(ctx, texts)
		if err != nil {
			if ctx.Err() != nil {
				// ctx was cancelled (shutdown, or a backfill/live lane
				// interruption) rather than the embedder genuinely
				// failing. The entry was never attempted to completion,
				// so recording it as a retryable failure would be
				// misleading and would pollute entry_index_state with a
				// "failed" row on every graceful shutdown — leave it
				// exactly as it was; PendingEntryIDs offers it again next
				// time, identical to an entry that was never attempted.
				return fmt.Errorf("%w #%d: %w", errInterrupted, entryID, err)
			}
			if errors.Is(err, embed.ErrUnavailable) {
				// LANE-level, not this entry's fault (spec §13.1): the
				// embedder itself cannot currently serve requests — a
				// network outage, or (see internal/embed/remote) a
				// remote that started serving a different model
				// mid-run. entry_index_state is left EXACTLY as it was,
				// deliberately mirroring the ctx-cancelled branch above:
				// marking this entry "failed" would be wrong (its
				// content was never the problem), and doing so for
				// every entry mid-batch during an outage is precisely
				// the failure mode this task exists to prevent — a
				// five-minute network blip must not mark thousands of
				// entries individually failed, needing retry backoff to
				// unwind it. The backfill and live lanes call
				// isEmbedderUnavailable on this error to pause
				// themselves instead of counting a per-entry failure.
				return fmt.Errorf("%w #%d: %w", errEmbedderUnavailable, entryID, err)
			}
			reason := fmt.Sprintf("embedding failed: %v", err)
			if markErr := idx.store.MarkEntryFailed(entryID, entry.ContentHash, reason); markErr != nil {
				return fmt.Errorf("%w #%d: could not be marked failed (%v) after: %w", errMarkFailed, entryID, markErr, err)
			}
			return fmt.Errorf("%w #%d: %w", errEmbed, entryID, err)
		}

		for i, vector := range vectors {
			cp := combined[batchStart+i]
			rows[batchStart+i] = store.PassageRow{
				Ordinal:   batchStart + i,
				Text:      cp.text,
				CharStart: cp.charStart,
				CharEnd:   cp.charEnd,
				Source:    cp.source,
				Embedding: vector,
			}
		}
	}

	if err := idx.store.ReplacePassages(entryID, entry.ContentHash, rows); err != nil {
		return fmt.Errorf("%w #%d: %w", errWritePassages, entryID, err)
	}

	return nil
}
