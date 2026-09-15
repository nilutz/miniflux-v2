// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package storage // import "miniflux.app/v2/internal/storage"

import (
	"testing"
	"time"
)

// insertPaginationTestEntry inserts one unread, non-hidden entry for feedID
// with an explicit published_at, so relative ordering in a fixture does
// not depend on insertion speed.
func insertPaginationTestEntry(t *testing.T, s *Storage, userID, feedID int64, hash string, publishedAt time.Time) int64 {
	t.Helper()

	var entryID int64
	if err := s.db.QueryRow(
		`INSERT INTO entries (title, hash, url, status, published_at, changed_at, user_id, feed_id, content, author)
		 VALUES ('Pagination test entry', $1, 'https://example.org/post', 'unread', $2, now(), $3, $4, '<p>excerpt</p>', '')
		 RETURNING id`,
		hash, publishedAt, userID, feedID,
	).Scan(&entryID); err != nil {
		t.Fatalf("unable to create entry: %v", err)
	}

	return entryID
}

// TestEntryPaginationWithNotHiddenOrEntryIDKeepsCurrentEntryReachable
// covers task 10's extension of the WithStatusOrEntryID pattern to the
// hidden filter: an entry hidden after (or during) the very view that is
// showing it must stay reachable as its own pagination anchor, while a
// hidden *neighbour* is still excluded as a prev/next candidate.
//
// The middle entry is itself hidden. Using plain WithHidden(false)
// instead of WithNotHiddenOrEntryID would drop it out of the pagination
// CTE entirely, making both prevEntry and nextEntry come back nil - that
// is the mutation this test is built to catch, not just "a hidden
// neighbour is skipped" (already covered for other views by
// TestUnreadEntryPaginationSkipsHiddenNeighbor).
func TestEntryPaginationWithNotHiddenOrEntryIDKeepsCurrentEntryReachable(t *testing.T) {
	store := testStorage(t)
	entryID, feedID := createTestEntry(t, store, "pagination-not-hidden-or-entry-id")
	userID, _, err := store.EntryOwner(entryID)
	if err != nil {
		t.Fatalf("unable to get entry owner: %v", err)
	}

	now := time.Now()
	// createTestEntry's own entry becomes the "old, hidden" neighbour;
	// two more are added around it in time.
	oldHiddenID := entryID
	middleHiddenID := insertPaginationTestEntry(t, store, userID, feedID, "hash-middle-hidden", now.Add(-1*time.Hour))
	newVisibleID := insertPaginationTestEntry(t, store, userID, feedID, "hash-new-visible", now)

	if err := store.SetEntriesHiddenState(userID, []int64{oldHiddenID, middleHiddenID}, true, ""); err != nil {
		t.Fatalf("unable to hide entries: %v", err)
	}

	prevEntry, nextEntry, err := store.NewEntryPaginationBuilder(userID, middleHiddenID, "published_at", "asc").
		WithNotHiddenOrEntryID(middleHiddenID).
		Entries()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if prevEntry != nil {
		t.Fatalf("expected no previous entry (the only older entry is hidden and is not the current one), got %+v", prevEntry)
	}
	if nextEntry == nil || nextEntry.ID != newVisibleID {
		t.Fatalf("expected the next entry to be the newer, non-hidden entry %d, got %+v", newVisibleID, nextEntry)
	}
}
