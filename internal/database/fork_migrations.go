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
// It is deliberately separate from the upstream `migrations` array. Upstream
// runs migrations by array index and records a single integer in
// `schema_version`, so a fork migration appended there would occupy an index
// that upstream may later use, and after a rebase the upstream migration at
// that index would silently never run.
//
// Order is important. Add new migrations at the end of the list.
var forkMigrations = [...]func(tx *sql.Tx) error{}

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
