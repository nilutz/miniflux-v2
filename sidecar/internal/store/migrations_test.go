// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package store // import "miniflux.app/v2/sidecar/internal/store"

import (
	"os"
	"testing"
)

func testStore(t *testing.T) *Store {
	t.Helper()

	dsn := os.Getenv("SIDECAR_DATABASE_URL")
	if dsn == "" {
		t.Skip("SIDECAR_DATABASE_URL is not set, skipping database test")
	}

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
