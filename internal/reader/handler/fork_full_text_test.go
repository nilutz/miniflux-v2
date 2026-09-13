// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package handler // import "miniflux.app/v2/internal/reader/handler"

import (
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	_ "github.com/lib/pq"

	"miniflux.app/v2/internal/config"
	"miniflux.app/v2/internal/model"
	"miniflux.app/v2/internal/storage"
)

// forkTestDB opens the integration database, or skips when there is none, so
// that "make test" stays hermetic.
func forkTestDB(t *testing.T) *sql.DB {
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

// configureForkTestOptions installs a configuration that lets the fetcher
// reach the httptest servers these tests point feeds at, which resolve to
// loopback addresses.
func configureForkTestOptions(t *testing.T) {
	t.Helper()

	t.Setenv("FETCHER_ALLOW_PRIVATE_NETWORKS", "1")

	parsedOptions, err := config.NewConfigParser().ParseEnvironmentVariables()
	if err != nil {
		t.Fatalf("unable to configure test options: %v", err)
	}

	previousOptions := config.Opts
	config.Opts = parsedOptions
	t.Cleanup(func() { config.Opts = previousOptions })
}

// createForkTestUser inserts a user and a category, and removes them when the
// test finishes. Feeds and entries are deleted along with the user.
func createForkTestUser(t *testing.T, db *sql.DB, username string) (userID, categoryID int64) {
	t.Helper()

	if err := db.QueryRow(
		`INSERT INTO users (username, password) VALUES ($1, 'x') RETURNING id`,
		username,
	).Scan(&userID); err != nil {
		t.Fatalf("unable to create user: %v", err)
	}
	t.Cleanup(func() {
		db.Exec(`DELETE FROM users WHERE id=$1`, userID)
	})

	if err := db.QueryRow(
		`INSERT INTO categories (user_id, title) VALUES ($1, 'Test') RETURNING id`,
		userID,
	).Scan(&categoryID); err != nil {
		t.Fatalf("unable to create category: %v", err)
	}

	return userID, categoryID
}

// TestCreateFeedRecordsFullTextForCrawledEntries covers the live crawler path:
// an entry the crawler scraped successfully must carry full_text_fetched_at,
// so the sidecar can tell a real article from an RSS excerpt and so the
// backfill command does not scrape it a second time. An entry whose page holds
// no article must stay pending instead.
func TestCreateFeedRecordsFullTextForCrawledEntries(t *testing.T) {
	db := forkTestDB(t)
	configureForkTestOptions(t)
	store := storage.NewStorage(db)

	userID, categoryID := createForkTestUser(t, db, "fork-crawler-marking")

	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		switch r.URL.Path {
		case "/feed.xml":
			w.Header().Set("Content-Type", "application/xml")
			fmt.Fprintf(w, feedXML, server.URL, server.URL, server.URL)
		case "/article":
			w.Write([]byte(articleHTML))
		case "/app-shell":
			w.Write([]byte(appShellHTML))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	feed, localizedError := CreateFeed(store, userID, &model.FeedCreationRequest{
		FeedURL:    server.URL + "/feed.xml",
		CategoryID: categoryID,
		Crawler:    true,
	})
	if localizedError != nil {
		t.Fatalf("unable to create feed: %v", localizedError.Error())
	}

	readEntry := func(url string) (content string, fetchedAt sql.NullTime) {
		t.Helper()
		if err := db.QueryRow(
			`SELECT content, full_text_fetched_at FROM entries WHERE feed_id=$1 AND url=$2`,
			feed.ID, url,
		).Scan(&content, &fetchedAt); err != nil {
			t.Fatalf("unable to read back entry %s: %v", url, err)
		}
		return content, fetchedAt
	}

	content, fetchedAt := readEntry(server.URL + "/article")
	if !fetchedAt.Valid {
		t.Fatalf("expected full_text_fetched_at to be set for a scraped entry, content: %s", content)
	}
	if !strings.Contains(content, "distinctive-marker-paragraph") {
		t.Fatalf("expected the scraped article to replace the feed excerpt, got: %s", content)
	}

	appShellContent, fetchedAt := readEntry(server.URL + "/app-shell")
	if fetchedAt.Valid {
		t.Fatal("expected full_text_fetched_at to stay null for a page holding no article")
	}
	if !strings.Contains(appShellContent, "another short rss excerpt") {
		t.Fatalf("expected an article-less scrape to leave the feed excerpt in place, got: %s", appShellContent)
	}
}

// TestCreateFeedLeavesEntriesPendingWithoutCrawler is the other half of the
// signal: without the crawler the entry only ever holds the feed excerpt, so
// it must stay pending for the backfill command.
func TestCreateFeedLeavesEntriesPendingWithoutCrawler(t *testing.T) {
	db := forkTestDB(t)
	configureForkTestOptions(t)
	store := storage.NewStorage(db)

	userID, categoryID := createForkTestUser(t, db, "fork-crawler-disabled")

	scrapes := 0
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/feed.xml":
			w.Header().Set("Content-Type", "application/xml")
			fmt.Fprintf(w, feedXML, server.URL, server.URL, server.URL)
		case "/article", "/app-shell":
			scrapes++
			w.Write([]byte(articleHTML))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	feed, localizedError := CreateFeed(store, userID, &model.FeedCreationRequest{
		FeedURL:    server.URL + "/feed.xml",
		CategoryID: categoryID,
		Crawler:    false,
	})
	if localizedError != nil {
		t.Fatalf("unable to create feed: %v", localizedError.Error())
	}

	if scrapes != 0 {
		t.Fatalf("expected no page to be scraped without the crawler, got %d", scrapes)
	}

	var pending int
	if err := db.QueryRow(
		`SELECT count(*) FROM entries WHERE feed_id=$1 AND full_text_fetched_at IS NULL`,
		feed.ID,
	).Scan(&pending); err != nil {
		t.Fatalf("unable to count pending entries: %v", err)
	}
	if pending != 2 {
		t.Fatalf("expected both entries to stay pending, got %d", pending)
	}
}

// feedXML is an RSS feed with two items: one pointing at a real article, one
// at a page that holds no article at all.
const feedXML = `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0">
<channel>
<title>Fork Test Feed</title>
<link>%s</link>
<item>
	<title>A Fork Test Article</title>
	<link>%s/article</link>
	<guid>fork-test-article</guid>
	<description>a short rss excerpt</description>
</item>
<item>
	<title>A Page Without An Article</title>
	<link>%s/app-shell</link>
	<guid>fork-test-app-shell</guid>
	<description>another short rss excerpt</description>
</item>
</channel>
</rss>`

// articleHTML is long enough, and structured enough, for readability to
// extract it as the main content rather than discarding it as boilerplate.
const articleHTML = `<!DOCTYPE html>
<html>
<head><title>A Fork Test Article</title></head>
<body>
<article>
<h1>A Fork Test Article</h1>
<p class="distinctive-marker-paragraph">This paragraph exists only so the test can assert that the scraped
content, and not the original RSS excerpt, ended up stored on the entry. It repeats a few
distinctive words: distinctive-marker-paragraph distinctive-marker-paragraph.</p>
<p>Readability-style content extraction favors pages with several substantial paragraphs of
real prose, so this article includes a handful of them, each long enough to look like genuine
body text rather than a navigation link or a footer notice.</p>
<p>Miniflux is a feed reader. This fork captures the full text of the articles it ingests so a
search index can be built over the corpus, which means recording which entries hold a real
article and which ones only ever held the excerpt that arrived in the feed.</p>
<p>The crawler runs while the feed is being refreshed, before the entries exist in the database,
so the column that carries that signal is written once the entries have been persisted, keyed
by the feed id and the entry hash they are stored under.</p>
<p>This final paragraph only pads the article out further, to comfortably clear whatever minimum
text density heuristic the readability extraction algorithm applies when deciding which parts of
the page are the article and which parts are chrome.</p>
</article>
</body>
</html>`

// appShellHTML holds no prose at all: readability returns markup with no text
// in it rather than failing, which must not count as full text.
const appShellHTML = `<!DOCTYPE html>
<html>
<head><title>Subscribe to read</title></head>
<body>
<div id="app"></div>
<script>window.__DATA__ = {};</script>
</body>
</html>`
