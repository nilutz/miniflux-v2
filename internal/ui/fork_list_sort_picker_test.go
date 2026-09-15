// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui // import "miniflux.app/v2/internal/ui"

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"miniflux.app/v2/internal/model"
	"miniflux.app/v2/internal/storage"
)

// The sort picker extends from search to the five remaining named views
// (unread, starred, history, feed, category). Each view gets: (a) proof the
// picker overrides the saved preference for that list, with a fixture
// where published_at and title order disagree so the assertion actually
// discriminates, and (b) proof the sibling single-entry view's prev/next
// pagination walks the same effective order, not the reader's global
// preference (the exact trap already fixed once for entry_search.go).

// TestUnreadPagePickerOverridesPreference covers the list side for
// unread. published_at is the reverse of title order, so a title-
// ascending render only happens if order=title actually took effect.
func TestUnreadPagePickerOverridesPreference(t *testing.T) {
	db := uiHiddenTestDB(t)
	store := storage.NewStorage(db)
	h := newUIHiddenTestHandler(t, store)

	username := "unread-sort-picker"
	userID, _, feedID := createUIHiddenTestUserAndFeed(t, db, username)

	now := time.Now()
	titleAlpha := "Alpha for " + username
	titleZulu := "Zulu for " + username
	insertUIHiddenTestEntry(t, db, userID, feedID, titleAlpha, "hash-alpha-"+username, now)
	insertUIHiddenTestEntry(t, db, userID, feedID, titleZulu, "hash-zulu-"+username, now.Add(-time.Hour))

	r := withUIHiddenTestContext(httptest.NewRequest(http.MethodGet, "/unread?order=title&direction=asc", nil), userID)
	w := httptest.NewRecorder()
	h.showUnreadPage(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("unexpected status code: %d, body: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	posAlpha := strings.Index(body, titleAlpha)
	posZulu := strings.Index(body, titleZulu)
	if posAlpha < 0 || posZulu < 0 {
		t.Fatalf("expected both entries present; body:\n%s", body)
	}
	if posAlpha >= posZulu {
		t.Fatalf("expected title-ascending order (Alpha before Zulu) under order=title, got positions Alpha=%d Zulu=%d", posAlpha, posZulu)
	}
}

// TestUnreadPageRetryBranchUsesPickerOrder covers showUnreadPage's
// second query - the stale-offset retry taken when offset >= countUnread
// - which has its own WithSorting call site, separate from the primary
// query's. A large, stale offset forces this branch.
func TestUnreadPageRetryBranchUsesPickerOrder(t *testing.T) {
	db := uiHiddenTestDB(t)
	store := storage.NewStorage(db)
	h := newUIHiddenTestHandler(t, store)

	username := "unread-retry-sort-picker"
	userID, _, feedID := createUIHiddenTestUserAndFeed(t, db, username)

	now := time.Now()
	titleAlpha := "Alpha for " + username
	titleZulu := "Zulu for " + username
	insertUIHiddenTestEntry(t, db, userID, feedID, titleAlpha, "hash-alpha-"+username, now)
	insertUIHiddenTestEntry(t, db, userID, feedID, titleZulu, "hash-zulu-"+username, now.Add(-time.Hour))

	// offset=50 is far beyond the 2 entries available, forcing the
	// offset-reset retry branch.
	r := withUIHiddenTestContext(httptest.NewRequest(http.MethodGet, "/unread?offset=50&order=title&direction=asc", nil), userID)
	w := httptest.NewRecorder()
	h.showUnreadPage(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("unexpected status code: %d, body: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	posAlpha := strings.Index(body, titleAlpha)
	posZulu := strings.Index(body, titleZulu)
	if posAlpha < 0 || posZulu < 0 {
		t.Fatalf("expected both entries present via the retry branch; body:\n%s", body)
	}
	if posAlpha >= posZulu {
		t.Fatalf("expected title-ascending order in the retry branch too, got positions Alpha=%d Zulu=%d", posAlpha, posZulu)
	}
}

// TestUnreadEntryPaginationWalksPickerOrder covers the single-entry
// side: prev/next must walk the same order=title the list was sorted
// by, not user.EntryOrder/user.EntryDirection.
func TestUnreadEntryPaginationWalksPickerOrder(t *testing.T) {
	db := uiHiddenTestDB(t)
	store := storage.NewStorage(db)
	h := newUIHiddenTestHandler(t, store)

	username := "unread-entry-pagination-order"
	userID, _, feedID := createUIHiddenTestUserAndFeed(t, db, username)

	now := time.Now()
	titleAlpha := "Alpha for " + username     // newest
	titleBravo := "Bravo for " + username     // middle
	titleCharlie := "Charlie for " + username // oldest

	insertUIHiddenTestEntry(t, db, userID, feedID, titleAlpha, "hash-alpha-"+username, now)
	bravoID := insertUIHiddenTestEntry(t, db, userID, feedID, titleBravo, "hash-bravo-"+username, now.Add(-time.Hour))
	insertUIHiddenTestEntry(t, db, userID, feedID, titleCharlie, "hash-charlie-"+username, now.Add(-2*time.Hour))

	r := withUIHiddenTestContext(httptest.NewRequest(http.MethodGet, "/unread/entry/"+strconv.FormatInt(bravoID, 10)+"?order=title&direction=asc", nil), userID)
	r.SetPathValue("entryID", strconv.FormatInt(bravoID, 10))
	w := httptest.NewRecorder()
	h.showUnreadEntryPage(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("unexpected status code: %d, body: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	prevTitle := extractAnchorAttrBefore(t, body, `rel="prev"`, "title")
	nextTitle := extractAnchorAttrBefore(t, body, `rel="next"`, "title")
	if prevTitle != titleAlpha {
		t.Fatalf("expected %q as the previous entry under order=title, got %q; body:\n%s", titleAlpha, prevTitle, body)
	}
	if nextTitle != titleCharlie {
		t.Fatalf("expected %q as the next entry under order=title, got %q; body:\n%s", titleCharlie, nextTitle, body)
	}
}

// TestStarredPagePickerOverridesPreference is the starred list's
// version of TestUnreadPagePickerOverridesPreference.
func TestStarredPagePickerOverridesPreference(t *testing.T) {
	db := uiHiddenTestDB(t)
	store := storage.NewStorage(db)
	h := newUIHiddenTestHandler(t, store)

	username := "starred-sort-picker"
	userID, _, feedID := createUIHiddenTestUserAndFeed(t, db, username)

	now := time.Now()
	titleAlpha := "Alpha for " + username
	titleZulu := "Zulu for " + username
	alphaID := insertUIHiddenTestEntry(t, db, userID, feedID, titleAlpha, "hash-alpha-"+username, now)
	zuluID := insertUIHiddenTestEntry(t, db, userID, feedID, titleZulu, "hash-zulu-"+username, now.Add(-time.Hour))
	if err := store.SetEntriesStarredState(userID, []int64{alphaID, zuluID}, true); err != nil {
		t.Fatalf("unable to star entries: %v", err)
	}

	r := withUIHiddenTestContext(httptest.NewRequest(http.MethodGet, "/starred?order=title&direction=asc", nil), userID)
	w := httptest.NewRecorder()
	h.showStarredPage(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("unexpected status code: %d, body: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	posAlpha := strings.Index(body, titleAlpha)
	posZulu := strings.Index(body, titleZulu)
	if posAlpha < 0 || posZulu < 0 {
		t.Fatalf("expected both entries present; body:\n%s", body)
	}
	if posAlpha >= posZulu {
		t.Fatalf("expected title-ascending order under order=title, got positions Alpha=%d Zulu=%d", posAlpha, posZulu)
	}
}

// TestStarredEntryPaginationWalksPickerOrder is the starred single-entry
// pagination version of TestUnreadEntryPaginationWalksPickerOrder.
func TestStarredEntryPaginationWalksPickerOrder(t *testing.T) {
	db := uiHiddenTestDB(t)
	store := storage.NewStorage(db)
	h := newUIHiddenTestHandler(t, store)

	username := "starred-entry-pagination-order"
	userID, _, feedID := createUIHiddenTestUserAndFeed(t, db, username)

	now := time.Now()
	titleAlpha := "Alpha for " + username
	titleBravo := "Bravo for " + username
	titleCharlie := "Charlie for " + username

	alphaID := insertUIHiddenTestEntry(t, db, userID, feedID, titleAlpha, "hash-alpha-"+username, now)
	bravoID := insertUIHiddenTestEntry(t, db, userID, feedID, titleBravo, "hash-bravo-"+username, now.Add(-time.Hour))
	charlieID := insertUIHiddenTestEntry(t, db, userID, feedID, titleCharlie, "hash-charlie-"+username, now.Add(-2*time.Hour))
	if err := store.SetEntriesStarredState(userID, []int64{alphaID, bravoID, charlieID}, true); err != nil {
		t.Fatalf("unable to star entries: %v", err)
	}

	r := withUIHiddenTestContext(httptest.NewRequest(http.MethodGet, "/starred/entry/"+strconv.FormatInt(bravoID, 10)+"?order=title&direction=asc", nil), userID)
	r.SetPathValue("entryID", strconv.FormatInt(bravoID, 10))
	w := httptest.NewRecorder()
	h.showStarredEntryPage(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("unexpected status code: %d, body: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	prevTitle := extractAnchorAttrBefore(t, body, `rel="prev"`, "title")
	nextTitle := extractAnchorAttrBefore(t, body, `rel="next"`, "title")
	if prevTitle != titleAlpha {
		t.Fatalf("expected %q as the previous entry under order=title, got %q; body:\n%s", titleAlpha, prevTitle, body)
	}
	if nextTitle != titleCharlie {
		t.Fatalf("expected %q as the next entry under order=title, got %q; body:\n%s", titleCharlie, nextTitle, body)
	}
}

// TestHistoryPagePickerOverridesPreference is the history list's version.
// History's own default is changed_at desc (not the saved preference),
// so this also proves the picker overrides *that* default.
func TestHistoryPagePickerOverridesPreference(t *testing.T) {
	db := uiHiddenTestDB(t)
	store := storage.NewStorage(db)
	h := newUIHiddenTestHandler(t, store)

	username := "history-sort-picker"
	userID, _, feedID := createUIHiddenTestUserAndFeed(t, db, username)

	now := time.Now()
	titleAlpha := "Alpha for " + username
	titleZulu := "Zulu for " + username
	alphaID := insertUIHiddenTestEntry(t, db, userID, feedID, titleAlpha, "hash-alpha-"+username, now)
	zuluID := insertUIHiddenTestEntry(t, db, userID, feedID, titleZulu, "hash-zulu-"+username, now.Add(-time.Hour))
	if err := store.SetEntriesStatus(userID, []int64{alphaID, zuluID}, model.EntryStatusRead); err != nil {
		t.Fatalf("unable to mark entries read: %v", err)
	}

	r := withUIHiddenTestContext(httptest.NewRequest(http.MethodGet, "/history?order=title&direction=asc", nil), userID)
	w := httptest.NewRecorder()
	h.showHistoryPage(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("unexpected status code: %d, body: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	posAlpha := strings.Index(body, titleAlpha)
	posZulu := strings.Index(body, titleZulu)
	if posAlpha < 0 || posZulu < 0 {
		t.Fatalf("expected both entries present; body:\n%s", body)
	}
	if posAlpha >= posZulu {
		t.Fatalf("expected title-ascending order under order=title, got positions Alpha=%d Zulu=%d", posAlpha, posZulu)
	}
}

// TestHistoryEntryPaginationWalksPickerOrder is the history single-entry
// pagination version.
func TestHistoryEntryPaginationWalksPickerOrder(t *testing.T) {
	db := uiHiddenTestDB(t)
	store := storage.NewStorage(db)
	h := newUIHiddenTestHandler(t, store)

	username := "history-entry-pagination-order"
	userID, _, feedID := createUIHiddenTestUserAndFeed(t, db, username)

	now := time.Now()
	titleAlpha := "Alpha for " + username
	titleBravo := "Bravo for " + username
	titleCharlie := "Charlie for " + username

	alphaID := insertUIHiddenTestEntry(t, db, userID, feedID, titleAlpha, "hash-alpha-"+username, now)
	bravoID := insertUIHiddenTestEntry(t, db, userID, feedID, titleBravo, "hash-bravo-"+username, now.Add(-time.Hour))
	charlieID := insertUIHiddenTestEntry(t, db, userID, feedID, titleCharlie, "hash-charlie-"+username, now.Add(-2*time.Hour))
	if err := store.SetEntriesStatus(userID, []int64{alphaID, bravoID, charlieID}, model.EntryStatusRead); err != nil {
		t.Fatalf("unable to mark entries read: %v", err)
	}

	r := withUIHiddenTestContext(httptest.NewRequest(http.MethodGet, "/history/entry/"+strconv.FormatInt(bravoID, 10)+"?order=title&direction=asc", nil), userID)
	r.SetPathValue("entryID", strconv.FormatInt(bravoID, 10))
	w := httptest.NewRecorder()
	h.showReadEntryPage(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("unexpected status code: %d, body: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	prevTitle := extractAnchorAttrBefore(t, body, `rel="prev"`, "title")
	nextTitle := extractAnchorAttrBefore(t, body, `rel="next"`, "title")
	if prevTitle != titleAlpha {
		t.Fatalf("expected %q as the previous entry under order=title, got %q; body:\n%s", titleAlpha, prevTitle, body)
	}
	if nextTitle != titleCharlie {
		t.Fatalf("expected %q as the next entry under order=title, got %q; body:\n%s", titleCharlie, nextTitle, body)
	}
}

// TestFeedEntriesPagePickerOverridesPreference is the feed list's
// version (the default, unread-scoped /feed/{id}/entries view).
func TestFeedEntriesPagePickerOverridesPreference(t *testing.T) {
	db := uiHiddenTestDB(t)
	store := storage.NewStorage(db)
	h := newUIHiddenTestHandler(t, store)

	username := "feed-sort-picker"
	userID, _, feedID := createUIHiddenTestUserAndFeed(t, db, username)

	now := time.Now()
	titleAlpha := "Alpha for " + username
	titleZulu := "Zulu for " + username
	insertUIHiddenTestEntry(t, db, userID, feedID, titleAlpha, "hash-alpha-"+username, now)
	insertUIHiddenTestEntry(t, db, userID, feedID, titleZulu, "hash-zulu-"+username, now.Add(-time.Hour))

	r := withUIHiddenTestContext(httptest.NewRequest(http.MethodGet, "/feed/"+strconv.FormatInt(feedID, 10)+"/entries?order=title&direction=asc", nil), userID)
	r.SetPathValue("feedID", strconv.FormatInt(feedID, 10))
	w := httptest.NewRecorder()
	h.showFeedEntriesPage(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("unexpected status code: %d, body: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	posAlpha := strings.Index(body, titleAlpha)
	posZulu := strings.Index(body, titleZulu)
	if posAlpha < 0 || posZulu < 0 {
		t.Fatalf("expected both entries present; body:\n%s", body)
	}
	if posAlpha >= posZulu {
		t.Fatalf("expected title-ascending order under order=title, got positions Alpha=%d Zulu=%d", posAlpha, posZulu)
	}
}

// TestUnreadFeedEntryPaginationWalksPickerOrder is the feed single-entry
// pagination version. showFeedEntriesPage's permalinks route here
// (showUnreadFeedEntryPage), not to showFeedEntryPage, because the
// template links to the unread-scoped entry route whenever the list is
// showing only unread entries.
func TestUnreadFeedEntryPaginationWalksPickerOrder(t *testing.T) {
	db := uiHiddenTestDB(t)
	store := storage.NewStorage(db)
	h := newUIHiddenTestHandler(t, store)

	username := "feed-entry-pagination-order"
	userID, _, feedID := createUIHiddenTestUserAndFeed(t, db, username)

	now := time.Now()
	titleAlpha := "Alpha for " + username
	titleBravo := "Bravo for " + username
	titleCharlie := "Charlie for " + username

	insertUIHiddenTestEntry(t, db, userID, feedID, titleAlpha, "hash-alpha-"+username, now)
	bravoID := insertUIHiddenTestEntry(t, db, userID, feedID, titleBravo, "hash-bravo-"+username, now.Add(-time.Hour))
	insertUIHiddenTestEntry(t, db, userID, feedID, titleCharlie, "hash-charlie-"+username, now.Add(-2*time.Hour))

	r := withUIHiddenTestContext(httptest.NewRequest(http.MethodGet, "/unread/feed/"+strconv.FormatInt(feedID, 10)+"/entry/"+strconv.FormatInt(bravoID, 10)+"?order=title&direction=asc", nil), userID)
	r.SetPathValue("feedID", strconv.FormatInt(feedID, 10))
	r.SetPathValue("entryID", strconv.FormatInt(bravoID, 10))
	w := httptest.NewRecorder()
	h.showUnreadFeedEntryPage(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("unexpected status code: %d, body: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	prevTitle := extractAnchorAttrBefore(t, body, `rel="prev"`, "title")
	nextTitle := extractAnchorAttrBefore(t, body, `rel="next"`, "title")
	if prevTitle != titleAlpha {
		t.Fatalf("expected %q as the previous entry under order=title, got %q; body:\n%s", titleAlpha, prevTitle, body)
	}
	if nextTitle != titleCharlie {
		t.Fatalf("expected %q as the next entry under order=title, got %q; body:\n%s", titleCharlie, nextTitle, body)
	}
}

// TestCategoryEntriesPagePickerOverridesPreference is the category
// list's version (the default, unread-scoped /category/{id}/entries
// view).
func TestCategoryEntriesPagePickerOverridesPreference(t *testing.T) {
	db := uiHiddenTestDB(t)
	store := storage.NewStorage(db)
	h := newUIHiddenTestHandler(t, store)

	username := "category-sort-picker"
	userID, categoryID, feedID := createUIHiddenTestUserAndFeed(t, db, username)

	now := time.Now()
	titleAlpha := "Alpha for " + username
	titleZulu := "Zulu for " + username
	insertUIHiddenTestEntry(t, db, userID, feedID, titleAlpha, "hash-alpha-"+username, now)
	insertUIHiddenTestEntry(t, db, userID, feedID, titleZulu, "hash-zulu-"+username, now.Add(-time.Hour))

	r := withUIHiddenTestContext(httptest.NewRequest(http.MethodGet, "/category/"+strconv.FormatInt(categoryID, 10)+"/entries?order=title&direction=asc", nil), userID)
	r.SetPathValue("categoryID", strconv.FormatInt(categoryID, 10))
	w := httptest.NewRecorder()
	h.showCategoryEntriesPage(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("unexpected status code: %d, body: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	posAlpha := strings.Index(body, titleAlpha)
	posZulu := strings.Index(body, titleZulu)
	if posAlpha < 0 || posZulu < 0 {
		t.Fatalf("expected both entries present; body:\n%s", body)
	}
	if posAlpha >= posZulu {
		t.Fatalf("expected title-ascending order under order=title, got positions Alpha=%d Zulu=%d", posAlpha, posZulu)
	}
}

// TestUnreadCategoryEntryPaginationWalksPickerOrder is the category
// single-entry pagination version. showCategoryEntriesPage's permalinks
// route here (showUnreadCategoryEntryPage) by default, for the same
// unread-scoped-template reason as the feed variant above.
func TestUnreadCategoryEntryPaginationWalksPickerOrder(t *testing.T) {
	db := uiHiddenTestDB(t)
	store := storage.NewStorage(db)
	h := newUIHiddenTestHandler(t, store)

	username := "category-entry-pagination-order"
	userID, categoryID, feedID := createUIHiddenTestUserAndFeed(t, db, username)

	now := time.Now()
	titleAlpha := "Alpha for " + username
	titleBravo := "Bravo for " + username
	titleCharlie := "Charlie for " + username

	insertUIHiddenTestEntry(t, db, userID, feedID, titleAlpha, "hash-alpha-"+username, now)
	bravoID := insertUIHiddenTestEntry(t, db, userID, feedID, titleBravo, "hash-bravo-"+username, now.Add(-time.Hour))
	insertUIHiddenTestEntry(t, db, userID, feedID, titleCharlie, "hash-charlie-"+username, now.Add(-2*time.Hour))

	r := withUIHiddenTestContext(httptest.NewRequest(http.MethodGet, "/unread/category/"+strconv.FormatInt(categoryID, 10)+"/entry/"+strconv.FormatInt(bravoID, 10)+"?order=title&direction=asc", nil), userID)
	r.SetPathValue("categoryID", strconv.FormatInt(categoryID, 10))
	r.SetPathValue("entryID", strconv.FormatInt(bravoID, 10))
	w := httptest.NewRecorder()
	h.showUnreadCategoryEntryPage(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("unexpected status code: %d, body: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	prevTitle := extractAnchorAttrBefore(t, body, `rel="prev"`, "title")
	nextTitle := extractAnchorAttrBefore(t, body, `rel="next"`, "title")
	if prevTitle != titleAlpha {
		t.Fatalf("expected %q as the previous entry under order=title, got %q; body:\n%s", titleAlpha, prevTitle, body)
	}
	if nextTitle != titleCharlie {
		t.Fatalf("expected %q as the next entry under order=title, got %q; body:\n%s", titleCharlie, nextTitle, body)
	}
}

// TestListPagePermalinkCarriesOrderAndDirection proves that order and
// direction survive opening a result and coming back, for one
// representative extended view - unread - via its result-link
// queryString dict.
func TestListPagePermalinkCarriesOrderAndDirection(t *testing.T) {
	db := uiHiddenTestDB(t)
	store := storage.NewStorage(db)
	h := newUIHiddenTestHandler(t, store)

	username := "unread-permalink-roundtrip"
	userID, _, feedID := createUIHiddenTestUserAndFeed(t, db, username)
	entryID := insertUIHiddenTestEntry(t, db, userID, feedID, "Roundtrip entry for "+username, "hash-"+username, time.Now())

	r := withUIHiddenTestContext(httptest.NewRequest(http.MethodGet, "/unread?order=title&direction=asc", nil), userID)
	w := httptest.NewRecorder()
	h.showUnreadPage(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("unexpected status code: %d, body: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	href := extractAnchorHrefBefore(t, body, "/unread/entry/"+strconv.FormatInt(entryID, 10))
	assertHrefHasParam(t, href, "order", "title")
	assertHrefHasParam(t, href, "direction", "asc")
}
