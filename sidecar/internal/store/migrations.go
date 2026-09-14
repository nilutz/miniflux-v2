// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package store // import "miniflux.app/v2/sidecar/internal/store"

import (
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
)

// migrations are owned entirely by the sidecar and tracked in
// search.schema_version, which is independent of both Miniflux's
// schema_version and the fork's fork_schema_version. Order matters: append
// only, never reorder.
var migrations = [...]func(tx *sql.Tx) error{
	func(tx *sql.Tx) error {
		// vector, its column type, and its operator class are all fully
		// schema-qualified so this migration never depends on the
		// connection's search_path (some hardened Postgres installs narrow
		// the default search_path to exclude public). pg_search's own
		// control file always installs it into the "paradedb" schema
		// regardless of search_path, so it needs no such qualification.
		_, err := tx.Exec(`
			CREATE EXTENSION IF NOT EXISTS vector SCHEMA public;
			CREATE EXTENSION IF NOT EXISTS pg_search;

			CREATE TABLE search.passages (
				id         bigserial PRIMARY KEY,
				entry_id   bigint NOT NULL,
				ordinal    int NOT NULL,
				text       text NOT NULL,
				char_start int NOT NULL,
				char_end   int NOT NULL,
				embedding  public.vector(384),
				UNIQUE (entry_id, ordinal)
			);

			CREATE INDEX passages_entry_id_idx ON search.passages (entry_id);

			CREATE TABLE search.entry_index_state (
				entry_id     bigint PRIMARY KEY,
				content_hash text NOT NULL,
				indexed_at   timestamptz,
				status       text NOT NULL,
				reason       text
			);

			CREATE INDEX entry_index_state_status_idx
				ON search.entry_index_state (status);
		`)
		return err
	},
	func(tx *sql.Tx) error {
		// Built separately from the table so a reindex can drop and rebuild
		// them without touching the data. vector_cosine_ops is schema-
		// qualified for the same search_path-independence reason as above.
		_, err := tx.Exec(`
			CREATE INDEX passages_embedding_idx
				ON search.passages USING hnsw (embedding public.vector_cosine_ops);

			CREATE INDEX passages_bm25_idx
				ON search.passages USING bm25 (id, text)
				WITH (key_field='id');
		`)
		return err
	},
}

var schemaVersion = len(migrations)

// Migrate creates the search schema and applies pending migrations.
func (s *Store) Migrate() error {
	if _, err := s.db.Exec(`CREATE SCHEMA IF NOT EXISTS search`); err != nil {
		return fmt.Errorf("store: unable to create schema: %w", err)
	}
	if _, err := s.db.Exec(
		`CREATE TABLE IF NOT EXISTS search.schema_version (version int not null)`,
	); err != nil {
		return fmt.Errorf("store: unable to create schema_version: %w", err)
	}

	var currentVersion int
	if err := s.db.QueryRow(`SELECT version FROM search.schema_version`).Scan(&currentVersion); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("store: unable to read schema version: %w", err)
	}

	slog.Info("Running sidecar migrations",
		slog.Int("current_version", currentVersion),
		slog.Int("latest_version", schemaVersion),
	)

	for version := currentVersion; version < schemaVersion; version++ {
		newVersion := version + 1

		tx, err := s.db.Begin()
		if err != nil {
			return fmt.Errorf("[sidecar migration v%d] %w", newVersion, err)
		}

		if err := migrations[version](tx); err != nil {
			tx.Rollback()
			return fmt.Errorf("[sidecar migration v%d] %w", newVersion, err)
		}
		if _, err := tx.Exec(`TRUNCATE search.schema_version`); err != nil {
			tx.Rollback()
			return fmt.Errorf("[sidecar migration v%d] %w", newVersion, err)
		}
		if _, err := tx.Exec(
			`INSERT INTO search.schema_version (version) VALUES ($1)`, newVersion,
		); err != nil {
			tx.Rollback()
			return fmt.Errorf("[sidecar migration v%d] %w", newVersion, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("[sidecar migration v%d] %w", newVersion, err)
		}
	}

	return nil
}
