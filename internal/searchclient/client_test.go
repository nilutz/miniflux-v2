// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package searchclient // import "miniflux.app/v2/internal/searchclient"

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestClientSearch_Success proves a well-formed sidecar response parses
// into the shape search.go's routing depends on: entry ids, scores and
// highlighted snippet text, in the order the sidecar returned them.
func TestClientSearch_Success(t *testing.T) {
	var gotPath string
	var gotQuery string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.Query().Get("q")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"mode":  "hybrid",
			"query": "coffee",
			"entries": []map[string]any{
				{
					"entry_id": 42,
					"score":    0.87,
					"snippet": map[string]any{
						"text":       "the best coffee in town",
						"highlights": []map[string]any{{"start": 9, "end": 15}},
					},
				},
			},
		})
	}))
	defer server.Close()

	client := NewClientWithTimeout(server.URL, time.Second)
	resp, err := client.Search(context.Background(), SearchRequest{Query: "coffee"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if gotPath != "/api/search" {
		t.Fatalf("expected request to /api/search, got %q", gotPath)
	}
	if gotQuery != "coffee" {
		t.Fatalf("expected q=coffee, got %q", gotQuery)
	}
	if resp.Mode != "hybrid" || resp.Query != "coffee" {
		t.Fatalf("unexpected response header fields: %+v", resp)
	}
	if len(resp.Entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(resp.Entries))
	}
	hit := resp.Entries[0]
	if hit.EntryID != 42 {
		t.Fatalf("expected entry id 42, got %d", hit.EntryID)
	}
	if hit.Score != 0.87 {
		t.Fatalf("expected score 0.87, got %v", hit.Score)
	}
	if hit.Snippet.Text != "the best coffee in town" {
		t.Fatalf("unexpected snippet text: %q", hit.Snippet.Text)
	}
	if len(hit.Snippet.Highlights) != 1 || hit.Snippet.Highlights[0].Start != 9 || hit.Snippet.Highlights[0].End != 15 {
		t.Fatalf("unexpected highlights: %+v", hit.Snippet.Highlights)
	}
}

// TestClientSearch_TimeoutBounded is the test spec §8.3 exists for: a
// sidecar that accepts the connection but never answers must not be
// allowed to hang the reader. It asserts the client's own configured
// timeout is what bounds the call — not merely that a timeout field is
// set to some value — by using a real listener that never responds and
// checking the call returns within a small multiple of the configured
// timeout, never anywhere near "not at all".
func TestClientSearch_TimeoutBounded(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("unable to open listener: %v", err)
	}
	defer listener.Close()

	// Accept connections but never write a response, simulating a
	// sidecar that is up but wedged.
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			// Read nothing, write nothing, just hold the connection open
			// until the test closes the listener.
			_ = conn
		}
	}()

	const configuredTimeout = 200 * time.Millisecond
	client := NewClientWithTimeout("http://"+listener.Addr().String(), configuredTimeout)

	start := time.Now()
	_, err = client.Search(context.Background(), SearchRequest{Query: "coffee"})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected an error from a sidecar that never responds")
	}
	// Generous upper bound: bounded by the configured timeout, not by
	// nothing. A client that ignored the timeout and relied on, say, a
	// default net/http.Client with no deadline would hang far longer
	// than this and fail the test via its own -timeout, or at minimum
	// blow well past this assertion.
	if elapsed > 2*time.Second {
		t.Fatalf("expected the call to be bounded by the %v timeout, took %v", configuredTimeout, elapsed)
	}
}

// TestClientSearch_ConnectionRefused proves the ordinary "sidecar isn't
// running" case — nothing listening on the port at all — surfaces
// promptly as an error rather than hanging.
func TestClientSearch_ConnectionRefused(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("unable to reserve a port: %v", err)
	}
	addr := listener.Addr().String()
	listener.Close() // nothing is listening here now

	client := NewClientWithTimeout("http://"+addr, time.Second)

	start := time.Now()
	_, err = client.Search(context.Background(), SearchRequest{Query: "coffee"})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected an error when nothing is listening")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("expected a prompt connection-refused error, took %v", elapsed)
	}
}

// TestClientSearch_NonOKStatus proves a non-200 sidecar response (its
// own {"error": "..."} body per search_handlers.go) is treated as a
// failure, never decoded as if it were a successful result.
func TestClientSearch_NonOKStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]any{"error": "search failed"})
	}))
	defer server.Close()

	client := NewClientWithTimeout(server.URL, time.Second)
	_, err := client.Search(context.Background(), SearchRequest{Query: "coffee"})
	if err == nil {
		t.Fatal("expected an error for a non-200 response")
	}
}

// TestClientSearch_MalformedBody proves a 200 response whose body is not
// valid JSON (a misbehaving or mismatched sidecar) is a failure, not a
// zero-value success.
func TestClientSearch_MalformedBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "{not json")
	}))
	defer server.Close()

	client := NewClientWithTimeout(server.URL, time.Second)
	_, err := client.Search(context.Background(), SearchRequest{Query: "coffee"})
	if err == nil {
		t.Fatal("expected an error for a malformed response body")
	}
	if !strings.Contains(err.Error(), "decod") {
		t.Fatalf("expected the error to mention decoding, got: %v", err)
	}
}

// TestClientSimilar_Success proves Similar parses the /api/similar shape.
func TestClientSimilar_Success(t *testing.T) {
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"entry_id": 7,
			"entries": []map[string]any{
				{"entry_id": 8, "score": 0.5, "snippet": map[string]any{"text": "related", "highlights": []any{}}},
			},
		})
	}))
	defer server.Close()

	client := NewClientWithTimeout(server.URL, time.Second)
	resp, err := client.Similar(context.Background(), SimilarRequest{EntryID: 7})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotPath != "/api/similar" {
		t.Fatalf("expected request to /api/similar, got %q", gotPath)
	}
	if resp.EntryID != 7 {
		t.Fatalf("expected entry_id 7, got %d", resp.EntryID)
	}
	if len(resp.Entries) != 1 || resp.Entries[0].EntryID != 8 {
		t.Fatalf("unexpected entries: %+v", resp.Entries)
	}
}

// TestClientSendsTheUserScope proves both endpoints put the caller's user
// id on the wire. The sidecar's index is global; without this parameter
// its candidate set is drawn from every user's content and the reader
// silently gets a shorter page of their own.
func TestClientSendsTheUserScope(t *testing.T) {
	var got string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Query().Get("user")
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"mode":"hybrid","query":"coffee","entries":[]}`)
	}))
	defer server.Close()

	client := NewClientWithTimeout(server.URL, time.Second)

	if _, err := client.Search(context.Background(), SearchRequest{Query: "coffee", UserID: 7}); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if got != "7" {
		t.Fatalf("Search sent user=%q, want %q", got, "7")
	}

	got = ""
	if _, err := client.Similar(context.Background(), SimilarRequest{EntryID: 1, UserID: 7}); err != nil {
		t.Fatalf("Similar: %v", err)
	}
	if got != "7" {
		t.Fatalf("Similar sent user=%q, want %q", got, "7")
	}

	// A zero UserID stays off the wire entirely, so the sidecar applies
	// its own documented "unscoped" default rather than being handed
	// user=0, which it rejects as a caller error.
	got = "unset"
	if _, err := client.Search(context.Background(), SearchRequest{Query: "coffee"}); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if got != "" {
		t.Fatalf("a zero UserID put user=%q on the wire; want the parameter omitted", got)
	}
}

// TestSimilarTimeoutIsShorterThanSearch pins the relationship the entry
// page depends on: the similar-articles block is a sidebar on an
// already-loaded page and must not be allowed to hold the render path as
// long as a search the reader actually asked for.
func TestSimilarTimeoutIsShorterThanSearch(t *testing.T) {
	if SimilarTimeout >= DefaultTimeout {
		t.Fatalf("SimilarTimeout (%v) must be shorter than DefaultTimeout (%v)", SimilarTimeout, DefaultTimeout)
	}
}

// TestClientTimeoutBoundsASlowSidecar proves the per-Client timeout still
// bounds a request now that the *http.Client is shared process-wide and
// carries no Timeout of its own — the bound moved onto the request
// context, and this is what checks it did not simply disappear.
func TestClientTimeoutBoundsASlowSidecar(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer func() {
		close(release)
		server.Close()
	}()

	client := NewClientWithTimeout(server.URL, 50*time.Millisecond)

	start := time.Now()
	_, err := client.Similar(context.Background(), SimilarRequest{EntryID: 1})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected a timeout error from a sidecar that never answers")
	}
	if elapsed > time.Second {
		t.Fatalf("the call was not bounded by the client's own timeout, took %v", elapsed)
	}
}

// TestClientsReuseOneConnection guards against a *http.Client per request:
// that would mean a Transport per request, so every search and every
// entry page view would open a fresh TCP connection to a service on
// loopback and throw the pool away. Counting connections at the server,
// rather than inspecting the client, is what actually proves the pool is
// shared.
func TestClientsReuseOneConnection(t *testing.T) {
	var mu sync.Mutex
	conns := map[net.Conn]struct{}{}

	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"mode":"hybrid","query":"coffee","entries":[]}`)
	}))
	server.Config.ConnState = func(c net.Conn, state http.ConnState) {
		if state == http.StateNew {
			mu.Lock()
			conns[c] = struct{}{}
			mu.Unlock()
		}
	}
	server.Start()
	defer server.Close()

	for i := 0; i < 5; i++ {
		// A fresh Client each time, exactly as the UI handlers build one
		// per page view.
		client := NewClientWithTimeout(server.URL, time.Second)
		if _, err := client.Search(context.Background(), SearchRequest{Query: "coffee"}); err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
	}

	mu.Lock()
	n := len(conns)
	mu.Unlock()

	if n != 1 {
		t.Fatalf("5 sequential requests opened %d connections; want 1 — the shared transport's keep-alive is not being used", n)
	}
}
