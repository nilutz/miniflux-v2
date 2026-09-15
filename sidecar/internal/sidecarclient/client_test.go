// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sidecarclient // import "miniflux.app/v2/sidecar/internal/sidecarclient"

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSearchSendsQueryModeLimitAndDecodesEntries(t *testing.T) {
	var gotPath, gotQuery, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		gotAuth = r.Header.Get("X-Auth-Token")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"mode":"hybrid","query":"widgets","entries":[
			{"entry_id":10,"score":1.5,"snippet":{"text":"first","highlights":[{"start":0,"end":5}]}}
		]}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "test-token")
	result, err := c.Search(context.Background(), SearchParams{Query: "widgets", Mode: "hybrid", Limit: 5})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	if gotPath != "/api/search" {
		t.Errorf("path = %q, want /api/search", gotPath)
	}
	if !strings.Contains(gotQuery, "q=widgets") || !strings.Contains(gotQuery, "mode=hybrid") || !strings.Contains(gotQuery, "limit=5") {
		t.Errorf("query = %q, want q=widgets, mode=hybrid and limit=5", gotQuery)
	}
	if gotAuth != "test-token" {
		t.Errorf("X-Auth-Token = %q, want %q", gotAuth, "test-token")
	}

	if len(result.Entries) != 1 || result.Entries[0].EntryID != 10 {
		t.Fatalf("Entries = %+v, want one entry with id 10", result.Entries)
	}
	if result.Entries[0].Snippet.Text != "first" {
		t.Errorf("snippet text = %q, want %q", result.Entries[0].Snippet.Text, "first")
	}
}

func TestSearchOmitsAuthHeaderWhenNoAPIKeyConfigured(t *testing.T) {
	var gotAuth string
	sawHeader := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, sawHeader = r.Header.Get("X-Auth-Token"), r.Header.Get("X-Auth-Token") != ""
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"mode":"hybrid","query":"x"}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "")
	if _, err := c.Search(context.Background(), SearchParams{Query: "x"}); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if sawHeader {
		t.Errorf("X-Auth-Token header was sent (%q) with no API key configured", gotAuth)
	}
}

func TestSearchZeroResultsIsNotAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"mode":"hybrid","query":"nonexistent-term"}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "")
	result, err := c.Search(context.Background(), SearchParams{Query: "nonexistent-term"})
	if err != nil {
		t.Fatalf("Search returned an error for a zero-result response: %v", err)
	}
	if len(result.Entries) != 0 || len(result.Passages) != 0 {
		t.Errorf("expected zero results, got Entries=%v Passages=%v", result.Entries, result.Passages)
	}
}

func TestSimilarSendsEntryIDAndLimit(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"entry_id":42,"entries":[{"entry_id":7,"score":0.9}]}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "")
	result, err := c.Similar(context.Background(), 42, 3)
	if err != nil {
		t.Fatalf("Similar: %v", err)
	}
	if !strings.Contains(gotQuery, "entry_id=42") || !strings.Contains(gotQuery, "limit=3") {
		t.Errorf("query = %q, want entry_id=42 and limit=3", gotQuery)
	}
	if len(result.Entries) != 1 || result.Entries[0].EntryID != 7 {
		t.Fatalf("Entries = %+v, want one entry with id 7", result.Entries)
	}
}

func TestArticleDecodesFullContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"entry_id":42,"title":"T","url":"https://example.com/a","published_at":"2026-01-02T03:04:05Z","content":"full text"}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "")
	a, err := c.Article(context.Background(), 42)
	if err != nil {
		t.Fatalf("Article: %v", err)
	}
	if a.Title != "T" || a.URL != "https://example.com/a" || a.Content != "full text" {
		t.Fatalf("Article = %+v, want title T, url https://example.com/a, content \"full text\"", a)
	}
}

// TestUnreachableSidecarReturnsUnreachableError is this package's
// discriminating case for the "sidecar is not running" failure mode: a
// client pointed at a port nothing listens on must return an
// *UnreachableError naming the base URL, not a generic error a caller
// has to string-match to recognise.
func TestUnreachableSidecarReturnsUnreachableError(t *testing.T) {
	// A server that is immediately closed frees its port but leaves
	// nothing listening on it -- a reliable way to force "connection
	// refused" without depending on any specific unused port number.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	baseURL := srv.URL
	srv.Close()

	c := New(baseURL, "")
	_, err := c.Search(context.Background(), SearchParams{Query: "x"})
	if err == nil {
		t.Fatal("expected an error calling a sidecar that is not listening")
	}
	var unreachable *UnreachableError
	if !errors.As(err, &unreachable) {
		t.Fatalf("error = %v (%T), want *UnreachableError", err, err)
	}
	if unreachable.BaseURL != baseURL {
		t.Errorf("UnreachableError.BaseURL = %q, want %q", unreachable.BaseURL, baseURL)
	}
	if !strings.Contains(err.Error(), baseURL) {
		t.Errorf("error message %q does not mention the base URL %q", err.Error(), baseURL)
	}
}

// TestUnauthorizedResponseReturnsAuthErrorNotUnreachable is the
// discriminating case for "the sidecar rejected the API key": a 401
// response is a live sidecar actively refusing the request, and must
// come back as *AuthError, never *UnreachableError -- an agent told
// "unreachable" when the real problem is a bad token would send the user
// to check the wrong thing (is the container running?) instead of the
// right one (is MINIFLUX_API_KEY set correctly?).
func TestUnauthorizedResponseReturnsAuthErrorNotUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"invalid API key"}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "wrong-token")
	_, err := c.Search(context.Background(), SearchParams{Query: "x"})
	if err == nil {
		t.Fatal("expected an error for a 401 response")
	}

	var unreachable *UnreachableError
	if errors.As(err, &unreachable) {
		t.Fatalf("a 401 response was reported as unreachable (%v); it must be an AuthError", err)
	}

	var authErr *AuthError
	if !errors.As(err, &authErr) {
		t.Fatalf("error = %v (%T), want *AuthError", err, err)
	}
	if authErr.Status != http.StatusUnauthorized {
		t.Errorf("AuthError.Status = %d, want 401", authErr.Status)
	}
	if !strings.Contains(err.Error(), "MINIFLUX_API_KEY") {
		t.Errorf("AuthError message %q does not say what to set", err.Error())
	}
}

func TestForbiddenResponseReturnsAuthError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"error":"not permitted"}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "some-token")
	_, err := c.Search(context.Background(), SearchParams{Query: "x"})

	var authErr *AuthError
	if !errors.As(err, &authErr) {
		t.Fatalf("error = %v (%T), want *AuthError", err, err)
	}
	if authErr.Status != http.StatusForbidden {
		t.Errorf("AuthError.Status = %d, want 403", authErr.Status)
	}
}

func TestBadRequestResponseReturnsAPIErrorWithSidecarMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"unknown mode \"bogus\": want one of keyword, semantic, hybrid, passages"}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "")
	_, err := c.Search(context.Background(), SearchParams{Query: "x", Mode: "bogus"})

	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error = %v (%T), want *APIError", err, err)
	}
	if apiErr.Status != http.StatusBadRequest {
		t.Errorf("APIError.Status = %d, want 400", apiErr.Status)
	}
	if !strings.Contains(apiErr.Message, "unknown mode") {
		t.Errorf("APIError.Message = %q, want it to carry the sidecar's own message", apiErr.Message)
	}
}

func TestArticleNotFoundReturnsAPIErrorNotAuthOrUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":"entry #999 does not exist"}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "")
	_, err := c.Article(context.Background(), 999)

	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error = %v (%T), want *APIError", err, err)
	}
	if apiErr.Status != http.StatusNotFound {
		t.Errorf("APIError.Status = %d, want 404", apiErr.Status)
	}
}
