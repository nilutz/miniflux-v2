// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package store // import "miniflux.app/v2/sidecar/internal/store"

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

// createTestEntry inserts a user, category, feed and entry with the given
// content into the real Miniflux tables, mirroring the P0 fork's own test
// fixtures (see internal/storage/fork_full_text_test.go at the repo root).
// Everything — including any search schema rows a test leaves behind — is
// removed when the test finishes.
func createTestEntry(t *testing.T, s *Store, username, content string) int64 {
	t.Helper()
	return createTestEntryWithTitle(t, s, username, "Test entry", content)
}

// createTestEntryWithTitle is createTestEntry with a caller-chosen title,
// for tests that need to exercise the title itself (an empty title, a
// title that changes independently of content, a title with distinctive
// multi-byte content).
func createTestEntryWithTitle(t *testing.T, s *Store, username, title, content string) int64 {
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
		 VALUES ($1, $2, 'https://example.org/'||$3, now(), now(), $4, $5, $6)
		 RETURNING id`,
		title, "hash-"+username, username, userID, feedID, content,
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

func updateEntryTitle(t *testing.T, s *Store, entryID int64, title string) {
	t.Helper()

	if _, err := s.db.Exec(`UPDATE entries SET title=$1, changed_at=now() WHERE id=$2`, title, entryID); err != nil {
		t.Fatalf("unable to update entry title: %v", err)
	}
}

func TestEntryForIndexingReturnsContentAndHash(t *testing.T) {
	s := testStore(t)
	if err := s.Migrate(); err != nil {
		t.Fatalf("migrate failed: %v", err)
	}

	entryID := createTestEntry(t, s, "entryfor-basic", "<p>Hello world.</p>")

	entry, err := s.EntryForIndexing(context.Background(), entryID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if entry.ID != entryID {
		t.Fatalf("expected id %d, got %d", entryID, entry.ID)
	}
	if entry.Content != "<p>Hello world.</p>" {
		t.Fatalf("unexpected content: %q", entry.Content)
	}
	if entry.Title != "Test entry" {
		t.Fatalf("unexpected title: %q", entry.Title)
	}
	if entry.ContentHash == "" {
		t.Fatal("expected a non-empty content hash")
	}

	// The hash must be a pure function of content: hashing the same content
	// twice must agree, or PendingEntryIDs' SQL-side hash comparison and
	// this Go-side hash would silently drift apart.
	again, err := s.EntryForIndexing(context.Background(), entryID)
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

	before, err := s.EntryForIndexing(context.Background(), entryID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	updateEntryContent(t, s, entryID, "<p>Changed.</p>")

	after, err := s.EntryForIndexing(context.Background(), entryID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if before.ContentHash == after.ContentHash {
		t.Fatal("expected content hash to change when content changes")
	}
}

// TestEntryForIndexingChangesHashWhenTitleChangesButContentDoesNot is the
// regression guard for the gap this task exists to close: before task 1.5,
// content_hash covered only the entry's content, so editing only its title
// (a headline fix, a feed re-publishing with a corrected title) left the
// recorded hash unchanged and the entry silently un-reindexed forever —
// its old, now-wrong title passage (or, before this task, no title passage
// at all) would never be refreshed.
func TestEntryForIndexingChangesHashWhenTitleChangesButContentDoesNot(t *testing.T) {
	s := testStore(t)
	if err := s.Migrate(); err != nil {
		t.Fatalf("migrate failed: %v", err)
	}

	entryID := createTestEntryWithTitle(t, s, "entryfor-title-changed", "Original Title", "<p>Body never changes.</p>")

	before, err := s.EntryForIndexing(context.Background(), entryID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	updateEntryTitle(t, s, entryID, "Completely Different Title")

	after, err := s.EntryForIndexing(context.Background(), entryID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if before.ContentHash == after.ContentHash {
		t.Fatal("expected content hash to change when only the title changes")
	}
	if after.Title != "Completely Different Title" {
		t.Fatalf("expected the new title to be returned, got %q", after.Title)
	}
}

func TestEntryForIndexingReturnsErrorForUnknownEntry(t *testing.T) {
	s := testStore(t)
	if err := s.Migrate(); err != nil {
		t.Fatalf("migrate failed: %v", err)
	}

	if _, err := s.EntryForIndexing(context.Background(), -1); err == nil {
		t.Fatal("expected an error for an entry id that does not exist")
	}
}

// TestEntryArticleReturnsTitleURLPublishedAtAndContent covers task 15's
// GET /api/article, which this method backs: it must return everything
// that endpoint promises -- title, URL, published date and full content
// -- by entry id, and nothing about feed/category/read state (see
// ArticleDetail's own doc comment).
func TestEntryArticleReturnsTitleURLPublishedAtAndContent(t *testing.T) {
	s := testStore(t)
	if err := s.Migrate(); err != nil {
		t.Fatalf("migrate failed: %v", err)
	}

	entryID := createTestEntryWithTitle(t, s, "article-fetch", "An Article Title", "<p>full body</p>")

	a, err := s.EntryArticle(context.Background(), entryID)
	if err != nil {
		t.Fatalf("EntryArticle: %v", err)
	}
	if a.ID != entryID {
		t.Errorf("ID = %d, want %d", a.ID, entryID)
	}
	if a.Title != "An Article Title" {
		t.Errorf("Title = %q, want %q", a.Title, "An Article Title")
	}
	if a.URL != "https://example.org/article-fetch" {
		t.Errorf("URL = %q, want %q", a.URL, "https://example.org/article-fetch")
	}
	if a.PublishedAt.IsZero() {
		t.Error("PublishedAt is zero, want the fixture's now()")
	}
	if a.Content != "<p>full body</p>" {
		t.Errorf("Content = %q, want %q", a.Content, "<p>full body</p>")
	}
}

// TestEntryArticleReturnsErrorForUnknownEntry is
// TestEntryForIndexingReturnsErrorForUnknownEntry's counterpart for
// EntryArticle: handleArticle (internal/web/article_handler.go)
// distinguishes "no such entry" from every other failure via
// errors.Is(err, sql.ErrNoRows), so the error EntryArticle returns for an
// unknown id must actually wrap sql.ErrNoRows, not merely be non-nil.
func TestEntryArticleReturnsErrorForUnknownEntry(t *testing.T) {
	s := testStore(t)
	if err := s.Migrate(); err != nil {
		t.Fatalf("migrate failed: %v", err)
	}

	_, err := s.EntryArticle(context.Background(), -1)
	if err == nil {
		t.Fatal("expected an error for an entry id that does not exist")
	}
	if !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("error = %v, want it to wrap sql.ErrNoRows", err)
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

	entry, err := s.EntryForIndexing(context.Background(), entryID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := s.ReplacePassages(entryID, entry.ContentHash, []PassageRow{
		{Ordinal: 0, Text: "Already indexed.", CharStart: 0, CharEnd: 17, Source: "content", Embedding: make([]float32, 768)},
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

	entry, err := s.EntryForIndexing(context.Background(), entryID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := s.ReplacePassages(entryID, entry.ContentHash, []PassageRow{
		{Ordinal: 0, Text: "Before edit.", CharStart: 0, CharEnd: 12, Source: "content", Embedding: make([]float32, 768)},
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

// TestPendingEntryIDsIncludesEntryWhoseTitleChanged mirrors
// TestPendingEntryIDsIncludesChangedEntry but edits only the title,
// content held fixed — the same gap TestEntryForIndexingChangesHash...
// covers, exercised through the SQL-side predicate PendingEntryIDs
// actually uses in the backfill, not just the Go-side hash function.
func TestPendingEntryIDsIncludesEntryWhoseTitleChanged(t *testing.T) {
	s := testStore(t)
	if err := s.Migrate(); err != nil {
		t.Fatalf("migrate failed: %v", err)
	}

	entryID := createTestEntryWithTitle(t, s, "pending-title-changed", "Before Title", "<p>Stable body.</p>")

	entry, err := s.EntryForIndexing(context.Background(), entryID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := s.ReplacePassages(entryID, entry.ContentHash, []PassageRow{
		{Ordinal: 0, Text: "Before Title", CharStart: 0, CharEnd: 12, Source: "title", Embedding: make([]float32, 768)},
		{Ordinal: 1, Text: "Stable body.", CharStart: 0, CharEnd: 12, Source: "content", Embedding: make([]float32, 768)},
	}); err != nil {
		t.Fatalf("unable to replace passages: %v", err)
	}

	updateEntryTitle(t, s, entryID, "After Title")

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
		t.Fatalf("expected entry #%d, whose title changed, to be pending again, got %v", entryID, ids)
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

// pendingSnapshot runs PendingEntryCount's and PendingEntryIDs' exact
// WHERE-clause logic together, scoped to afterID, inside one REPEATABLE
// READ read-only transaction — so the count and the list are guaranteed
// to reflect the identical MVCC snapshot, not two separate points in
// time. It duplicates the production query text rather than calling the
// exported Store methods, because those run on the connection pool
// (s.db), not any specific transaction, and there is no way to route
// them through one without adding transaction support to the Store API.
//
// This exists because a non-transactional pair — call PendingEntryIDs,
// then separately call PendingEntryCount (or vice versa) — is not
// "astronomically" race-free just because the two calls are adjacent: a
// concurrently running package's own insert or index-state update can
// land in the gap between them, and under combined `indexer`+`store`
// load this reviewer measured it as roughly a 1-in-5 event, at BOTH of
// two call sites that used to make this exact mistake (fix round 4).
// Every comparison in this test now goes through this helper instead.
func pendingSnapshot(t *testing.T, s *Store, afterID int64) (count int64, ids []int64) {
	t.Helper()

	tx, err := s.db.BeginTx(context.Background(), &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		t.Fatalf("unable to begin snapshot transaction: %v", err)
	}
	defer tx.Rollback()

	// Deliberately excludes "AND NOT e.hidden" or any equivalent: spec
	// §13.3 requires a hidden entry (an article a reader marked "not
	// interested") to stay searchable, so it stays fully eligible for
	// (re-)indexing here too. Do not add a hidden filter to this
	// predicate, or to production's copies below.
	const pendingWhere = `
		FROM entries e
		LEFT JOIN search.entry_index_state s ON s.entry_id = e.id
		WHERE e.id > $1
		  AND (
		    s.entry_id IS NULL
		    OR s.status = 'failed'
		    OR s.content_hash <> md5($2 || $3 || coalesce(e.title, '') || coalesce(e.content, ''))
		  )
	`

	if err := tx.QueryRow(`SELECT count(*) `+pendingWhere, afterID, pipelineVersion, modelIdentity).Scan(&count); err != nil {
		t.Fatalf("unable to count pending entries: %v", err)
	}

	rows, err := tx.Query(`SELECT e.id `+pendingWhere, afterID, pipelineVersion, modelIdentity)
	if err != nil {
		t.Fatalf("unable to list pending entries: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("unable to scan pending id: %v", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("unable to read pending ids: %v", err)
	}

	return count, ids
}

// TestPendingEntryCountMatchesPendingEntryIDs checks that PendingEntryCount
// uses the exact same "pending" criteria as PendingEntryIDs, both
// unscoped (afterID=0, the whole table) and scoped to just above one
// fixture, before and after that fixture is indexed — every comparison
// via pendingSnapshot, so none of it depends on two separate
// non-transactional calls agreeing by luck.
func TestPendingEntryCountMatchesPendingEntryIDs(t *testing.T) {
	s := testStore(t)
	if err := s.Migrate(); err != nil {
		t.Fatalf("migrate failed: %v", err)
	}

	// A fixture of our own guarantees the pending set is never empty,
	// regardless of what any other concurrently running package's tests
	// are doing to the shared table.
	entryID := createTestEntry(t, s, "pending-count-agree", "<p>a</p>")

	// Unscoped: the two methods must agree on the whole table's pending
	// set, and our fixture must be among it.
	wholeCount, wholeIDs := pendingSnapshot(t, s, 0)
	if int64(len(wholeIDs)) != wholeCount {
		t.Fatalf("expected count(*) (%d) to exactly match the number of listed pending ids (%d) within the same "+
			"snapshot -- this is what PendingEntryCount and PendingEntryIDs must each also individually agree with", wholeCount, len(wholeIDs))
	}
	foundOurs := false
	for _, id := range wholeIDs {
		if id == entryID {
			foundOurs = true
		}
	}
	if !foundOurs {
		t.Fatalf("expected our own fixture #%d to be among the pending ids counted", entryID)
	}

	// Scoped to just above our fixture (afterID = entryID-1), before
	// indexing: still agree, and our fixture is still present.
	beforeCount, beforeIDs := pendingSnapshot(t, s, entryID-1)
	if int64(len(beforeIDs)) != beforeCount {
		t.Fatalf("expected PendingEntryCount(%d) (%d) to match len(PendingEntryIDs(%d, ...)) (%d) before indexing",
			entryID-1, beforeCount, entryID-1, len(beforeIDs))
	}
	foundOurs = false
	for _, id := range beforeIDs {
		if id == entryID {
			foundOurs = true
		}
	}
	if !foundOurs {
		t.Fatalf("expected our own fixture #%d to be pending before indexing", entryID)
	}

	entry, err := s.EntryForIndexing(context.Background(), entryID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := s.ReplacePassages(entryID, entry.ContentHash, []PassageRow{
		{Ordinal: 0, Text: "a", CharStart: 0, CharEnd: 1, Source: "content", Embedding: make([]float32, 768)},
	}); err != nil {
		t.Fatalf("unable to replace passages: %v", err)
	}

	// Same scope, after indexing: still agree, and our fixture must now
	// be absent.
	afterCount, afterIDs := pendingSnapshot(t, s, entryID-1)
	if int64(len(afterIDs)) != afterCount {
		t.Fatalf("expected PendingEntryCount(%d) (%d) to match len(PendingEntryIDs(%d, ...)) (%d) after indexing",
			entryID-1, afterCount, entryID-1, len(afterIDs))
	}
	for _, id := range afterIDs {
		if id == entryID {
			t.Fatalf("expected entry #%d to no longer be pending after indexing", entryID)
		}
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

// Bumping the pipeline version makes every already-indexed entry pending
// again, without any change to the entry's own content (whole-branch fix
// wave, finding 6). This is the mechanism that stops a change to
// internal/passage's ExtractText/Split from silently leaving stored
// char_start/char_end offsets pointing into a plaintext nothing derives
// that way any more.
func TestPipelineVersionBumpMakesIndexedEntriesPendingAgain(t *testing.T) {
	s := testStore(t)
	if err := s.Migrate(); err != nil {
		t.Fatalf("migrate failed: %v", err)
	}

	entryID := createTestEntry(t, s, "pipeline-version-bump", "<p>Content that never changes at all.</p>")

	entry, err := s.EntryForIndexing(context.Background(), entryID)
	if err != nil {
		t.Fatalf("EntryForIndexing failed: %v", err)
	}
	if err := s.ReplacePassages(entryID, entry.ContentHash, []PassageRow{{
		Ordinal: 0, Text: "Content that never changes at all.", CharStart: 0, CharEnd: 34,
		Source: "content", Embedding: make([]float32, 768),
	}}); err != nil {
		t.Fatalf("ReplacePassages failed: %v", err)
	}

	if containsID(pendingIDsFrom(t, s, entryID-1), entryID) {
		t.Fatalf("entry #%d should not be pending right after being indexed at the current pipeline version", entryID)
	}

	original := pipelineVersion
	t.Cleanup(func() { pipelineVersion = original })
	pipelineVersion = original + "-bumped"

	if !containsID(pendingIDsFrom(t, s, entryID-1), entryID) {
		t.Fatalf("entry #%d should be pending again after the pipeline version was bumped, but PendingEntryIDs did not return it", entryID)
	}

	// The Go-side hash must move with it too, or the re-index would write
	// back the old hash and the entry would be re-offered forever.
	rehashed, err := s.EntryForIndexing(context.Background(), entryID)
	if err != nil {
		t.Fatalf("EntryForIndexing after bump failed: %v", err)
	}
	if rehashed.ContentHash == entry.ContentHash {
		t.Fatalf("expected EntryForIndexing to compute a different hash after a pipeline version bump, got %q both times", entry.ContentHash)
	}

	// And the count predicate must agree with the ids predicate.
	count, err := s.PendingEntryCount(entryID - 1)
	if err != nil {
		t.Fatalf("PendingEntryCount failed: %v", err)
	}
	if count < 1 {
		t.Fatalf("expected PendingEntryCount to count the bumped entry, got %d", count)
	}
}

// TestModelIdentityChangeMakesIndexedEntriesPendingAgain is
// TestPipelineVersionBumpMakesIndexedEntriesPendingAgain's counterpart for
// the embedding model (spec §13.1). contentHash did not used to cover the
// configured model at all: switching from one embedding model to another
// changed no hash, marked nothing pending, and left search.passages holding
// vectors from two different models in one HNSW graph, where cosine
// similarity is meaningless. Folding the model's identity into the hash
// exactly like PipelineVersion closes that gap the same way a pipeline
// change already does.
func TestModelIdentityChangeMakesIndexedEntriesPendingAgain(t *testing.T) {
	s := testStore(t)
	if err := s.Migrate(); err != nil {
		t.Fatalf("migrate failed: %v", err)
	}

	entryID := createTestEntry(t, s, "model-identity-bump", "<p>Content that never changes at all.</p>")

	original := modelIdentity
	t.Cleanup(func() { modelIdentity = original })
	modelIdentity = "model-a@rev1#768"

	entry, err := s.EntryForIndexing(context.Background(), entryID)
	if err != nil {
		t.Fatalf("EntryForIndexing failed: %v", err)
	}
	if err := s.ReplacePassages(entryID, entry.ContentHash, []PassageRow{{
		Ordinal: 0, Text: "Content that never changes at all.", CharStart: 0, CharEnd: 34,
		Source: "content", Embedding: make([]float32, 768),
	}}); err != nil {
		t.Fatalf("ReplacePassages failed: %v", err)
	}

	if containsID(pendingIDsFrom(t, s, entryID-1), entryID) {
		t.Fatalf("entry #%d should not be pending right after being indexed under model A", entryID)
	}

	// Switch to model B: same pipeline version, same entry content, only
	// the configured model's identity changes.
	modelIdentity = "model-b@rev1#768"

	if !containsID(pendingIDsFrom(t, s, entryID-1), entryID) {
		t.Fatalf("entry #%d should be pending again after the configured model changed, but PendingEntryIDs did not return it", entryID)
	}

	// The Go-side hash must move with it too, or a re-index under model B
	// would write back model A's hash and the entry would be re-offered
	// forever.
	rehashed, err := s.EntryForIndexing(context.Background(), entryID)
	if err != nil {
		t.Fatalf("EntryForIndexing after model change failed: %v", err)
	}
	if rehashed.ContentHash == entry.ContentHash {
		t.Fatalf("expected EntryForIndexing to compute a different hash after the model changed, got %q both times", entry.ContentHash)
	}

	count, err := s.PendingEntryCount(entryID - 1)
	if err != nil {
		t.Fatalf("PendingEntryCount failed: %v", err)
	}
	if count < 1 {
		t.Fatalf("expected PendingEntryCount to count the entry after the model changed, got %d", count)
	}
}

// TestModelIdentityUnchangedLeavesIndexedEntryNotPending is the negative
// case spec §13.1 requires alongside the bump test above: an entry indexed
// under the currently configured model must NOT be re-offered just because
// PendingEntryIDs/PendingEntryCount were asked again with nothing having
// changed. Without this check, a hash formula that folded in something
// unstable (e.g. depended on map iteration order, or on anything besides
// the model's own name/revision/dimensions) could pass the bump test above
// while still spuriously re-queuing every already-indexed entry on every
// call.
func TestModelIdentityUnchangedLeavesIndexedEntryNotPending(t *testing.T) {
	s := testStore(t)
	if err := s.Migrate(); err != nil {
		t.Fatalf("migrate failed: %v", err)
	}

	entryID := createTestEntry(t, s, "model-identity-unchanged", "<p>Stable content, stable model.</p>")

	original := modelIdentity
	t.Cleanup(func() { modelIdentity = original })
	modelIdentity = "model-a@rev1#768"

	entry, err := s.EntryForIndexing(context.Background(), entryID)
	if err != nil {
		t.Fatalf("EntryForIndexing failed: %v", err)
	}
	if err := s.ReplacePassages(entryID, entry.ContentHash, []PassageRow{{
		Ordinal: 0, Text: "Stable content, stable model.", CharStart: 0, CharEnd: 30,
		Source: "content", Embedding: make([]float32, 768),
	}}); err != nil {
		t.Fatalf("ReplacePassages failed: %v", err)
	}

	if containsID(pendingIDsFrom(t, s, entryID-1), entryID) {
		t.Fatalf("entry #%d should not be pending right after indexing", entryID)
	}

	// Re-check with the same model configured (nothing changed): the
	// entry must still be absent from the pending set, and its hash must
	// still compute the same.
	if containsID(pendingIDsFrom(t, s, entryID-1), entryID) {
		t.Fatalf("entry #%d became pending again with the model unchanged", entryID)
	}

	rehashed, err := s.EntryForIndexing(context.Background(), entryID)
	if err != nil {
		t.Fatalf("EntryForIndexing failed: %v", err)
	}
	if rehashed.ContentHash != entry.ContentHash {
		t.Fatalf("hash changed with nothing but re-checking: %q vs %q", entry.ContentHash, rehashed.ContentHash)
	}

	count, err := s.PendingEntryCount(entryID - 1)
	if err != nil {
		t.Fatalf("PendingEntryCount failed: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected PendingEntryCount to be 0 for this scope with the model unchanged, got %d", count)
	}
}

// TestPendingEntryIDsAndPendingEntryCountAgreeAfterModelIdentityChange is
// the model-identity counterpart of TestPendingEntryCountMatchesPendingEntryIDs,
// deliberately calling the two real exported methods directly rather than
// a third hand-copied SQL fragment: PendingEntryIDs and PendingEntryCount
// each carry their own copy of the "pending" predicate in production code,
// and an earlier task in this project already had to fix a drift between
// exactly those two copies. Comparing the actual methods, not a test-side
// re-derivation of the query, is what would have caught that drift.
func TestPendingEntryIDsAndPendingEntryCountAgreeAfterModelIdentityChange(t *testing.T) {
	s := testStore(t)
	if err := s.Migrate(); err != nil {
		t.Fatalf("migrate failed: %v", err)
	}

	entryID := createTestEntry(t, s, "model-identity-ids-count-agree", "<p>Content for the agreement check.</p>")
	afterID := entryID - 1

	original := modelIdentity
	t.Cleanup(func() { modelIdentity = original })
	modelIdentity = "model-a@rev1#768"

	entry, err := s.EntryForIndexing(context.Background(), entryID)
	if err != nil {
		t.Fatalf("EntryForIndexing failed: %v", err)
	}
	if err := s.ReplacePassages(entryID, entry.ContentHash, []PassageRow{{
		Ordinal: 0, Text: "Content for the agreement check.", CharStart: 0, CharEnd: 33,
		Source: "content", Embedding: make([]float32, 768),
	}}); err != nil {
		t.Fatalf("ReplacePassages failed: %v", err)
	}

	// Before a model change: both methods must agree the entry is not
	// pending.
	ids, err := s.PendingEntryIDs(afterID, 100)
	if err != nil {
		t.Fatalf("PendingEntryIDs failed: %v", err)
	}
	count, err := s.PendingEntryCount(afterID)
	if err != nil {
		t.Fatalf("PendingEntryCount failed: %v", err)
	}
	if containsID(ids, entryID) || count != 0 {
		t.Fatalf("expected both methods to agree the entry is not pending before a model change; PendingEntryIDs=%v PendingEntryCount=%d", ids, count)
	}

	// After a model change: both methods must agree the entry IS pending.
	modelIdentity = "model-b@rev1#768"

	ids, err = s.PendingEntryIDs(afterID, 100)
	if err != nil {
		t.Fatalf("PendingEntryIDs failed: %v", err)
	}
	count, err = s.PendingEntryCount(afterID)
	if err != nil {
		t.Fatalf("PendingEntryCount failed: %v", err)
	}
	if !containsID(ids, entryID) {
		t.Fatalf("expected PendingEntryIDs to include #%d after the model changed, got %v", entryID, ids)
	}
	if count != int64(len(ids)) {
		t.Fatalf("expected PendingEntryCount (%d) to equal len(PendingEntryIDs) (%d) after the model changed", count, len(ids))
	}
}

func pendingIDsFrom(t *testing.T, s *Store, afterID int64) []int64 {
	t.Helper()
	ids, err := s.PendingEntryIDs(afterID, 100)
	if err != nil {
		t.Fatalf("PendingEntryIDs failed: %v", err)
	}
	return ids
}

func containsID(ids []int64, want int64) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

// PendingEntryCountApprox must track PendingEntryCount closely enough to
// drive an ETA, without detoasting or hashing anything. The one case
// where they legitimately differ — an entry edited since it was indexed —
// is asserted explicitly, so the approximation's shape is recorded rather
// than merely tolerated (whole-branch fix wave, finding 5).
func TestPendingEntryCountApproxTracksTheExactCount(t *testing.T) {
	s := testStore(t)
	if err := s.Migrate(); err != nil {
		t.Fatalf("migrate failed: %v", err)
	}

	entryID := createTestEntry(t, s, "pending-approx", "<p>Body of the approximation fixture.</p>")
	after := entryID - 1

	exact, err := s.PendingEntryCount(after)
	if err != nil {
		t.Fatalf("PendingEntryCount failed: %v", err)
	}
	approx, err := s.PendingEntryCountApprox(after)
	if err != nil {
		t.Fatalf("PendingEntryCountApprox failed: %v", err)
	}
	if approx != exact {
		t.Fatalf("with nothing indexed above #%d the two counts must agree, got exact=%d approx=%d", after, exact, approx)
	}

	entry, err := s.EntryForIndexing(context.Background(), entryID)
	if err != nil {
		t.Fatalf("EntryForIndexing failed: %v", err)
	}
	if err := s.ReplacePassages(entryID, entry.ContentHash, []PassageRow{
		{Ordinal: 0, Text: "x", CharStart: 0, CharEnd: 1, Source: "content", Embedding: make([]float32, 768)},
	}); err != nil {
		t.Fatalf("ReplacePassages failed: %v", err)
	}

	exact, err = s.PendingEntryCount(after)
	if err != nil {
		t.Fatalf("PendingEntryCount failed: %v", err)
	}
	approx, err = s.PendingEntryCountApprox(after)
	if err != nil {
		t.Fatalf("PendingEntryCountApprox failed: %v", err)
	}
	if approx != exact {
		t.Fatalf("after indexing the fixture the two counts must still agree, got exact=%d approx=%d", exact, approx)
	}

	// A failed entry is pending to both.
	if err := s.MarkEntryFailed(entryID, entry.ContentHash, "synthetic failure"); err != nil {
		t.Fatalf("MarkEntryFailed failed: %v", err)
	}
	exact, err = s.PendingEntryCount(after)
	if err != nil {
		t.Fatalf("PendingEntryCount failed: %v", err)
	}
	approx, err = s.PendingEntryCountApprox(after)
	if err != nil {
		t.Fatalf("PendingEntryCountApprox failed: %v", err)
	}
	if approx != exact {
		t.Fatalf("a failed entry must be pending to both counts, got exact=%d approx=%d", exact, approx)
	}

	// The documented divergence: content edited after a successful index.
	if err := s.ReplacePassages(entryID, entry.ContentHash, []PassageRow{
		{Ordinal: 0, Text: "x", CharStart: 0, CharEnd: 1, Source: "content", Embedding: make([]float32, 768)},
	}); err != nil {
		t.Fatalf("ReplacePassages failed: %v", err)
	}
	updateEntryContent(t, s, entryID, "<p>Rewritten body, hash no longer matches what was recorded.</p>")

	exact, err = s.PendingEntryCount(after)
	if err != nil {
		t.Fatalf("PendingEntryCount failed: %v", err)
	}
	approx, err = s.PendingEntryCountApprox(after)
	if err != nil {
		t.Fatalf("PendingEntryCountApprox failed: %v", err)
	}
	if exact != approx+1 {
		t.Fatalf("an entry edited since it was indexed must be pending to the exact count and (by design) missed by the approximation: got exact=%d approx=%d", exact, approx)
	}
}
