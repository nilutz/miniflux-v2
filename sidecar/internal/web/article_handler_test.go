// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package web // import "miniflux.app/v2/sidecar/internal/web"

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"miniflux.app/v2/sidecar/internal/store"
)

// fakeArticles is a hermetic stand-in for ArticleLookup: it satisfies the
// interface with an in-memory map, so these tests never need a database.
type fakeArticles struct {
	byID map[int64]*store.ArticleDetail
	err  error // when set, EntryArticle always returns this instead of consulting byID

	// ownerOf, when non-nil, maps entryID -> the user id that "owns" it,
	// mirroring store.Store.EntryArticle's own AND user_id=$2 predicate: a
	// userID that does not match comes back exactly like an unknown entry.
	// Left nil by tests that don't care about ownership, in which case
	// any userID is accepted.
	ownerOf map[int64]int64

	lastUserID int64 // the userID EntryArticle was most recently called with
}

func (f *fakeArticles) EntryArticle(_ context.Context, entryID, userID int64) (*store.ArticleDetail, error) {
	f.lastUserID = userID
	if f.err != nil {
		return nil, f.err
	}
	if f.ownerOf != nil {
		if owner, ok := f.ownerOf[entryID]; !ok || owner != userID {
			return nil, fmt.Errorf("store: entry #%d does not exist: %w", entryID, sql.ErrNoRows)
		}
	}
	d, ok := f.byID[entryID]
	if !ok {
		return nil, fmt.Errorf("store: entry #%d does not exist: %w", entryID, sql.ErrNoRows)
	}
	return d, nil
}

// newTestArticleServer builds a Server wired with articles and a
// permissive fakeKeyValidator (testAuthToken -> testAuthUserID,
// testOtherAuthToken -> testOtherUserID), returning a handler wrapped in
// authedHandler (server_test.go) so every test in this file that is not
// itself about authentication keeps exercising its own, unrelated
// behaviour without setting a header itself; only the auth/scoping-specific
// tests below set their own.
func newTestArticleServer(t *testing.T, articles ArticleLookup) http.Handler {
	t.Helper()
	keys := newFakeKeyValidator(map[string]int64{
		testAuthToken:      testAuthUserID,
		testOtherAuthToken: testOtherUserID,
	})
	srv, err := New(&fakeBackfill{}, nil, nil, nil, articles, nil, nil, keys, nil, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return authedHandler{h: srv.Handler()}
}

func TestArticleReturnsFullContentByEntryID(t *testing.T) {
	published := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	fa := &fakeArticles{byID: map[int64]*store.ArticleDetail{
		42: {
			ID:          42,
			Title:       "A Long Article",
			URL:         "https://example.com/a-long-article",
			PublishedAt: published,
			Content:     "<p>the full content</p>",
		},
	}}
	handler := newTestArticleServer(t, fa)

	req := httptest.NewRequest(http.MethodGet, "/api/article?entry_id=42", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	ct := rec.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}

	var got struct {
		EntryID     int64     `json:"entry_id"`
		Title       string    `json:"title"`
		URL         string    `json:"url"`
		PublishedAt time.Time `json:"published_at"`
		Content     string    `json:"content"`
	}
	decodeJSONBody(t, rec, &got)

	if got.EntryID != 42 {
		t.Errorf("entry_id = %d, want 42", got.EntryID)
	}
	if got.Title != "A Long Article" {
		t.Errorf("title = %q, want %q", got.Title, "A Long Article")
	}
	if got.URL != "https://example.com/a-long-article" {
		t.Errorf("url = %q, want %q", got.URL, "https://example.com/a-long-article")
	}
	if !got.PublishedAt.Equal(published) {
		t.Errorf("published_at = %v, want %v", got.PublishedAt, published)
	}
	if got.Content != "<p>the full content</p>" {
		t.Errorf("content = %q, want %q", got.Content, "<p>the full content</p>")
	}
}

func TestArticleMissingEntryIDIsRejectedWith400(t *testing.T) {
	handler := newTestArticleServer(t, &fakeArticles{byID: map[int64]*store.ArticleDetail{}})

	req := httptest.NewRequest(http.MethodGet, "/api/article", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s, want 400", rec.Code, rec.Body.String())
	}
}

func TestArticleNonNumericEntryIDIsRejectedWith400(t *testing.T) {
	handler := newTestArticleServer(t, &fakeArticles{byID: map[int64]*store.ArticleDetail{}})

	req := httptest.NewRequest(http.MethodGet, "/api/article?entry_id=not-a-number", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s, want 400", rec.Code, rec.Body.String())
	}
}

// TestArticleUnknownEntryIDIsA404NotA500 is the discriminating case for
// handleArticle's errors.Is(err, sql.ErrNoRows) branch: an id genuinely
// absent from the corpus is a CALLER error (404), not a server error
// (500) -- and the 404 body must say which entry was missing, not hide it
// behind a generic "article lookup failed" the way a real 500 does (see
// TestArticleLookupFailureReturns500WithGenericMessage below).
func TestArticleUnknownEntryIDIsA404NotA500(t *testing.T) {
	handler := newTestArticleServer(t, &fakeArticles{byID: map[int64]*store.ArticleDetail{}})

	req := httptest.NewRequest(http.MethodGet, "/api/article?entry_id=999", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body = %s, want 404", rec.Code, rec.Body.String())
	}
	var body map[string]string
	decodeJSONBody(t, rec, &body)
	if !strings.Contains(body["error"], "999") {
		t.Errorf("error body = %q, want it to mention entry id 999", body["error"])
	}
}

// TestArticleLookupFailureReturns500WithGenericMessage is
// TestArticleUnknownEntryIDIsA404NotA500's counterpart: a lookup error
// that is NOT sql.ErrNoRows (a transient store failure, say) must come
// back as 500 with the generic "article lookup failed" text, never the
// real error string -- see search_handlers.go's apiError doc comment for
// why a driver/SQL error must never reach an HTTP client.
func TestArticleLookupFailureReturns500WithGenericMessage(t *testing.T) {
	handler := newTestArticleServer(t, &fakeArticles{err: errors.New("connection reset by peer")})

	req := httptest.NewRequest(http.MethodGet, "/api/article?entry_id=1", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, body = %s, want 500", rec.Code, rec.Body.String())
	}
	var body map[string]string
	decodeJSONBody(t, rec, &body)
	if strings.Contains(body["error"], "connection reset") {
		t.Errorf("error body leaked the internal error: %q", body["error"])
	}
}

// TestArticleScopesLookupToTheAuthenticatedUser proves handleArticle
// passes the AUTHENTICATED caller's user id to ArticleLookup, not some
// other value -- the "results scoped to that key's user" half of the
// required coverage, which a status-code-only assertion would not catch
// (see fakeArticles.lastUserID).
func TestArticleScopesLookupToTheAuthenticatedUser(t *testing.T) {
	fa := &fakeArticles{
		byID:    map[int64]*store.ArticleDetail{42: {ID: 42, Title: "t"}},
		ownerOf: map[int64]int64{42: testAuthUserID},
	}
	handler := newTestArticleServer(t, fa)

	req := httptest.NewRequest(http.MethodGet, "/api/article?entry_id=42", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if fa.lastUserID != testAuthUserID {
		t.Fatalf("EntryArticle called with userID=%d, want the authenticated user %d", fa.lastUserID, testAuthUserID)
	}
}

// TestArticleDoesNotReturnAnotherUsersEntry seeds one entry owned by
// testAuthUserID and proves a DIFFERENT authenticated user
// (testOtherAuthToken -> testOtherUserID) requesting the same entry id
// gets a 404, exactly like an entry that does not exist -- "a token
// belonging to user A must not return user B's entries", specifically
// for GET /api/article.
func TestArticleDoesNotReturnAnotherUsersEntry(t *testing.T) {
	fa := &fakeArticles{
		byID:    map[int64]*store.ArticleDetail{42: {ID: 42, Title: "owner's article"}},
		ownerOf: map[int64]int64{42: testAuthUserID},
	}
	handler := newTestArticleServer(t, fa)

	req := httptest.NewRequest(http.MethodGet, "/api/article?entry_id=42", nil)
	req.Header.Set(authTokenHeader, testOtherAuthToken)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body = %s, want 404 (a different user's entry must look like it doesn't exist)", rec.Code, rec.Body.String())
	}
	if fa.lastUserID != testOtherUserID {
		t.Fatalf("EntryArticle called with userID=%d, want %d", fa.lastUserID, testOtherUserID)
	}
}
