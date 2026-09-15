// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package store // import "miniflux.app/v2/sidecar/internal/store"

import (
	"database/sql"
	"strings"
	"testing"

	"miniflux.app/v2/sidecar/internal/testdb"
)

// fixedVector returns a dims-length embedding filled with fill, formatted
// as a pgvector text literal via passages.go's own vectorLiteral -- for
// seeding a real vector(N) column directly via SQL in tests that need
// actual, non-empty embedding data rather than a fresh, empty table.
func fixedVector(dims int, fill float32) string {
	v := make([]float32, dims)
	for i := range v {
		v[i] = fill
	}
	return vectorLiteral(v)
}

// embeddingColumnType reads search.passages.embedding's actual formatted
// type ("vector(768)", say) straight from the catalog -- vector(N)'s width
// isn't visible through information_schema.columns the way a varchar(N)'s
// is, so this is the one reliable way to assert on it.
func embeddingColumnType(t *testing.T, s *Store) string {
	t.Helper()

	var formatted string
	err := s.db.QueryRow(`
		SELECT format_type(atttypid, atttypmod)
		FROM pg_attribute
		WHERE attrelid = 'search.passages'::regclass AND attname = 'embedding'
	`).Scan(&formatted)
	if err != nil {
		t.Fatalf("unable to inspect search.passages.embedding's type: %v", err)
	}
	return formatted
}

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
// gains a NOT NULL 'source' column defaulting to 'content', so pre-existing
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

// assertPassagesAutovacuumOptions checks the exact values migration 4 sets,
// not merely that reloptions is non-empty: a test that only checked
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
// idempotency check above to confirm the specific values migration 4 sets
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

// TestMigrateAppliesTuningToExistingDatabase simulates the path that
// actually breaks: a database that already ran migrations 1-3 (the shape
// every already-deployed sidecar is on today) before migration 4 existed.
// ALTER TABLE ... SET
// requires search.passages to already exist -- unlike a from-scratch
// Migrate() call, which creates and tunes it in the same run and could
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

// TestMigrateFromCrashLoopedVersionFourIsANoOp is the recovery path for
// anyone who ran the now-reverted migration 5 (the HNSW index-rebuild
// migration that crash-looped a container against its 64 MiB /dev/shm).
// That migration's autovacuum predecessor (migration 4) commits and
// advances schema_version to 4 in its own transaction before migration 5
// ever ran, so a sidecar that hit migration 5's shared-memory failure was
// left with schema_version = 4, not damaged or partially migrated.
//
// Migration 5 (search.embedder_settings) was appended after this test was
// written, so a from-scratch Migrate() no longer stops at 4 the way it
// did then -- seeding has to replay migrations 1-4 by hand
// (mirroring TestMigrateAppliesTuningToExistingDatabase's own technique
// for the same reason) rather than relying on a fresh Migrate() call to
// land there on its own. The behaviour under test is unchanged: Migrate()
// against a database already fully migrated up to whatever schemaVersion
// currently is must succeed with no further work and no error.
func TestMigrateFromCrashLoopedVersionFourIsANoOp(t *testing.T) {
	s := testStore(t)

	// testStore's database is shared across this file's tests; start from
	// a genuinely empty schema before manually replaying migrations 1-4.
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
	for i := range 4 {
		if err := migrations[i](tx); err != nil {
			tx.Rollback()
			t.Fatalf("pre-migration %d: %v", i, err)
		}
	}
	if _, err := tx.Exec(`INSERT INTO search.schema_version (version) VALUES (4)`); err != nil {
		tx.Rollback()
		t.Fatalf("seed schema_version: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	var seededVersion int
	if err := s.db.QueryRow(`SELECT version FROM search.schema_version`).Scan(&seededVersion); err != nil {
		t.Fatalf("unable to read seeded schema version: %v", err)
	}
	if seededVersion != 4 {
		t.Fatalf("seeded schema_version = %d, want 4 (this test's premise no longer holds)", seededVersion)
	}

	// The realistic recovery case: Migrate() runs, as it does on every
	// sidecar start, against a database seeded at exactly the
	// crash-looped state above.
	if err := s.Migrate(); err != nil {
		t.Fatalf("migrate against an already-at-version-4 database failed: %v", err)
	}

	var version int
	if err := s.db.QueryRow(`SELECT version FROM search.schema_version`).Scan(&version); err != nil {
		t.Fatalf("unable to read schema version: %v", err)
	}
	if version != schemaVersion {
		t.Fatalf("schema_version = %d, want %d", version, schemaVersion)
	}

	assertPassagesAutovacuumOptions(t, s)
}

// TestSchemaVersionIsPinned asserts the literal migration count rather than
// just len(migrations): a migration closure silently deleted from the
// array (as happened when the since-reverted index-rebuild migration was
// removed to test this) drops
// schemaVersion without any other test here failing -- every other test
// asserts against schemaVersion itself, which moves right along with the
// bug. This is the one check that has to hardcode the number so a
// disappearing migration is caught rather than silently accepted.
//
// Update this literal deliberately, in the same commit, whenever a
// migration is appended or (never) removed.
func TestSchemaVersionIsPinned(t *testing.T) {
	const want = 6
	if schemaVersion != want {
		t.Fatalf("schemaVersion = %d, want %d -- a migration was added or removed without updating this pinned assertion", schemaVersion, want)
	}
}

// TestMigrateWidensEmbeddingColumnTo768 pins migration 5: a from-scratch
// database ends with search.passages.embedding at vector(768), not the
// vector(384) migration 0 originally created --
// this package's fixed-shape query pattern would otherwise silently keep
// writing at the wrong width with no compile-time signal.
func TestMigrateWidensEmbeddingColumnTo768(t *testing.T) {
	s := testStore(t)

	if err := s.Migrate(); err != nil {
		t.Fatalf("migrate failed: %v", err)
	}

	if got, want := embeddingColumnType(t, s), "vector(768)"; got != want {
		t.Fatalf("search.passages.embedding = %q, want %q", got, want)
	}
}

// TestMigrateWidensExistingPopulatedColumn guards against a specific trap:
// a test asserting the column is vector(768) passes
// on a fresh database whether or not the *widening* path actually works,
// because CREATE TABLE never has to reconcile incompatible existing data.
// This seeds a database at schema_version 5 -- every already-deployed
// sidecar's shape before this task, mirroring
// TestMigrateAppliesTuningToExistingDatabase's own technique -- with a
// real, populated vector(384) row, then migrates it: the path that
// actually exercises ALTER COLUMN ... TYPE against non-empty data.
func TestMigrateWidensExistingPopulatedColumn(t *testing.T) {
	s := testStore(t)

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
	for i := range 5 {
		if err := migrations[i](tx); err != nil {
			tx.Rollback()
			t.Fatalf("pre-migration %d: %v", i, err)
		}
	}
	if _, err := tx.Exec(`INSERT INTO search.schema_version (version) VALUES (5)`); err != nil {
		tx.Rollback()
		t.Fatalf("seed schema_version: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// A real, populated vector(384) row -- the data this migration must
	// not choke on, unlike a fresh empty table.
	if _, err := s.db.Exec(`
		INSERT INTO search.passages (entry_id, ordinal, text, char_start, char_end, embedding)
		VALUES (1, 0, 'pre-nomic passage', 0, 18, $1::vector)
	`, fixedVector(384, 0.25)); err != nil {
		t.Fatalf("seed a populated vector(384) row: %v", err)
	}

	if err := s.Migrate(); err != nil {
		t.Fatalf("migrate against an existing populated vector(384) column failed: %v", err)
	}

	var version int
	if err := s.db.QueryRow(`SELECT version FROM search.schema_version`).Scan(&version); err != nil {
		t.Fatalf("unable to read schema version: %v", err)
	}
	if version != schemaVersion {
		t.Fatalf("schema_version = %d, want %d", version, schemaVersion)
	}

	if got, want := embeddingColumnType(t, s), "vector(768)"; got != want {
		t.Fatalf("search.passages.embedding = %q, want %q", got, want)
	}

	// The pre-existing row must have survived the migration -- widening a
	// column is not a delete, even though its 384-d embedding could not be
	// reinterpreted as 768-d and was necessarily discarded.
	var count int
	if err := s.db.QueryRow(`SELECT count(*) FROM search.passages`).Scan(&count); err != nil {
		t.Fatalf("count passages: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected the pre-existing row to survive the migration, got %d rows", count)
	}

	// A genuinely 768-d vector must write cleanly post-migration -- proof
	// the column actually accepts the new width, not merely reports it.
	if _, err := s.db.Exec(`
		UPDATE search.passages SET embedding = $1::vector WHERE entry_id = 1 AND ordinal = 0
	`, fixedVector(768, 0.25)); err != nil {
		t.Fatalf("write a 768-d vector into the widened column: %v", err)
	}

	// The index must have been rebuilt against the new width, not left
	// dangling/invalid from the DROP that preceded it.
	var indexValid bool
	if err := s.db.QueryRow(`
		SELECT indisvalid FROM pg_index
		WHERE indexrelid = 'search.passages_embedding_idx'::regclass
	`).Scan(&indexValid); err != nil {
		t.Fatalf("unable to inspect passages_embedding_idx: %v", err)
	}
	if !indexValid {
		t.Fatal("expected passages_embedding_idx to be valid after the rebuild")
	}

	// Idempotency: migrating an already-migrated database again (the
	// sidecar does this on every start) must not error and must leave the
	// column at the same width.
	if err := s.Migrate(); err != nil {
		t.Fatalf("second migrate failed: %v", err)
	}
	if got, want := embeddingColumnType(t, s), "vector(768)"; got != want {
		t.Fatalf("after second migrate: search.passages.embedding = %q, want %q", got, want)
	}
}

// TestMigrationDoesNotLeakMaintenanceSettings is migration 5's SET LOCAL
// guarantee: max_parallel_maintenance_workers and maintenance_work_mem are
// both changed for the migration's own transaction only. A prior version of
// this exact migration set max_parallel_maintenance_workers globally (via
// SET rather than SET LOCAL) and crash-looped the sidecar's HNSW build
// against a 64 MiB /dev/shm -- this pins the fix, on the same physical
// connection the migration itself ran on (SetMaxOpenConns(1)), which is
// what actually proves the setting didn't leak.
func TestMigrationDoesNotLeakMaintenanceSettings(t *testing.T) {
	s := testStore(t)
	s.db.SetMaxOpenConns(1)

	var memBefore, workersBefore string
	if err := s.db.QueryRow(`SHOW maintenance_work_mem`).Scan(&memBefore); err != nil {
		t.Fatalf("read maintenance_work_mem before migrate: %v", err)
	}
	if err := s.db.QueryRow(`SHOW max_parallel_maintenance_workers`).Scan(&workersBefore); err != nil {
		t.Fatalf("read max_parallel_maintenance_workers before migrate: %v", err)
	}

	if err := s.Migrate(); err != nil {
		t.Fatalf("migrate failed: %v", err)
	}

	var memAfter, workersAfter string
	if err := s.db.QueryRow(`SHOW maintenance_work_mem`).Scan(&memAfter); err != nil {
		t.Fatalf("read maintenance_work_mem after migrate: %v", err)
	}
	if err := s.db.QueryRow(`SHOW max_parallel_maintenance_workers`).Scan(&workersAfter); err != nil {
		t.Fatalf("read max_parallel_maintenance_workers after migrate: %v", err)
	}

	if memAfter != memBefore {
		t.Fatalf("maintenance_work_mem leaked past the migration transaction: before=%q after=%q", memBefore, memAfter)
	}
	if workersAfter != workersBefore {
		t.Fatalf("max_parallel_maintenance_workers leaked past the migration transaction: before=%q after=%q", workersBefore, workersAfter)
	}
}
