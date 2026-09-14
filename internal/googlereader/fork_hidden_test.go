// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package googlereader // import "miniflux.app/v2/internal/googlereader"

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"

	_ "github.com/lib/pq"

	"miniflux.app/v2/internal/http/request"
	"miniflux.app/v2/internal/storage"
)

func greaderTestDB(t *testing.T) *sql.DB {
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

func createGreaderTestEntry(t *testing.T, db *sql.DB, username string) (userID, entryID int64) {
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

// TestReadingListUnreadStreamByteIdenticalRegardlessOfHidden pins the same
// property as the Fever test above, on the Google Reader side: the unread
// stream (reading-list minus read) has no concept of hidden and its response
// must not change because of it. Unlike Fever's base response, this payload
// carries no timestamp, so a literal byte comparison is meaningful.
func TestReadingListUnreadStreamByteIdenticalRegardlessOfHidden(t *testing.T) {
	db := greaderTestDB(t)
	store := storage.NewStorage(db)
	userID, entryID := createGreaderTestEntry(t, db, "greader-hidden")

	h := &greaderHandler{store: store}

	do := func() []byte {
		target := fmt.Sprintf(
			"/reader/api/0/stream/items/ids?output=json&s=user/%d/state/com.google/reading-list&xt=user/%d/state/com.google/read",
			userID, userID,
		)
		r := httptest.NewRequest(http.MethodGet, target, nil)
		r = r.WithContext(context.WithValue(r.Context(), request.UserIDContextKey, userID))
		w := httptest.NewRecorder()
		h.streamItemIDsHandler(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("unexpected status code: %d, body: %s", w.Code, w.Body.String())
		}
		return w.Body.Bytes()
	}

	before := do()
	if !bytes.Contains(before, []byte(strconv.FormatInt(entryID, 10))) {
		t.Fatalf("expected entry #%d in the unread stream before hiding, got %s", entryID, before)
	}

	if err := store.SetEntriesHiddenState(userID, []int64{entryID}, true); err != nil {
		t.Fatalf("unable to hide entry: %v", err)
	}

	after := do()

	if !bytes.Equal(before, after) {
		t.Fatalf("googlereader unread stream response changed when the entry was hidden:\nbefore: %s\nafter:  %s", before, after)
	}
}
