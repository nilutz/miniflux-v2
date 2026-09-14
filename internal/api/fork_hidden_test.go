// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package api // import "miniflux.app/v2/internal/api"

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"

	_ "github.com/lib/pq"

	"miniflux.app/v2/internal/config"
	"miniflux.app/v2/internal/http/request"
	"miniflux.app/v2/internal/storage"
)

// apiHiddenTestDB opens the integration database, or skips when there is
// none, so that "make test" stays hermetic.
func apiHiddenTestDB(t *testing.T) *sql.DB {
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

// configureApiHiddenTestOptions installs a parsed configuration so that
// findEntries (which rewrites content through config.Opts.MediaProxyMode)
// does not panic on a nil config.
func configureApiHiddenTestOptions(t *testing.T) {
	t.Helper()

	parsedOptions, err := config.NewConfigParser().ParseEnvironmentVariables()
	if err != nil {
		t.Fatalf("unable to configure test options: %v", err)
	}

	previousOptions := config.Opts
	config.Opts = parsedOptions
	t.Cleanup(func() { config.Opts = previousOptions })
}

// createHiddenTestEntry inserts a user, category, feed and entry, and
// removes them when the test finishes.
func createHiddenTestEntry(t *testing.T, db *sql.DB, username string) (userID, entryID int64) {
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

func withUserContext(r *http.Request, userID int64) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), request.UserIDContextKey, userID))
}

// TestFindEntriesExcludesHiddenOnlyForTheUnreadStatusFilter is the API-level
// version of spec §13.3: GET /v1/entries?status=unread is "the unread list"
// and must exclude a hidden entry, while the same endpoint with no status
// filter (as used to browse a feed or a category) must still return it.
func TestFindEntriesExcludesHiddenOnlyForTheUnreadStatusFilter(t *testing.T) {
	db := apiHiddenTestDB(t)
	configureApiHiddenTestOptions(t)
	store := storage.NewStorage(db)
	userID, entryID := createHiddenTestEntry(t, db, "hidden-findentries")

	if err := store.SetEntriesHiddenState(userID, []int64{entryID}, true); err != nil {
		t.Fatalf("unable to hide entry: %v", err)
	}

	h := &handler{store: store}

	fetch := func(target string) entriesResponse {
		r := withUserContext(httptest.NewRequest(http.MethodGet, target, nil), userID)
		w := httptest.NewRecorder()
		h.getEntriesHandler(w, r)

		if w.Code != http.StatusOK {
			t.Fatalf("unexpected status code for %s: %d, body: %s", target, w.Code, w.Body.String())
		}

		var response entriesResponse
		if err := json.NewDecoder(w.Body).Decode(&response); err != nil {
			t.Fatalf("unable to decode response for %s: %v", target, err)
		}
		return response
	}

	unreadOnly := fetch("/v1/entries?status=unread")
	if unreadOnly.Total != 0 || len(unreadOnly.Entries) != 0 {
		t.Fatalf("expected the hidden entry to be excluded from status=unread, got total=%d entries=%d", unreadOnly.Total, len(unreadOnly.Entries))
	}

	unfiltered := fetch("/v1/entries")
	if unfiltered.Total != 1 || len(unfiltered.Entries) != 1 {
		t.Fatalf("expected the hidden entry to still be reachable with no status filter, got total=%d entries=%d", unfiltered.Total, len(unfiltered.Entries))
	}
	if unfiltered.Entries[0].ID != entryID || !unfiltered.Entries[0].Hidden {
		t.Fatalf("expected to find the hidden entry #%d, got %+v", entryID, unfiltered.Entries[0])
	}
}

// TestToggleHiddenHandlerFlipsTheFlag exercises the API path that sets the
// flag, mirroring toggleStarredHandler.
func TestToggleHiddenHandlerFlipsTheFlag(t *testing.T) {
	db := apiHiddenTestDB(t)
	store := storage.NewStorage(db)
	userID, entryID := createHiddenTestEntry(t, db, "hidden-toggle-api")

	h := &handler{store: store}

	toggle := func() {
		r := withUserContext(httptest.NewRequest(http.MethodPut, "/v1/entries/"+strconv.FormatInt(entryID, 10)+"/hide", nil), userID)
		r.SetPathValue("entryID", strconv.FormatInt(entryID, 10))
		w := httptest.NewRecorder()
		h.toggleHiddenHandler(w, r)
		if w.Code != http.StatusNoContent {
			t.Fatalf("unexpected status code: %d, body: %s", w.Code, w.Body.String())
		}
	}

	var hidden bool
	if err := db.QueryRow(`SELECT hidden FROM entries WHERE id=$1`, entryID).Scan(&hidden); err != nil {
		t.Fatalf("unable to read back entry: %v", err)
	}
	if hidden {
		t.Fatal("expected a freshly created entry to not be hidden")
	}

	toggle()
	if err := db.QueryRow(`SELECT hidden FROM entries WHERE id=$1`, entryID).Scan(&hidden); err != nil {
		t.Fatalf("unable to read back entry: %v", err)
	}
	if !hidden {
		t.Fatal("expected the entry to be hidden after PUT .../hide")
	}

	toggle()
	if err := db.QueryRow(`SELECT hidden FROM entries WHERE id=$1`, entryID).Scan(&hidden); err != nil {
		t.Fatalf("unable to read back entry: %v", err)
	}
	if hidden {
		t.Fatal("expected the entry to no longer be hidden after toggling again")
	}
}
