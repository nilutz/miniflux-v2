// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui // import "miniflux.app/v2/internal/ui"

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"miniflux.app/v2/internal/model"
	"miniflux.app/v2/internal/storage"
)

func readEntryHiddenState(t *testing.T, db *sql.DB, entryID int64) (hidden bool, hiddenReason sql.NullString) {
	t.Helper()
	if err := db.QueryRow(`SELECT hidden, hidden_reason FROM entries WHERE id=$1`, entryID).Scan(&hidden, &hiddenReason); err != nil {
		t.Fatalf("unable to read hidden state of entry #%d: %v", entryID, err)
	}
	return hidden, hiddenReason
}

// TestMarkFeedAsHiddenHandlerHidesOnlyThatFeed exercises the
// POST /feed/{feedID}/mark-all-as-hidden route end to end and proves the
// negative scope explicitly: another feed's unread entry, belonging to the
// same user, must stay untouched.
func TestMarkFeedAsHiddenHandlerHidesOnlyThatFeed(t *testing.T) {
	db := uiHiddenTestDB(t)
	store := storage.NewStorage(db)
	h := &handler{store: store}

	username := "mark-feed-hidden-handler"
	userID, categoryID, targetFeedID := createUIHiddenTestUserAndFeed(t, db, username)
	otherFeedID := createUIHiddenTestFeed(t, db, userID, categoryID, username+"-other")

	// markFeedAsHidden mirrors markFeedAsRead's "before checked_at" cutoff
	// (see MarkFeedAsHidden's doc comment), and the feed rows above were just
	// created with checked_at defaulting to now(); published_at must predate
	// that or the entries would look like they arrived after the last fetch.
	published := time.Now().Add(-time.Hour)
	targetEntryID := insertUIHiddenTestEntry(t, db, userID, targetFeedID, "Target feed entry", "hash-target-"+username, published)
	otherEntryID := insertUIHiddenTestEntry(t, db, userID, otherFeedID, "Other feed entry", "hash-other-"+username, published)

	// Establish the positive case first: both entries exist and are unhidden.
	if hidden, _ := readEntryHiddenState(t, db, targetEntryID); hidden {
		t.Fatal("setup: expected the target entry to start unhidden")
	}
	if hidden, _ := readEntryHiddenState(t, db, otherEntryID); hidden {
		t.Fatal("setup: expected the other feed's entry to start unhidden")
	}

	r := withUIHiddenTestContext(httptest.NewRequest(http.MethodPost, "/feed/"+strconv.FormatInt(targetFeedID, 10)+"/mark-all-as-hidden", nil), userID)
	r.SetPathValue("feedID", strconv.FormatInt(targetFeedID, 10))
	w := httptest.NewRecorder()
	h.markFeedAsHidden(w, r)
	if w.Code != http.StatusFound && w.Code != http.StatusOK {
		t.Fatalf("unexpected status code: %d, body: %s", w.Code, w.Body.String())
	}

	if hidden, reason := readEntryHiddenState(t, db, targetEntryID); !hidden || !reason.Valid || reason.String != model.EntryHiddenReasonBulk {
		t.Fatalf("expected the target feed's entry to be hidden with reason 'bulk', got hidden=%v reason=%+v", hidden, reason)
	}
	if hidden, _ := readEntryHiddenState(t, db, otherEntryID); hidden {
		t.Fatal("expected another feed's entry to stay untouched")
	}
}

// TestMarkCategoryAsHiddenHandlerHidesOnlyThatCategory exercises
// POST /category/{categoryID}/mark-all-as-hidden and proves the negative
// scope: another category's entry must stay untouched.
func TestMarkCategoryAsHiddenHandlerHidesOnlyThatCategory(t *testing.T) {
	db := uiHiddenTestDB(t)
	store := storage.NewStorage(db)
	h := &handler{store: store}

	username := "mark-category-hidden-handler"
	userID, targetCategoryID, targetFeedID := createUIHiddenTestUserAndFeed(t, db, username)
	otherCategoryID := createUIHiddenTestCategory(t, db, userID, username+"-other-category")
	otherFeedID := createUIHiddenTestFeed(t, db, userID, otherCategoryID, username+"-other-feed")

	targetEntryID := insertUIHiddenTestEntry(t, db, userID, targetFeedID, "Target category entry", "hash-cat-target-"+username, time.Now())
	otherEntryID := insertUIHiddenTestEntry(t, db, userID, otherFeedID, "Other category entry", "hash-cat-other-"+username, time.Now())

	r := withUIHiddenTestContext(httptest.NewRequest(http.MethodPost, "/category/"+strconv.FormatInt(targetCategoryID, 10)+"/mark-all-as-hidden", nil), userID)
	r.SetPathValue("categoryID", strconv.FormatInt(targetCategoryID, 10))
	w := httptest.NewRecorder()
	h.markCategoryAsHidden(w, r)
	if w.Code != http.StatusFound && w.Code != http.StatusOK {
		t.Fatalf("unexpected status code: %d, body: %s", w.Code, w.Body.String())
	}

	if hidden, reason := readEntryHiddenState(t, db, targetEntryID); !hidden || !reason.Valid || reason.String != model.EntryHiddenReasonBulk {
		t.Fatalf("expected the target category's entry to be hidden with reason 'bulk', got hidden=%v reason=%+v", hidden, reason)
	}
	if hidden, _ := readEntryHiddenState(t, db, otherEntryID); hidden {
		t.Fatal("expected another category's entry to stay untouched")
	}
}

// TestMarkAllAsHiddenHandlerHidesAcrossFeedsButNotAnotherUser exercises
// POST /mark-all-as-hidden and proves the negative scope: another user's
// entry must stay untouched even though it is otherwise identical.
func TestMarkAllAsHiddenHandlerHidesAcrossFeedsButNotAnotherUser(t *testing.T) {
	db := uiHiddenTestDB(t)
	store := storage.NewStorage(db)
	h := &handler{store: store}

	username := "mark-all-hidden-handler"
	userID, _, feedID := createUIHiddenTestUserAndFeed(t, db, username)
	entryID := insertUIHiddenTestEntry(t, db, userID, feedID, "Global entry", "hash-global-"+username, time.Now())

	otherUsername := username + "-other-user"
	otherUserID, _, otherFeedID := createUIHiddenTestUserAndFeed(t, db, otherUsername)
	otherEntryID := insertUIHiddenTestEntry(t, db, otherUserID, otherFeedID, "Other user's entry", "hash-global-other-"+username, time.Now())

	r := withUIHiddenTestContext(httptest.NewRequest(http.MethodPost, "/mark-all-as-hidden", nil), userID)
	w := httptest.NewRecorder()
	h.markAllAsHidden(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("unexpected status code: %d, body: %s", w.Code, w.Body.String())
	}

	if hidden, reason := readEntryHiddenState(t, db, entryID); !hidden || !reason.Valid || reason.String != model.EntryHiddenReasonBulk {
		t.Fatalf("expected the entry to be hidden with reason 'bulk', got hidden=%v reason=%+v", hidden, reason)
	}
	if hidden, _ := readEntryHiddenState(t, db, otherEntryID); hidden {
		t.Fatal("expected another user's entry to stay untouched")
	}
}

// TestHideEntriesHandlerHidesOnlyTheRequestedEntries is the backend for
// "Mark page as hidden". An entry that exists but was not included in the
// request (as if it were on a different page of the unread list) must
// stay untouched, and the entries that were requested must carry
// hidden_reason = 'bulk'.
func TestHideEntriesHandlerHidesOnlyTheRequestedEntries(t *testing.T) {
	db := uiHiddenTestDB(t)
	store := storage.NewStorage(db)
	h := &handler{store: store}

	username := "hide-entries-handler"
	userID, _, feedID := createUIHiddenTestUserAndFeed(t, db, username)
	pageEntryID := insertUIHiddenTestEntry(t, db, userID, feedID, "On this page", "hash-page-"+username, time.Now())
	nextPageEntryID := insertUIHiddenTestEntry(t, db, userID, feedID, "On the next page", "hash-next-page-"+username, time.Now())

	body, err := json.Marshal(map[string]any{"entry_ids": []int64{pageEntryID}})
	if err != nil {
		t.Fatalf("unable to marshal request body: %v", err)
	}

	r := withUIHiddenTestContext(httptest.NewRequest(http.MethodPost, "/entries/hide", bytes.NewReader(body)), userID)
	w := httptest.NewRecorder()
	h.hideEntries(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("unexpected status code: %d, body: %s", w.Code, w.Body.String())
	}

	if hidden, reason := readEntryHiddenState(t, db, pageEntryID); !hidden || !reason.Valid || reason.String != model.EntryHiddenReasonBulk {
		t.Fatalf("expected the requested entry to be hidden with reason 'bulk', got hidden=%v reason=%+v", hidden, reason)
	}
	if hidden, _ := readEntryHiddenState(t, db, nextPageEntryID); hidden {
		t.Fatal("expected an entry not included in the request (as if on a different page) to stay untouched")
	}
}

// TestHideEntriesHandlerRejectsAnEmptyList guards the validation path: an
// empty entry_ids list must be rejected rather than silently doing nothing
// on the server while the client believes it succeeded.
func TestHideEntriesHandlerRejectsAnEmptyList(t *testing.T) {
	db := uiHiddenTestDB(t)
	store := storage.NewStorage(db)
	h := &handler{store: store}

	username := "hide-entries-empty"
	userID, _, _ := createUIHiddenTestUserAndFeed(t, db, username)

	body, err := json.Marshal(map[string]any{"entry_ids": []int64{}})
	if err != nil {
		t.Fatalf("unable to marshal request body: %v", err)
	}

	r := withUIHiddenTestContext(httptest.NewRequest(http.MethodPost, "/entries/hide", bytes.NewReader(body)), userID)
	w := httptest.NewRecorder()
	h.hideEntries(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected a 400 for an empty entry list, got %d, body: %s", w.Code, w.Body.String())
	}
}

// createUIHiddenTestCategory inserts an additional category for userID.
func createUIHiddenTestCategory(t *testing.T, db *sql.DB, userID int64, title string) int64 {
	t.Helper()

	var categoryID int64
	if err := db.QueryRow(
		`INSERT INTO categories (user_id, title) VALUES ($1, $2) RETURNING id`,
		userID, title,
	).Scan(&categoryID); err != nil {
		t.Fatalf("unable to create category: %v", err)
	}
	return categoryID
}

// createUIHiddenTestFeed inserts an additional feed for userID/categoryID.
func createUIHiddenTestFeed(t *testing.T, db *sql.DB, userID, categoryID int64, slug string) int64 {
	t.Helper()

	var feedID int64
	if err := db.QueryRow(
		`INSERT INTO feeds (feed_url, site_url, title, category_id, user_id)
		 VALUES ($1, $1, 'Test feed', $2, $3) RETURNING id`,
		"https://example.org/"+slug+".xml", categoryID, userID,
	).Scan(&feedID); err != nil {
		t.Fatalf("unable to create feed: %v", err)
	}
	return feedID
}
