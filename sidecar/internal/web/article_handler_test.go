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
}

func (f *fakeArticles) EntryArticle(_ context.Context, entryID int64) (*store.ArticleDetail, error) {
	if f.err != nil {
		return nil, f.err
	}
	d, ok := f.byID[entryID]
	if !ok {
		return nil, fmt.Errorf("store: entry #%d does not exist: %w", entryID, sql.ErrNoRows)
	}
	return d, nil
}

func newTestArticleServer(t *testing.T, articles ArticleLookup) http.Handler {
	t.Helper()
	srv, err := New(&fakeBackfill{}, nil, nil, nil, articles, nil, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return srv.Handler()
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
