// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package store // import "miniflux.app/v2/sidecar/internal/store"

import (
	"context"
	"database/sql"
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

// TestPendingEntryCountMatchesPendingEntryIDs checks that PendingEntryCount
// uses the exact same "pending" criteria as PendingEntryIDs. Two separate,
// non-transactional queries computing "the same thing" moments apart are
// NOT safe to compare for exact equality here: this module's own tests
// (this file's own createTestEntry, and every other concurrently running
// package's) mutate the very same shared entries table throughout, and an
// earlier version of this test comparing PendingEntryCount() against a
// separately-fetched len(PendingEntryIDs(...)) flaked under exactly that
// interference (a concurrent insert or ReplacePassages landing between
// the two round trips changes one snapshot but not the other) -- the same
// class of risk live.go's own doc comment documents at length. The fix
// is not a tighter race but no race at all: both queries run inside one
// REPEATABLE READ transaction, so Postgres guarantees they observe the
// identical MVCC snapshot regardless of what any other connection commits
// meanwhile.
func TestPendingEntryCountMatchesPendingEntryIDs(t *testing.T) {
	s := testStore(t)
	if err := s.Migrate(); err != nil {
		t.Fatalf("migrate failed: %v", err)
	}

	// A fixture of our own guarantees the pending set is never empty,
	// regardless of what any other concurrently running package's tests
	// are doing to the shared table.
	entryID := createTestEntry(t, s, "pending-count-agree", "<p>a</p>")

	tx, err := s.db.BeginTx(context.Background(), &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		t.Fatalf("unable to begin transaction: %v", err)
	}
	defer tx.Rollback()

	const pendingWhere = `
		FROM entries e
		LEFT JOIN search.entry_index_state s ON s.entry_id = e.id
		WHERE e.id > 0
		  AND (
		    s.entry_id IS NULL
		    OR s.status = 'failed'
		    OR s.content_hash <> md5(coalesce(e.content, ''))
		  )
	`

	var count int64
	if err := tx.QueryRow(`SELECT count(*) ` + pendingWhere).Scan(&count); err != nil {
		t.Fatalf("unable to count pending entries: %v", err)
	}

	rows, err := tx.Query(`SELECT e.id ` + pendingWhere)
	if err != nil {
		t.Fatalf("unable to list pending entries: %v", err)
	}
	defer rows.Close()
	var ids []int64
	var foundOurs bool
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("unable to scan pending id: %v", err)
		}
		ids = append(ids, id)
		if id == entryID {
			foundOurs = true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("unable to read pending ids: %v", err)
	}

	if int64(len(ids)) != count {
		t.Fatalf("expected count(*) (%d) to exactly match the number of listed pending ids (%d) within the same "+
			"snapshot -- this is what PendingEntryCount and PendingEntryIDs must each also individually agree with", count, len(ids))
	}
	if !foundOurs {
		t.Fatalf("expected our own fixture #%d to be among the pending ids counted", entryID)
	}

	if err := tx.Rollback(); err != nil && err != sql.ErrTxDone {
		t.Fatalf("unable to roll back snapshot transaction: %v", err)
	}

	// Now exercise the real, exported methods against each other --
	// scoped tightly to just above our own fixture (afterID = entryID-1)
	// and executed back-to-back with no test-owned work in between. This
	// is as close to race-free as two separate non-transactional round
	// trips can get without threading a shared transaction through the
	// public API (as the snapshot check above does): only a different
	// package's insert landing in the sub-millisecond gap between these
	// two specific calls could disagree -- an astronomically narrower
	// window than comparing over this whole test's original
	// multi-statement duration, which is what actually flaked before
	// (fix round 2, finding 5). It is NOT compared against the snapshot
	// count above by any inequality: the live global count is not
	// monotonic across two separate points in time under concurrent
	// load -- another connection can just as easily have completed
	// indexing something (decreasing it) as inserted something new
	// (increasing it) in between, so neither ">=" nor "<=" holds in
	// general over that longer span. (An earlier version of this test
	// asserted such an inequality and flaked for exactly that reason.)
	scopedIDs, err := s.PendingEntryIDs(entryID-1, 1000)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	scopedCount, err := s.PendingEntryCount(entryID - 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if int64(len(scopedIDs)) != scopedCount {
		t.Fatalf("expected PendingEntryCount(%d) (%d) to match len(PendingEntryIDs(%d, big)) (%d)",
			entryID-1, scopedCount, entryID-1, len(scopedIDs))
	}

	// PendingEntryCount must also actually react to OUR entry leaving the
	// pending set -- scoped to this one id via PendingEntryIDs' own
	// afterID bound, so this half needs no snapshot over the whole shared
	// table either.
	entry, err := s.EntryForIndexing(entryID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := s.ReplacePassages(entryID, entry.ContentHash, []PassageRow{
		{Ordinal: 0, Text: "a", CharStart: 0, CharEnd: 1, Embedding: make([]float32, 384)},
	}); err != nil {
		t.Fatalf("unable to replace passages: %v", err)
	}

	afterIDs, err := s.PendingEntryIDs(entryID-1, 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, id := range afterIDs {
		if id == entryID {
			t.Fatalf("expected entry #%d to no longer be pending after indexing", entryID)
		}
	}

	// PendingEntryCount(entryID-1) must agree with PendingEntryIDs over
	// the same bound. By now several DB round trips have elapsed since
	// entryID was created (EntryForIndexing, ReplacePassages, the check
	// above) -- real wall-clock time for a concurrently running package's
	// own tests to have created a fixture somewhere above entryID-1,
	// which afterID does not bound from above. Comparing two separate,
	// non-transactional calls at this point is exactly what flaked in an
	// earlier version of this test (asserting the count was exactly 0),
	// so this repeats the snapshot-transaction technique from the top of
	// this test rather than hoping the gap between two plain calls stays
	// small enough.
	tx2, err := s.db.BeginTx(context.Background(), &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		t.Fatalf("unable to begin transaction: %v", err)
	}
	defer tx2.Rollback()

	var afterCountInTx int64
	if err := tx2.QueryRow(`SELECT count(*) FROM entries e LEFT JOIN search.entry_index_state s ON s.entry_id = e.id
		WHERE e.id > $1 AND (s.entry_id IS NULL OR s.status = 'failed' OR s.content_hash <> md5(coalesce(e.content, '')))`,
		entryID-1).Scan(&afterCountInTx); err != nil {
		t.Fatalf("unable to count pending entries in tx: %v", err)
	}

	rows2, err := tx2.Query(`SELECT e.id FROM entries e LEFT JOIN search.entry_index_state s ON s.entry_id = e.id
		WHERE e.id > $1 AND (s.entry_id IS NULL OR s.status = 'failed' OR s.content_hash <> md5(coalesce(e.content, '')))`,
		entryID-1)
	if err != nil {
		t.Fatalf("unable to list pending entries in tx: %v", err)
	}
	defer rows2.Close()
	var afterIDsInTx []int64
	for rows2.Next() {
		var id int64
		if err := rows2.Scan(&id); err != nil {
			t.Fatalf("unable to scan pending id: %v", err)
		}
		afterIDsInTx = append(afterIDsInTx, id)
	}
	if err := rows2.Err(); err != nil {
		t.Fatalf("unable to read pending ids: %v", err)
	}

	if int64(len(afterIDsInTx)) != afterCountInTx {
		t.Fatalf("expected the same-snapshot count(*) (%d) to match the number of listed pending ids (%d) after indexing",
			afterCountInTx, len(afterIDsInTx))
	}
	for _, id := range afterIDsInTx {
		if id == entryID {
			t.Fatalf("expected entry #%d to no longer be pending after indexing", entryID)
		}
	}

	// And the real exported methods, called non-transactionally as
	// production always does, must still agree with each other here --
	// the same immediate back-to-back comparison used above the first
	// time, not a vacuous "count(*) is non-negative" check (which cannot
	// fail regardless of whether the two methods agree on anything).
	liveIDs, err := s.PendingEntryIDs(entryID-1, 1000)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	liveCount, err := s.PendingEntryCount(entryID - 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if int64(len(liveIDs)) != liveCount {
		t.Fatalf("expected PendingEntryCount(%d) (%d) to match len(PendingEntryIDs(%d, big)) (%d) after indexing",
			entryID-1, liveCount, entryID-1, len(liveIDs))
	}
}

func TestMaxEntryIDReflectsHighestEntry(t *testing.T) {
	s := testStore(t)
	if err := s.Migrate(); err != nil {
		t.Fatalf("migrate failed: %v", err)
	}

	before, err := s.MaxEntryID()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	entryID := createTestEntry(t, s, "maxid-a", "<p>a</p>")

	after, err := s.MaxEntryID()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if after < entryID {
		t.Fatalf("expected MaxEntryID to be at least the just-created entry #%d, got %d", entryID, after)
	}
	if after < before {
		t.Fatalf("expected MaxEntryID to never decrease: was %d, now %d", before, after)
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
