// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package database // import "miniflux.app/v2/internal/database"

import (
	"database/sql"
	"fmt"
	"log/slog"
)

// forkMigrations holds schema changes owned by this fork.
//
// Deliberately separate from upstream's `migrations` array: upstream runs
// migrations by array index and records a single integer in
// `schema_version`, so a fork migration appended there would occupy an
// index upstream may later use, and after a rebase the upstream migration
// at that index would silently never run.
//
// Order is important. Add new migrations at the end of the list.
var forkMigrations = [...]func(tx *sql.Tx) error{
	func(tx *sql.Tx) (err error) {
		_, err = tx.Exec(`
			ALTER TABLE entries ADD COLUMN full_text_fetched_at timestamp with time zone;

			CREATE INDEX entries_full_text_pending_idx
				ON entries (id)
				WHERE full_text_fetched_at IS NULL;
		`)
		return err
	},
	func(tx *sql.Tx) (err error) {
		_, err = tx.Exec(`UPDATE feeds SET crawler = true WHERE crawler = false`)
		return err
	},
	func(tx *sql.Tx) (err error) {
		// A separate boolean, exactly like `starred`, rather than a third
		// `status` value (spec §13.3): keeps every existing status-handling
		// code path unchanged, including Fever and Google Reader, which have
		// no concept of hidden.
		//
		// No dedicated index: the existing (user_id, status, ...) indexes
		// already carry the unread-list query, and EXPLAIN ANALYZE on a
		// 300k-row synthetic table showed the extra `hidden IS FALSE` filter
		// adding well under a millisecond.
		_, err = tx.Exec(`ALTER TABLE entries ADD COLUMN hidden boolean not null default false`)
		return err
	},
	func(tx *sql.Tx) (err error) {
		// Distinguishes *why* an entry is hidden, since `hidden` is a
		// preference signal (spec §13.3): NULL means a human hid this one
		// entry by hand — a real negative for goal 3's daily best-of. 'bulk'
		// means a mass action (the backlog-hide checkbox on subscribe, or a
		// mark-all-as-hidden route) that hid entries nobody read, so it must
		// never be read back as a negative preference.
		//
		// Unhiding always clears this back to NULL, so goal 3 can trust
		// `WHERE hidden AND hidden_reason IS NULL` without also checking
		// `hidden` separately.
		//
		// No CHECK constraint: the only writers are this fork's own Go code.
		_, err = tx.Exec(`ALTER TABLE entries ADD COLUMN hidden_reason text`)
		return err
	},
}

var forkSchemaVersion = len(forkMigrations)

// MigrateFork executes fork-owned database migrations. It must run after
// database.Migrate.
func MigrateFork(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS fork_schema_version (version int not null)`); err != nil {
		return fmt.Errorf("[Fork migration] unable to create fork_schema_version: %v", err)
	}

	var currentVersion int
	db.QueryRow(`SELECT version FROM fork_schema_version`).Scan(&currentVersion)

	slog.Info("Running fork database migrations",
		slog.Int("current_version", currentVersion),
		slog.Int("latest_version", forkSchemaVersion),
	)

	for version := currentVersion; version < forkSchemaVersion; version++ {
		newVersion := version + 1

		tx, err := db.Begin()
		if err != nil {
			return fmt.Errorf("[Fork migration v%d] %v", newVersion, err)
		}

		if err := forkMigrations[version](tx); err != nil {
			tx.Rollback()
			return fmt.Errorf("[Fork migration v%d] %v", newVersion, err)
		}

		if _, err := tx.Exec(`TRUNCATE fork_schema_version`); err != nil {
			tx.Rollback()
			return fmt.Errorf("[Fork migration v%d] %v", newVersion, err)
		}

		if _, err := tx.Exec(`INSERT INTO fork_schema_version (version) VALUES ($1)`, newVersion); err != nil {
			tx.Rollback()
			return fmt.Errorf("[Fork migration v%d] %v", newVersion, err)
		}

		if err := tx.Commit(); err != nil {
			return fmt.Errorf("[Fork migration v%d] %v", newVersion, err)
		}
	}

	return nil
}
