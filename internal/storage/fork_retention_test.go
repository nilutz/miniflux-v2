// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package storage // import "miniflux.app/v2/internal/storage"

import (
	"testing"
	"time"
)

// TestArchiveEntriesIsDisabledByNegativeInterval documents the upstream
// contract the fork relies on: only a negative interval disables archiving,
// and a zero interval deletes entries older than a single day.
func TestArchiveEntriesIsDisabledByNegativeInterval(t *testing.T) {
	store := testStorage(t)
	entryID, _ := createTestEntry(t, store, "retention-negative")

	if _, err := store.db.Exec(
		`UPDATE entries SET status='read', created_at=now() - interval '400 days' WHERE id=$1`,
		entryID,
	); err != nil {
		t.Fatalf("unable to age the entry: %v", err)
	}

	if _, err := store.ArchiveEntries("read", -1*time.Hour, 100); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var count int
	if err := store.db.QueryRow(`SELECT count(*) FROM entries WHERE id=$1`, entryID).Scan(&count); err != nil {
		t.Fatalf("unable to count entries: %v", err)
	}
	if count != 1 {
		t.Fatal("a negative interval must not delete anything")
	}
}

// TestCleanupPathPreservesOldReadEntries is the regression test for the
// corpus: the cleanup path must never delete entries, however old, and must
// never write entry_tombstones for this fixture's feed.
func TestCleanupPathPreservesOldReadEntries(t *testing.T) {
	store := testStorage(t)
	entryID, feedID := createTestEntry(t, store, "retention-cleanup")

	if _, err := store.db.Exec(
		`UPDATE entries SET status='read', created_at=now() - interval '400 days' WHERE id=$1`,
		entryID,
	); err != nil {
		t.Fatalf("unable to age the entry: %v", err)
	}

	if _, err := store.CleanOldWebSessions(30 * 24 * time.Hour); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := store.CleanupOrphanIcons(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var count int
	if err := store.db.QueryRow(`SELECT count(*) FROM entries WHERE id=$1`, entryID).Scan(&count); err != nil {
		t.Fatalf("unable to count entries: %v", err)
	}
	if count != 1 {
		t.Fatalf("entry #%d was deleted by the cleanup path", entryID)
	}

	var tombstones int
	if err := store.db.QueryRow(`SELECT count(*) FROM entry_tombstones WHERE feed_id=$1`, feedID).Scan(&tombstones); err != nil {
		t.Fatalf("unable to count tombstones: %v", err)
	}
	if tombstones != 0 {
		t.Fatalf("cleanup created %d tombstones for feed #%d; it must create none", tombstones, feedID)
	}
}
