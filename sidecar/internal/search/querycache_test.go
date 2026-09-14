// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package search // import "miniflux.app/v2/sidecar/internal/search"

import (
	"context"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
)

// countingFakeEmbedder is a hermetic stand-in for embed.Embedder: no
// database, no ONNX runtime, just a call counter and a deterministic
// vector per input, so these tests can assert exactly how many times
// embedCached actually reached the embedder.
type countingFakeEmbedder struct {
	calls atomic.Int64
}

func (f *countingFakeEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	f.calls.Add(1)
	out := make([][]float32, len(texts))
	for i, t := range texts {
		out[i] = []float32{float32(len(t)), 0, 0}
	}
	return out, nil
}

func (f *countingFakeEmbedder) Dimensions() int  { return 3 }
func (f *countingFakeEmbedder) Identity() string { return "counting-fake-embedder@test#3" }
func (f *countingFakeEmbedder) Close() error     { return nil }

// TestEmbedCachedEmbedsRepeatedQueryOnce is the cache's whole reason to
// exist (spec §6.5): the same query text embedded twice must invoke the
// embedder only once, returning the identical vector both times.
func TestEmbedCachedEmbedsRepeatedQueryOnce(t *testing.T) {
	fe := &countingFakeEmbedder{}
	cache := newQueryCache(128)
	ctx := context.Background()

	v1, err := embedCached(ctx, cache, fe, "hello world")
	if err != nil {
		t.Fatalf("embedCached: unexpected error: %v", err)
	}
	v2, err := embedCached(ctx, cache, fe, "hello world")
	if err != nil {
		t.Fatalf("embedCached: unexpected error: %v", err)
	}

	if calls := fe.calls.Load(); calls != 1 {
		t.Fatalf("expected the embedder to be called exactly once for a repeated query, got %d calls", calls)
	}
	if !reflect.DeepEqual(v1, v2) {
		t.Fatalf("expected the cached vector to be identical across calls, got %v and %v", v1, v2)
	}
}

// TestEmbedCachedEvictsLeastRecentlyUsedAtCapacity fills a capacity-2
// cache with "a" and "b", re-touches "a" (making "b" the least recently
// used entry), then inserts "c". "b" must be evicted and re-embedded on
// its next lookup; "a" must survive and stay cached.
func TestEmbedCachedEvictsLeastRecentlyUsedAtCapacity(t *testing.T) {
	fe := &countingFakeEmbedder{}
	cache := newQueryCache(2)
	ctx := context.Background()

	mustEmbed := func(query string) []float32 {
		t.Helper()
		v, err := embedCached(ctx, cache, fe, query)
		if err != nil {
			t.Fatalf("embedCached(%q): unexpected error: %v", query, err)
		}
		return v
	}

	mustEmbed("a") // miss: calls=1
	mustEmbed("b") // miss: calls=2                     order: [b, a] (b most recent)
	mustEmbed("a") // hit:  calls=2, "a" now most recent order: [a, b]
	if calls := fe.calls.Load(); calls != 2 {
		t.Fatalf("expected 2 calls after warming a,b and re-touching a, got %d", calls)
	}

	mustEmbed("c") // miss: calls=3, over capacity evicts the LRU entry, "b"
	if calls := fe.calls.Load(); calls != 3 {
		t.Fatalf("expected 3 calls after inserting c, got %d", calls)
	}

	// "a" was touched more recently than "b" (step 3) and must have
	// survived c's eviction: looking it up now must still be a hit,
	// with no further call to the embedder.
	mustEmbed("a")
	if calls := fe.calls.Load(); calls != 3 {
		t.Fatalf("expected a to have survived b's eviction (still 3 calls total), got %d", calls)
	}

	// "b" was the entry evicted to make room for "c": looking it up
	// now must miss and re-embed.
	mustEmbed("b")
	if calls := fe.calls.Load(); calls != 4 {
		t.Fatalf("expected b's eviction to force a re-embed (4 calls total), got %d", calls)
	}
}

// TestEmbedCachedConcurrentAccessIsRaceFree drives many goroutines through
// a small, shared cache with an overlapping set of query strings. It makes
// no assertion about call counts (concurrent misses on the same key are
// expected and harmless); it exists to be run under `go test -race`, where
// an unsynchronised cache would be reported as a data race regardless of
// whether any single run happens to produce a wrong-looking count.
func TestEmbedCachedConcurrentAccessIsRaceFree(t *testing.T) {
	fe := &countingFakeEmbedder{}
	cache := newQueryCache(16)
	ctx := context.Background()

	queries := []string{"alpha", "bravo", "charlie", "delta", "echo", "foxtrot", "golf", "hotel"}

	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			q := queries[i%len(queries)]
			if _, err := embedCached(ctx, cache, fe, q); err != nil {
				t.Errorf("embedCached(%q): unexpected error: %v", q, err)
			}
		}(i)
	}
	wg.Wait()
}
