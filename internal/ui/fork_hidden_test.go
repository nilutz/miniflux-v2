// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui // import "miniflux.app/v2/internal/ui"

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/lib/pq"

	"miniflux.app/v2/internal/config"
	"miniflux.app/v2/internal/http/request"
	"miniflux.app/v2/internal/model"
	"miniflux.app/v2/internal/storage"
	"miniflux.app/v2/internal/template"
	"miniflux.app/v2/internal/ui/static"
)

// uiHiddenTestDB opens the integration database, or skips when there is
// none, so that "make test" stays hermetic. Mirrors
// internal/api/fork_hidden_test.go's apiHiddenTestDB.
func uiHiddenTestDB(t *testing.T) *sql.DB {
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

// configureUIHiddenTestOptions installs a parsed configuration so template
// functions and handlers that read config.Opts (WebAuthn, base path, ...)
// do not panic on a nil config.
func configureUIHiddenTestOptions(t *testing.T) {
	t.Helper()

	parsedOptions, err := config.NewConfigParser().ParseEnvironmentVariables()
	if err != nil {
		t.Fatalf("unable to configure test options: %v", err)
	}

	previousOptions := config.Opts
	config.Opts = parsedOptions
	t.Cleanup(func() { config.Opts = previousOptions })
}

// uiHiddenBundlesOnce generates the static asset bundles once for the whole
// test binary: they are read-only lookup tables the template funcMap and
// view.New() consult, expensive to regenerate, and safe to share across
// tests.
var uiHiddenBundlesOnce sync.Once

func configureUIHiddenTestBundles(t *testing.T) {
	t.Helper()

	var err error
	uiHiddenBundlesOnce.Do(func() {
		if genErr := static.GenerateBinaryBundles(); genErr != nil {
			err = genErr
			return
		}
		if genErr := static.GenerateStylesheetsBundles(); genErr != nil {
			err = genErr
			return
		}
		err = static.GenerateJavascriptBundles(false)
	})
	if err != nil {
		t.Fatalf("unable to generate static bundles: %v", err)
	}
}

// newUIHiddenTestHandler builds a real *handler backed by the integration
// database and a fully parsed template engine, so the handler functions
// under test render exactly as they would in production.
func newUIHiddenTestHandler(t *testing.T, store *storage.Storage) *handler {
	t.Helper()

	configureUIHiddenTestOptions(t)
	configureUIHiddenTestBundles(t)

	engine := template.NewEngine("")
	engine.ParseTemplates()

	return &handler{basePath: "", store: store, tpl: engine}
}

// uiHiddenTestFixture is a user, category, feed and single unread entry,
// ready to be hidden.
type uiHiddenTestFixture struct {
	userID     int64
	categoryID int64
	feedID     int64
	entryID    int64
	title      string
}

// createUIHiddenTestUserAndFeed inserts a user, category and feed, all
// removed (via FK cascade through the user row) when the test finishes.
// Callers insert as many entries as they need with insertUIHiddenTestEntry.
func createUIHiddenTestUserAndFeed(t *testing.T, db *sql.DB, username string) (userID, categoryID, feedID int64) {
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

	if err := db.QueryRow(
		`INSERT INTO feeds (feed_url, site_url, title, category_id, user_id)
		 VALUES ($1, $1, 'Test feed', $2, $3) RETURNING id`,
		"https://example.org/"+username+".xml", categoryID, userID,
	).Scan(&feedID); err != nil {
		t.Fatalf("unable to create feed: %v", err)
	}

	return userID, categoryID, feedID
}

// insertUIHiddenTestEntry inserts one unread entry for the given feed with
// an explicit published_at, so relative ordering between entries in a
// fixture is deterministic regardless of insertion speed.
func insertUIHiddenTestEntry(t *testing.T, db *sql.DB, userID, feedID int64, title, hash string, publishedAt time.Time) int64 {
	t.Helper()

	var entryID int64
	if err := db.QueryRow(
		`INSERT INTO entries (title, hash, url, status, published_at, changed_at, user_id, feed_id, content, author)
		 VALUES ($1, $2, 'https://example.org/post', 'unread', $3, now(), $4, $5, '<p>excerpt</p>', '')
		 RETURNING id`,
		title, hash, publishedAt, userID, feedID,
	).Scan(&entryID); err != nil {
		t.Fatalf("unable to create entry: %v", err)
	}

	return entryID
}

// createUIHiddenTestFixture inserts a user, category, feed and one unread
// entry with a distinctive title, all removed (via FK cascade through the
// user row) when the test finishes.
func createUIHiddenTestFixture(t *testing.T, db *sql.DB, username string) uiHiddenTestFixture {
	t.Helper()

	fixture := uiHiddenTestFixture{title: "Rating control entry for " + username}
	fixture.userID, fixture.categoryID, fixture.feedID = createUIHiddenTestUserAndFeed(t, db, username)
	fixture.entryID = insertUIHiddenTestEntry(t, db, fixture.userID, fixture.feedID, fixture.title, "hash-"+username, time.Now())

	return fixture
}

// withUIHiddenTestContext attaches a user ID and a fresh (unauthenticated
// state, valid otherwise) web session to the request, the same two pieces
// of context the web-session and CSRF middleware would normally supply.
func withUIHiddenTestContext(r *http.Request, userID int64) *http.Request {
	webSession, _ := model.NewWebSession("test-agent", "127.0.0.1")
	ctx := context.WithValue(r.Context(), request.UserIDContextKey, userID)
	ctx = context.WithValue(ctx, request.WebSessionContextKey, webSession)
	return r.WithContext(ctx)
}

// TestUnreadPageDropsHiddenEntryAndItsCount is the handler-level proof of
// spec §13.3's core requirement: hiding an entry must actually remove it
// from the rendered /unread page and from the unread counter, not merely
// from a query builder unit test. It asserts the entry was on the page
// first (a "not found" assertion can otherwise pass for the wrong reason),
// then that hiding makes it - and only its contribution to the counter -
// disappear.
func TestUnreadPageDropsHiddenEntryAndItsCount(t *testing.T) {
	db := uiHiddenTestDB(t)
	store := storage.NewStorage(db)
	fixture := createUIHiddenTestFixture(t, db, "unread-page-hide")
	h := newUIHiddenTestHandler(t, store)

	render := func() string {
		r := withUIHiddenTestContext(httptest.NewRequest(http.MethodGet, "/unread", nil), fixture.userID)
		w := httptest.NewRecorder()
		h.showUnreadPage(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("unexpected status code: %d, body: %s", w.Code, w.Body.String())
		}
		return w.Body.String()
	}

	before := render()
	if !strings.Contains(before, fixture.title) {
		t.Fatalf("expected the entry %q to be on the unread page before hiding it; body:\n%s", fixture.title, before)
	}
	if !strings.Contains(before, `unread-counter">1<`) {
		t.Fatalf("expected the unread counter to read 1 before hiding; body:\n%s", before)
	}

	if err := store.ToggleHidden(fixture.userID, fixture.entryID); err != nil {
		t.Fatalf("unable to hide entry: %v", err)
	}

	after := render()
	if strings.Contains(after, fixture.title) {
		t.Fatalf("expected the hidden entry %q to be gone from the unread page; body:\n%s", fixture.title, after)
	}
	if !strings.Contains(after, `unread-counter">0<`) {
		t.Fatalf("expected the unread counter to have dropped to 0 after hiding; body:\n%s", after)
	}
	if !strings.Contains(after, "There are no unread entries") {
		t.Fatalf("expected the unread list to render as empty after hiding the only entry; body:\n%s", after)
	}
}

// TestHiddenEntryStillReachableInFeedAndCategoryAllViews proves the other
// half of spec §13.3: hiding drops an entry from the unread list and
// nothing else. Both "all entries" views (which never filter by status)
// must still show it.
func TestHiddenEntryStillReachableInFeedAndCategoryAllViews(t *testing.T) {
	db := uiHiddenTestDB(t)
	store := storage.NewStorage(db)
	fixture := createUIHiddenTestFixture(t, db, "hidden-still-reachable")
	h := newUIHiddenTestHandler(t, store)

	if err := store.ToggleHidden(fixture.userID, fixture.entryID); err != nil {
		t.Fatalf("unable to hide entry: %v", err)
	}

	t.Run("feed entries all", func(t *testing.T) {
		r := withUIHiddenTestContext(httptest.NewRequest(http.MethodGet, "/feed/"+strconv.FormatInt(fixture.feedID, 10)+"/entries/all", nil), fixture.userID)
		r.SetPathValue("feedID", strconv.FormatInt(fixture.feedID, 10))
		w := httptest.NewRecorder()
		h.showFeedEntriesAllPage(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("unexpected status code: %d, body: %s", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), fixture.title) {
			t.Fatalf("expected the hidden entry %q to still appear under the feed's all-entries view", fixture.title)
		}
	})

	t.Run("category entries all", func(t *testing.T) {
		r := withUIHiddenTestContext(httptest.NewRequest(http.MethodGet, "/category/"+strconv.FormatInt(fixture.categoryID, 10)+"/entries/all", nil), fixture.userID)
		r.SetPathValue("categoryID", strconv.FormatInt(fixture.categoryID, 10))
		w := httptest.NewRecorder()
		h.showCategoryEntriesAllPage(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("unexpected status code: %d, body: %s", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), fixture.title) {
			t.Fatalf("expected the hidden entry %q to still appear under the category's all-entries view", fixture.title)
		}
	})
}

// TestFeedAndCategoryUnreadViewsDropHiddenEntry covers the default (unread
// only) feed and category views, the counterpart to the badge coverage
// already given to feed_query_builder.go / category.go: the list itself,
// not just the sidebar counter, must exclude a hidden entry.
func TestFeedAndCategoryUnreadViewsDropHiddenEntry(t *testing.T) {
	db := uiHiddenTestDB(t)
	store := storage.NewStorage(db)
	fixture := createUIHiddenTestFixture(t, db, "unread-scoped-views")
	h := newUIHiddenTestHandler(t, store)

	renderFeed := func() string {
		r := withUIHiddenTestContext(httptest.NewRequest(http.MethodGet, "/feed/"+strconv.FormatInt(fixture.feedID, 10)+"/entries", nil), fixture.userID)
		r.SetPathValue("feedID", strconv.FormatInt(fixture.feedID, 10))
		w := httptest.NewRecorder()
		h.showFeedEntriesPage(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("unexpected status code: %d, body: %s", w.Code, w.Body.String())
		}
		return w.Body.String()
	}

	renderCategory := func() string {
		r := withUIHiddenTestContext(httptest.NewRequest(http.MethodGet, "/category/"+strconv.FormatInt(fixture.categoryID, 10)+"/entries", nil), fixture.userID)
		r.SetPathValue("categoryID", strconv.FormatInt(fixture.categoryID, 10))
		w := httptest.NewRecorder()
		h.showCategoryEntriesPage(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("unexpected status code: %d, body: %s", w.Code, w.Body.String())
		}
		return w.Body.String()
	}

	if !strings.Contains(renderFeed(), fixture.title) {
		t.Fatalf("expected the entry %q on the feed's unread view before hiding it", fixture.title)
	}
	if !strings.Contains(renderCategory(), fixture.title) {
		t.Fatalf("expected the entry %q on the category's unread view before hiding it", fixture.title)
	}

	if err := store.ToggleHidden(fixture.userID, fixture.entryID); err != nil {
		t.Fatalf("unable to hide entry: %v", err)
	}

	if strings.Contains(renderFeed(), fixture.title) {
		t.Fatal("expected the hidden entry to be gone from the feed's unread view")
	}
	if strings.Contains(renderCategory(), fixture.title) {
		t.Fatal("expected the hidden entry to be gone from the category's unread view")
	}
}

// TestUnreadEntryPaginationSkipsHiddenNeighbor proves
// entryPaginationBuilder.WithHidden actually reaches the single-entry
// unread view's prev/next navigation: hiding the older neighbour must
// remove it as a "previous entry" candidate, consistent with it being
// gone from the unread list itself.
func TestUnreadEntryPaginationSkipsHiddenNeighbor(t *testing.T) {
	db := uiHiddenTestDB(t)
	store := storage.NewStorage(db)
	h := newUIHiddenTestHandler(t, store)

	username := "unread-pagination-hidden"
	userID, _, feedID := createUIHiddenTestUserAndFeed(t, db, username)

	olderTitle := "Older neighbour for " + username
	olderID := insertUIHiddenTestEntry(t, db, userID, feedID, olderTitle, "hash-older-"+username, time.Now().Add(-time.Hour))
	newerID := insertUIHiddenTestEntry(t, db, userID, feedID, "Newer entry for "+username, "hash-newer-"+username, time.Now())

	render := func() string {
		r := withUIHiddenTestContext(httptest.NewRequest(http.MethodGet, "/unread/entry/"+strconv.FormatInt(newerID, 10), nil), userID)
		r.SetPathValue("entryID", strconv.FormatInt(newerID, 10))
		w := httptest.NewRecorder()
		h.showUnreadEntryPage(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("unexpected status code: %d, body: %s", w.Code, w.Body.String())
		}
		return w.Body.String()
	}

	before := render()
	if !strings.Contains(before, `title="`+olderTitle+`"`) {
		t.Fatalf("expected the older entry %q to be offered as the previous entry before hiding it; body:\n%s", olderTitle, before)
	}

	if err := store.ToggleHidden(userID, olderID); err != nil {
		t.Fatalf("unable to hide entry: %v", err)
	}

	after := render()
	if strings.Contains(after, `title="`+olderTitle+`"`) {
		t.Fatalf("expected the hidden entry %q to no longer be offered as the previous entry; body:\n%s", olderTitle, after)
	}
}

// TestUnreadCategoryEntryPaginationSkipsHiddenNeighbor is
// TestUnreadEntryPaginationSkipsHiddenNeighbor's category-scoped sibling.
// showUnreadCategoryEntryPage has its own entryPaginationBuilder chain and
// its own WithHidden(false) call site (internal/ui/unread_entry_category.go)
// - proven here on its own, not folded into the plain /unread/entry test,
// so removing it here alone (and not from unread_entry_feed.go too) is what
// makes this test fail, rather than the two call sites masking each other.
func TestUnreadCategoryEntryPaginationSkipsHiddenNeighbor(t *testing.T) {
	db := uiHiddenTestDB(t)
	store := storage.NewStorage(db)
	h := newUIHiddenTestHandler(t, store)

	username := "unread-category-pagination-hidden"
	userID, categoryID, feedID := createUIHiddenTestUserAndFeed(t, db, username)

	olderTitle := "Older category neighbour for " + username
	olderID := insertUIHiddenTestEntry(t, db, userID, feedID, olderTitle, "hash-cat-older-"+username, time.Now().Add(-time.Hour))
	newerID := insertUIHiddenTestEntry(t, db, userID, feedID, "Newer category entry for "+username, "hash-cat-newer-"+username, time.Now())

	render := func() string {
		r := withUIHiddenTestContext(httptest.NewRequest(http.MethodGet, "/unread/category/"+strconv.FormatInt(categoryID, 10)+"/entry/"+strconv.FormatInt(newerID, 10), nil), userID)
		r.SetPathValue("categoryID", strconv.FormatInt(categoryID, 10))
		r.SetPathValue("entryID", strconv.FormatInt(newerID, 10))
		w := httptest.NewRecorder()
		h.showUnreadCategoryEntryPage(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("unexpected status code: %d, body: %s", w.Code, w.Body.String())
		}
		return w.Body.String()
	}

	before := render()
	if !strings.Contains(before, `title="`+olderTitle+`"`) {
		t.Fatalf("expected the older entry %q to be offered as the previous entry before hiding it; body:\n%s", olderTitle, before)
	}

	if err := store.ToggleHidden(userID, olderID); err != nil {
		t.Fatalf("unable to hide entry: %v", err)
	}

	after := render()
	if strings.Contains(after, `title="`+olderTitle+`"`) {
		t.Fatalf("expected the hidden entry %q to no longer be offered as the previous entry in the category-scoped unread view; body:\n%s", olderTitle, after)
	}
}

// TestUnreadFeedEntryPaginationSkipsHiddenNeighbor is
// TestUnreadEntryPaginationSkipsHiddenNeighbor's feed-scoped sibling; see
// the category variant's comment for why it is proven independently.
func TestUnreadFeedEntryPaginationSkipsHiddenNeighbor(t *testing.T) {
	db := uiHiddenTestDB(t)
	store := storage.NewStorage(db)
	h := newUIHiddenTestHandler(t, store)

	username := "unread-feed-pagination-hidden"
	userID, _, feedID := createUIHiddenTestUserAndFeed(t, db, username)

	olderTitle := "Older feed neighbour for " + username
	olderID := insertUIHiddenTestEntry(t, db, userID, feedID, olderTitle, "hash-feed-older-"+username, time.Now().Add(-time.Hour))
	newerID := insertUIHiddenTestEntry(t, db, userID, feedID, "Newer feed entry for "+username, "hash-feed-newer-"+username, time.Now())

	render := func() string {
		r := withUIHiddenTestContext(httptest.NewRequest(http.MethodGet, "/unread/feed/"+strconv.FormatInt(feedID, 10)+"/entry/"+strconv.FormatInt(newerID, 10), nil), userID)
		r.SetPathValue("feedID", strconv.FormatInt(feedID, 10))
		r.SetPathValue("entryID", strconv.FormatInt(newerID, 10))
		w := httptest.NewRecorder()
		h.showUnreadFeedEntryPage(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("unexpected status code: %d, body: %s", w.Code, w.Body.String())
		}
		return w.Body.String()
	}

	before := render()
	if !strings.Contains(before, `title="`+olderTitle+`"`) {
		t.Fatalf("expected the older entry %q to be offered as the previous entry before hiding it; body:\n%s", olderTitle, before)
	}

	if err := store.ToggleHidden(userID, olderID); err != nil {
		t.Fatalf("unable to hide entry: %v", err)
	}

	after := render()
	if strings.Contains(after, `title="`+olderTitle+`"`) {
		t.Fatalf("expected the hidden entry %q to no longer be offered as the previous entry in the feed-scoped unread view; body:\n%s", olderTitle, after)
	}
}

// TestUnreadPageOffsetFallbackExcludesHiddenEntry covers
// showUnreadPage's second query, the offset-reset fallback taken when the
// requested offset is stale or out of range (offset >= countUnread &&
// countUnread > 0). It has its own WithHidden(false) call, separate from
// the primary query's, and nothing else in this suite drives a request
// down that branch.
func TestUnreadPageOffsetFallbackExcludesHiddenEntry(t *testing.T) {
	db := uiHiddenTestDB(t)
	store := storage.NewStorage(db)
	h := newUIHiddenTestHandler(t, store)

	username := "unread-offset-fallback"
	userID, _, feedID := createUIHiddenTestUserAndFeed(t, db, username)

	hiddenTitle := "Fallback hidden entry for " + username
	hiddenID := insertUIHiddenTestEntry(t, db, userID, feedID, hiddenTitle, "hash-fallback-hidden-"+username, time.Now())
	visibleTitle := "Fallback visible entry for " + username
	insertUIHiddenTestEntry(t, db, userID, feedID, visibleTitle, "hash-fallback-visible-"+username, time.Now())

	if err := store.ToggleHidden(userID, hiddenID); err != nil {
		t.Fatalf("unable to hide entry: %v", err)
	}

	// The one non-hidden entry makes countUnread 1; any offset >= 1 forces
	// showUnreadPage's fallback branch, which re-runs the query with
	// offset reset to 0.
	r := withUIHiddenTestContext(httptest.NewRequest(http.MethodGet, "/unread?offset=50", nil), userID)
	w := httptest.NewRecorder()
	h.showUnreadPage(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("unexpected status code: %d, body: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	if !strings.Contains(body, visibleTitle) {
		t.Fatalf("expected the visible entry %q to appear via the offset-reset fallback query; body:\n%s", visibleTitle, body)
	}
	if strings.Contains(body, hiddenTitle) {
		t.Fatalf("expected the hidden entry %q to stay excluded from the offset-reset fallback query; body:\n%s", hiddenTitle, body)
	}
	if !strings.Contains(body, `unread-counter">1<`) {
		t.Fatalf("expected the fallback query's count to read 1, excluding the hidden entry; body:\n%s", body)
	}
}

// TestToggleHiddenHandlerFlipsTheFlag exercises the UI route added for the
// rating control, mirroring toggleStarred's own coverage.
func TestToggleHiddenHandlerFlipsTheFlag(t *testing.T) {
	db := uiHiddenTestDB(t)
	store := storage.NewStorage(db)
	fixture := createUIHiddenTestFixture(t, db, "toggle-hidden-ui")
	h := &handler{store: store}

	toggle := func() {
		r := withUIHiddenTestContext(httptest.NewRequest(http.MethodPost, "/entry/hide/"+strconv.FormatInt(fixture.entryID, 10), nil), fixture.userID)
		r.SetPathValue("entryID", strconv.FormatInt(fixture.entryID, 10))
		w := httptest.NewRecorder()
		h.toggleHidden(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("unexpected status code: %d, body: %s", w.Code, w.Body.String())
		}
	}

	var hidden bool
	if err := db.QueryRow(`SELECT hidden FROM entries WHERE id=$1`, fixture.entryID).Scan(&hidden); err != nil {
		t.Fatalf("unable to read back entry: %v", err)
	}
	if hidden {
		t.Fatal("expected a freshly created entry to not be hidden")
	}

	toggle()
	if err := db.QueryRow(`SELECT hidden FROM entries WHERE id=$1`, fixture.entryID).Scan(&hidden); err != nil {
		t.Fatalf("unable to read back entry: %v", err)
	}
	if !hidden {
		t.Fatal("expected the entry to be hidden after POST /entry/hide/{id}")
	}

	toggle()
	if err := db.QueryRow(`SELECT hidden FROM entries WHERE id=$1`, fixture.entryID).Scan(&hidden); err != nil {
		t.Fatalf("unable to read back entry: %v", err)
	}
	if hidden {
		t.Fatal("expected the entry to no longer be hidden after toggling again")
	}
}
