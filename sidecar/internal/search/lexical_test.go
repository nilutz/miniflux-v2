// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package search // import "miniflux.app/v2/sidecar/internal/search"

import (
	"context"
	"database/sql"
	"os"
	"strconv"
	"testing"
	"time"

	_ "github.com/lib/pq"

	"miniflux.app/v2/sidecar/internal/store"
)

// testStore opens a real connection for these tests, skipping without
// SIDECAR_DATABASE_URL per the plan's global constraints. Every fixture
// this file creates uses a distinctive, made-up vocabulary (a "zzyzx..."
// prefix) so its rows can never collide with anything already in the
// real, shared corpus these tests run alongside — and every fixture is
// cleaned up in t.Cleanup, exactly like store's own tests.
func testStore(t *testing.T) *store.Store {
	t.Helper()

	dsn := os.Getenv("SIDECAR_DATABASE_URL")
	if dsn == "" {
		t.Skip("SIDECAR_DATABASE_URL is not set, skipping database test")
	}

	s, err := store.New(dsn)
	if err != nil {
		t.Fatalf("unable to open database: %v", err)
	}
	if err := s.Migrate(); err != nil {
		t.Fatalf("migrate failed: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	return s
}

// testDB opens its own direct database connection for fixture setup that
// needs to INSERT/UPDATE — creating users, feeds and entries, or flipping
// an entry's status/starred/published_at. This is deliberately separate
// from store.Store's own connection: production code reaches the
// database only through store.Reader (QueryContext/QueryRowContext, no
// Exec), so fixture writes get their own direct connection here rather
// than routing through anything Searcher itself could use to write.
func testDB(t *testing.T) *sql.DB {
	t.Helper()

	dsn := os.Getenv("SIDECAR_DATABASE_URL")
	if dsn == "" {
		t.Skip("SIDECAR_DATABASE_URL is not set, skipping database test")
	}

	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("unable to open database: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	return db
}

// fixtureEntry is one entry this file's tests create, plus the ids of
// everything it depends on (user, category, feed) so a caller can filter
// on them.
type fixtureEntry struct {
	EntryID    int64
	FeedID     int64
	CategoryID int64
}

// createFixtureEntry inserts a user, category, feed and entry directly —
// mirroring store's own createTestEntryWithTitle test helper — because
// that helper is package-private to store and search has no exported
// entry-creation method of its own (by design: package store only reads
// public.entries, per spec §4, it does not create test fixtures for other
// packages). It takes a *sql.DB rather than a *store.Store because
// fixture setup needs to INSERT, and store.Reader (what production code
// uses) deliberately cannot — see testDB's doc comment. Status defaults
// to Miniflux's own default ('unread'); tests that need 'read' or
// starred update it explicitly afterward.
func createFixtureEntry(t *testing.T, db *sql.DB, tag, title, content string) fixtureEntry {
	t.Helper()

	var userID int64
	if err := db.QueryRow(
		`INSERT INTO users (username, password) VALUES ($1, 'x') RETURNING id`,
		"search-lexical-"+tag,
	).Scan(&userID); err != nil {
		t.Fatalf("unable to create user: %v", err)
	}
	t.Cleanup(func() { db.Exec(`DELETE FROM users WHERE id=$1`, userID) })

	var categoryID int64
	if err := db.QueryRow(
		`INSERT INTO categories (user_id, title) VALUES ($1, 'Test') RETURNING id`,
		userID,
	).Scan(&categoryID); err != nil {
		t.Fatalf("unable to create category: %v", err)
	}

	var feedID int64
	if err := db.QueryRow(
		`INSERT INTO feeds (feed_url, site_url, title, category_id, user_id)
		 VALUES ($1, $1, 'Test feed', $2, $3) RETURNING id`,
		"https://example.org/search-lexical-"+tag+".xml", categoryID, userID,
	).Scan(&feedID); err != nil {
		t.Fatalf("unable to create feed: %v", err)
	}

	var entryID int64
	if err := db.QueryRow(
		`INSERT INTO entries (title, hash, url, published_at, changed_at, user_id, feed_id, content)
		 VALUES ($1, $2, 'https://example.org/'||$3, now(), now(), $4, $5, $6)
		 RETURNING id`,
		title, "hash-search-lexical-"+tag, tag, userID, feedID, content,
	).Scan(&entryID); err != nil {
		t.Fatalf("unable to create entry: %v", err)
	}
	t.Cleanup(func() {
		db.Exec(`DELETE FROM search.passages WHERE entry_id=$1`, entryID)
		db.Exec(`DELETE FROM search.entry_index_state WHERE entry_id=$1`, entryID)
	})

	return fixtureEntry{EntryID: entryID, FeedID: feedID, CategoryID: categoryID}
}

// writePassages replaces an entry's passages with rows carrying a zero
// embedding — Lexical never reads the embedding column, so its exact
// value is irrelevant to every test in this file, only its presence
// (search.passages.embedding is nullable, but ReplacePassages always
// writes a full 384-dim vector, matching every other fixture in this
// codebase, e.g. store/passages_test.go).
func writePassages(t *testing.T, s *store.Store, entryID int64, rows []store.PassageRow) {
	t.Helper()
	if err := s.ReplacePassages(entryID, "hash-"+strconv.FormatInt(entryID, 10), rows); err != nil {
		t.Fatalf("unable to write passages for entry #%d: %v", entryID, err)
	}
}

func zeroEmbedding() []float32 { return make([]float32, 384) }

func hasEntryID(hits []PassageHit, entryID int64) bool {
	for _, h := range hits {
		if h.EntryID == entryID {
			return true
		}
	}
	return false
}

// TestLexicalFindsEntryByTitleOnly is the regression guard task 1.5 was
// meant to close: a query matching only an entry's title (never
// mentioned anywhere in its body) must still find that entry, because
// title text is now indexed as its own passage.
func TestLexicalFindsEntryByTitleOnly(t *testing.T) {
	s := testStore(t)
	db := testDB(t)
	searcher := NewSearcher(s)

	fx := createFixtureEntry(t, db, "title-only", "Zzyzxvorlon Frobnication Guide", "<p>This body never repeats the headline's own words.</p>")
	writePassages(t, s, fx.EntryID, []store.PassageRow{
		{Ordinal: 0, Text: "Zzyzxvorlon Frobnication Guide", CharStart: 0, CharEnd: 31, Source: "title", Embedding: zeroEmbedding()},
		{Ordinal: 1, Text: "This body never repeats the headline's own words.", CharStart: 0, CharEnd: 50, Source: "content", Embedding: zeroEmbedding()},
	})

	hits, err := searcher.Lexical(context.Background(), "Zzyzxvorlon Frobnication", 10, Filters{})
	if err != nil {
		t.Fatalf("Lexical: unexpected error: %v", err)
	}
	if !hasEntryID(hits, fx.EntryID) {
		t.Fatalf("expected entry #%d (matched only by its title) among hits, got %+v", fx.EntryID, hits)
	}

	var titleHit *PassageHit
	for i := range hits {
		if hits[i].EntryID == fx.EntryID && hits[i].Source == "title" {
			titleHit = &hits[i]
		}
	}
	if titleHit == nil {
		t.Fatalf("expected the matching hit to be the title passage (source='title'), got %+v", hits)
	}
}

// TestLexicalExactPhraseReturnsThePassageContainingIt uses a distinctive,
// multi-word phrase that appears verbatim in exactly one passage among
// several fixture passages, and checks that querying for it returns that
// passage — ranked first, since it is the only one containing every
// query term.
func TestLexicalExactPhraseReturnsThePassageContainingIt(t *testing.T) {
	s := testStore(t)
	db := testDB(t)
	searcher := NewSearcher(s)

	fx := createFixtureEntry(t, db, "exact-phrase", "Zzyzx Exact Phrase Fixture",
		"<p>Zzyzx unrelated filler one.</p><p>Zzyzx unrelated filler two.</p>")
	writePassages(t, s, fx.EntryID, []store.PassageRow{
		{Ordinal: 0, Text: "Zzyzx Exact Phrase Fixture", CharStart: 0, CharEnd: 26, Source: "title", Embedding: zeroEmbedding()},
		{Ordinal: 1, Text: "the quixotic zzyzxglorbnaxplorf marmoset assembled a bibliography", CharStart: 0, CharEnd: 67, Source: "content", Embedding: zeroEmbedding()},
		{Ordinal: 2, Text: "an entirely different passage about zzyzx pelicans and lighthouses", CharStart: 67, CharEnd: 135, Source: "content", Embedding: zeroEmbedding()},
	})

	hits, err := searcher.Lexical(context.Background(), "quixotic zzyzxglorbnaxplorf marmoset bibliography", 10, Filters{})
	if err != nil {
		t.Fatalf("Lexical: unexpected error: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("expected at least one hit, got none")
	}
	if hits[0].EntryID != fx.EntryID || hits[0].Ordinal != 1 {
		t.Fatalf("expected the exact-phrase passage (ordinal 1) to rank first, got %+v", hits[0])
	}
}

func TestLexicalReturnsNoErrorAndNoHitsForNoMatch(t *testing.T) {
	s := testStore(t)
	searcher := NewSearcher(s)

	hits, err := searcher.Lexical(context.Background(), "zzyzxnonexistentnonsensetoken12345", 10, Filters{})
	if err != nil {
		t.Fatalf("expected no error for a query matching nothing, got: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("expected no hits, got %+v", hits)
	}
}

// TestLexicalOrdersByScoreDescendingWithConsistentRank builds three
// passages with a deliberately increasing count of the query term, so
// BM25 term-frequency scoring orders them predictably, and checks both
// that Score is descending and that Rank is exactly 1,2,3 in that order.
func TestLexicalOrdersByScoreDescendingWithConsistentRank(t *testing.T) {
	s := testStore(t)
	db := testDB(t)
	searcher := NewSearcher(s)

	fx := createFixtureEntry(t, db, "ranking", "Zzyzx Ranking Fixture", "<p>irrelevant</p>")
	writePassages(t, s, fx.EntryID, []store.PassageRow{
		{Ordinal: 0, Text: "zzyzxrankterm appears exactly once here", CharStart: 0, CharEnd: 40, Source: "content", Embedding: zeroEmbedding()},
		{Ordinal: 1, Text: "zzyzxrankterm zzyzxrankterm zzyzxrankterm repeated three times", CharStart: 40, CharEnd: 104, Source: "content", Embedding: zeroEmbedding()},
		{Ordinal: 2, Text: "zzyzxrankterm zzyzxrankterm repeated twice here", CharStart: 104, CharEnd: 152, Source: "content", Embedding: zeroEmbedding()},
	})

	hits, err := searcher.Lexical(context.Background(), "zzyzxrankterm", 10, Filters{})
	if err != nil {
		t.Fatalf("Lexical: unexpected error: %v", err)
	}
	if len(hits) < 3 {
		t.Fatalf("expected at least 3 hits, got %d: %+v", len(hits), hits)
	}

	for i := 0; i < len(hits)-1; i++ {
		if hits[i].Score < hits[i+1].Score {
			t.Fatalf("expected non-increasing score order, got hits[%d].Score=%v < hits[%d].Score=%v", i, hits[i].Score, i+1, hits[i+1].Score)
		}
	}
	for i, h := range hits {
		if h.Rank != i+1 {
			t.Fatalf("expected hits[%d].Rank == %d, got %d", i, i+1, h.Rank)
		}
	}

	// The thrice-repeated passage (ordinal 1) must outrank the
	// twice-repeated one (ordinal 2), which must outrank the
	// once-only one (ordinal 0) — BM25 term frequency, ascending.
	rankOf := map[int]int{}
	for _, h := range hits {
		if h.EntryID == fx.EntryID {
			rankOf[h.Ordinal] = h.Rank
		}
	}
	if !(rankOf[1] < rankOf[2] && rankOf[2] < rankOf[0]) {
		t.Fatalf("expected rank order ordinal1 < ordinal2 < ordinal0 by term frequency, got ranks %+v", rankOf)
	}
}

func TestLexicalHonoursLimit(t *testing.T) {
	s := testStore(t)
	db := testDB(t)
	searcher := NewSearcher(s)

	fx := createFixtureEntry(t, db, "limit", "Zzyzx Limit Fixture", "<p>irrelevant</p>")
	writePassages(t, s, fx.EntryID, []store.PassageRow{
		{Ordinal: 0, Text: "zzyzxlimitterm one", CharStart: 0, CharEnd: 18, Source: "content", Embedding: zeroEmbedding()},
		{Ordinal: 1, Text: "zzyzxlimitterm two", CharStart: 18, CharEnd: 36, Source: "content", Embedding: zeroEmbedding()},
		{Ordinal: 2, Text: "zzyzxlimitterm three", CharStart: 36, CharEnd: 56, Source: "content", Embedding: zeroEmbedding()},
	})

	hits, err := searcher.Lexical(context.Background(), "zzyzxlimitterm", 2, Filters{})
	if err != nil {
		t.Fatalf("Lexical: unexpected error: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("expected exactly 2 hits with limit=2, got %d: %+v", len(hits), hits)
	}
}

// TestLexicalFiltersNarrowResultsByFeed creates two fixture entries in
// two different feeds, both matching the same query term, and checks
// that filtering to one feed's id excludes the other feed's entry.
func TestLexicalFiltersNarrowResultsByFeed(t *testing.T) {
	s := testStore(t)
	db := testDB(t)
	searcher := NewSearcher(s)

	fxA := createFixtureEntry(t, db, "filter-feed-a", "Zzyzx Filter Feed A", "<p>irrelevant</p>")
	writePassages(t, s, fxA.EntryID, []store.PassageRow{
		{Ordinal: 0, Text: "zzyzxfilterfeedterm in feed a", CharStart: 0, CharEnd: 30, Source: "content", Embedding: zeroEmbedding()},
	})

	fxB := createFixtureEntry(t, db, "filter-feed-b", "Zzyzx Filter Feed B", "<p>irrelevant</p>")
	writePassages(t, s, fxB.EntryID, []store.PassageRow{
		{Ordinal: 0, Text: "zzyzxfilterfeedterm in feed b", CharStart: 0, CharEnd: 30, Source: "content", Embedding: zeroEmbedding()},
	})

	unfiltered, err := searcher.Lexical(context.Background(), "zzyzxfilterfeedterm", 10, Filters{})
	if err != nil {
		t.Fatalf("Lexical: unexpected error: %v", err)
	}
	if !hasEntryID(unfiltered, fxA.EntryID) || !hasEntryID(unfiltered, fxB.EntryID) {
		t.Fatalf("expected both fixtures unfiltered, got %+v", unfiltered)
	}

	filtered, err := searcher.Lexical(context.Background(), "zzyzxfilterfeedterm", 10, Filters{FeedIDs: []int64{fxA.FeedID}})
	if err != nil {
		t.Fatalf("Lexical: unexpected error: %v", err)
	}
	if !hasEntryID(filtered, fxA.EntryID) {
		t.Fatalf("expected feed A's entry to survive the feed filter, got %+v", filtered)
	}
	if hasEntryID(filtered, fxB.EntryID) {
		t.Fatalf("expected feed B's entry to be excluded by the feed filter, got %+v", filtered)
	}
}

// TestLexicalFiltersNarrowResultsByUnreadOnly checks the entry-status
// filter: a starred/read/unread predicate against public.entries.
func TestLexicalFiltersNarrowResultsByUnreadOnly(t *testing.T) {
	s := testStore(t)
	searcher := NewSearcher(s)
	db := testDB(t)

	fxUnread := createFixtureEntry(t, db, "filter-unread", "Zzyzx Filter Unread", "<p>irrelevant</p>")
	writePassages(t, s, fxUnread.EntryID, []store.PassageRow{
		{Ordinal: 0, Text: "zzyzxstatusterm still unread", CharStart: 0, CharEnd: 29, Source: "content", Embedding: zeroEmbedding()},
	})

	fxRead := createFixtureEntry(t, db, "filter-read", "Zzyzx Filter Read", "<p>irrelevant</p>")
	writePassages(t, s, fxRead.EntryID, []store.PassageRow{
		{Ordinal: 0, Text: "zzyzxstatusterm already read", CharStart: 0, CharEnd: 29, Source: "content", Embedding: zeroEmbedding()},
	})
	if _, err := db.Exec(`UPDATE entries SET status='read' WHERE id=$1`, fxRead.EntryID); err != nil {
		t.Fatalf("unable to mark fixture read: %v", err)
	}

	hits, err := searcher.Lexical(context.Background(), "zzyzxstatusterm", 10, Filters{UnreadOnly: true})
	if err != nil {
		t.Fatalf("Lexical: unexpected error: %v", err)
	}
	if !hasEntryID(hits, fxUnread.EntryID) {
		t.Fatalf("expected the unread fixture to survive UnreadOnly, got %+v", hits)
	}
	if hasEntryID(hits, fxRead.EntryID) {
		t.Fatalf("expected the read fixture to be excluded by UnreadOnly, got %+v", hits)
	}
}

// TestLexicalFiltersNarrowResultsByCategory mirrors the feed filter test
// but through CategoryIDs, which joins to public.feeds rather than
// filtering public.entries directly.
func TestLexicalFiltersNarrowResultsByCategory(t *testing.T) {
	s := testStore(t)
	db := testDB(t)
	searcher := NewSearcher(s)

	fxA := createFixtureEntry(t, db, "filter-cat-a", "Zzyzx Filter Category A", "<p>irrelevant</p>")
	writePassages(t, s, fxA.EntryID, []store.PassageRow{
		{Ordinal: 0, Text: "zzyzxfiltercatterm in category a", CharStart: 0, CharEnd: 33, Source: "content", Embedding: zeroEmbedding()},
	})

	fxB := createFixtureEntry(t, db, "filter-cat-b", "Zzyzx Filter Category B", "<p>irrelevant</p>")
	writePassages(t, s, fxB.EntryID, []store.PassageRow{
		{Ordinal: 0, Text: "zzyzxfiltercatterm in category b", CharStart: 0, CharEnd: 33, Source: "content", Embedding: zeroEmbedding()},
	})

	filtered, err := searcher.Lexical(context.Background(), "zzyzxfiltercatterm", 10, Filters{CategoryIDs: []int64{fxA.CategoryID}})
	if err != nil {
		t.Fatalf("Lexical: unexpected error: %v", err)
	}
	if !hasEntryID(filtered, fxA.EntryID) {
		t.Fatalf("expected category A's entry to survive the category filter, got %+v", filtered)
	}
	if hasEntryID(filtered, fxB.EntryID) {
		t.Fatalf("expected category B's entry to be excluded by the category filter, got %+v", filtered)
	}
}

// TestLexicalFiltersNarrowResultsByStarred checks StarredOnly.
func TestLexicalFiltersNarrowResultsByStarred(t *testing.T) {
	s := testStore(t)
	searcher := NewSearcher(s)
	db := testDB(t)

	fxStarred := createFixtureEntry(t, db, "filter-starred-yes", "Zzyzx Filter Starred", "<p>irrelevant</p>")
	writePassages(t, s, fxStarred.EntryID, []store.PassageRow{
		{Ordinal: 0, Text: "zzyzxstarredterm is starred", CharStart: 0, CharEnd: 28, Source: "content", Embedding: zeroEmbedding()},
	})
	if _, err := db.Exec(`UPDATE entries SET starred=true WHERE id=$1`, fxStarred.EntryID); err != nil {
		t.Fatalf("unable to star fixture: %v", err)
	}

	fxUnstarred := createFixtureEntry(t, db, "filter-starred-no", "Zzyzx Filter Unstarred", "<p>irrelevant</p>")
	writePassages(t, s, fxUnstarred.EntryID, []store.PassageRow{
		{Ordinal: 0, Text: "zzyzxstarredterm is not starred", CharStart: 0, CharEnd: 32, Source: "content", Embedding: zeroEmbedding()},
	})

	hits, err := searcher.Lexical(context.Background(), "zzyzxstarredterm", 10, Filters{StarredOnly: true})
	if err != nil {
		t.Fatalf("Lexical: unexpected error: %v", err)
	}
	if !hasEntryID(hits, fxStarred.EntryID) {
		t.Fatalf("expected the starred fixture to survive StarredOnly, got %+v", hits)
	}
	if hasEntryID(hits, fxUnstarred.EntryID) {
		t.Fatalf("expected the unstarred fixture to be excluded by StarredOnly, got %+v", hits)
	}
}

// TestLexicalFiltersNarrowResultsByDateRange checks Since/Until against
// entries.published_at.
func TestLexicalFiltersNarrowResultsByDateRange(t *testing.T) {
	s := testStore(t)
	searcher := NewSearcher(s)
	db := testDB(t)

	fxOld := createFixtureEntry(t, db, "filter-date-old", "Zzyzx Filter Date Old", "<p>irrelevant</p>")
	writePassages(t, s, fxOld.EntryID, []store.PassageRow{
		{Ordinal: 0, Text: "zzyzxdaterangeterm published long ago", CharStart: 0, CharEnd: 38, Source: "content", Embedding: zeroEmbedding()},
	})
	if _, err := db.Exec(`UPDATE entries SET published_at = '2000-01-01T00:00:00Z' WHERE id=$1`, fxOld.EntryID); err != nil {
		t.Fatalf("unable to set old published_at: %v", err)
	}

	fxRecent := createFixtureEntry(t, db, "filter-date-recent", "Zzyzx Filter Date Recent", "<p>irrelevant</p>")
	writePassages(t, s, fxRecent.EntryID, []store.PassageRow{
		{Ordinal: 0, Text: "zzyzxdaterangeterm published recently", CharStart: 0, CharEnd: 38, Source: "content", Embedding: zeroEmbedding()},
	})

	since := time.Now().Add(-24 * time.Hour)
	hits, err := searcher.Lexical(context.Background(), "zzyzxdaterangeterm", 10, Filters{Since: since})
	if err != nil {
		t.Fatalf("Lexical: unexpected error: %v", err)
	}
	if !hasEntryID(hits, fxRecent.EntryID) {
		t.Fatalf("expected the recent fixture to survive the Since filter, got %+v", hits)
	}
	if hasEntryID(hits, fxOld.EntryID) {
		t.Fatalf("expected the old fixture to be excluded by the Since filter, got %+v", hits)
	}
}

// TestLexicalHandlesHostilePgSearchSyntaxWithoutError is the safety test
// the task brief calls out explicitly: pg_search's query-string parser
// (what a naked `text @@@ $1::text` cast invokes) errors on unbalanced
// parens and reserved boolean keywords — verified directly against this
// database before writing this test. Lexical must never surface that as
// an error to a caller, and must never behave as though the hostile
// input were syntax rather than data.
func TestLexicalHandlesHostilePgSearchSyntaxWithoutError(t *testing.T) {
	s := testStore(t)
	searcher := NewSearcher(s)

	for _, q := range []string{
		"))",
		"AND OR NOT",
		"text:(())",
		`"unterminated quote`,
		"field:value OR 1=1",
		"((()))",
		"~*&^%$#@!",
		"NOT NOT NOT",
	} {
		hits, err := searcher.Lexical(context.Background(), q, 10, Filters{})
		if err != nil {
			t.Fatalf("Lexical(%q): expected no error for hostile input, got: %v", q, err)
		}
		_ = hits // no assertion on contents — only that it does not error
	}
}
