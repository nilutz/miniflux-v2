// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package store // import "miniflux.app/v2/sidecar/internal/store"

import (
	"testing"
)

// createTestEntry inserts a user, category, feed and entry with the given
// content into the real Miniflux tables, mirroring the P0 fork's own test
// fixtures (see internal/storage/fork_full_text_test.go at the repo root).
// Everything — including any search schema rows a test leaves behind — is
// removed when the test finishes.
func createTestEntry(t *testing.T, s *Store, username, content string) int64 {
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

	var feedID int64
	if err := s.db.QueryRow(
		`INSERT INTO feeds (feed_url, site_url, title, category_id, user_id)
		 VALUES ($1, $1, 'Test feed', $2, $3) RETURNING id`,
		"https://example.org/"+username+".xml", categoryID, userID,
	).Scan(&feedID); err != nil {
		t.Fatalf("unable to create feed: %v", err)
	}

	var entryID int64
	if err := s.db.QueryRow(
		`INSERT INTO entries (title, hash, url, published_at, changed_at, user_id, feed_id, content)
		 VALUES ('Test entry', $1, 'https://example.org/'||$2, now(), now(), $3, $4, $5)
		 RETURNING id`,
		"hash-"+username, username, userID, feedID, content,
	).Scan(&entryID); err != nil {
		t.Fatalf("unable to create entry: %v", err)
	}
	t.Cleanup(func() {
		s.db.Exec(`DELETE FROM search.passages WHERE entry_id=$1`, entryID)
		s.db.Exec(`DELETE FROM search.entry_index_state WHERE entry_id=$1`, entryID)
	})

	return entryID
}

func updateEntryContent(t *testing.T, s *Store, entryID int64, content string) {
	t.Helper()

	if _, err := s.db.Exec(`UPDATE entries SET content=$1, changed_at=now() WHERE id=$2`, content, entryID); err != nil {
		t.Fatalf("unable to update entry content: %v", err)
	}
}

func TestEntryForIndexingReturnsContentAndHash(t *testing.T) {
	s := testStore(t)
	if err := s.Migrate(); err != nil {
		t.Fatalf("migrate failed: %v", err)
	}

	entryID := createTestEntry(t, s, "entryfor-basic", "<p>Hello world.</p>")

	entry, err := s.EntryForIndexing(entryID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if entry.ID != entryID {
		t.Fatalf("expected id %d, got %d", entryID, entry.ID)
	}
	if entry.Content != "<p>Hello world.</p>" {
		t.Fatalf("unexpected content: %q", entry.Content)
	}
	if entry.ContentHash == "" {
		t.Fatal("expected a non-empty content hash")
	}

	// The hash must be a pure function of content: hashing the same content
	// twice must agree, or PendingEntryIDs' SQL-side hash comparison and
	// this Go-side hash would silently drift apart.
	again, err := s.EntryForIndexing(entryID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if again.ContentHash != entry.ContentHash {
		t.Fatalf("hash is not deterministic: %q vs %q", entry.ContentHash, again.ContentHash)
	}
}

func TestEntryForIndexingChangesHashWhenContentChanges(t *testing.T) {
	s := testStore(t)
	if err := s.Migrate(); err != nil {
		t.Fatalf("migrate failed: %v", err)
	}

	entryID := createTestEntry(t, s, "entryfor-changed", "<p>Original.</p>")

	before, err := s.EntryForIndexing(entryID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	updateEntryContent(t, s, entryID, "<p>Changed.</p>")

	after, err := s.EntryForIndexing(entryID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if before.ContentHash == after.ContentHash {
		t.Fatal("expected content hash to change when content changes")
	}
}

func TestEntryForIndexingReturnsErrorForUnknownEntry(t *testing.T) {
	s := testStore(t)
	if err := s.Migrate(); err != nil {
		t.Fatalf("migrate failed: %v", err)
	}

	if _, err := s.EntryForIndexing(-1); err == nil {
		t.Fatal("expected an error for an entry id that does not exist")
	}
}

func TestPendingEntryIDsIncludesNeverIndexedEntry(t *testing.T) {
	s := testStore(t)
	if err := s.Migrate(); err != nil {
		t.Fatalf("migrate failed: %v", err)
	}

	entryID := createTestEntry(t, s, "pending-never", "<p>Never indexed.</p>")

	ids, err := s.PendingEntryIDs(entryID-1, 100)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	found := false
	for _, id := range ids {
		if id == entryID {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected entry #%d in the pending list, got %v", entryID, ids)
	}
}

func TestPendingEntryIDsExcludesUpToDateOKEntry(t *testing.T) {
	s := testStore(t)
	if err := s.Migrate(); err != nil {
		t.Fatalf("migrate failed: %v", err)
	}

	entryID := createTestEntry(t, s, "pending-ok", "<p>Already indexed.</p>")

	entry, err := s.EntryForIndexing(entryID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := s.ReplacePassages(entryID, entry.ContentHash, []PassageRow{
		{Ordinal: 0, Text: "Already indexed.", CharStart: 0, CharEnd: 17, Embedding: make([]float32, 384)},
	}); err != nil {
		t.Fatalf("unable to replace passages: %v", err)
	}

	ids, err := s.PendingEntryIDs(entryID-1, 100)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, id := range ids {
		if id == entryID {
			t.Fatalf("expected up-to-date entry #%d to be excluded from the pending list", entryID)
		}
	}
}

func TestPendingEntryIDsIncludesChangedEntry(t *testing.T) {
	s := testStore(t)
	if err := s.Migrate(); err != nil {
		t.Fatalf("migrate failed: %v", err)
	}

	entryID := createTestEntry(t, s, "pending-changed", "<p>Before edit.</p>")

	entry, err := s.EntryForIndexing(entryID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := s.ReplacePassages(entryID, entry.ContentHash, []PassageRow{
		{Ordinal: 0, Text: "Before edit.", CharStart: 0, CharEnd: 12, Embedding: make([]float32, 384)},
	}); err != nil {
		t.Fatalf("unable to replace passages: %v", err)
	}

	updateEntryContent(t, s, entryID, "<p>After edit.</p>")

	ids, err := s.PendingEntryIDs(entryID-1, 100)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	found := false
	for _, id := range ids {
		if id == entryID {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected changed entry #%d to be pending again, got %v", entryID, ids)
	}
}

func TestPendingEntryIDsIncludesFailedEntry(t *testing.T) {
	s := testStore(t)
	if err := s.Migrate(); err != nil {
		t.Fatalf("migrate failed: %v", err)
	}

	entryID := createTestEntry(t, s, "pending-failed", "<p>Will fail.</p>")

	if err := s.MarkEntryFailed(entryID, "irrelevant-hash", "embedding backend unavailable"); err != nil {
		t.Fatalf("unable to mark entry failed: %v", err)
	}

	ids, err := s.PendingEntryIDs(entryID-1, 100)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	found := false
	for _, id := range ids {
		if id == entryID {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected failed entry #%d to be pending, got %v", entryID, ids)
	}
}

func TestPendingEntryIDsRespectsLimit(t *testing.T) {
	s := testStore(t)
	if err := s.Migrate(); err != nil {
		t.Fatalf("migrate failed: %v", err)
	}

	createTestEntry(t, s, "pending-limit-a", "<p>a</p>")
	createTestEntry(t, s, "pending-limit-b", "<p>b</p>")

	ids, err := s.PendingEntryIDs(0, 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ids) != 1 {
		t.Fatalf("expected exactly 1 entry id, got %d", len(ids))
	}
}
