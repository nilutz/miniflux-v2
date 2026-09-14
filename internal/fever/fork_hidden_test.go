// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package fever // import "miniflux.app/v2/internal/fever"

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"

	_ "github.com/lib/pq"

	"miniflux.app/v2/internal/http/request"
	"miniflux.app/v2/internal/storage"
)

func feverTestDB(t *testing.T) *sql.DB {
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

func createFeverTestEntry(t *testing.T, db *sql.DB, username string) (userID, entryID int64) {
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

	if err := db.QueryRow(
		`INSERT INTO entries (title, hash, url, published_at, changed_at, user_id, feed_id, content, author)
		 VALUES ('Test entry', $1, 'https://example.org/post', now(), now(), $2, $3, '<p>excerpt</p>', '')
		 RETURNING id`,
		"hash-"+username, userID, feedID,
	).Scan(&entryID); err != nil {
		t.Fatalf("unable to create entry: %v", err)
	}

	return userID, entryID
}

// stripVolatileFeverFields removes response fields that legitimately change
// from one call to the next regardless of the hidden flag (the Fever base
// response stamps last_refreshed_on_time with time.Now() on every call), so
// the remaining bytes reflect only what the hidden flag could have changed.
func stripVolatileFeverFields(t *testing.T, body []byte) []byte {
	t.Helper()

	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("unable to parse fever response %s: %v", body, err)
	}
	delete(parsed, "last_refreshed_on_time")

	out, err := json.Marshal(parsed)
	if err != nil {
		t.Fatalf("unable to re-marshal fever response: %v", err)
	}
	return out
}

// TestUnreadItemIDsByteIdenticalRegardlessOfHidden pins the property that
// justifies making hidden a separate boolean instead of a third status:
// Fever has no concept of hidden, and its unread_item_ids response must be
// unaffected by it.
func TestUnreadItemIDsByteIdenticalRegardlessOfHidden(t *testing.T) {
	db := feverTestDB(t)
	store := storage.NewStorage(db)
	userID, entryID := createFeverTestEntry(t, db, "fever-hidden")

	h := &feverHandler{store: store}

	do := func() []byte {
		r := httptest.NewRequest(http.MethodGet, "/?unread_item_ids", nil)
		r = r.WithContext(context.WithValue(r.Context(), request.UserIDContextKey, userID))
		w := httptest.NewRecorder()
		h.serve(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("unexpected status code: %d, body: %s", w.Code, w.Body.String())
		}
		return w.Body.Bytes()
	}

	before := do()
	if !strings.Contains(string(before), strconv.FormatInt(entryID, 10)) {
		t.Fatalf("expected entry #%d in the unread_item_ids response before hiding, got %s", entryID, before)
	}

	if err := store.SetEntriesHiddenState(userID, []int64{entryID}, true); err != nil {
		t.Fatalf("unable to hide entry: %v", err)
	}

	after := do()

	strippedBefore := stripVolatileFeverFields(t, before)
	strippedAfter := stripVolatileFeverFields(t, after)

	if string(strippedBefore) != string(strippedAfter) {
		t.Fatalf("fever unread_item_ids response changed when the entry was hidden:\nbefore: %s\nafter:  %s", strippedBefore, strippedAfter)
	}
}
