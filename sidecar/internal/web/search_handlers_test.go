// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package web // import "miniflux.app/v2/sidecar/internal/web"

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"miniflux.app/v2/sidecar/internal/search"
	"miniflux.app/v2/sidecar/internal/store"
)

// fakeSearcher is a hermetic stand-in for *search.Searcher: it satisfies
// SearchService with no store, no embedder and no database connection, so
// these tests never need a database at all. It records the last
// request/call it received so a test can assert query parameters were
// parsed and threaded through to the Searcher correctly (the "filters
// round-trip" requirement), and that an invalid request never reaches it
// at all (the "unknown mode" requirement).
type fakeSearcher struct {
	response search.Response
	err      error

	lastRequest search.Request

	similarHits   []search.EntryHit
	similarErr    error
	lastEntryID   int64
	lastLimit     int
	lastFilters   search.Filters
	similarCalled bool
}

func (f *fakeSearcher) Search(ctx context.Context, req search.Request) (search.Response, error) {
	f.lastRequest = req
	if f.err != nil {
		return search.Response{}, f.err
	}
	return f.response, nil
}

func (f *fakeSearcher) Similar(ctx context.Context, entryID int64, limit int, filters search.Filters) ([]search.EntryHit, error) {
	f.similarCalled = true
	f.lastEntryID = entryID
	f.lastLimit = limit
	f.lastFilters = filters
	if f.similarErr != nil {
		return nil, f.similarErr
	}
	return f.similarHits, nil
}

// fakeEntries is a hermetic stand-in for EntryLookup, backed by in-memory
// maps rather than a real store.Store / database connection.
type fakeEntries struct {
	entries map[int64]*store.Entry
	states  map[int64]*store.IndexState
}

func (f *fakeEntries) EntryForIndexing(_ context.Context, entryID int64) (*store.Entry, error) {
	e, ok := f.entries[entryID]
	if !ok {
		return nil, fmt.Errorf("web_test: no such entry #%d", entryID)
	}
	return e, nil
}

func (f *fakeEntries) EntryIndexState(_ context.Context, entryID int64) (*store.IndexState, error) {
	return f.states[entryID], nil
}

func newTestSearchServer(t *testing.T, searcher SearchService, entries EntryLookup) http.Handler {
	t.Helper()
	srv, err := New(&fakeBackfill{}, nil, searcher, entries, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return srv.Handler()
}

func decodeJSONBody(t *testing.T, rec *httptest.ResponseRecorder, out any) {
	t.Helper()
	if err := json.NewDecoder(rec.Body).Decode(out); err != nil {
		t.Fatalf("decode response body: %v, body = %s", err, rec.Body.String())
	}
}

func TestSearchReturnsRankedJSON(t *testing.T) {
	fs := &fakeSearcher{response: search.Response{
		Mode: search.ModeHybrid,
		Entries: []search.EntryHit{
			{EntryID: 10, Score: 1.5, Best: search.PassageHit{Text: "first"}},
			{EntryID: 20, Score: 1.2, Best: search.PassageHit{Text: "second"}},
		},
	}}
	handler := newTestSearchServer(t, fs, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/search?q=widgets", nil)
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
		Mode    string `json:"mode"`
		Entries []struct {
			EntryID int64   `json:"entry_id"`
			Score   float64 `json:"score"`
		} `json:"entries"`
	}
	decodeJSONBody(t, rec, &got)

	if got.Mode != "hybrid" {
		t.Errorf("mode = %q, want hybrid", got.Mode)
	}
	if len(got.Entries) != 2 || got.Entries[0].EntryID != 10 || got.Entries[1].EntryID != 20 {
		t.Fatalf("entries = %+v, want entry_id 10 then 20, in order", got.Entries)
	}
	if got.Entries[0].Score != 1.5 || got.Entries[1].Score != 1.2 {
		t.Errorf("scores = %+v, want [1.5, 1.2]", got.Entries)
	}
	if fs.lastRequest.Query != "widgets" {
		t.Errorf("searcher received query %q, want %q", fs.lastRequest.Query, "widgets")
	}
}

func TestSearchUnknownModeRejectedWith400NotSilentlyDefaulted(t *testing.T) {
	fs := &fakeSearcher{}
	handler := newTestSearchServer(t, fs, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/search?q=widgets&mode=bogus", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s, want 400", rec.Code, rec.Body.String())
	}
	if fs.lastRequest.Query != "" {
		t.Errorf("searcher was invoked (lastRequest = %+v) for an unknown mode; it must be rejected before ever reaching the Searcher, not silently defaulted to hybrid", fs.lastRequest)
	}

	var body map[string]string
	decodeJSONBody(t, rec, &body)
	if body["error"] == "" {
		t.Errorf("error body missing non-empty \"error\" field: %+v", body)
	}
}

func TestSearchLimitIsClampedToASaneMaximum(t *testing.T) {
	fs := &fakeSearcher{response: search.Response{Mode: search.ModeHybrid}}
	handler := newTestSearchServer(t, fs, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/search?q=widgets&limit=100000", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if fs.lastRequest.Limit != webMaxSearchLimit {
		t.Errorf("Limit sent to Searcher = %d, want clamped to webMaxSearchLimit = %d", fs.lastRequest.Limit, webMaxSearchLimit)
	}
}

func TestSearchLimitRejectsZeroAndNegative(t *testing.T) {
	for _, limit := range []string{"0", "-1", "-100"} {
		t.Run(limit, func(t *testing.T) {
			fs := &fakeSearcher{}
			handler := newTestSearchServer(t, fs, nil)

			req := httptest.NewRequest(http.MethodGet, "/api/search?q=widgets&limit="+limit, nil)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("limit=%s: status = %d, body = %s, want 400", limit, rec.Code, rec.Body.String())
			}
			if fs.lastRequest.Query != "" {
				t.Errorf("limit=%s: searcher was invoked despite a nonsense limit", limit)
			}
		})
	}
}

func TestSearchMissingQueryIsBadRequest(t *testing.T) {
	fs := &fakeSearcher{}
	handler := newTestSearchServer(t, fs, nil)

	for _, qs := range []string{"", "?q=", "?q=%20%20"} {
		t.Run(qs, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/search"+qs, nil)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %s, want 400", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestSearchErrorIs500WithNoInternalDetailLeaked(t *testing.T) {
	sensitive := `pq: relation "search.passages" does not exist, connection to 10.0.0.5:5432 failed`
	fs := &fakeSearcher{err: errors.New(sensitive)}
	handler := newTestSearchServer(t, fs, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/search?q=widgets", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, body = %s, want 500", rec.Code, rec.Body.String())
	}

	body := rec.Body.String()
	for _, leak := range []string{"pq:", "10.0.0.5", "search.passages", sensitive} {
		if strings.Contains(body, leak) {
			t.Fatalf("response body leaked internal detail %q: %s", leak, body)
		}
	}

	var decoded map[string]string
	if err := json.NewDecoder(strings.NewReader(body)).Decode(&decoded); err != nil {
		t.Fatalf("decode error body: %v, body = %s", err, body)
	}
	if decoded["error"] == "" {
		t.Errorf("error body missing non-empty \"error\" field: %+v", decoded)
	}
}

func TestSearchFiltersRoundTripFromQueryParameters(t *testing.T) {
	fs := &fakeSearcher{response: search.Response{Mode: search.ModeKeyword}}
	handler := newTestSearchServer(t, fs, nil)

	qs := url.Values{}
	qs.Set("q", "widgets")
	qs.Set("mode", "keyword")
	qs.Set("limit", "5")
	qs["feed"] = []string{"1", "2,3"}
	qs["category"] = []string{"7"}
	qs.Set("unread", "true")
	qs.Set("starred", "true")
	qs.Set("since", "2024-01-01")
	qs.Set("until", "2024-06-15T00:00:00Z")

	req := httptest.NewRequest(http.MethodGet, "/api/search?"+qs.Encode(), nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	got := fs.lastRequest
	if got.Mode != search.ModeKeyword {
		t.Errorf("Mode = %v, want ModeKeyword", got.Mode)
	}
	if got.Limit != 5 {
		t.Errorf("Limit = %d, want 5", got.Limit)
	}
	if want := []int64{1, 2, 3}; !reflect.DeepEqual(got.Filters.FeedIDs, want) {
		t.Errorf("FeedIDs = %v, want %v", got.Filters.FeedIDs, want)
	}
	if want := []int64{7}; !reflect.DeepEqual(got.Filters.CategoryIDs, want) {
		t.Errorf("CategoryIDs = %v, want %v", got.Filters.CategoryIDs, want)
	}
	if !got.Filters.UnreadOnly {
		t.Errorf("UnreadOnly = false, want true")
	}
	if !got.Filters.StarredOnly {
		t.Errorf("StarredOnly = false, want true")
	}
	wantSince, _ := time.Parse("2006-01-02", "2024-01-01")
	if !got.Filters.Since.Equal(wantSince) {
		t.Errorf("Since = %v, want %v", got.Filters.Since, wantSince)
	}
	wantUntil, _ := time.Parse(time.RFC3339, "2024-06-15T00:00:00Z")
	if !got.Filters.Until.Equal(wantUntil) {
		t.Errorf("Until = %v, want %v", got.Filters.Until, wantUntil)
	}
}

func TestSearchInvalidFilterValueIsBadRequest(t *testing.T) {
	fs := &fakeSearcher{}
	handler := newTestSearchServer(t, fs, nil)

	for _, qs := range []string{
		"?q=widgets&feed=notanumber",
		"?q=widgets&category=notanumber",
		"?q=widgets&unread=maybe",
		"?q=widgets&starred=maybe",
		"?q=widgets&since=not-a-date",
		"?q=widgets&until=not-a-date",
	} {
		t.Run(qs, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/search"+qs, nil)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %s, want 400", rec.Code, rec.Body.String())
			}
		})
	}
}

// TestSearchAllowsCrossOriginRequests proves the read-only search endpoint
// does NOT carry the sameOriginOrNoOrigin check the control endpoints use
// (TestPauseRejectsCrossOriginRequest's counterpart): a GET carrying a
// foreign Origin header must still succeed.
func TestSearchAllowsCrossOriginRequests(t *testing.T) {
	fs := &fakeSearcher{response: search.Response{Mode: search.ModeHybrid}}
	handler := newTestSearchServer(t, fs, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/search?q=widgets", nil)
	req.Header.Set("Origin", "https://evil.example")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("cross-origin GET /api/search was rejected: status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestSearchBuildsHighlightedSnippetsFromEntryLookup(t *testing.T) {
	const text = "Wonderful widgets for everyone."
	fe := &fakeEntries{
		entries: map[int64]*store.Entry{
			42: {ID: 42, Title: "About widgets", Content: "<p>" + text + "</p>", ContentHash: "hash-42"},
		},
		states: map[int64]*store.IndexState{
			42: {ContentHash: "hash-42", Status: "ok"},
		},
	}
	fs := &fakeSearcher{response: search.Response{
		Mode: search.ModeHybrid,
		Entries: []search.EntryHit{
			{EntryID: 42, Score: 1, Best: search.PassageHit{
				EntryID: 42, Source: "content", Text: text,
				CharStart: 0, CharEnd: len(text),
			}},
		},
	}}
	handler := newTestSearchServer(t, fs, fe)

	req := httptest.NewRequest(http.MethodGet, "/api/search?q=widgets", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	var got struct {
		Entries []struct {
			Snippet struct {
				Text       string `json:"text"`
				Highlights []struct {
					Start int `json:"start"`
					End   int `json:"end"`
				} `json:"highlights"`
			} `json:"snippet"`
		} `json:"entries"`
	}
	decodeJSONBody(t, rec, &got)

	if len(got.Entries) != 1 {
		t.Fatalf("entries = %+v, want exactly 1", got.Entries)
	}
	snippet := got.Entries[0].Snippet
	if !strings.Contains(strings.ToLower(snippet.Text), "widgets") {
		t.Fatalf("snippet text = %q, want it to contain %q", snippet.Text, "widgets")
	}
	if len(snippet.Highlights) == 0 {
		t.Errorf("no highlights found for query term %q in snippet %q", "widgets", snippet.Text)
	}
}

func TestSimilarReturnsRankedJSON(t *testing.T) {
	fs := &fakeSearcher{similarHits: []search.EntryHit{
		{EntryID: 100, Score: 0.1, Best: search.PassageHit{Text: "a"}},
		{EntryID: 200, Score: 0.2, Best: search.PassageHit{Text: "b"}},
	}}
	handler := newTestSearchServer(t, fs, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/similar?entry_id=42&limit=5", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !fs.similarCalled {
		t.Fatalf("Searcher.Similar was never called")
	}
	if fs.lastEntryID != 42 {
		t.Errorf("entry id sent to Similar = %d, want 42", fs.lastEntryID)
	}
	if fs.lastLimit != 5 {
		t.Errorf("limit sent to Similar = %d, want 5", fs.lastLimit)
	}

	var got struct {
		EntryID int64 `json:"entry_id"`
		Entries []struct {
			EntryID int64 `json:"entry_id"`
		} `json:"entries"`
	}
	decodeJSONBody(t, rec, &got)
	if got.EntryID != 42 {
		t.Errorf("response entry_id = %d, want 42", got.EntryID)
	}
	if len(got.Entries) != 2 || got.Entries[0].EntryID != 100 || got.Entries[1].EntryID != 200 {
		t.Fatalf("entries = %+v, want [100, 200] in order", got.Entries)
	}
}

func TestSimilarMissingEntryIDIsBadRequest(t *testing.T) {
	fs := &fakeSearcher{}
	handler := newTestSearchServer(t, fs, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/similar", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s, want 400", rec.Code, rec.Body.String())
	}
	if fs.similarCalled {
		t.Errorf("Searcher.Similar was called despite a missing entry_id")
	}
}

func TestSimilarInvalidEntryIDIsBadRequest(t *testing.T) {
	fs := &fakeSearcher{}
	handler := newTestSearchServer(t, fs, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/similar?entry_id=notanumber", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s, want 400", rec.Code, rec.Body.String())
	}
}

func TestSimilarErrorIs500WithNoInternalDetailLeaked(t *testing.T) {
	fs := &fakeSearcher{similarErr: errors.New("dial tcp 10.0.0.5:5432: connect: connection refused")}
	handler := newTestSearchServer(t, fs, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/similar?entry_id=42", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, body = %s, want 500", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "10.0.0.5") {
		t.Fatalf("response body leaked internal detail: %s", rec.Body.String())
	}
}

// --- user scoping (whole-branch review, finding 3) ------------------------

// countingEntries is an EntryLookup that records how many times it was
// consulted, so a test can assert that a handler did no snippet work at
// all rather than merely that the snippet came out empty.
type countingEntries struct {
	entryCalls int
	stateCalls int
}

func (c *countingEntries) EntryForIndexing(_ context.Context, entryID int64) (*store.Entry, error) {
	c.entryCalls++
	return &store.Entry{ID: entryID, Title: "t", Content: "<p>body</p>"}, nil
}

func (c *countingEntries) EntryIndexState(_ context.Context, entryID int64) (*store.IndexState, error) {
	c.stateCalls++
	return nil, nil
}

// TestSearchUserParameterReachesTheSearcher is the fix for the review's
// finding 3 at the API boundary: search.passages is global, so unless the
// caller's user id reaches search.Filters the top-N is drawn from every
// user's content and the caller silently gets a short page of their own.
func TestSearchUserParameterReachesTheSearcher(t *testing.T) {
	fs := &fakeSearcher{}
	handler := newTestSearchServer(t, fs, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/search?q=widgets&user=7", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if fs.lastRequest.Filters.UserID != 7 {
		t.Fatalf("Filters.UserID sent to Search = %d, want 7", fs.lastRequest.Filters.UserID)
	}
}

// TestSearchWithoutUserParameterIsUnscoped pins the documented default:
// an absent "user" still means "every user's content", for the operator
// and eval callers that legitimately want that.
func TestSearchWithoutUserParameterIsUnscoped(t *testing.T) {
	fs := &fakeSearcher{}
	handler := newTestSearchServer(t, fs, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/search?q=widgets", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if fs.lastRequest.Filters.UserID != 0 {
		t.Fatalf("Filters.UserID = %d, want 0 (unscoped)", fs.lastRequest.Filters.UserID)
	}
}

func TestSimilarUserParameterReachesTheSearcher(t *testing.T) {
	fs := &fakeSearcher{}
	handler := newTestSearchServer(t, fs, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/similar?entry_id=42&user=7", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if fs.lastFilters.UserID != 7 {
		t.Fatalf("Filters.UserID sent to Similar = %d, want 7", fs.lastFilters.UserID)
	}
}

// TestBadUserParameterIsBadRequest proves a caller that meant to scope
// and got it wrong is told so, rather than quietly answered with the
// whole corpus.
func TestBadUserParameterIsBadRequest(t *testing.T) {
	for _, target := range []string{
		"/api/search?q=widgets&user=nope",
		"/api/search?q=widgets&user=0",
		"/api/search?q=widgets&user=-3",
		"/api/similar?entry_id=42&user=nope",
		"/api/similar?entry_id=42&user=0",
	} {
		fs := &fakeSearcher{}
		handler := newTestSearchServer(t, fs, nil)

		req := httptest.NewRequest(http.MethodGet, target, nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Errorf("GET %s: status = %d, body = %s, want 400", target, rec.Code, rec.Body.String())
		}
	}
}

// --- no snippet work on the similar path (finding 2) ----------------------

// TestSimilarDoesNoSnippetWork is the fix for the review's finding 2: the
// fork's entry page renders a similar article's title, feed and category
// from its own database and never reads a snippet, so /api/similar must
// not pay for one. Asserting on the EntryLookup call count rather than on
// the response body is deliberate — an empty snippet field would look the
// same either way; only the call count proves the round trips and the
// HTML extraction did not happen.
func TestSimilarDoesNoSnippetWork(t *testing.T) {
	fs := &fakeSearcher{similarHits: []search.EntryHit{
		{EntryID: 100, Score: 0.1, Best: search.PassageHit{Text: "a"}},
		{EntryID: 200, Score: 0.2, Best: search.PassageHit{Text: "b"}},
	}}
	lookup := &countingEntries{}
	handler := newTestSearchServer(t, fs, lookup)

	req := httptest.NewRequest(http.MethodGet, "/api/similar?entry_id=42", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if lookup.entryCalls != 0 || lookup.stateCalls != 0 {
		t.Fatalf("EntryLookup consulted %d/%d times building a similar-article response; want 0 — no snippets there",
			lookup.entryCalls, lookup.stateCalls)
	}
	if strings.Contains(rec.Body.String(), "snippet") {
		t.Fatalf("/api/similar response still carries a snippet field: %s", rec.Body.String())
	}
}

// TestSearchStillBuildsSnippets is the counterweight: dropping snippets
// from /api/similar must not drop them from /api/search, where they are
// what explains the match.
func TestSearchStillBuildsSnippets(t *testing.T) {
	fs := &fakeSearcher{response: search.Response{
		Mode:    search.ModeHybrid,
		Entries: []search.EntryHit{{EntryID: 100, Best: search.PassageHit{Text: "a"}}},
	}}
	lookup := &countingEntries{}
	handler := newTestSearchServer(t, fs, lookup)

	req := httptest.NewRequest(http.MethodGet, "/api/search?q=widgets", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if lookup.entryCalls == 0 {
		t.Fatal("/api/search built no snippet: EntryLookup was never consulted")
	}
}
