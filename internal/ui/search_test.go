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

// fallbackEntries is what the pre-existing WithSearchQuery path would
// have returned; resolveSearchResults must return exactly this whenever
// it falls back, whatever the reason.
func fallbackEntries() (model.Entries, int, error) {
	return model.Entries{
		{ID: 101, Title: "Fallback result one"},
		{ID: 102, Title: "Fallback result two"},
	}, 2, nil
}

// TestResolveSearchResults_NoSidecarConfigured is the "feature is off"
// case (SEARCH_SIDECAR_URL unset): resolveSearchResults must go straight
// to fallback, not degraded (there was nothing to fail).
func TestResolveSearchResults_NoSidecarConfigured(t *testing.T) {
	entries, count, degraded, err := resolveSearchResults(
		context.Background(),
		"",
		"coffee",
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
	if count != 2 || len(entries) != 2 {
		t.Fatalf("expected the fallback's 2 entries, got %d (count %d)", len(entries), count)
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
	entries, count, degraded, err := resolveSearchResults(
		context.Background(),
		"http://"+addr,
		"coffee",
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
	if count != 2 || len(entries) != 2 {
		t.Fatalf("expected the page to render the fallback's 2 entries, got %d (count %d)", len(entries), count)
	}
	if entries[0].Title != "Fallback result one" || entries[1].Title != "Fallback result two" {
		t.Fatalf("unexpected fallback entries rendered: %+v", entries)
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

	entries, count, degraded, err := resolveSearchResults(
		context.Background(), server.URL, "coffee", false, 0, 10,
		func(ids []int64) (model.Entries, error) { return nil, nil },
		fallbackEntries,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !degraded {
		t.Fatal("expected degraded=true for a non-200 sidecar response")
	}
	if count != 2 || len(entries) != 2 {
		t.Fatalf("expected fallback entries, got %d (count %d)", len(entries), count)
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

	entries, count, degraded, err := resolveSearchResults(
		context.Background(), server.URL, "coffee", false, 0, 10,
		func(ids []int64) (model.Entries, error) { return nil, nil },
		fallbackEntries,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !degraded {
		t.Fatal("expected degraded=true for a malformed sidecar response body")
	}
	if count != 2 || len(entries) != 2 {
		t.Fatalf("expected fallback entries, got %d (count %d)", len(entries), count)
	}
}

// TestResolveSearchResults_SidecarSuccessOrdersAndHydrates proves the
// happy path: sidecar hits are hydrated into full model.Entry values via
// hydrate, in the sidecar's own ranked order, and NOT degraded.
func TestResolveSearchResults_SidecarSuccessOrdersAndHydrates(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"mode":"hybrid","query":"coffee","entries":[
			{"entry_id":5,"score":0.9,"snippet":{"text":"a","highlights":[]}},
			{"entry_id":3,"score":0.8,"snippet":{"text":"b","highlights":[]}}
		]}`))
	}))
	defer server.Close()

	var hydratedIDs []int64
	entries, count, degraded, err := resolveSearchResults(
		context.Background(), server.URL, "coffee", false, 0, 10,
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
	if len(hydratedIDs) != 2 || hydratedIDs[0] != 5 || hydratedIDs[1] != 3 {
		t.Fatalf("expected hydrate to be called with [5 3], got %v", hydratedIDs)
	}
	if count != 2 || len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d (count %d)", len(entries), count)
	}
	if entries[0].ID != 5 || entries[1].ID != 3 {
		t.Fatalf("expected entries ordered [5 3] per the sidecar ranking, got [%d %d]", entries[0].ID, entries[1].ID)
	}
}

// TestResolveSearchResults_OffsetSkipsSidecar proves paging past the
// first page always uses the fallback path (the sidecar API has no
// offset parameter), and that this is NOT reported as degraded - it is a
// deliberate scope limit, not a failure.
func TestResolveSearchResults_OffsetSkipsSidecar(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"mode":"hybrid","query":"coffee","entries":[]}`))
	}))
	defer server.Close()

	_, _, degraded, err := resolveSearchResults(
		context.Background(), server.URL, "coffee", false, 20, 10,
		func(ids []int64) (model.Entries, error) { return nil, nil },
		fallbackEntries,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if called {
		t.Fatal("expected the sidecar not to be called for a non-zero offset")
	}
	if degraded {
		t.Fatal("skipping the sidecar for pagination is not a degradation")
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

	entries, count, degraded, err := resolveSearchResults(
		context.Background(), server.URL, "coffee", false, 0, 10,
		func(ids []int64) (model.Entries, error) { return nil, errors.New("store exploded") },
		fallbackEntries,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !degraded {
		t.Fatal("expected degraded=true when hydrating sidecar hits fails")
	}
	if count != 2 || len(entries) != 2 {
		t.Fatalf("expected fallback entries, got %d (count %d)", len(entries), count)
	}
}
