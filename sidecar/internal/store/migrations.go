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
	func(tx *sql.Tx) error {
		// Entry titles are now indexed as their own passages
		// (ordinal 0, source='title'), alongside body passages
		// (source='content'). DEFAULT 'content' backfills every existing
		// row correctly with no data migration of its own: every passage
		// written before this column existed is, definitionally, a body
		// passage.
		//
		// passages_bm25_idx deliberately still covers only (id, text), not
		// (id, text, source): no BM25 query today filters or weights by
		// source, so widening it now would be paying an index-size and
		// reindex cost against a need that hasn't shown yet. Filtering
		// by source, if it turns out to matter, can fall back to the same
		// "overfetch from BM25, then filter in the join" pattern already
		// used for feed/date/status filters -- or the index can be
		// rebuilt to include it later; either is cheap at this corpus's
		// current size.
		_, err := tx.Exec(`
			ALTER TABLE search.passages
				ADD COLUMN source text NOT NULL DEFAULT 'content';
		`)
		return err
	},
	func(tx *sql.Tx) error {
		// search.passages uses the cluster's autovacuum defaults
		// (autovacuum_vacuum_threshold=50, autovacuum_vacuum_scale_factor=0.2),
		// so vacuum fires at 50 + 0.2*rows. That is exactly what let this
		// project lose debugging time to a real incident: 1,066 dead tuples
		// silently truncated HNSW index scans by a constant 31 rows, with no
		// error anywhere. At the corpus size where that happened (5,681
		// passages) the cluster default would not have fired until 1,186
		// dead tuples -- the incident sat just under the trigger.
		//
		// 500 + 0.05*rows crosses below the cluster-default trigger at
		// roughly 3,000 rows and stays below it at every larger size that
		// matters: 784 vs. 1,186 at 5,681 rows, 7,900 vs. 29,650 at 148k
		// rows. 1000 was considered and rejected -- its crossover against
		// the cluster default is ~6,300 rows, above today's corpus, so it
		// would be *looser* than the status quo right now and would not
		// have caught the incident above. Do not round 500 up to 1000.
		_, err := tx.Exec(`
			ALTER TABLE search.passages SET (
				autovacuum_vacuum_scale_factor = 0.05,
				autovacuum_vacuum_threshold = 500
			);
		`)
		return err
	},
	func(tx *sql.Tx) error {
		// The operator's embedder choice (spec §13.1) ("local" or
		// "remote", and for "remote" the URL) now lives here, not only in
		// the SIDECAR_EMBEDDER/SIDECAR_REMOTE_EMBEDDER_URL environment
		// variables cmd/sidecar reads once at startup. Without this, the
		// compose stack's `restart: unless-stopped` would silently revert
		// an operator's remote choice back to local on every container
		// restart -- and because that changes the model identity, it would
		// re-index the entire corpus with nobody asking.
		//
		// A single-row table, enforced by the CHECK(id = 1) singleton
		// pattern rather than a second table or a well-known key in some
		// generic settings table: there is exactly one embedder configured
		// at a time, an UPSERT is simpler than a DELETE-then-INSERT pair,
		// and a CHECK constraint makes "more than one row" a schema-level
		// impossibility rather than an invariant application code has to
		// maintain. Absent (no row at all) is a distinct, meaningful state
		// -- "nothing has ever been switched" -- from any row's own
		// content, which is exactly what lets the environment variables
		// keep acting as the startup default: see
		// store.GetEmbedderSettings.
		//
		// No SET/SET LOCAL of any kind here -- this migration only creates
		// a table, nothing that could hold a lock across a slow operation
		// the way migration 5's now-reverted HNSW rebuild did (see the
		// comment below this migration).
		_, err := tx.Exec(`
			CREATE TABLE search.embedder_settings (
				id         int NOT NULL CHECK (id = 1),
				kind       text NOT NULL,
				remote_url text NOT NULL DEFAULT '',
				updated_at timestamptz NOT NULL DEFAULT now(),
				PRIMARY KEY (id)
			);
		`)
		return err
	},
	// There is deliberately no migration here to rebuild
	// passages_embedding_idx under a raised maintenance_work_mem. That was
	// tried and reverted: maintenance_work_mem only controls HNSW build
	// *speed* (in-memory vs. a slower two-pass disk build), not the
	// resulting graph -- pgvector's on-disk build path inserts each vector
	// with the same neighbour-selection logic as the in-memory path, just
	// without WAL-logging it, and a direct measurement (60,000 clustered
	// 384-d vectors, '1MB' vs. '1GB') produced a byte-identical index and
	// statistically indistinguishable recall. A DROP INDEX + CREATE INDEX
	// migration bought nothing but held an AccessExclusiveLock on
	// search.passages for the whole rebuild -- confirmed via pg_locks and
	// a concurrent SELECT to block completely, not merely degrade -- for
	// minutes at a 148k-passage reference point, on every
	// deploy that crossed this migration. See README.md's "Rebuilding
	// passages_embedding_idx" section for the actual place this advice
	// belongs: an operator-driven REINDEX INDEX CONCURRENTLY, which does
	// not hold that lock and does not need to run inside a migration's
	// transaction at all.
	func(tx *sql.Tx) error {
		// Swapping bge-small-en-v1.5 (384-d) for nomic-embed-text-v1.5
		// (768-d) requires widening this column, and an HNSW index's
		// operator class is bound to its column's vector width, so the
		// index has to be dropped and rebuilt alongside it -- there is no
		// ALTER INDEX that simply widens one in place.
		//
		// USING NULL rather than any attempt to cast the existing
		// vector(384) values: a 384-d embedding is not a 768-d embedding
		// with values missing, it is a vector from a model that no longer
		// runs in this deployment, and there is no meaningful conversion
		// between the two. Discarding it here is safe and not this
		// migration's job to work around: swapping the embedder changes
		// embed.Identity, which changes store.contentHash for every
		// entry, so the existing pending/backfill machinery (spec §13.1)
		// re-embeds the entire corpus from scratch on its own, driven by
		// that hash mismatch -- independent of what this migration does
		// to the column's contents. A fresh database (embedding already
		// NULL/empty) takes the same USING NULL path trivially.
		//
		// The rebuilt index is roughly twice the previous one's size (768
		// vs. 384 dimensions per vector). An earlier version of exactly
		// this migration set max_parallel_maintenance_workers = 4 here
		// (guidance written for the smaller 384-d index) and
		// crash-looped the sidecar on every restart: pgvector's parallel
		// HNSW build requested a shared-memory segment sized against
		// maintenance_work_mem (~1.02 GB at '1GB'), backed by the
		// container's /dev/shm, whose Docker default is 64 MiB --
		// "could not resize shared memory segment ... No space left on
		// device (53100)", not a transient condition, unrecoverable short
		// of an operator dropping the index by hand outside migration. A
		// twice-as-big index makes that worse, not better, so this
		// explicitly pins max_parallel_maintenance_workers = 0 rather
		// than leaving it at whatever the cluster happens to default to
		// (2 on this project's dev stack -- already enough to trigger a
		// parallel build). maintenance_work_mem is still raised, but only
		// for in-memory-vs-disk build *speed*: measured directly
		// (README.md's "Rebuilding passages_embedding_idx" section) to
		// produce a byte-identical index either way, so this is not
		// expected to change the resulting graph, only how long building
		// it takes. Both are SET LOCAL, scoped to this transaction only,
		// so neither leaks onto the pooled connection once it commits --
		// see TestMigrationDoesNotLeakMaintenanceSettings.
		_, err := tx.Exec(`
			SET LOCAL max_parallel_maintenance_workers = 0;
			SET LOCAL maintenance_work_mem = '1GB';

			ALTER TABLE search.passages
				ALTER COLUMN embedding TYPE public.vector(768) USING NULL;

			DROP INDEX search.passages_embedding_idx;

			CREATE INDEX passages_embedding_idx
				ON search.passages USING hnsw (embedding public.vector_cosine_ops);
		`)
		return err
	},
	func(tx *sql.Tx) error {
		// The backfill lane's live-editable runtime configuration (spec
		// §9.2: schedule window, worker floor/ceiling, load threshold,
		// batch size, page size, poll interval, idle resweep interval)
		// now lives here too, following migration 5
		// (search.embedder_settings) exactly -- same singleton-row
		// pattern, same reason: POST /api/backfill/config was in-memory
		// only, so an operator's change (min_workers=1, say, dialed down
		// specifically to survive defect 1's crash-restart loop) reverted
		// to the SIDECAR_BACKFILL_* environment variables' default on
		// every container restart, with nobody asking.
		//
		// A single-row table, enforced by the same CHECK(id = 1) singleton
		// pattern as search.embedder_settings, for the identical reasons:
		// there is exactly one runtime configuration in effect at a time,
		// an UPSERT is simpler than a DELETE-then-INSERT pair, and a CHECK
		// constraint makes "more than one row" a schema-level
		// impossibility. Absent (no row at all) is a distinct, meaningful
		// state -- "no operator has ever changed the config from this
		// page" -- from any row's own content, which is what lets the
		// SIDECAR_BACKFILL_* environment variables keep acting as the
		// startup default: see store.GetBackfillSettings.
		//
		// No SET/SET LOCAL of any kind, and no index -- this migration
		// only creates a small table, nothing that could hold a lock
		// across a slow operation the way the since-reverted parallel HNSW
		// rebuild (see the comment on the migration two above this one)
		// did.
		_, err := tx.Exec(`
			CREATE TABLE search.backfill_settings (
				id                            int NOT NULL CHECK (id = 1),
				window_start                  int NOT NULL DEFAULT 0,
				window_end                    int NOT NULL DEFAULT 0,
				min_workers                   int NOT NULL,
				max_workers                   int NOT NULL,
				load_threshold                double precision NOT NULL,
				batch_size                    int NOT NULL,
				page_size                     int NOT NULL,
				poll_interval_seconds         double precision NOT NULL,
				idle_resweep_interval_seconds double precision NOT NULL,
				updated_at                    timestamptz NOT NULL DEFAULT now(),
				PRIMARY KEY (id)
			);
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
