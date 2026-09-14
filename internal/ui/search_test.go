// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui // import "miniflux.app/v2/internal/ui"

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"miniflux.app/v2/internal/model"
)

// testUserID is the reader every resolveSearchResults call in this file
// is made on behalf of. Its value does not matter to these tests beyond
// being non-zero: what matters is that it reaches the sidecar, which
// TestResolveSearchResults_SendsTheUserScope asserts directly.
const testUserID int64 = 42

// fallbackEntries is what the pre-existing WithSearchQuery path would
// have returned; resolveSearchResults must return exactly these entries
// (wrapped as rows with no snippet) whenever it falls back, whatever the
// reason.
func fallbackEntries() (model.Entries, int, error) {
	return model.Entries{
		{ID: 101, Title: "Fallback result one"},
		{ID: 102, Title: "Fallback result two"},
	}, 2, nil
}

// rowTitles extracts each row's entry title, in order, for assertions.
func rowTitles(rows []searchRow) []string {
	titles := make([]string, len(rows))
	for i, row := range rows {
		titles[i] = row.Entry.Title
	}
	return titles
}

// TestResolveSearchResults_NoSidecarConfigured is the "feature is off"
// case (SEARCH_SIDECAR_URL unset): resolveSearchResults must go straight
// to fallback, not degraded (there was nothing to fail).
func TestResolveSearchResults_NoSidecarConfigured(t *testing.T) {
	rows, count, degraded, err := resolveSearchResults(
		context.Background(),
		"",
		testUserID,
		"coffee",
		"hybrid",
		false,
		0,
		10,
		func(ids []int64) (model.Entries, error) {
			t.Fatal("hydrate should not be called when the sidecar is not configured")
			return nil, nil
		},
		fallbackEntries,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if degraded {
		t.Fatal("expected degraded=false when the sidecar was never configured")
	}
	if count != 2 || len(rows) != 2 {
		t.Fatalf("expected the fallback's 2 rows, got %d (count %d)", len(rows), count)
	}
	for _, row := range rows {
		if row.Segments != nil {
			t.Fatalf("expected no snippet segments from the fallback path, got %+v", row.Segments)
		}
	}
}

// TestResolveSearchResults_SidecarUnreachableFallsBack is the property
// spec §8.3 exists for: with the sidecar unreachable, the search page
// must still render results — from the fallback path — rather than
// erroring out or hanging. This is the one most likely to be quietly
// broken later, per the task brief, so it asserts on the actual
// rendered entries, not just "no error".
func TestResolveSearchResults_SidecarUnreachableFallsBack(t *testing.T) {
	// Reserve a port and immediately stop listening on it, so any
	// connection attempt is refused - "the sidecar is unreachable".
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("unable to reserve a port: %v", err)
	}
	addr := listener.Addr().String()
	listener.Close()

	start := time.Now()
	rows, count, degraded, err := resolveSearchResults(
		context.Background(),
		"http://"+addr,
		testUserID,
		"coffee",
		"hybrid",
		false,
		0,
		10,
		func(ids []int64) (model.Entries, error) {
			t.Fatal("hydrate should not be reached when Search itself fails")
			return nil, nil
		},
		fallbackEntries,
	)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("the search page must still render on a sidecar failure, got error: %v", err)
	}
	if !degraded {
		t.Fatal("expected degraded=true: the sidecar was tried and failed")
	}
	if count != 2 || len(rows) != 2 {
		t.Fatalf("expected the page to render the fallback's 2 rows, got %d (count %d)", len(rows), count)
	}
	titles := rowTitles(rows)
	if titles[0] != "Fallback result one" || titles[1] != "Fallback result two" {
		t.Fatalf("unexpected fallback rows rendered: %+v", titles)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("expected the page to resolve promptly rather than block on the sidecar, took %v", elapsed)
	}
}

// TestResolveSearchResults_SidecarNonOKFallsBack proves the fallback
// triggers for a sidecar that answers but with an error status, not just
// for connection failures.
func TestResolveSearchResults_SidecarNonOKFallsBack(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	rows, count, degraded, err := resolveSearchResults(
		context.Background(), server.URL, testUserID, "coffee", "hybrid", false, 0, 10,
		func(ids []int64) (model.Entries, error) { return nil, nil },
		fallbackEntries,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !degraded {
		t.Fatal("expected degraded=true for a non-200 sidecar response")
	}
	if count != 2 || len(rows) != 2 {
		t.Fatalf("expected fallback rows, got %d (count %d)", len(rows), count)
	}
}

// TestResolveSearchResults_SidecarMalformedBodyFallsBack proves the
// fallback triggers for a sidecar that answers 200 with a body that does
// not parse.
func TestResolveSearchResults_SidecarMalformedBodyFallsBack(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("{not json"))
	}))
	defer server.Close()

	rows, count, degraded, err := resolveSearchResults(
		context.Background(), server.URL, testUserID, "coffee", "hybrid", false, 0, 10,
		func(ids []int64) (model.Entries, error) { return nil, nil },
		fallbackEntries,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !degraded {
		t.Fatal("expected degraded=true for a malformed sidecar response body")
	}
	if count != 2 || len(rows) != 2 {
		t.Fatalf("expected fallback rows, got %d (count %d)", len(rows), count)
	}
}

// TestResolveSearchResults_SidecarSuccessOrdersAndHydrates proves the
// happy path: sidecar hits are hydrated into full model.Entry values,
// carrying the sidecar's own snippet, in the sidecar's own ranked order,
// and NOT degraded.
func TestResolveSearchResults_SidecarSuccessOrdersAndHydrates(t *testing.T) {
	var gotMode string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMode = r.URL.Query().Get("mode")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"mode":"hybrid","query":"coffee","entries":[
			{"entry_id":5,"score":0.9,"snippet":{"text":"a good cup","highlights":[{"start":2,"end":6}]}},
			{"entry_id":3,"score":0.8,"snippet":{"text":"b","highlights":[]}}
		]}`))
	}))
	defer server.Close()

	var hydratedIDs []int64
	rows, count, degraded, err := resolveSearchResults(
		context.Background(), server.URL, testUserID, "coffee", "hybrid", false, 0, 10,
		func(ids []int64) (model.Entries, error) {
			hydratedIDs = ids
			// Return them out of order on purpose, to prove
			// resolveSearchResults re-orders by the sidecar's ranking
			// rather than trusting the store's own order.
			return model.Entries{
				{ID: 3, Title: "Three"},
				{ID: 5, Title: "Five"},
			}, nil
		},
		func() (model.Entries, int, error) {
			t.Fatal("fallback should not be called on a sidecar success")
			return nil, 0, nil
		},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if degraded {
		t.Fatal("expected degraded=false on a sidecar success")
	}
	if gotMode != "hybrid" {
		t.Fatalf("expected the requested mode to reach the sidecar as mode=hybrid, got %q", gotMode)
	}
	if len(hydratedIDs) != 2 || hydratedIDs[0] != 5 || hydratedIDs[1] != 3 {
		t.Fatalf("expected hydrate to be called with [5 3], got %v", hydratedIDs)
	}
	if count != 2 || len(rows) != 2 {
		t.Fatalf("expected 2 rows, got %d (count %d)", len(rows), count)
	}
	if rows[0].Entry.ID != 5 || rows[1].Entry.ID != 3 {
		t.Fatalf("expected rows ordered [5 3] per the sidecar ranking, got [%d %d]", rows[0].Entry.ID, rows[1].Entry.ID)
	}
	if len(rows[0].Segments) != 3 || rows[0].Segments[1].Text != "good" || !rows[0].Segments[1].Highlight {
		t.Fatalf("expected the first row's snippet to be highlighted per its offsets, got %+v", rows[0].Segments)
	}
	if len(rows[1].Segments) != 1 || rows[1].Segments[0].Highlight {
		t.Fatalf("expected a single, unhighlighted segment for a snippet with no highlights, got %+v", rows[1].Segments)
	}
}

// TestResolveSearchResults_PassagesModeBuildsOneRowPerPassage proves
// passages mode (spec §6.4) is aggregated differently from the other
// three: one row per passage, not per entry, with the sidecar's 0-based
// Ordinal surfaced as a 1-based row Ordinal, and the same entry allowed
// to appear more than once.
func TestResolveSearchResults_PassagesModeBuildsOneRowPerPassage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"mode":"passages","query":"coffee","passages":[
			{"entry_id":5,"passage_id":1,"ordinal":0,"score":0.9,"snippet":{"text":"first passage","highlights":[]}},
			{"entry_id":5,"passage_id":2,"ordinal":2,"score":0.7,"snippet":{"text":"third passage","highlights":[]}}
		]}`))
	}))
	defer server.Close()

	rows, count, degraded, err := resolveSearchResults(
		context.Background(), server.URL, testUserID, "coffee", "passages", false, 0, 10,
		func(ids []int64) (model.Entries, error) {
			return model.Entries{{ID: 5, Title: "Five"}}, nil
		},
		func() (model.Entries, int, error) {
			t.Fatal("fallback should not be called on a sidecar success")
			return nil, 0, nil
		},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if degraded {
		t.Fatal("expected degraded=false on a sidecar success")
	}
	if count != 2 || len(rows) != 2 {
		t.Fatalf("expected 2 passage rows, got %d (count %d)", len(rows), count)
	}
	if rows[0].Entry.ID != 5 || rows[1].Entry.ID != 5 {
		t.Fatalf("expected both rows to reference entry 5, got %+v", rows)
	}
	if rows[0].Ordinal != 1 || rows[1].Ordinal != 3 {
		t.Fatalf("expected 1-based ordinals [1 3], got [%d %d]", rows[0].Ordinal, rows[1].Ordinal)
	}
}

// TestResolveSearchResults_HydrateErrorFallsBack proves a failure while
// loading full entry data for the sidecar's hit ids (a store error) also
// falls back, rather than propagating a hard error to the page.
func TestResolveSearchResults_HydrateErrorFallsBack(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"mode":"hybrid","query":"coffee","entries":[{"entry_id":5,"score":0.9,"snippet":{"text":"a","highlights":[]}}]}`))
	}))
	defer server.Close()

	rows, count, degraded, err := resolveSearchResults(
		context.Background(), server.URL, testUserID, "coffee", "hybrid", false, 0, 10,
		func(ids []int64) (model.Entries, error) { return nil, errors.New("store exploded") },
		fallbackEntries,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !degraded {
		t.Fatal("expected degraded=true when hydrating sidecar hits fails")
	}
	if count != 2 || len(rows) != 2 {
		t.Fatalf("expected fallback rows, got %d (count %d)", len(rows), count)
	}
}

// TestResolveSearchResults_PassagesHydrateErrorFallsBack proves the same
// fallback behaviour holds for passages mode's own hydrate call.
func TestResolveSearchResults_PassagesHydrateErrorFallsBack(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"mode":"passages","query":"coffee","passages":[{"entry_id":5,"passage_id":1,"ordinal":0,"score":0.9,"snippet":{"text":"a","highlights":[]}}]}`))
	}))
	defer server.Close()

	rows, count, degraded, err := resolveSearchResults(
		context.Background(), server.URL, testUserID, "coffee", "passages", false, 0, 10,
		func(ids []int64) (model.Entries, error) { return nil, errors.New("store exploded") },
		fallbackEntries,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !degraded {
		t.Fatal("expected degraded=true when hydrating passage hits fails")
	}
	if count != 2 || len(rows) != 2 {
		t.Fatalf("expected fallback rows, got %d (count %d)", len(rows), count)
	}
}

// TestParseSearchMode proves unrecognised or absent mode values resolve
// to the documented default rather than being passed through verbatim.
func TestParseSearchMode(t *testing.T) {
	cases := map[string]string{
		"":         "hybrid",
		"hybrid":   "hybrid",
		"keyword":  "keyword",
		"semantic": "semantic",
		"passages": "passages",
		"bogus":    "hybrid",
		"Keyword":  "hybrid", // case-sensitive on purpose: this is a fixed <select> value, not free text
	}
	for in, want := range cases {
		if got := parseSearchMode(in); got != want {
			t.Errorf("parseSearchMode(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestResolveSearchResults_SendsTheUserScope is the fork half of the
// whole-branch review's finding 3. The sidecar indexes every user's
// passages in one table, so a search that does not name its user draws
// its candidate set from the whole corpus; the fork's own hydration then
// removes what it does not own, leaving the reader with a short page —
// or an empty one — for a query their own articles match. Nothing about
// that is visible in the rendered page, which is why it needs a test at
// the wire level rather than an assertion about rows.
func TestResolveSearchResults_SendsTheUserScope(t *testing.T) {
	var gotUser string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUser = r.URL.Query().Get("user")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"mode":"hybrid","query":"coffee","entries":[]}`))
	}))
	defer server.Close()

	_, _, _, err := resolveSearchResults(
		context.Background(), server.URL, testUserID, "coffee", "hybrid", false, 0, 10,
		func(ids []int64) (model.Entries, error) { return model.Entries{}, nil },
		func() (model.Entries, int, error) {
			t.Fatal("fallback should not be called on a sidecar success")
			return nil, 0, nil
		},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotUser != "42" {
		t.Fatalf("the sidecar was asked for user=%q, want %q — an unscoped search silently shortens the reader's own results", gotUser, "42")
	}
}

// TestResolveSearchResults_OffsetFallbackIsDegraded is the review's
// finding 7. Paging past the first page cannot use the sidecar (its API
// has no offset), so those results come from the built-in search — a
// different engine, with a different ranking, under a mode picker still
// showing the mode the reader chose. That has to be said out loud.
func TestResolveSearchResults_OffsetFallbackIsDegraded(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.Write([]byte(`{"mode":"hybrid","query":"coffee","entries":[]}`))
	}))
	defer server.Close()

	rows, count, degraded, err := resolveSearchResults(
		context.Background(), server.URL, testUserID, "coffee", "hybrid", false, 20, 10,
		func(ids []int64) (model.Entries, error) { return nil, nil },
		fallbackEntries,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if called {
		t.Fatal("expected the sidecar not to be called for a non-zero offset")
	}
	if !degraded {
		t.Fatal("expected degraded=true: page 2 is served by the built-in search, not the mode the page says it is using")
	}
	if count != 2 || len(rows) != 2 {
		t.Fatalf("expected the fallback's rows to still render, got %d (count %d)", len(rows), count)
	}
}

// TestSidecarSearchLimitCapsThePageSize is the review's finding 5. The
// sidecar multiplies its limit by 25 on the way to the index and builds
// one snippet per result, so handing it EntriesPerPage (100 by default)
// made every search far more expensive than the page it produced.
func TestSidecarSearchLimitCapsThePageSize(t *testing.T) {
	cases := map[int]int{
		100: maxSidecarSearchLimit, // the Miniflux default
		50:  maxSidecarSearchLimit,
		21:  maxSidecarSearchLimit,
		20:  20,
		10:  10, // a smaller page size is honoured, so the count matches the pagination
		1:   1,
		0:   maxSidecarSearchLimit, // unset/nonsense falls back to the cap, never to zero
		-5:  maxSidecarSearchLimit,
	}
	for in, want := range cases {
		if got := sidecarSearchLimit(in); got != want {
			t.Errorf("sidecarSearchLimit(%d) = %d, want %d", in, got, want)
		}
	}
}

// TestResolveSearchResults_HonoursTheCappedLimit proves the cap is what
// actually reaches the sidecar, not just what the helper returns.
func TestResolveSearchResults_HonoursTheCappedLimit(t *testing.T) {
	var gotLimit string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotLimit = r.URL.Query().Get("limit")
		w.Write([]byte(`{"mode":"hybrid","query":"coffee","entries":[]}`))
	}))
	defer server.Close()

	_, _, _, err := resolveSearchResults(
		context.Background(), server.URL, testUserID, "coffee", "hybrid", false, 0,
		sidecarSearchLimit(100),
		func(ids []int64) (model.Entries, error) { return model.Entries{}, nil },
		func() (model.Entries, int, error) {
			t.Fatal("fallback should not be called on a sidecar success")
			return nil, 0, nil
		},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotLimit != "20" {
		t.Fatalf("the sidecar was asked for limit=%q, want %q", gotLimit, "20")
	}
}
