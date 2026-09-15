// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package store // import "miniflux.app/v2/sidecar/internal/store"

import (
	"context"
	"testing"
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
	embedding := make([]float32, 768)
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

	// (Fix round 2 finding 2.) DatabaseSizeBytes previously had no
	// independent re-read-and-compare of its own, unlike the two checks
	// below -- a hardcoded LARGE constant (e.g. 9999999999) passed every
	// other assertion in this file, including the containment checks
	// further down, since nothing capped it from above. Read
	// pg_database_size directly here and require an exact match, the same
	// way the HNSW index size and schema size already are.
	var wantDatabaseSize int64
	if err := s.db.QueryRow(`SELECT pg_database_size(current_database())`).Scan(&wantDatabaseSize); err != nil {
		t.Fatalf("unable to read the database size directly: %v", err)
	}
	if m.DatabaseSizeBytes != wantDatabaseSize {
		t.Fatalf("expected DatabaseSizeBytes %d (read directly), got %d", wantDatabaseSize, m.DatabaseSizeBytes)
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
	embedding := make([]float32, 768)
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

	// pg_stat_user_tables is fed from shared-memory statistics that a
	// backend flushes on its own schedule, not necessarily the instant a
	// transaction commits -- pg_stat_force_next_flush() (present on this
	// PG18 instance) forces the NEXT flush from THIS session to happen
	// immediately, making the read below deterministic with no sleep or
	// polling (fix round 2 finding 3: a 5-second polling loop was here
	// before and was unnecessary).
	if _, err := s.db.Exec(`SELECT pg_stat_force_next_flush()`); err != nil {
		t.Fatalf("unable to force a stats flush: %v", err)
	}

	after, err := s.DatabaseMetrics(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if after.DeadTuples <= before.DeadTuples {
		t.Fatalf("expected DeadTuples to increase after a delete+insert churn; before=%d after=%d",
			before.DeadTuples, after.DeadTuples)
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

// TestDatabaseMetricsDistinguishesUnanalyzedFromRealZero pins fix round 2
// finding 1: pg_class.reltuples is -1 -- Postgres' OWN sentinel -- for a
// relation that has never been ANALYZEd, which search.passages hits for
// real right at the start of a fresh backfill, exactly when an operator
// is most likely watching this page. Before this fix, -1 was silently
// coalesced to 0, making "no estimate yet" indistinguishable from "the
// backfill is producing nothing".
//
// Directly setting pg_class.reltuples (legal for a superuser, and the
// only deterministic way to reproduce Postgres' own -1 state without
// waiting on autovacuum or dropping the shared search.passages table
// every other test in this package assumes is already analyzed) exercises
// the real SELECT/Scan path this method actually runs, not a mocked
// stand-in for it.
func TestDatabaseMetricsDistinguishesUnanalyzedFromRealZero(t *testing.T) {
	s := testStore(t)
	if err := s.Migrate(); err != nil {
		t.Fatalf("migrate failed: %v", err)
	}
	ctx := context.Background()

	// Restore normal statistics for every test that runs after this one
	// in the same package binary -- this simulates a transient Postgres
	// state, not a real change to the table's contents, and must not
	// leak into sibling tests that assume search.passages has been
	// analyzed.
	t.Cleanup(func() {
		if _, err := s.db.Exec(`ANALYZE search.passages`); err != nil {
			t.Logf("cleanup: unable to restore search.passages statistics: %v", err)
		}
	})

	if _, err := s.db.Exec(`UPDATE pg_class SET reltuples = -1 WHERE oid = 'search.passages'::regclass`); err != nil {
		t.Fatalf("unable to simulate a never-analyzed table: %v", err)
	}

	m, err := s.DatabaseMetrics(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !m.PassageCountUnknown {
		t.Fatal("expected PassageCountUnknown = true when pg_class.reltuples reports -1 (never analyzed)")
	}
	if m.PassageCount != 0 {
		t.Fatalf("expected PassageCount = 0 as the placeholder value while unknown, got %d", m.PassageCount)
	}
	if !m.PassagesPerEntryUnknown {
		t.Fatal("expected PassagesPerEntryUnknown = true when the passage count itself is unknown")
	}
	if m.PassagesPerEntry != 0 {
		t.Fatalf("expected PassagesPerEntry = 0 as the placeholder value while unknown, got %v", m.PassagesPerEntry)
	}
}

// TestDatabaseMetricsDoesNotMarkARealZeroAsUnknown is the other half of
// the distinguishability finding 1 asks for: reltuples = 0 (a genuine,
// analyzed reading of an empty table) must NOT set the Unknown flags --
// only Postgres' own -1 sentinel does. A fix that turned "unknown" into
// "anything <= 0" would pass the test above but fail this one.
func TestDatabaseMetricsDoesNotMarkARealZeroAsUnknown(t *testing.T) {
	s := testStore(t)
	if err := s.Migrate(); err != nil {
		t.Fatalf("migrate failed: %v", err)
	}
	ctx := context.Background()

	t.Cleanup(func() {
		if _, err := s.db.Exec(`ANALYZE search.passages`); err != nil {
			t.Logf("cleanup: unable to restore search.passages statistics: %v", err)
		}
	})

	if _, err := s.db.Exec(`UPDATE pg_class SET reltuples = 0 WHERE oid = 'search.passages'::regclass`); err != nil {
		t.Fatalf("unable to simulate a genuinely-empty, analyzed table: %v", err)
	}

	m, err := s.DatabaseMetrics(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if m.PassageCountUnknown {
		t.Fatal("expected PassageCountUnknown = false for a genuine zero (reltuples=0), not -1 -- these must not collapse into each other")
	}
	if m.PassageCount != 0 {
		t.Fatalf("expected PassageCount = 0, got %d", m.PassageCount)
	}
}
