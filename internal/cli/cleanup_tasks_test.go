// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cli // import "miniflux.app/v2/internal/cli"

import (
	"database/sql"
	"os"
	"testing"

	_ "github.com/lib/pq"

	"miniflux.app/v2/internal/config"
	"miniflux.app/v2/internal/storage"
)

// testDB opens a database connection for the cleanup task tests, skipping
// when DATABASE_URL is unset so make test stays hermetic. This duplicates
// internal/storage's identically shaped helper on purpose: that one is
// unexported and lives in a different package, and runCleanupTasks (also
// unexported) can only be called from package cli.
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

// createTestEntryWithStatus inserts a user, category, feed and entry with the
// given status, aged 400 days into the past, and returns the entry id and
// feed id. Everything is removed when the test finishes.
func createTestEntryWithStatus(t *testing.T, db *sql.DB, username, status string) (entryID int64, feedID int64) {
	t.Helper()

	var userID int64
	if err := db.QueryRow(
		`INSERT INTO users (username, password) VALUES ($1, 'x') RETURNING id`,
		username,
	).Scan(&userID); err != nil {
		t.Fatalf("unable to create user: %v", err)
	}
	t.Cleanup(func() {
		db.Exec(`DELETE FROM users WHERE id=$1`, userID)
	})

	var categoryID int64
	if err := db.QueryRow(
		`INSERT INTO categories (user_id, title) VALUES ($1, 'Test') RETURNING id`,
		userID,
	).Scan(&categoryID); err != nil {
		t.Fatalf("unable to create category: %v", err)
	}

	if err := db.QueryRow(
		`INSERT INTO feeds (feed_url, site_url, title, category_id, user_id)
		 VALUES ($1, $1, 'Test feed', $2, $3) RETURNING id`,
		"https://example.org/"+username+".xml", categoryID, userID,
	).Scan(&feedID); err != nil {
		t.Fatalf("unable to create feed: %v", err)
	}

	if err := db.QueryRow(
		`INSERT INTO entries (title, hash, url, published_at, changed_at, user_id, feed_id, content, status, created_at)
		 VALUES ('Test entry', $1, 'https://example.org/post', now(), now(), $2, $3, '<p>excerpt</p>', $4, now() - interval '400 days')
		 RETURNING id`,
		"hash-"+username, userID, feedID, status,
	).Scan(&entryID); err != nil {
		t.Fatalf("unable to create entry: %v", err)
	}

	return entryID, feedID
}

// TestRunCleanupTasksPreservesOldEntries calls runCleanupTasks directly (the
// only place in the fork that can, since it is unexported) with entries aged
// well past both historical archive windows, in both the read and unread
// statuses runCleanupTasks used to archive separately. It is the regression
// guard for the corpus: the cleanup path must never delete entries, however
// old, and must never write entry_tombstones for them.
func TestRunCleanupTasksPreservesOldEntries(t *testing.T) {
	db := testDB(t)
	store := storage.NewStorage(db)

	if config.Opts == nil {
		cfg := config.NewConfigParser()
		opts, err := cfg.ParseEnvironmentVariables()
		if err != nil {
			t.Fatalf("unable to parse configuration: %v", err)
		}
		config.Opts = opts
	}

	readEntryID, readFeedID := createTestEntryWithStatus(t, db, "cleanup-read", "read")
	unreadEntryID, unreadFeedID := createTestEntryWithStatus(t, db, "cleanup-unread", "unread")

	runCleanupTasks(store)

	for _, tc := range []struct {
		name    string
		entryID int64
		feedID  int64
	}{
		{"read", readEntryID, readFeedID},
		{"unread", unreadEntryID, unreadFeedID},
	} {
		var count int
		if err := db.QueryRow(`SELECT count(*) FROM entries WHERE id=$1`, tc.entryID).Scan(&count); err != nil {
			t.Fatalf("%s: unable to count entries: %v", tc.name, err)
		}
		if count != 1 {
			t.Fatalf("%s: entry #%d was deleted by runCleanupTasks", tc.name, tc.entryID)
		}

		var tombstones int
		if err := db.QueryRow(`SELECT count(*) FROM entry_tombstones WHERE feed_id=$1`, tc.feedID).Scan(&tombstones); err != nil {
			t.Fatalf("%s: unable to count tombstones: %v", tc.name, err)
		}
		if tombstones != 0 {
			t.Fatalf("%s: runCleanupTasks created %d tombstones for feed #%d; it must create none", tc.name, tombstones, tc.feedID)
		}
	}
}
