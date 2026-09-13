// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cli // import "miniflux.app/v2/internal/cli"

import (
	"bytes"
	"database/sql"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"miniflux.app/v2/internal/config"
	"miniflux.app/v2/internal/storage"
)

// TestMain sets FETCHER_ALLOW_PRIVATE_NETWORKS before any test in this
// package parses config.Opts (a package-level singleton in package config,
// shared for the lifetime of the test binary). Without it, the scraper
// refuses to fetch the httptest servers these tests point entries at,
// because they resolve to loopback addresses.
func TestMain(m *testing.M) {
	os.Setenv("FETCHER_ALLOW_PRIVATE_NETWORKS", "1")
	os.Exit(m.Run())
}

// initTestConfig mirrors the guard in cleanup_tasks_test.go: config.Opts is a
// package-level singleton, so only the first test to run needs to parse it.
func initTestConfig(t *testing.T) {
	t.Helper()

	if config.Opts != nil {
		return
	}

	cfg := config.NewConfigParser()
	opts, err := cfg.ParseEnvironmentVariables()
	if err != nil {
		t.Fatalf("unable to parse configuration: %v", err)
	}
	config.Opts = opts
}

// createBackfillTestEntry inserts a user, category, feed and entry pointing
// at entryURL, with the given content, and returns the entry id. Everything
// is removed when the test finishes.
func createBackfillTestEntry(t *testing.T, db *sql.DB, username, entryURL, content string) (entryID int64) {
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

	var feedID int64
	if err := db.QueryRow(
		`INSERT INTO feeds (feed_url, site_url, title, category_id, user_id)
		 VALUES ($1, $1, 'Test feed', $2, $3) RETURNING id`,
		"https://example.org/"+username+".xml", categoryID, userID,
	).Scan(&feedID); err != nil {
		t.Fatalf("unable to create feed: %v", err)
	}

	if err := db.QueryRow(
		`INSERT INTO entries (title, hash, url, author, published_at, changed_at, user_id, feed_id, content)
		 VALUES ('Test entry', $1, $2, '', now(), now(), $3, $4, $5)
		 RETURNING id`,
		"hash-"+username, entryURL, userID, feedID, content,
	).Scan(&entryID); err != nil {
		t.Fatalf("unable to create entry: %v", err)
	}

	return entryID
}

// captureLogs redirects the default slog logger to a buffer for the duration
// of the test and restores the previous logger afterwards.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()

	var buf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	return &buf
}

// TestBackfillFullTextSkipsEntriesAlreadyMarkedFetched is the checkpoint
// regression guard: an entry already marked via MarkFullTextFetched must not
// be revisited (and, in particular, must never trigger a network fetch).
func TestBackfillFullTextSkipsEntriesAlreadyMarkedFetched(t *testing.T) {
	db := testDB(t)
	initTestConfig(t)
	store := storage.NewStorage(db)

	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Write([]byte("<html><body>should never be fetched</body></html>"))
	}))
	defer server.Close()

	const originalContent = "<p>original excerpt</p>"
	entryID := createBackfillTestEntry(t, db, "backfill-checkpoint", server.URL, originalContent)
	fetchedAt := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := store.MarkFullTextFetched(entryID, fetchedAt); err != nil {
		t.Fatalf("unable to mark entry as fetched: %v", err)
	}

	backfillFullText(store, 0)

	if requests != 0 {
		t.Fatalf("expected the checkpointed entry to never be fetched, got %d requests", requests)
	}

	var content string
	var refetchedAt sql.NullTime
	if err := db.QueryRow(`SELECT content, full_text_fetched_at FROM entries WHERE id=$1`, entryID).Scan(&content, &refetchedAt); err != nil {
		t.Fatalf("unable to read back entry: %v", err)
	}
	if content != originalContent {
		t.Fatalf("expected content to remain untouched, got: %s", content)
	}
	if !refetchedAt.Valid || !refetchedAt.Time.Equal(fetchedAt) {
		t.Fatalf("expected full_text_fetched_at to remain %v, got %v", fetchedAt, refetchedAt)
	}
}

// TestBackfillFullTextPaginatesAcrossBatches creates more pending entries
// than backfillBatchSize and verifies the loop advances its checkpoint far
// enough to visit every one of them in a single call, across multiple pages.
func TestBackfillFullTextPaginatesAcrossBatches(t *testing.T) {
	db := testDB(t)
	initTestConfig(t)
	store := storage.NewStorage(db)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(articleHTML))
	}))
	defer server.Close()

	// One more than a full page is all it takes to cross the batch boundary,
	// and each extra entry costs a real scrape.
	entryCount := backfillBatchSize + 1
	entryIDs := make([]int64, 0, entryCount)
	for i := 0; i < entryCount; i++ {
		username := fmt.Sprintf("backfill-page-%d", i)
		entryIDs = append(entryIDs, createBackfillTestEntry(t, db, username, server.URL, "<p>excerpt</p>"))
	}

	backfillFullText(store, 0)

	for _, entryID := range entryIDs {
		var fetchedAt sql.NullTime
		if err := db.QueryRow(`SELECT full_text_fetched_at FROM entries WHERE id=$1`, entryID).Scan(&fetchedAt); err != nil {
			t.Fatalf("unable to read back entry #%d: %v", entryID, err)
		}
		if !fetchedAt.Valid {
			t.Fatalf("entry #%d was not marked as fetched; pagination missed it", entryID)
		}
	}
}

// TestBackfillFullTextScrapesAndPersistsContent exercises the real scrape
// path against an httptest server standing in for the public internet: the
// entry's excerpt must be replaced with the scraped article content, and
// full_text_fetched_at must be set. It drives backfillEntry rather than the
// whole-database walk so the assertions cannot be disturbed by entries other
// tests write to the same database concurrently.
func TestBackfillFullTextScrapesAndPersistsContent(t *testing.T) {
	db := testDB(t)
	initTestConfig(t)
	store := storage.NewStorage(db)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(articleHTML))
	}))
	defer server.Close()

	const excerpt = "<p>a short rss excerpt</p>"
	entryID := createBackfillTestEntry(t, db, "backfill-scrape", server.URL, excerpt)

	logs := captureLogs(t)
	if !backfillEntry(store, entryID) {
		t.Fatalf("expected the scrape to succeed, logs: %s", logs.String())
	}

	var content string
	var fetchedAt sql.NullTime
	if err := db.QueryRow(`SELECT content, full_text_fetched_at FROM entries WHERE id=$1`, entryID).Scan(&content, &fetchedAt); err != nil {
		t.Fatalf("unable to read back entry: %v", err)
	}

	if !fetchedAt.Valid {
		t.Logf("logs: %s", logs.String())
		t.Fatal("expected full_text_fetched_at to be set after a successful scrape")
	}
	if len(content) <= len(excerpt) {
		t.Fatalf("expected scraped content to be longer than the excerpt (%d bytes); got %d bytes: %s", len(excerpt), len(content), content)
	}
	if !strings.Contains(content, "distinctive-marker-paragraph") {
		t.Fatalf("expected scraped content to contain the article body, got: %s", content)
	}
}

// TestBackfillFullTextLeavesFailedEntryRetryable is the failure-path
// regression guard: when the target page cannot be scraped (a 404 here),
// full_text_fetched_at must be left null so a later run retries the entry.
func TestBackfillFullTextLeavesFailedEntryRetryable(t *testing.T) {
	db := testDB(t)
	initTestConfig(t)
	store := storage.NewStorage(db)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer server.Close()

	const originalContent = "<p>excerpt</p>"
	entryID := createBackfillTestEntry(t, db, "backfill-failure", server.URL, originalContent)

	if backfillEntry(store, entryID) {
		t.Fatal("expected a 404 to be reported as a failed scrape")
	}

	var content string
	var fetchedAt sql.NullTime
	if err := db.QueryRow(`SELECT content, full_text_fetched_at FROM entries WHERE id=$1`, entryID).Scan(&content, &fetchedAt); err != nil {
		t.Fatalf("unable to read back entry: %v", err)
	}
	if fetchedAt.Valid {
		t.Fatal("expected full_text_fetched_at to remain null after a failed scrape")
	}
	if content != originalContent {
		t.Fatalf("expected content to remain untouched after a failed scrape, got: %s", content)
	}

	pending, err := store.EntryIDsWithoutFullText(entryID-1, 10)
	if err != nil {
		t.Fatalf("unable to list pending entries: %v", err)
	}
	found := false
	for _, id := range pending {
		if id == entryID {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected entry #%d to still be pending after a failed scrape", entryID)
	}
}

// TestBackfillEntryLeavesPageWithoutArticlePending is the regression guard for
// the silent-success path: a page that fetches fine but holds no extractable
// article (a paywall, a JavaScript-only app shell, a link farm) makes
// scraper.ScrapeWebsite discard readability's error and return an empty
// string, so ProcessEntryWebPage reports no error and leaves entry.Content
// alone. The entry still holds its feed excerpt, so it must stay pending
// rather than being checkpointed forever.
func TestBackfillEntryLeavesPageWithoutArticlePending(t *testing.T) {
	db := testDB(t)
	initTestConfig(t)
	store := storage.NewStorage(db)

	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(appShellHTML))
	}))
	defer server.Close()

	const originalContent = "<p>the only text this entry will ever have</p>"
	entryID := createBackfillTestEntry(t, db, "backfill-no-article", server.URL, originalContent)

	logs := captureLogs(t)

	if backfillEntry(store, entryID) {
		var stored string
		db.QueryRow(`SELECT content FROM entries WHERE id=$1`, entryID).Scan(&stored)
		t.Fatalf("expected a page without an article to count as a failed scrape, stored content: %q", stored)
	}

	if requests != 1 {
		t.Fatalf("expected exactly one fetch of the page, got %d", requests)
	}

	var content string
	var fetchedAt sql.NullTime
	if err := db.QueryRow(`SELECT content, full_text_fetched_at FROM entries WHERE id=$1`, entryID).Scan(&content, &fetchedAt); err != nil {
		t.Fatalf("unable to read back entry: %v", err)
	}
	if fetchedAt.Valid {
		t.Fatal("expected full_text_fetched_at to remain null when no article was extracted")
	}
	if content != originalContent {
		t.Fatalf("expected the excerpt to remain untouched, got: %s", content)
	}

	if !strings.Contains(logs.String(), "Scraper returned no article content") {
		t.Fatalf("expected a log line explaining why the entry stays pending, got: %s", logs.String())
	}

	pending, err := store.EntryIDsWithoutFullText(entryID-1, 10)
	if err != nil {
		t.Fatalf("unable to list pending entries: %v", err)
	}
	if !slices.Contains(pending, entryID) {
		t.Fatalf("expected entry #%d to still be pending", entryID)
	}
}

// appShellHTML is a page that answers 200 OK with well-formed HTML that holds
// no prose at all, the shape a JavaScript-only single page app serves to a
// scraper that does not run scripts. Readability finds no candidate and
// returns an empty string without an error.
//
// Note that "no extractable article" is not the same as "no extracted
// content": a paywall page whose markup still contains navigation and a
// footer yields that boilerplate as the extracted article, which no amount of
// checking here can tell apart from a real one.
const appShellHTML = `<!DOCTYPE html>
<html>
<head><title>Subscribe to read</title></head>
<body>
<div id="app"></div>
<script>window.__DATA__ = {};</script>
</body>
</html>`

// articleHTML is long enough, and structured enough, for go-readability to
// extract it as the main content rather than discarding it as boilerplate.
const articleHTML = `<!DOCTYPE html>
<html>
<head><title>A Backfill Test Article</title></head>
<body>
<article>
<h1>A Backfill Test Article</h1>
<p class="distinctive-marker-paragraph">This paragraph exists only so the test can assert that the scraped
content, and not the original RSS excerpt, ended up stored on the entry. It repeats a few
distinctive words: distinctive-marker-paragraph distinctive-marker-paragraph.</p>
<p>Readability-style content extraction favors pages with several substantial paragraphs of
real prose, so this article includes a handful of them, each long enough to look like genuine
body text rather than a navigation link or a footer notice.</p>
<p>Miniflux is a feed reader. This fork adds a full-text backfill command that walks entries
which predate the crawler being turned on by default, scrapes their original web page, and
stores the extracted article content in place of the short excerpt that arrived in the feed.</p>
<p>The backfill command checkpoints on the last entry id it processed, so an interrupted run can
be resumed later without re-scraping entries that already succeeded, and without re-attempting
entries from a different, unrelated failure in a tight loop within the same run.</p>
<p>This final paragraph only pads the article out further, to comfortably clear whatever minimum
text density heuristic the readability extraction algorithm applies when deciding which parts of
the page are the article and which parts are chrome.</p>
</article>
</body>
</html>`
