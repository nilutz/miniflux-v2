// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package store // import "miniflux.app/v2/sidecar/internal/store"

import (
	"context"
	"testing"
	"time"
)

// TestDatabaseMetricsTracksInsertedPassagesAndEntries pins the trap called
// out in the task brief: a "greater than zero" assertion passes whether or
// not DatabaseMetrics queried the right object, and passes just as well
// against a constant. This asserts the DELTA the method reports actually
// tracks passages and entries this test itself inserted -- if
// DatabaseMetrics read the wrong table, summed the wrong schema, or
// returned a hardcoded value, the before/after counts would not move by
// the amount inserted here.
//
// PassageCount/EntryCount come from pg_class.reltuples (see DatabaseMetrics'
// doc comment for why), which only advances on ANALYZE -- so this forces
// one after inserting fixtures, exactly as autovacuum would eventually do
// on its own.
func TestDatabaseMetricsTracksInsertedPassagesAndEntries(t *testing.T) {
	s := testStore(t)
	if err := s.Migrate(); err != nil {
		t.Fatalf("migrate failed: %v", err)
	}

	ctx := context.Background()

	// A prior test in this package may have left search.passages or
	// entries rows that were since deleted without a following ANALYZE:
	// pg_class.reltuples is only ever as fresh as the last ANALYZE, so
	// without this the "before" baseline can be an arbitrary stale
	// number left over from an entirely different test. ANALYZE here
	// first so "before" reflects the true state at this instant, exactly
	// like "after" will below -- the delta is what the test asserts on,
	// and both ends of it must be equally fresh for that to mean
	// anything.
	if _, err := s.db.Exec(`ANALYZE search.passages`); err != nil {
		t.Fatalf("unable to analyze search.passages: %v", err)
	}
	if _, err := s.db.Exec(`ANALYZE entries`); err != nil {
		t.Fatalf("unable to analyze entries: %v", err)
	}

	before, err := s.DatabaseMetrics(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !before.CountsAreEstimated {
		t.Fatalf("expected CountsAreEstimated to be true (reltuples-based), got false")
	}

	entryID := createTestEntry(t, s, "metrics-tracks-inserts", "<p>Some content.</p>")
	embedding := make([]float32, 384)
	if err := s.ReplacePassages(entryID, "hash-metrics-1", []PassageRow{
		{Ordinal: 0, Text: "passage one", CharStart: 0, CharEnd: 11, Source: "content", Embedding: embedding},
		{Ordinal: 1, Text: "passage two", CharStart: 11, CharEnd: 22, Source: "content", Embedding: embedding},
		{Ordinal: 2, Text: "passage three", CharStart: 22, CharEnd: 35, Source: "content", Embedding: embedding},
	}); err != nil {
		t.Fatalf("unexpected error replacing passages: %v", err)
	}

	if _, err := s.db.Exec(`ANALYZE search.passages`); err != nil {
		t.Fatalf("unable to analyze search.passages: %v", err)
	}
	if _, err := s.db.Exec(`ANALYZE entries`); err != nil {
		t.Fatalf("unable to analyze entries: %v", err)
	}

	after, err := s.DatabaseMetrics(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if gotDelta := after.PassageCount - before.PassageCount; gotDelta != 3 {
		t.Fatalf("expected passage count to advance by 3 after ANALYZE, got delta %d (before=%d after=%d)",
			gotDelta, before.PassageCount, after.PassageCount)
	}
	if gotDelta := after.EntryCount - before.EntryCount; gotDelta != 1 {
		t.Fatalf("expected entry count to advance by 1 after ANALYZE, got delta %d (before=%d after=%d)",
			gotDelta, before.EntryCount, after.EntryCount)
	}

	wantRatio := float64(after.PassageCount) / float64(after.EntryCount)
	if after.PassagesPerEntry != wantRatio {
		t.Fatalf("expected PassagesPerEntry %.4f, got %.4f", wantRatio, after.PassagesPerEntry)
	}
}

// TestDatabaseMetricsIndexAndSchemaSizesTrackRealObjects pins that
// HNSWIndexSizeBytes and SearchSchemaSizeBytes are wired to the actual
// Postgres objects they claim to report, not to a placeholder or to each
// other's numbers by coincidence: it recomputes both independently in the
// test itself and requires an exact match, then checks the containment
// relationship (the HNSW index is part of the schema's total, which is
// part of the whole database) that only holds if each number came from
// where it says it did.
func TestDatabaseMetricsIndexAndSchemaSizesTrackRealObjects(t *testing.T) {
	s := testStore(t)
	if err := s.Migrate(); err != nil {
		t.Fatalf("migrate failed: %v", err)
	}

	ctx := context.Background()
	m, err := s.DatabaseMetrics(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var wantHNSWSize int64
	if err := s.db.QueryRow(`SELECT pg_relation_size('search.passages_embedding_idx')`).Scan(&wantHNSWSize); err != nil {
		t.Fatalf("unable to read HNSW index size directly: %v", err)
	}
	if m.HNSWIndexSizeBytes != wantHNSWSize {
		t.Fatalf("expected HNSWIndexSizeBytes %d (read directly), got %d", wantHNSWSize, m.HNSWIndexSizeBytes)
	}

	var wantSchemaSize int64
	if err := s.db.QueryRow(`
		SELECT COALESCE(sum(pg_total_relation_size(c.oid)), 0)
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'search'
	`).Scan(&wantSchemaSize); err != nil {
		t.Fatalf("unable to read search schema size directly: %v", err)
	}
	if m.SearchSchemaSizeBytes != wantSchemaSize {
		t.Fatalf("expected SearchSchemaSizeBytes %d (read directly), got %d", wantSchemaSize, m.SearchSchemaSizeBytes)
	}

	if m.HNSWIndexSizeBytes <= 0 {
		t.Fatalf("expected a non-zero HNSW index size once migrated, got %d", m.HNSWIndexSizeBytes)
	}
	if m.SearchSchemaSizeBytes < m.HNSWIndexSizeBytes {
		t.Fatalf("expected the search schema's total size (%d) to be at least the HNSW index's own size (%d)",
			m.SearchSchemaSizeBytes, m.HNSWIndexSizeBytes)
	}
	if m.DatabaseSizeBytes < m.SearchSchemaSizeBytes {
		t.Fatalf("expected the whole database's size (%d) to be at least the search schema's size (%d)",
			m.DatabaseSizeBytes, m.SearchSchemaSizeBytes)
	}
}

// TestDatabaseMetricsDeadTuplesTracksActualChurn pins the one number this
// section exists to surface: search.passages' dead tuple count, whose
// staleness is the exact failure mode (spec §13.2) that cost real
// debugging time once already -- a stale VACUUM silently truncating HNSW
// index scans. ReplacePassages called twice for the same entry deletes the
// first passage row before inserting its replacement, which is exactly how
// dead tuples accumulate in production; this asserts DatabaseMetrics
// actually observes that churn, not a constant or an unrelated table's
// count.
func TestDatabaseMetricsDeadTuplesTracksActualChurn(t *testing.T) {
	s := testStore(t)
	if err := s.Migrate(); err != nil {
		t.Fatalf("migrate failed: %v", err)
	}

	ctx := context.Background()

	before, err := s.DatabaseMetrics(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	entryID := createTestEntry(t, s, "metrics-dead-tuples", "<p>Original.</p>")
	embedding := make([]float32, 384)
	if err := s.ReplacePassages(entryID, "hash-dead-1", []PassageRow{
		{Ordinal: 0, Text: "original passage", CharStart: 0, CharEnd: 17, Source: "content", Embedding: embedding},
	}); err != nil {
		t.Fatalf("unexpected error replacing passages (1st): %v", err)
	}
	// The second call DELETEs the row ReplacePassages just inserted before
	// inserting its replacement -- the same "old row becomes dead, still
	// occupies the index, until VACUUM reclaims it" mechanism the spec's
	// dead-tuple story is about.
	if err := s.ReplacePassages(entryID, "hash-dead-2", []PassageRow{
		{Ordinal: 0, Text: "replacement passage", CharStart: 0, CharEnd: 19, Source: "content", Embedding: embedding},
	}); err != nil {
		t.Fatalf("unexpected error replacing passages (2nd): %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	var after DatabaseMetrics
	for {
		after, err = s.DatabaseMetrics(ctx)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if after.DeadTuples > before.DeadTuples {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected DeadTuples to increase after a delete+insert churn within 5s; before=%d after=%d",
				before.DeadTuples, after.DeadTuples)
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Not just "it went up" -- it must be search.passages' own
	// n_dead_tup, read directly here, not some other table's (an
	// entry_index_state row is upserted by the very same ReplacePassages
	// calls, so a DeadTuples wired to the wrong table would rise here
	// too, and "increased" alone would not catch that).
	var wantDeadTuples int64
	if err := s.db.QueryRow(`
		SELECT n_dead_tup FROM pg_stat_user_tables
		WHERE schemaname = 'search' AND relname = 'passages'
	`).Scan(&wantDeadTuples); err != nil {
		t.Fatalf("unable to read search.passages' n_dead_tup directly: %v", err)
	}
	if after.DeadTuples != wantDeadTuples {
		t.Fatalf("expected DeadTuples %d (search.passages' own n_dead_tup, read directly), got %d",
			wantDeadTuples, after.DeadTuples)
	}
}
