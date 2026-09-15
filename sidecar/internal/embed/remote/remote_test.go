// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package remote // import "miniflux.app/v2/sidecar/internal/embed/remote"

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"miniflux.app/v2/sidecar/internal/embed"
)

// fakeVector returns a deterministic 384-dimension vector so tests can
// assert on its contents without caring about real embedding math.
func fakeVector(fill float32) []float32 {
	v := make([]float32, 384)
	for i := range v {
		v[i] = fill
	}
	return v
}

// newFakeServer starts an httptest server implementing this package's
// wire protocol: POST /embed, {"texts": [...]} in, {"vectors": [...],
// "model": {...}} out. model is fixed per test; each vector is filled
// with its index+1 so callers can tell which input produced which
// vector. It also records every request path/method it saw.
func newFakeServer(t *testing.T, dimensions int, name, revision string) (*httptest.Server, *int32) {
	t.Helper()
	var requests int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)

		if r.Method != http.MethodPost || r.URL.Path != "/embed" {
			http.NotFound(w, r)
			return
		}

		var req struct {
			Texts []string `json:"texts"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		vectors := make([][]float32, len(req.Texts))
		for i := range req.Texts {
			vectors[i] = fakeVector(float32(i + 1))
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"vectors": vectors,
			"model": map[string]any{
				"name":       name,
				"revision":   revision,
				"dimensions": dimensions,
			},
		})
	}))
	t.Cleanup(srv.Close)
	return srv, &requests
}

func TestNewProbesRemoteAndBatchRoundTrips(t *testing.T) {
	srv, requests := newFakeServer(t, 384, "bge-small-en-v1.5", "abc123")

	e, err := New(Config{URL: srv.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer e.Close()

	if atomic.LoadInt32(requests) != 1 {
		t.Fatalf("expected exactly 1 request from New's probe, got %d", atomic.LoadInt32(requests))
	}

	vectors, err := e.Embed(context.Background(), []string{"first", "second"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(vectors) != 2 {
		t.Fatalf("expected 2 vectors, got %d", len(vectors))
	}
	for i, v := range vectors {
		if len(v) != e.Dimensions() {
			t.Fatalf("vector %d has length %d, want %d", i, len(v), e.Dimensions())
		}
	}
	if vectors[0][0] != 1 || vectors[1][0] != 2 {
		t.Fatalf("vectors not in request order: got %v / %v", vectors[0][:1], vectors[1][:1])
	}
	if e.Dimensions() != 384 {
		t.Fatalf("Dimensions() = %d, want 384", e.Dimensions())
	}
}

func TestIdentitySurfacesRemoteModel(t *testing.T) {
	srv, _ := newFakeServer(t, 384, "bge-small-en-v1.5", "abc123")

	e, err := New(Config{URL: srv.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer e.Close()

	want := embed.Identity("bge-small-en-v1.5", "abc123", 384)
	if got := e.Identity(); got != want {
		t.Fatalf("Identity() = %q, want %q", got, want)
	}
}

func TestEmbedWithNoTextsDoesNotCallRemote(t *testing.T) {
	srv, requests := newFakeServer(t, 384, "bge-small-en-v1.5", "abc123")

	e, err := New(Config{URL: srv.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer e.Close()

	before := atomic.LoadInt32(requests)
	vectors, err := e.Embed(context.Background(), nil)
	if err != nil {
		t.Fatalf("Embed with no texts returned an error: %v", err)
	}
	if vectors != nil {
		t.Fatalf("expected no vectors for no input, got %v", vectors)
	}
	if got := atomic.LoadInt32(requests); got != before {
		t.Fatalf("Embed with no texts made a network call: request count went from %d to %d", before, got)
	}
}

func TestDimensionMismatchFailsAtConstruction(t *testing.T) {
	srv, _ := newFakeServer(t, 768, "some-other-model", "xyz")

	_, err := New(Config{URL: srv.URL})
	if err == nil {
		t.Fatal("expected an error for a dimension mismatch, got nil")
	}
	if !strings.Contains(err.Error(), "384") || !strings.Contains(err.Error(), "768") {
		t.Fatalf("error should name both dimensions, got: %v", err)
	}
}

func TestIdentityWithAmbiguousSeparatorIsRejected(t *testing.T) {
	srv, _ := newFakeServer(t, 384, "bge-small@evil", "abc123")

	_, err := New(Config{URL: srv.URL})
	if err == nil {
		t.Fatal("expected an error for a model name containing '@', got nil")
	}
}

func TestConnectionFailureReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close() // nothing is listening here anymore

	_, err := New(Config{URL: url, Timeout: time.Second})
	if err == nil {
		t.Fatal("expected a connection error, got nil")
	}
}

func TestNonOKStatusReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	_, err := New(Config{URL: srv.URL})
	if err == nil {
		t.Fatal("expected an error for a non-200 response, got nil")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Fatalf("error should mention the status code, got: %v", err)
	}
}

func TestMalformedBodyReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("not json"))
	}))
	t.Cleanup(srv.Close)

	_, err := New(Config{URL: srv.URL})
	if err == nil {
		t.Fatal("expected an error for a malformed response body, got nil")
	}
}

func TestTimeoutIsHonoured(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"vectors": [][]float32{},
			"model":   map[string]any{"name": "m", "revision": "r", "dimensions": 384},
		})
	}))
	t.Cleanup(srv.Close)

	start := time.Now()
	_, err := New(Config{URL: srv.URL, Timeout: 10 * time.Millisecond})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected a timeout error, got nil")
	}
	if elapsed > 90*time.Millisecond {
		t.Fatalf("New did not honour the configured timeout: took %s", elapsed)
	}
}

func TestNewRequiresURL(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("expected an error for an empty Config.URL, got nil")
	}
}
