// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package database // import "miniflux.app/v2/internal/database"

import (
	"database/sql"
	"os"
	"testing"

	_ "github.com/lib/pq"
)

// testDB returns a connection to the integration test database, or skips the
// test when DATABASE_URL is not configured.
func testDB(t *testing.T) *sql.DB {
	t.Helper()

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL is not set, skipping database integration test")
	}

	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("unable to open database: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	return db
}

func TestMigrateForkDoesNotTouchUpstreamSchemaVersion(t *testing.T) {
	db := testDB(t)

	if err := Migrate(db); err != nil {
		t.Fatalf("upstream migrations failed: %v", err)
	}

	var upstreamBefore int
	if err := db.QueryRow(`SELECT version FROM schema_version`).Scan(&upstreamBefore); err != nil {
		t.Fatalf("unable to read schema_version: %v", err)
	}

	if err := MigrateFork(db); err != nil {
		t.Fatalf("fork migrations failed: %v", err)
	}

	var forkVersion int
	if err := db.QueryRow(`SELECT coalesce(max(version), 0) FROM fork_schema_version`).Scan(&forkVersion); err != nil {
		t.Fatalf("unable to read fork_schema_version: %v", err)
	}
	if forkVersion != forkSchemaVersion {
		t.Fatalf("expected fork schema version %d, got %d", forkSchemaVersion, forkVersion)
	}

	var upstreamAfter int
	if err := db.QueryRow(`SELECT version FROM schema_version`).Scan(&upstreamAfter); err != nil {
		t.Fatalf("unable to read schema_version: %v", err)
	}
	if upstreamAfter != upstreamBefore {
		t.Fatalf("fork migrations changed upstream schema_version from %d to %d", upstreamBefore, upstreamAfter)
	}
}

func TestMigrateForkIsIdempotent(t *testing.T) {
	db := testDB(t)

	if err := Migrate(db); err != nil {
		t.Fatalf("upstream migrations failed: %v", err)
	}
	if err := MigrateFork(db); err != nil {
		t.Fatalf("first MigrateFork run failed: %v", err)
	}
	if err := MigrateFork(db); err != nil {
		t.Fatalf("second MigrateFork run failed: %v", err)
	}
}

func TestForkMigrationAddsFullTextFetchedAtColumn(t *testing.T) {
	db := testDB(t)

	if err := Migrate(db); err != nil {
		t.Fatalf("upstream migrations failed: %v", err)
	}
	if err := MigrateFork(db); err != nil {
		t.Fatalf("fork migrations failed: %v", err)
	}

	var exists bool
	err := db.QueryRow(`
		SELECT EXISTS (
			SELECT 1 FROM information_schema.columns
			WHERE table_name='entries' AND column_name='full_text_fetched_at'
		)
	`).Scan(&exists)
	if err != nil {
		t.Fatalf("unable to inspect columns: %v", err)
	}
	if !exists {
		t.Fatal("expected entries.full_text_fetched_at to exist")
	}
}
