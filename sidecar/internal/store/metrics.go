// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package store // import "miniflux.app/v2/sidecar/internal/store"

import (
	"context"
	"fmt"
)

// DatabaseMetrics is the database-size and health section of the admin
// page (spec §13.2). The spec's own §5.5 sizing estimate assumed 4-6
// passages per entry and 20-40 GB at a million entries; the measured
// corpus is 13 passages per entry and roughly 150-200 GB at a million --
// wrong by about 5x. This exists so the next such surprise is visible on
// the admin page instead of discovered when a disk fills.
type DatabaseMetrics struct {
	// DatabaseSizeBytes, SearchSchemaSizeBytes and HNSWIndexSizeBytes are
	// exact, current sizes in bytes (pg_database_size,
	// pg_total_relation_size summed over every object in the search
	// schema, and pg_relation_size on search.passages_embedding_idx
	// specifically -- the HNSW index is called out on its own because it
	// grows fastest and was folded into a table total nowhere else in the
	// spec's own sizing discussion).
	DatabaseSizeBytes     int64
	SearchSchemaSizeBytes int64
	HNSWIndexSizeBytes    int64

	// PassageCount and EntryCount are ESTIMATES from pg_class.reltuples,
	// not count(*) -- see DatabaseMetrics' own doc comment below for why.
	// CountsAreEstimated is always true today; it exists as a field
	// (rather than a doc comment only) so the admin page can render the
	// word "estimated" next to them instead of silently presenting an
	// approximation as an exact count.
	PassageCount       int64
	EntryCount         int64
	CountsAreEstimated bool

	// PassageCountUnknown/EntryCountUnknown are true when Postgres itself
	// has no estimate yet: pg_class.reltuples is -1 for a relation that
	// has never been ANALYZEd -- which search.passages hits for real
	// right at the START of a fresh backfill, exactly when an operator is
	// most likely watching this page. Without this, "-1, coalesced to 0"
	// is indistinguishable from a genuine zero, and the page would report
	// "the backfill is producing nothing" during the one window it is
	// working hardest. When either flag is true, the corresponding Count
	// field is 0, but that 0 is NOT a reading -- callers (buildView, the
	// template) must check the Unknown flag before trusting the Count.
	PassageCountUnknown bool
	EntryCountUnknown   bool

	// PassagesPerEntry is PassageCount divided by EntryCount -- EVERY
	// entry Miniflux has, indexed or not. This is deliberately NOT the
	// number the admin page renders (see web.buildView, which divides by
	// the backfill lane's own reconciled Indexed count instead): a
	// passage exists only for an entry that has actually been indexed,
	// so dividing by every entry in the corpus dilutes this ratio toward
	// zero on exactly the corpus spec §13.2's own number is most useful
	// for -- one still catching up on a large backlog, where most entries
	// have no passages yet for a completely ordinary reason. Kept as its
	// own, honestly-named field (passages per ENTRY, not per INDEXED
	// entry) rather than removed: it is still a well-defined, correct
	// ratio, just not the one the page needs, and store package tests
	// pin it against the pre-existing exact PassageCount/EntryCount
	// arithmetic. Zero when EntryCount is a genuine zero.
	// PassagesPerEntryUnknown is true whenever either count feeding this
	// ratio is itself unknown (see PassageCountUnknown/EntryCountUnknown)
	// -- a ratio computed from a 0 that is really "no estimate yet" is
	// not a small number, it is no number, and a caller must say so
	// rather than render 0.00.
	PassagesPerEntry        float64
	PassagesPerEntryUnknown bool

	// DeadTuples is search.passages' pg_stat_user_tables.n_dead_tup. Not
	// routine: a stale VACUUM silently truncates HNSW index scans, and in
	// this project 1,066 dead tuples once produced a constant 31-row
	// deficit in search results with no error anywhere. An operator
	// looking at a result count that seems short needs to be able to
	// check this number without already knowing that story.
	DeadTuples int64
}

// DatabaseMetrics reports the current database-size and health numbers
// for the admin page (spec §13.2), in one round trip. Requires Migrate()
// to have already run: it names search.passages and
// search.passages_embedding_idx directly, and errors if either does not
// exist.
//
// Every value here comes from Postgres' own catalog and statistics views
// -- pg_database_size, pg_total_relation_size, pg_relation_size,
// pg_class.reltuples and pg_stat_user_tables.n_dead_tup -- never from a
// scan of search.passages or entries themselves, and in particular never
// entries.content. Measured with EXPLAIN (ANALYZE, BUFFERS) against a
// live corpus: every one of those primitives is a metadata or statistics
// lookup whose cost does not grow with the size of entries or
// search.passages, and the combined query executes in single-
// digit milliseconds. That is what makes it safe to call on every render
// of a page that refreshes every 10 seconds (spec §9.4) with no caching
// at all -- unlike store.PendingEntryCount, which this project already
// had to fix exactly this mistake for once (see its own doc comment and
// PendingEntryCountApprox), nothing here detoasts or scans a table whose
// size is proportional to the corpus.
//
// PassageCount and EntryCount are the one place this trades exactness for
// that same scale-independence: an index-only-scan count(*) is cheap
// today, on a corpus in the low thousands, but its cost is proportional to
// the table's size, unlike every other number this method reads -- at a
// million entries and ~13M passages (spec §13.2's own extrapolation) a
// count(*) run every 10 seconds against the same database Miniflux itself
// serves is the PendingEntryCount mistake again. pg_class.reltuples is
// Postgres' own planner estimate, refreshed by ANALYZE (which autovacuum
// runs automatically past a churn threshold, without operator action);
// it can lag a burst of inserts by however long until the next ANALYZE,
// which is why CountsAreEstimated exists and why the admin page must
// label these two numbers "estimated" rather than presenting them as
// exact.
func (s *Store) DatabaseMetrics(ctx context.Context) (DatabaseMetrics, error) {
	var m DatabaseMetrics
	var passageTuples, entryTuples float64

	err := s.db.QueryRowContext(ctx, `
		SELECT
			pg_database_size(current_database()),
			COALESCE((
				SELECT sum(pg_total_relation_size(c.oid))
				FROM pg_class c
				JOIN pg_namespace n ON n.oid = c.relnamespace
				WHERE n.nspname = 'search'
			), 0),
			pg_relation_size('search.passages_embedding_idx'),
			COALESCE((SELECT reltuples FROM pg_class WHERE oid = 'search.passages'::regclass), 0),
			COALESCE((SELECT reltuples FROM pg_class WHERE oid = 'entries'::regclass), 0),
			COALESCE((
				SELECT n_dead_tup FROM pg_stat_user_tables
				WHERE schemaname = 'search' AND relname = 'passages'
			), 0)
	`).Scan(
		&m.DatabaseSizeBytes,
		&m.SearchSchemaSizeBytes,
		&m.HNSWIndexSizeBytes,
		&passageTuples,
		&entryTuples,
		&m.DeadTuples,
	)
	if err != nil {
		return DatabaseMetrics{}, fmt.Errorf("store: unable to read database metrics: %w", err)
	}

	// reltuples is -1 for a relation that has never been vacuumed or
	// analyzed -- Postgres' own "no estimate yet" sentinel, not a
	// negative count. That state must survive as PassageCountUnknown /
	// EntryCountUnknown, not collapse into a Count of 0: a 0 here is a
	// reading (the table really is empty, and ANALYZE has said so), while
	// -1 is the absence of a reading, and the two look identical to an
	// operator unless this method keeps them apart (see the struct's own
	// doc comment).
	if passageTuples < 0 {
		m.PassageCountUnknown = true
		passageTuples = 0
	}
	if entryTuples < 0 {
		m.EntryCountUnknown = true
		entryTuples = 0
	}
	m.PassageCount = int64(passageTuples)
	m.EntryCount = int64(entryTuples)
	m.CountsAreEstimated = true

	switch {
	case m.PassageCountUnknown || m.EntryCountUnknown:
		// A ratio built from a 0 that is really "unknown" is not a small
		// ratio, it is no ratio -- do not compute one.
		m.PassagesPerEntryUnknown = true
	case m.EntryCount > 0:
		m.PassagesPerEntry = float64(m.PassageCount) / float64(m.EntryCount)
	}

	return m, nil
}
