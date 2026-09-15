// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package store // import "miniflux.app/v2/sidecar/internal/store"

import (
	"database/sql"
	"strings"
	"testing"

	"miniflux.app/v2/sidecar/internal/testdb"
)

func testStore(t *testing.T) *Store {
	t.Helper()

	dsn := testdb.DSN(t)

	s, err := New(dsn)
	if err != nil {
		t.Fatalf("unable to open database: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	return s
}

func TestMigrateCreatesSearchSchema(t *testing.T) {
	s := testStore(t)

	if err := s.Migrate(); err != nil {
		t.Fatalf("migrate failed: %v", err)
	}

	for _, table := range []string{"passages", "entry_index_state"} {
		var exists bool
		err := s.db.QueryRow(`
			SELECT EXISTS (
				SELECT 1 FROM information_schema.tables
				WHERE table_schema='search' AND table_name=$1
			)`, table).Scan(&exists)
		if err != nil {
			t.Fatalf("unable to inspect tables: %v", err)
		}
		if !exists {
			t.Fatalf("expected table search.%s to exist", table)
		}
	}
}

func TestMigrateInstallsBothExtensions(t *testing.T) {
	s := testStore(t)

	if err := s.Migrate(); err != nil {
		t.Fatalf("migrate failed: %v", err)
	}

	for _, extension := range []string{"vector", "pg_search"} {
		var installed bool
		err := s.db.QueryRow(
			`SELECT EXISTS (SELECT 1 FROM pg_extension WHERE extname=$1)`, extension,
		).Scan(&installed)
		if err != nil {
			t.Fatalf("unable to inspect extensions: %v", err)
		}
		if !installed {
			t.Fatalf("expected extension %q to be installed; is this the ParadeDB image?", extension)
		}
	}
}

func TestMigrateCreatesIndexes(t *testing.T) {
	s := testStore(t)

	if err := s.Migrate(); err != nil {
		t.Fatalf("migrate failed: %v", err)
	}

	// These are built in migration 2, separately from the table in
	// migration 1, precisely so a reindex can drop and rebuild them
	// without touching the data — verify they actually exist.
	for _, index := range []string{"passages_embedding_idx", "passages_bm25_idx"} {
		var exists bool
		err := s.db.QueryRow(`
			SELECT EXISTS (
				SELECT 1 FROM pg_indexes
				WHERE schemaname='search' AND tablename='passages' AND indexname=$1
			)`, index).Scan(&exists)
		if err != nil {
			t.Fatalf("unable to inspect indexes: %v", err)
		}
		if !exists {
			t.Fatalf("expected index search.%s on passages to exist", index)
		}
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	s := testStore(t)

	if err := s.Migrate(); err != nil {
		t.Fatalf("first migrate failed: %v", err)
	}
	if err := s.Migrate(); err != nil {
		t.Fatalf("second migrate failed: %v", err)
	}
}

// TestMigrateAddsPassagesSourceColumn pins migration 3: search.passages
// gains a NOT NULL 'source' column defaulting to 'content', so pre-task-1.5
// rows (all of which are body passages) remain valid without a backfill of
// their own, and the indexer can write 'title' rows going forward.
func TestMigrateAddsPassagesSourceColumn(t *testing.T) {
	s := testStore(t)

	if err := s.Migrate(); err != nil {
		t.Fatalf("migrate failed: %v", err)
	}

	var dataType, isNullable string
	var columnDefault sql.NullString
	err := s.db.QueryRow(`
		SELECT data_type, is_nullable, column_default
		FROM information_schema.columns
		WHERE table_schema='search' AND table_name='passages' AND column_name='source'
	`).Scan(&dataType, &isNullable, &columnDefault)
	if err != nil {
		t.Fatalf("expected search.passages.source to exist: %v", err)
	}
	if dataType != "text" {
		t.Fatalf("expected source to be text, got %q", dataType)
	}
	if isNullable != "NO" {
		t.Fatalf("expected source to be NOT NULL, got is_nullable=%q", isNullable)
	}
	if !strings.Contains(columnDefault.String, "content") {
		t.Fatalf("expected source's default to mention 'content', got %q", columnDefault.String)
	}
}

// passagesReloptions reads back search.passages' storage parameters as a
// key/value map, e.g. {"autovacuum_vacuum_threshold": "500"}.
func passagesReloptions(t *testing.T, s *Store) map[string]string {
	t.Helper()

	rows, err := s.db.Query(`
		SELECT unnest(reloptions) FROM pg_class
		WHERE relname = 'passages' AND relnamespace = 'search'::regnamespace
	`)
	if err != nil {
		t.Fatalf("unable to query reloptions: %v", err)
	}
	defer rows.Close()

	opts := map[string]string{}
	for rows.Next() {
		var opt string
		if err := rows.Scan(&opt); err != nil {
			t.Fatalf("unable to scan reloption: %v", err)
		}
		key, value, ok := strings.Cut(opt, "=")
		if !ok {
			t.Fatalf("unexpected reloption format: %q", opt)
		}
		opts[key] = value
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reloptions rows error: %v", err)
	}
	return opts
}

// assertPassagesAutovacuumOptions checks the exact values task 12 sets, not
// merely that reloptions is non-empty: a test that only checked
// non-emptiness would pass whether or not the values were right, and would
// pass if only one of the two settings had been applied.
func assertPassagesAutovacuumOptions(t *testing.T, s *Store) {
	t.Helper()

	opts := passagesReloptions(t, s)

	if got, want := opts["autovacuum_vacuum_scale_factor"], "0.05"; got != want {
		t.Fatalf("autovacuum_vacuum_scale_factor = %q, want %q (full reloptions: %v)", got, want, opts)
	}
	if got, want := opts["autovacuum_vacuum_threshold"], "500"; got != want {
		t.Fatalf("autovacuum_vacuum_threshold = %q, want %q (full reloptions: %v)", got, want, opts)
	}
	if len(opts) != 2 {
		t.Fatalf("expected exactly 2 reloptions on search.passages, got %v", opts)
	}
}

// TestMigrateSetsPassagesAutovacuumOptions pins migration 4: search.passages
// fires autovacuum at 500 + 0.05*rows instead of the cluster default's
// 50 + 0.2*rows, so it would have caught the 1,066-dead-tuple incident
// documented in README.md instead of sitting just under its trigger.
func TestMigrateSetsPassagesAutovacuumOptions(t *testing.T) {
	s := testStore(t)

	if err := s.Migrate(); err != nil {
		t.Fatalf("migrate failed: %v", err)
	}

	assertPassagesAutovacuumOptions(t, s)
}

// TestMigrateAutovacuumOptionsStableOnRerun goes past the generic
// idempotency check above to confirm the specific values task 12 sets
// don't drift or duplicate across a second Migrate() call -- the sidecar
// runs migrations on every start, so this path runs constantly in
// practice.
func TestMigrateAutovacuumOptionsStableOnRerun(t *testing.T) {
	s := testStore(t)

	if err := s.Migrate(); err != nil {
		t.Fatalf("first migrate failed: %v", err)
	}
	if err := s.Migrate(); err != nil {
		t.Fatalf("second migrate failed: %v", err)
	}

	assertPassagesAutovacuumOptions(t, s)
}

// TestMigrateAppliesTuningToExistingDatabase simulates the path the task
// 12 brief calls out as the one that actually breaks: a database that
// already ran migrations 1-3 (the shape every already-deployed sidecar is
// on today) before task 12's two migrations existed. ALTER TABLE ... SET
// and dropping/rebuilding the index both require search.passages and
// passages_embedding_idx to already exist -- unlike a from-scratch
// Migrate() call, which creates and tunes them in the same run and could
// silently hide an ordering bug.
func TestMigrateAppliesTuningToExistingDatabase(t *testing.T) {
	s := testStore(t)

	// testStore's database is shared across this file's tests, and every
	// other test here already leaves it fully migrated. Drop and
	// recreate the search schema so this test starts from a genuinely
	// empty database before manually replaying only migrations 1-3.
	if _, err := s.db.Exec(`DROP SCHEMA IF EXISTS search CASCADE`); err != nil {
		t.Fatalf("drop schema: %v", err)
	}

	if _, err := s.db.Exec(`CREATE SCHEMA IF NOT EXISTS search`); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS search.schema_version (version int not null)`); err != nil {
		t.Fatalf("create schema_version: %v", err)
	}

	tx, err := s.db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	for i := range 3 {
		if err := migrations[i](tx); err != nil {
			tx.Rollback()
			t.Fatalf("pre-migration %d: %v", i, err)
		}
	}
	if _, err := tx.Exec(`INSERT INTO search.schema_version (version) VALUES (3)`); err != nil {
		tx.Rollback()
		t.Fatalf("seed schema_version: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	if err := s.Migrate(); err != nil {
		t.Fatalf("migrate failed: %v", err)
	}

	var version int
	if err := s.db.QueryRow(`SELECT version FROM search.schema_version`).Scan(&version); err != nil {
		t.Fatalf("unable to read schema version: %v", err)
	}
	if version != schemaVersion {
		t.Fatalf("schema_version = %d, want %d (fresh and existing databases must land on the same version)", version, schemaVersion)
	}

	assertPassagesAutovacuumOptions(t, s)
}

// TestMigrateDoesNotLeakMaintenanceWorkMem is the SET LOCAL guarantee: the
// migration that rebuilds passages_embedding_idx raises
// maintenance_work_mem for its own transaction only. A plain SET here
// would leak the setting onto the pooled connection for whatever query
// runs next -- this codebase already shipped one bug from a GUC applied
// at the wrong scope (an hnsw.ef_search fix wrapped in a MATERIALIZED CTE
// that silently never ran), so this is asserted directly rather than
// trusted.
func TestMigrateDoesNotLeakMaintenanceWorkMem(t *testing.T) {
	s := testStore(t)

	// Force the pool down to one physical connection so the query below
	// is guaranteed to reuse the exact connection the migration
	// transaction ran on -- the leak this guards against is a pooled
	// connection carrying a session-level SET past the transaction that
	// set it.
	s.db.SetMaxOpenConns(1)

	var baseline string
	if err := s.db.QueryRow(`SHOW maintenance_work_mem`).Scan(&baseline); err != nil {
		t.Fatalf("unable to read baseline maintenance_work_mem: %v", err)
	}

	if err := s.Migrate(); err != nil {
		t.Fatalf("migrate failed: %v", err)
	}

	var after string
	if err := s.db.QueryRow(`SHOW maintenance_work_mem`).Scan(&after); err != nil {
		t.Fatalf("unable to read maintenance_work_mem after migrate: %v", err)
	}
	if after != baseline {
		t.Fatalf("maintenance_work_mem leaked past the migration transaction: got %q, want baseline %q back (SET LOCAL should confine it to the migration's own transaction)", after, baseline)
	}
	if after == "1GB" {
		t.Fatalf("maintenance_work_mem is '1GB' on the connection after migrate -- the migration's SET LOCAL leaked")
	}

	var workers string
	if err := s.db.QueryRow(`SHOW max_parallel_maintenance_workers`).Scan(&workers); err != nil {
		t.Fatalf("unable to read max_parallel_maintenance_workers: %v", err)
	}
	if workers == "4" {
		t.Fatalf("max_parallel_maintenance_workers is '4' on the connection after migrate -- the migration's SET LOCAL leaked")
	}
}
