// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mcpserver // import "miniflux.app/v2/sidecar/internal/mcpserver"

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"miniflux.app/v2/sidecar/internal/sidecarclient"
)

// fakeSidecar is a hermetic httptest stand-in for the real sidecar's HTTP
// API (internal/web/search_handlers.go, article_handler.go), so these
// tests never run against a live sidecar -- see this repo's own
// constraint against starting one. It serves fixed, caller-set JSON
// bodies for /api/search, /api/similar and /api/article, keyed by the
// request's query string, and records every request path+query it saw.
type fakeSidecar struct {
	t *testing.T

	// responses maps "path?query" (as httptest's *http.Request.URL.RequestURI
	// renders it, minus the leading path's host) to a canned status/body.
	// A request whose exact URI is not present here fails the test loudly
	// rather than silently serving a zero value -- an unexpected request
	// shape is exactly the kind of bug these tests exist to catch.
	responses map[string]fakeResponse

	requests []string
}

type fakeResponse struct {
	status int
	body   string
}

func newFakeSidecar(t *testing.T) *fakeSidecar {
	return &fakeSidecar{t: t, responses: map[string]fakeResponse{}}
}

func (f *fakeSidecar) on(pathAndQuery string, status int, body string) {
	f.responses[pathAndQuery] = fakeResponse{status: status, body: body}
}

func (f *fakeSidecar) server() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests = append(f.requests, r.URL.RequestURI())
		resp, ok := f.responses[r.URL.RequestURI()]
		if !ok {
			f.t.Errorf("fakeSidecar: unexpected request %s (configured: %v)", r.URL.RequestURI(), f.responses)
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"error":"unconfigured in test"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.status)
		w.Write([]byte(resp.body))
	}))
}

func newTestToolset(client *sidecarclient.Client) *toolset {
	return &toolset{client: client}
}

// --- search ---

func TestSearchEnrichesResultsWithTitleAndURL(t *testing.T) {
	fs := newFakeSidecar(t)
	fs.on("/api/search?limit=5&mode=hybrid&q=widgets", http.StatusOK, `{
		"mode":"hybrid","query":"widgets",
		"entries":[{"entry_id":10,"score":1.5,"snippet":{"text":"first widget"}}]
	}`)
	fs.on("/api/article?entry_id=10", http.StatusOK, `{
		"entry_id":10,"title":"About Widgets","url":"https://example.com/widgets","published_at":"2026-01-01T00:00:00Z","content":"full"
	}`)
	srv := fs.server()
	defer srv.Close()

	ts := newTestToolset(sidecarclient.New(srv.URL, ""))
	_, out, err := ts.search(context.Background(), nil, SearchInput{Query: "widgets", Mode: "hybrid", Limit: 5})
	if err != nil {
		t.Fatalf("search: %v", err)
	}

	if out.ResultCount != 1 {
		t.Fatalf("ResultCount = %d, want 1", out.ResultCount)
	}
	got := out.Results[0]
	if got.EntryID != 10 {
		t.Errorf("EntryID = %d, want 10", got.EntryID)
	}
	if got.Title != "About Widgets" {
		t.Errorf("Title = %q, want %q", got.Title, "About Widgets")
	}
	if got.URL != "https://example.com/widgets" {
		t.Errorf("URL = %q, want %q", got.URL, "https://example.com/widgets")
	}
	if got.Snippet != "first widget" {
		t.Errorf("Snippet = %q, want %q", got.Snippet, "first widget")
	}
	if out.Message != "" {
		t.Errorf("Message = %q, want empty for a non-empty result set", out.Message)
	}
}

func TestSearchPassesModeAndLimitThroughToSidecar(t *testing.T) {
	fs := newFakeSidecar(t)
	fs.on("/api/search?limit=3&mode=passages&q=foo", http.StatusOK, `{"mode":"passages","query":"foo","passages":[]}`)
	srv := fs.server()
	defer srv.Close()

	ts := newTestToolset(sidecarclient.New(srv.URL, ""))
	if _, _, err := ts.search(context.Background(), nil, SearchInput{Query: "foo", Mode: "passages", Limit: 3}); err != nil {
		t.Fatalf("search: %v", err)
	}
}

// TestSearchZeroResultsIsReportedNotAsAnError is the discriminating case
// for this package's doc-comment item 1: a successful, empty result set
// must come back as (nil error, ResultCount 0, a Message), never as a Go
// error -- an error return would make [mcp.AddTool]'s generated handler
// set CallToolResult.IsError, misreporting "nothing matched" as a tool
// failure.
func TestSearchZeroResultsIsReportedNotAsAnError(t *testing.T) {
	fs := newFakeSidecar(t)
	fs.on("/api/search?mode=hybrid&q=nonexistent", http.StatusOK, `{"mode":"hybrid","query":"nonexistent"}`)
	srv := fs.server()
	defer srv.Close()

	ts := newTestToolset(sidecarclient.New(srv.URL, ""))
	_, out, err := ts.search(context.Background(), nil, SearchInput{Query: "nonexistent", Mode: "hybrid"})
	if err != nil {
		t.Fatalf("search returned an error for zero results: %v", err)
	}
	if out.ResultCount != 0 {
		t.Errorf("ResultCount = %d, want 0", out.ResultCount)
	}
	if out.Message == "" {
		t.Error("Message is empty; a zero-result response must explain itself")
	}
}

// TestSearchUnreachableSidecarReturnsDescriptiveError is this package's
// half of the "sidecar not running" failure mode: search must surface
// *sidecarclient.UnreachableError's own message (naming the base URL)
// via a plain Go error, not swallow it or generic-ify it.
func TestSearchUnreachableSidecarReturnsDescriptiveError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	baseURL := srv.URL
	srv.Close()

	ts := newTestToolset(sidecarclient.New(baseURL, ""))
	_, _, err := ts.search(context.Background(), nil, SearchInput{Query: "x"})
	if err == nil {
		t.Fatal("expected an error calling a sidecar that is not listening")
	}
	if !strings.Contains(err.Error(), "unreachable") || !strings.Contains(err.Error(), baseURL) {
		t.Errorf("error = %q, want it to say \"unreachable\" and name %q", err.Error(), baseURL)
	}
}

func TestSearchAuthErrorMentionsAPIKey(t *testing.T) {
	fs := newFakeSidecar(t)
	fs.on("/api/search?mode=hybrid&q=x", http.StatusUnauthorized, `{"error":"invalid API key"}`)
	srv := fs.server()
	defer srv.Close()

	ts := newTestToolset(sidecarclient.New(srv.URL, "bad-token"))
	_, _, err := ts.search(context.Background(), nil, SearchInput{Query: "x", Mode: "hybrid"})
	if err == nil {
		t.Fatal("expected an error for a 401 response")
	}
	if !strings.Contains(err.Error(), "MINIFLUX_API_KEY") {
		t.Errorf("error = %q, want it to mention MINIFLUX_API_KEY", err.Error())
	}
	if strings.Contains(err.Error(), "unreachable") {
		t.Errorf("error = %q, an auth rejection must not be reported as \"unreachable\"", err.Error())
	}
}

// --- similar ---

func TestSimilarEnrichesResultsAndCarriesNoSnippet(t *testing.T) {
	fs := newFakeSidecar(t)
	fs.on("/api/similar?entry_id=42", http.StatusOK, `{"entry_id":42,"entries":[{"entry_id":7,"score":0.9}]}`)
	fs.on("/api/article?entry_id=7", http.StatusOK, `{"entry_id":7,"title":"Related","url":"https://example.com/related","published_at":"2026-01-01T00:00:00Z","content":"full"}`)
	srv := fs.server()
	defer srv.Close()

	ts := newTestToolset(sidecarclient.New(srv.URL, ""))
	_, out, err := ts.similar(context.Background(), nil, SimilarInput{EntryID: 42})
	if err != nil {
		t.Fatalf("similar: %v", err)
	}
	if out.ResultCount != 1 || out.Results[0].EntryID != 7 {
		t.Fatalf("Results = %+v, want one entry with id 7", out.Results)
	}
	if out.Results[0].Title != "Related" {
		t.Errorf("Title = %q, want %q", out.Results[0].Title, "Related")
	}
}

func TestSimilarZeroResultsIsReportedNotAsAnError(t *testing.T) {
	fs := newFakeSidecar(t)
	fs.on("/api/similar?entry_id=42", http.StatusOK, `{"entry_id":42,"entries":[]}`)
	srv := fs.server()
	defer srv.Close()

	ts := newTestToolset(sidecarclient.New(srv.URL, ""))
	_, out, err := ts.similar(context.Background(), nil, SimilarInput{EntryID: 42})
	if err != nil {
		t.Fatalf("similar returned an error for zero results: %v", err)
	}
	if out.ResultCount != 0 || out.Message == "" {
		t.Errorf("out = %+v, want ResultCount 0 and a non-empty Message", out)
	}
}

// --- fetch_article ---

func TestFetchArticleReturnsFullContent(t *testing.T) {
	fs := newFakeSidecar(t)
	fs.on("/api/article?entry_id=42", http.StatusOK, `{"entry_id":42,"title":"T","url":"https://example.com/a","published_at":"2026-01-02T03:04:05Z","content":"the full text"}`)
	srv := fs.server()
	defer srv.Close()

	ts := newTestToolset(sidecarclient.New(srv.URL, ""))
	_, out, err := ts.fetchArticle(context.Background(), nil, FetchArticleInput{EntryID: 42})
	if err != nil {
		t.Fatalf("fetchArticle: %v", err)
	}
	if out.Title != "T" || out.URL != "https://example.com/a" || out.Content != "the full text" {
		t.Fatalf("out = %+v", out)
	}
	if out.PublishedAt != "2026-01-02T03:04:05Z" {
		t.Errorf("PublishedAt = %q, want RFC3339 %q", out.PublishedAt, "2026-01-02T03:04:05Z")
	}
}

func TestFetchArticleUnknownEntryReturnsDescriptiveError(t *testing.T) {
	fs := newFakeSidecar(t)
	fs.on("/api/article?entry_id=999", http.StatusNotFound, `{"error":"entry #999 does not exist"}`)
	srv := fs.server()
	defer srv.Close()

	ts := newTestToolset(sidecarclient.New(srv.URL, ""))
	_, _, err := ts.fetchArticle(context.Background(), nil, FetchArticleInput{EntryID: 999})
	if err == nil {
		t.Fatal("expected an error for an unknown entry id")
	}
	if !strings.Contains(err.Error(), "999") {
		t.Errorf("error = %q, want it to mention entry id 999", err.Error())
	}
}

func TestFetchArticleUnreachableSidecarReturnsDescriptiveError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	baseURL := srv.URL
	srv.Close()

	ts := newTestToolset(sidecarclient.New(baseURL, ""))
	_, _, err := ts.fetchArticle(context.Background(), nil, FetchArticleInput{EntryID: 1})
	if err == nil {
		t.Fatal("expected an error calling a sidecar that is not listening")
	}
	if !strings.Contains(err.Error(), "unreachable") || !strings.Contains(err.Error(), baseURL) {
		t.Errorf("error = %q, want it to say \"unreachable\" and name %q", err.Error(), baseURL)
	}
}

// --- output shape sanity: field names/json tags an agent depends on ---

func TestSearchOutputMarshalsExpectedFieldNames(t *testing.T) {
	out := SearchOutput{
		Mode: "hybrid", Query: "x", ResultCount: 1,
		Results: []SearchResultItem{{EntryID: 1, Title: "T", URL: "https://x", Score: 1, Snippet: "s"}},
	}
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	for _, field := range []string{`"entry_id"`, `"title"`, `"url"`, `"snippet"`, `"score"`, `"result_count"`} {
		if !strings.Contains(string(b), field) {
			t.Errorf("marshaled output missing %s: %s", field, b)
		}
	}
}
