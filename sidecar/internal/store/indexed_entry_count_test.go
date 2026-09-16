// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package store // import "miniflux.app/v2/sidecar/internal/store"

import (
	"context"
	"testing"
)

// TestIndexedEntryCountCountsOnlyOKEntries is defect 6's fix, at the
// store layer: IndexedEntryCount must count an entry once it is
// genuinely indexed (status='ok'), zero before that, and must not count
// a merely-attempted-and-failed or skipped entry as indexed.
func TestIndexedEntryCountCountsOnlyOKEntries(t *testing.T) {
	s := testStore(t)
	if err := s.Migrate(); err != nil {
		t.Fatalf("migrate failed: %v", err)
	}

	entryID := createTestEntry(t, s, "indexed-count-ok", "<p>Body of the indexed-count fixture.</p>")
	after := entryID - 1

	count, err := s.IndexedEntryCount(after)
	if err != nil {
		t.Fatalf("IndexedEntryCount failed: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected 0 indexed entries above #%d before anything is indexed, got %d", after, count)
	}

	entry, err := s.EntryForIndexing(context.Background(), entryID)
	if err != nil {
		t.Fatalf("EntryForIndexing failed: %v", err)
	}
	if err := s.ReplacePassages(entryID, entry.ContentHash, []PassageRow{
		{Ordinal: 0, Text: "x", CharStart: 0, CharEnd: 1, Source: "content", Embedding: make([]float32, 768)},
	}); err != nil {
		t.Fatalf("ReplacePassages failed: %v", err)
	}

	count, err = s.IndexedEntryCount(after)
	if err != nil {
		t.Fatalf("IndexedEntryCount failed: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected 1 indexed entry above #%d after ReplacePassages, got %d", after, count)
	}
}

// TestIndexedEntryCountExcludesSkippedAndFailedEntries proves the status
// filter is exact: a skipped entry (no usable text) and a failed one
// (embedding error) must never be counted as indexed, even though both
// have a row in search.entry_index_state.
func TestIndexedEntryCountExcludesSkippedAndFailedEntries(t *testing.T) {
	s := testStore(t)
	if err := s.Migrate(); err != nil {
		t.Fatalf("migrate failed: %v", err)
	}

	skippedID := createTestEntry(t, s, "indexed-count-skipped", "")
	failedID := createTestEntry(t, s, "indexed-count-failed", "<p>Will be marked failed.</p>")
	after := min(skippedID, failedID) - 1

	skippedEntry, err := s.EntryForIndexing(context.Background(), skippedID)
	if err != nil {
		t.Fatalf("EntryForIndexing (skipped) failed: %v", err)
	}
	if err := s.MarkEntrySkipped(skippedID, skippedEntry.ContentHash, "no usable text"); err != nil {
		t.Fatalf("MarkEntrySkipped failed: %v", err)
	}

	failedEntry, err := s.EntryForIndexing(context.Background(), failedID)
	if err != nil {
		t.Fatalf("EntryForIndexing (failed) failed: %v", err)
	}
	if err := s.MarkEntryFailed(failedID, failedEntry.ContentHash, "embedding failed: simulated"); err != nil {
		t.Fatalf("MarkEntryFailed failed: %v", err)
	}

	count, err := s.IndexedEntryCount(after)
	if err != nil {
		t.Fatalf("IndexedEntryCount failed: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected 0 indexed entries (one skipped, one failed, neither 'ok'), got %d", count)
	}
}
