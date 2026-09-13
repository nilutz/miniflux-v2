// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package storage // import "miniflux.app/v2/internal/storage"

import (
	"database/sql"
	"os"
	"testing"
	"time"

	_ "github.com/lib/pq"
)

func testStorage(t *testing.T) *Storage {
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

	return NewStorage(db)
}

// createTestEntry inserts a user, category, feed and entry, and returns the
// entry id and feed id. Everything is removed when the test finishes.
func createTestEntry(t *testing.T, s *Storage, username string) (entryID int64, feedID int64) {
	t.Helper()

	var userID int64
	if err := s.db.QueryRow(
		`INSERT INTO users (username, password) VALUES ($1, 'x') RETURNING id`,
		username,
	).Scan(&userID); err != nil {
		t.Fatalf("unable to create user: %v", err)
	}
	t.Cleanup(func() {
		s.db.Exec(`DELETE FROM users WHERE id=$1`, userID)
	})

	var categoryID int64
	if err := s.db.QueryRow(
		`INSERT INTO categories (user_id, title) VALUES ($1, 'Test') RETURNING id`,
		userID,
	).Scan(&categoryID); err != nil {
		t.Fatalf("unable to create category: %v", err)
	}

	if err := s.db.QueryRow(
		`INSERT INTO feeds (feed_url, site_url, title, category_id, user_id)
		 VALUES ($1, $1, 'Test feed', $2, $3) RETURNING id`,
		"https://example.org/"+username+".xml", categoryID, userID,
	).Scan(&feedID); err != nil {
		t.Fatalf("unable to create feed: %v", err)
	}

	if err := s.db.QueryRow(
		`INSERT INTO entries (title, hash, url, published_at, changed_at, user_id, feed_id, content)
		 VALUES ('Test entry', $1, 'https://example.org/post', now(), now(), $2, $3, '<p>excerpt</p>')
		 RETURNING id`,
		"hash-"+username, userID, feedID,
	).Scan(&entryID); err != nil {
		t.Fatalf("unable to create entry: %v", err)
	}

	return entryID, feedID
}

func TestEntryIDsWithoutFullTextReturnsUnfetchedEntries(t *testing.T) {
	store := testStorage(t)
	entryID, _ := createTestEntry(t, store, "fulltext-pending")

	entryIDs, err := store.EntryIDsWithoutFullText(entryID-1, 100)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	found := false
	for _, id := range entryIDs {
		if id == entryID {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected entry #%d in the pending list, got %v", entryID, entryIDs)
	}
}

func TestMarkFullTextFetchedRemovesEntryFromPendingList(t *testing.T) {
	store := testStorage(t)
	entryID, _ := createTestEntry(t, store, "fulltext-done")

	if err := store.MarkFullTextFetched(entryID, time.Now()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	entryIDs, err := store.EntryIDsWithoutFullText(entryID-1, 100)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, id := range entryIDs {
		if id == entryID {
			t.Fatalf("entry #%d should no longer be pending", entryID)
		}
	}
}

func TestEntryIDsWithoutFullTextRespectsLimit(t *testing.T) {
	store := testStorage(t)
	createTestEntry(t, store, "fulltext-limit-a")
	createTestEntry(t, store, "fulltext-limit-b")

	entryIDs, err := store.EntryIDsWithoutFullText(0, 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(entryIDs) != 1 {
		t.Fatalf("expected exactly 1 entry id, got %d", len(entryIDs))
	}
}

func TestEntryOwnerReturnsUserAndFeedID(t *testing.T) {
	store := testStorage(t)
	entryID, feedID := createTestEntry(t, store, "fulltext-owner")

	userID, gotFeedID, err := store.EntryOwner(entryID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if userID <= 0 {
		t.Fatalf("expected a positive user id, got %d", userID)
	}
	if gotFeedID != feedID {
		t.Fatalf("expected feed id %d, got %d", feedID, gotFeedID)
	}
}

func TestEntryOwnerReturnsErrorForUnknownEntry(t *testing.T) {
	store := testStorage(t)

	if _, _, err := store.EntryOwner(-1); err == nil {
		t.Fatal("expected an error for an entry id that does not exist")
	}
}

// TestMarkFullTextFetchedByHashesMarksOnlyTheNamedEntries covers the live
// crawler path, which knows the entries it scraped by their (feed_id, hash)
// key rather than by id, because it scrapes them before they are persisted.
func TestMarkFullTextFetchedByHashesMarksOnlyTheNamedEntries(t *testing.T) {
	s := testStorage(t)

	entryID, feedID := createTestEntry(t, s, "mark-by-hash")

	var otherEntryID int64
	if err := s.db.QueryRow(
		`INSERT INTO entries (title, hash, url, published_at, changed_at, user_id, feed_id, content)
		 SELECT 'Other entry', 'hash-mark-by-hash-other', 'https://example.org/other', now(), now(), user_id, feed_id, '<p>excerpt</p>'
		 FROM entries WHERE id=$1
		 RETURNING id`,
		entryID,
	).Scan(&otherEntryID); err != nil {
		t.Fatalf("unable to create the second entry: %v", err)
	}

	fetchedAt := time.Now().Truncate(time.Second)
	if err := s.MarkFullTextFetchedByHashes(feedID, []string{"hash-mark-by-hash", "hash-that-matches-nothing"}, fetchedAt); err != nil {
		t.Fatalf("unable to mark entries as fetched: %v", err)
	}

	var markedAt sql.NullTime
	if err := s.db.QueryRow(`SELECT full_text_fetched_at FROM entries WHERE id=$1`, entryID).Scan(&markedAt); err != nil {
		t.Fatalf("unable to read back the entry: %v", err)
	}
	if !markedAt.Valid || !markedAt.Time.Equal(fetchedAt) {
		t.Fatalf("expected full_text_fetched_at to be %v, got %v", fetchedAt, markedAt)
	}

	var otherMarkedAt sql.NullTime
	if err := s.db.QueryRow(`SELECT full_text_fetched_at FROM entries WHERE id=$1`, otherEntryID).Scan(&otherMarkedAt); err != nil {
		t.Fatalf("unable to read back the second entry: %v", err)
	}
	if otherMarkedAt.Valid {
		t.Fatal("expected an entry whose hash was not named to stay pending")
	}
}

// TestMarkFullTextFetchedByHashesIgnoresAnEmptyList guards the common case: a
// refresh where the crawler scraped nothing must not issue a statement that
// could match every entry of the feed.
func TestMarkFullTextFetchedByHashesIgnoresAnEmptyList(t *testing.T) {
	s := testStorage(t)

	entryID, feedID := createTestEntry(t, s, "mark-by-hash-empty")

	if err := s.MarkFullTextFetchedByHashes(feedID, nil, time.Now()); err != nil {
		t.Fatalf("unable to mark an empty list of entries: %v", err)
	}

	var markedAt sql.NullTime
	if err := s.db.QueryRow(`SELECT full_text_fetched_at FROM entries WHERE id=$1`, entryID).Scan(&markedAt); err != nil {
		t.Fatalf("unable to read back the entry: %v", err)
	}
	if markedAt.Valid {
		t.Fatal("expected the entry to stay pending")
	}
}
