// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package api // import "miniflux.app/v2/internal/api"

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"miniflux.app/v2/internal/model"
	"miniflux.app/v2/internal/storage"
)

// createHiddenTestEntries inserts a user, category, feed and n entries, and
// removes them when the test finishes.
func createHiddenTestEntries(t *testing.T, db *sql.DB, username string, n int) (userID int64, entryIDs []int64) {
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

	for i := range n {
		var entryID int64
		if err := db.QueryRow(
			`INSERT INTO entries (title, hash, url, published_at, changed_at, user_id, feed_id, content, author)
			 VALUES ('Test entry', $1, 'https://example.org/post', now(), now(), $2, $3, '<p>excerpt</p>', '')
			 RETURNING id`,
			username+"-hash-"+string(rune('a'+i)), userID, feedID,
		).Scan(&entryID); err != nil {
			t.Fatalf("unable to create entry: %v", err)
		}
		entryIDs = append(entryIDs, entryID)
	}

	return userID, entryIDs
}

func readEntriesHiddenReason(t *testing.T, db *sql.DB, entryID int64) (hidden bool, hiddenReason sql.NullString) {
	t.Helper()
	if err := db.QueryRow(`SELECT hidden, hidden_reason FROM entries WHERE id=$1`, entryID).Scan(&hidden, &hiddenReason); err != nil {
		t.Fatalf("unable to read hidden state of entry #%d: %v", entryID, err)
	}
	return hidden, hiddenReason
}

func putEntriesHidden(t *testing.T, h *handler, userID int64, entryIDs []int64) {
	t.Helper()

	body, err := json.Marshal(map[string]any{"entry_ids": entryIDs, "hidden": true})
	if err != nil {
		t.Fatalf("unable to marshal request body: %v", err)
	}

	r := withUserContext(httptest.NewRequest(http.MethodPut, "/v1/entries", bytes.NewReader(body)), userID)
	w := httptest.NewRecorder()
	h.setEntryStatusAndStarredHandler(w, r)
	if w.Code != http.StatusNoContent {
		t.Fatalf("unexpected status code: %d, body: %s", w.Code, w.Body.String())
	}
}

// TestSetEntryStatusAndStarredHandlerRecordsHandReasonForASingleEntry pins
// the spec §13.3 literal reading: "hidden by hand, one entry at a time."
// PUT /v1/entries with a single-element entry_ids list must be
// indistinguishable, reason-wise, from the dedicated single-entry hide
// control — a client reimplementing that button through the bulk endpoint
// must still record NULL, not 'bulk'.
func TestSetEntryStatusAndStarredHandlerRecordsHandReasonForASingleEntry(t *testing.T) {
	db := apiHiddenTestDB(t)
	store := storage.NewStorage(db)
	userID, entryIDs := createHiddenTestEntries(t, db, "hidden-bulk-single", 1)
	h := &handler{store: store}

	// Establish the positive case first: the entry exists and starts
	// unhidden, so "hidden with reason NULL" below is not a trivial pass.
	if hidden, _ := readEntriesHiddenReason(t, db, entryIDs[0]); hidden {
		t.Fatal("setup: expected a freshly created entry to not be hidden")
	}

	putEntriesHidden(t, h, userID, entryIDs)

	if hidden, reason := readEntriesHiddenReason(t, db, entryIDs[0]); !hidden || reason.Valid {
		t.Fatalf("expected a single-id request to hide with reason NULL (hand), got hidden=%v reason=%+v", hidden, reason)
	}
}

// TestSetEntryStatusAndStarredHandlerRecordsBulkReasonForSeveralEntries is
// the other half: a multi-element entry_ids list is structurally identical
// to the mark-all-as-hidden routes (a caller-supplied scope, not a single
// judged entry), so it must record 'bulk', not NULL — otherwise the server
// fabricates hand-hidden negatives for goal 3 out of a scripted sweep.
func TestSetEntryStatusAndStarredHandlerRecordsBulkReasonForSeveralEntries(t *testing.T) {
	db := apiHiddenTestDB(t)
	store := storage.NewStorage(db)
	userID, entryIDs := createHiddenTestEntries(t, db, "hidden-bulk-several", 3)
	h := &handler{store: store}

	for _, entryID := range entryIDs {
		if hidden, _ := readEntriesHiddenReason(t, db, entryID); hidden {
			t.Fatal("setup: expected freshly created entries to not be hidden")
		}
	}

	putEntriesHidden(t, h, userID, entryIDs)

	for _, entryID := range entryIDs {
		if hidden, reason := readEntriesHiddenReason(t, db, entryID); !hidden || !reason.Valid || reason.String != model.EntryHiddenReasonBulk {
			t.Fatalf("expected entry #%d to be hidden with reason %q, got hidden=%v reason=%+v", entryID, model.EntryHiddenReasonBulk, hidden, reason)
		}
	}
}
