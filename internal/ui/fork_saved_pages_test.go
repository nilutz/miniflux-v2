// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui // import "miniflux.app/v2/internal/ui"

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"miniflux.app/v2/internal/storage"
)

// createUISavedPagesTestEntry gets-or-creates userID's saved-pages feed
// (the same lazy-creation path saveWebPage uses) and inserts one entry
// into it, for fixtures that need an existing saved page. It is a test
// setup helper, not the code under test: showSavedPagesPage itself must
// never call GetOrCreateSavedPagesFeed - see
// TestSavedPagesPageEmptyStateNeverCreatesFeed below, which asserts that
// directly against the database rather than only against rendered HTML.
func createUISavedPagesTestEntry(t *testing.T, db *sql.DB, store *storage.Storage, userID, categoryID int64, title, hash string, publishedAt time.Time) (feedID, entryID int64) {
	t.Helper()

	feed, err := store.GetOrCreateSavedPagesFeed(userID, categoryID, "Saved pages")
	if err != nil {
		t.Fatalf("unable to create saved-pages feed fixture: %v", err)
	}

	entryID = insertUIHiddenTestEntry(t, db, userID, feed.ID, title, hash, publishedAt)
	return feed.ID, entryID
}

// TestSavedPagesPageEmptyStateNeverCreatesFeed is the trap the task brief
// calls out explicitly: GetOrCreateSavedPagesFeed creates the feed lazily,
// so a *listing* page must look it up read-only (storage.SavedPagesFeed)
// or merely opening /saved-pages would conjure an empty synthetic feed
// into the feed list of every user who has never saved anything. Asserting
// only on the rendered HTML would pass even if the handler called the
// creating path, so this checks the feeds table directly, after the
// request.
func TestSavedPagesPageEmptyStateNeverCreatesFeed(t *testing.T) {
	db := uiHiddenTestDB(t)
	store := storage.NewStorage(db)
	userID, _, _ := createUIHiddenTestUserAndFeed(t, db, "saved-pages-empty")
	h := newUIHiddenTestHandler(t, store)

	r := withUIHiddenTestContext(httptest.NewRequest(http.MethodGet, "/saved-pages", nil), userID)
	w := httptest.NewRecorder()
	h.showSavedPagesPage(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("unexpected status code: %d, body: %s", w.Code, w.Body.String())
	}

	body := w.Body.String()
	if !strings.Contains(body, "haven&#39;t saved any pages") && !strings.Contains(body, "haven't saved any pages") {
		t.Fatalf("expected the empty-state alert on a page with no saved pages; body:\n%s", body)
	}

	var feedCount int
	if err := db.QueryRow(
		`SELECT count(*) FROM feeds WHERE user_id=$1 AND feed_url=$2`,
		userID, storage.SavedPagesFeedURL(userID),
	).Scan(&feedCount); err != nil {
		t.Fatalf("unable to query feeds table: %v", err)
	}
	if feedCount != 0 {
		t.Fatalf("expected visiting /saved-pages with nothing saved to create no feed row, found %d", feedCount)
	}
}

// TestSavedPagesPageListsOwnEntries proves the straightforward positive
// case: a user who has saved a page sees it listed.
func TestSavedPagesPageListsOwnEntries(t *testing.T) {
	db := uiHiddenTestDB(t)
	store := storage.NewStorage(db)
	userID, categoryID, _ := createUIHiddenTestUserAndFeed(t, db, "saved-pages-list")
	title := "My saved article"
	createUISavedPagesTestEntry(t, db, store, userID, categoryID, title, "hash-saved-list", time.Now())
	h := newUIHiddenTestHandler(t, store)

	r := withUIHiddenTestContext(httptest.NewRequest(http.MethodGet, "/saved-pages", nil), userID)
	w := httptest.NewRecorder()
	h.showSavedPagesPage(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("unexpected status code: %d, body: %s", w.Code, w.Body.String())
	}

	body := w.Body.String()
	if !strings.Contains(body, title) {
		t.Fatalf("expected the saved page %q to be listed on /saved-pages; body:\n%s", title, body)
	}
}

// TestSavedPagesPageIsolatedPerUser proves one user's saved pages are
// never visible to another: both users get their own saved-pages feed
// (spec §13.4, "one feed per user"), and each user's /saved-pages must
// only ever render their own.
func TestSavedPagesPageIsolatedPerUser(t *testing.T) {
	db := uiHiddenTestDB(t)
	store := storage.NewStorage(db)
	userA, categoryA, _ := createUIHiddenTestUserAndFeed(t, db, "saved-pages-isolation-a")
	userB, categoryB, _ := createUIHiddenTestUserAndFeed(t, db, "saved-pages-isolation-b")

	titleA := "User A's saved page"
	titleB := "User B's saved page"
	createUISavedPagesTestEntry(t, db, store, userA, categoryA, titleA, "hash-isolation-a", time.Now())
	createUISavedPagesTestEntry(t, db, store, userB, categoryB, titleB, "hash-isolation-b", time.Now())

	h := newUIHiddenTestHandler(t, store)

	render := func(userID int64) string {
		r := withUIHiddenTestContext(httptest.NewRequest(http.MethodGet, "/saved-pages", nil), userID)
		w := httptest.NewRecorder()
		h.showSavedPagesPage(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("unexpected status code: %d, body: %s", w.Code, w.Body.String())
		}
		return w.Body.String()
	}

	bodyA := render(userA)
	if !strings.Contains(bodyA, titleA) {
		t.Fatalf("expected user A to see their own saved page %q; body:\n%s", titleA, bodyA)
	}
	if strings.Contains(bodyA, titleB) {
		t.Fatalf("expected user A to never see user B's saved page %q; body:\n%s", titleB, bodyA)
	}

	bodyB := render(userB)
	if !strings.Contains(bodyB, titleB) {
		t.Fatalf("expected user B to see their own saved page %q; body:\n%s", titleB, bodyB)
	}
	if strings.Contains(bodyB, titleA) {
		t.Fatalf("expected user B to never see user A's saved page %q; body:\n%s", titleA, bodyB)
	}
}

// TestSavedPagesPageEntryLinksCarrySortAndHiddenParams proves the sort
// picker's order/direction and the hidden filter round-trip through the
// list's per-entry links, the same way task 10 proved it for search.
func TestSavedPagesPageEntryLinksCarrySortAndHiddenParams(t *testing.T) {
	db := uiHiddenTestDB(t)
	store := storage.NewStorage(db)
	userID, categoryID, _ := createUIHiddenTestUserAndFeed(t, db, "saved-pages-roundtrip")
	_, entryID := createUISavedPagesTestEntry(t, db, store, userID, categoryID, "Round trip entry", "hash-roundtrip", time.Now())
	h := newUIHiddenTestHandler(t, store)

	r := withUIHiddenTestContext(httptest.NewRequest(http.MethodGet, "/saved-pages?order=title&direction=asc&excludeHidden=1", nil), userID)
	w := httptest.NewRecorder()
	h.showSavedPagesPage(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("unexpected status code: %d, body: %s", w.Code, w.Body.String())
	}

	body := w.Body.String()
	wantHref := "/saved-pages/entry/" + strconv.FormatInt(entryID, 10) + "?direction=asc&amp;excludeHidden=1&amp;order=title"
	if !strings.Contains(body, wantHref) {
		t.Fatalf("expected the entry link to carry order/direction/excludeHidden as %q; body:\n%s", wantHref, body)
	}
}

// TestSavedPagesEntryPageHonoursOrderAndCarriesParamsInNeighborLinks
// proves the failure task 10 shipped twice for search does not recur here:
// the single-entry page's prev/next links must walk the *same* order as
// the list, not the reader's saved default. It picks an order (title,
// ascending) that disagrees with insertion/published order, so a handler
// that silently ignored the "order" parameter and fell back to
// published_at would produce the wrong neighbour here.
func TestSavedPagesEntryPageHonoursOrderAndCarriesParamsInNeighborLinks(t *testing.T) {
	db := uiHiddenTestDB(t)
	store := storage.NewStorage(db)
	userID, categoryID, _ := createUIHiddenTestUserAndFeed(t, db, "saved-pages-order")
	feed, err := store.GetOrCreateSavedPagesFeed(userID, categoryID, "Saved pages")
	if err != nil {
		t.Fatalf("unable to create saved-pages feed fixture: %v", err)
	}

	// Beta is published *before* Alpha, so published_at order would put
	// Beta first - only sorting by title (ascending) puts Alpha first.
	alphaID := insertUIHiddenTestEntry(t, db, userID, feed.ID, "Alpha saved entry", "hash-order-alpha", time.Now())
	betaID := insertUIHiddenTestEntry(t, db, userID, feed.ID, "Beta saved entry", "hash-order-beta", time.Now().Add(-time.Hour))

	h := newUIHiddenTestHandler(t, store)

	r := withUIHiddenTestContext(httptest.NewRequest(http.MethodGet, "/saved-pages/entry/"+strconv.FormatInt(alphaID, 10)+"?order=title&direction=asc&excludeHidden=1", nil), userID)
	r.SetPathValue("entryID", strconv.FormatInt(alphaID, 10))
	w := httptest.NewRecorder()
	h.showSavedPagesEntryPage(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("unexpected status code: %d, body: %s", w.Code, w.Body.String())
	}

	body := w.Body.String()
	wantNextHref := "/saved-pages/entry/" + strconv.FormatInt(betaID, 10) + "?direction=asc&amp;excludeHidden=1&amp;order=title"
	if !strings.Contains(body, wantNextHref) {
		t.Fatalf("expected the title-ascending next-entry link %q (Alpha, then Beta); body:\n%s", wantNextHref, body)
	}
}

// TestSavedPagesHiddenEntryExcludedFromUnreadCountButStillListed covers
// spec §13.3 as it applies here: hiding a saved page drops it from the
// unread counter (the same rule as every other entry) but it must stay
// reachable on /saved-pages, since hiding means "not now", not "never",
// and the whole point of this view is to still be able to find it.
func TestSavedPagesHiddenEntryExcludedFromUnreadCountButStillListed(t *testing.T) {
	db := uiHiddenTestDB(t)
	store := storage.NewStorage(db)
	userID, categoryID, _ := createUIHiddenTestUserAndFeed(t, db, "saved-pages-hidden")
	title := "Hideable saved page"
	_, entryID := createUISavedPagesTestEntry(t, db, store, userID, categoryID, title, "hash-hidden-saved", time.Now())
	h := newUIHiddenTestHandler(t, store)

	render := func() string {
		r := withUIHiddenTestContext(httptest.NewRequest(http.MethodGet, "/saved-pages", nil), userID)
		w := httptest.NewRecorder()
		h.showSavedPagesPage(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("unexpected status code: %d, body: %s", w.Code, w.Body.String())
		}
		return w.Body.String()
	}

	navBefore, err := store.GetNavMetadata(userID)
	if err != nil {
		t.Fatalf("unable to get nav metadata: %v", err)
	}
	if navBefore.CountUnread != 1 {
		t.Fatalf("expected the freshly-saved page to count toward the unread badge, got %d", navBefore.CountUnread)
	}
	if !strings.Contains(render(), title) {
		t.Fatalf("expected the saved page to be listed before hiding it")
	}

	if err := store.ToggleHidden(userID, entryID); err != nil {
		t.Fatalf("unable to hide entry: %v", err)
	}

	navAfter, err := store.GetNavMetadata(userID)
	if err != nil {
		t.Fatalf("unable to get nav metadata: %v", err)
	}
	if navAfter.CountUnread != 0 {
		t.Fatalf("expected hiding the saved page to drop it from the unread count, got %d", navAfter.CountUnread)
	}
	if !strings.Contains(render(), title) {
		t.Fatalf("expected the hidden saved page to remain reachable on /saved-pages by default")
	}
}

// TestSavedPagesPageExcludeHiddenFiltersHiddenEntry proves the
// "excludeHidden" checkbox actually filters, mirroring search's own
// coverage for the same parameter (task 10 part A).
func TestSavedPagesPageExcludeHiddenFiltersHiddenEntry(t *testing.T) {
	db := uiHiddenTestDB(t)
	store := storage.NewStorage(db)
	userID, categoryID, _ := createUIHiddenTestUserAndFeed(t, db, "saved-pages-exclude-hidden")
	title := "Excludable saved page"
	_, entryID := createUISavedPagesTestEntry(t, db, store, userID, categoryID, title, "hash-exclude-hidden", time.Now())
	h := newUIHiddenTestHandler(t, store)

	if err := store.ToggleHidden(userID, entryID); err != nil {
		t.Fatalf("unable to hide entry: %v", err)
	}

	renderWith := func(query string) string {
		r := withUIHiddenTestContext(httptest.NewRequest(http.MethodGet, "/saved-pages"+query, nil), userID)
		w := httptest.NewRecorder()
		h.showSavedPagesPage(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("unexpected status code: %d, body: %s", w.Code, w.Body.String())
		}
		return w.Body.String()
	}

	if !strings.Contains(renderWith(""), title) {
		t.Fatalf("expected the hidden saved page to remain listed without excludeHidden")
	}
	if strings.Contains(renderWith("?excludeHidden=1"), title) {
		t.Fatalf("expected excludeHidden=1 to drop the hidden saved page from the list")
	}
}
