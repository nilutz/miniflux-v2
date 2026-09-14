// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package search // import "miniflux.app/v2/sidecar/internal/search"

import (
	"container/list"
	"context"
	"fmt"
	"sync"

	"miniflux.app/v2/sidecar/internal/embed"
)

// defaultQueryCacheSize is how many distinct query strings queryCache
// keeps embeddings for before it starts evicting. Spec §6.5 calls the
// query path "single-digit milliseconds on CPU, with a small LRU cache
// for repeated queries" without pinning a number; 128 is small enough
// that even the worst case (128 full-width float32 vectors) is a
// trivially small, fixed amount of memory, and generous enough to cover
// a burst of repeated or refined searches from one user session.
const defaultQueryCacheSize = 128

// queryCache is a fixed-capacity, least-recently-used cache from query
// text to its embedding. It exists so that re-running or paginating the
// same search — or two different callers searching for the same text —
// costs one embedding forward pass instead of one per call.
//
// Its zero value is not usable; construct one with newQueryCache. A
// queryCache is safe for concurrent use: Searcher is documented as safe
// to share across concurrent requests, and this is the only mutable
// state it holds.
type queryCache struct {
	mu       sync.Mutex
	capacity int
	order    *list.List // front = most recently used, back = least
	entries  map[string]*list.Element
}

// queryCacheEntry is the value stored in queryCache.order; the element
// also lives in queryCache.entries under entry.query, so eviction (which
// only has a *list.Element from the back of order) can find the map key
// to delete without a second lookup.
type queryCacheEntry struct {
	query string
	vec   []float32
}

// newQueryCache builds an empty cache holding up to capacity entries.
// capacity must be positive; callers in this package only ever pass
// defaultQueryCacheSize or a small fixed test value.
func newQueryCache(capacity int) *queryCache {
	return &queryCache{
		capacity: capacity,
		order:    list.New(),
		entries:  make(map[string]*list.Element, capacity),
	}
}

// get returns query's cached embedding, if present, marking it the most
// recently used entry so it survives future evictions the longest.
func (c *queryCache) get(query string) ([]float32, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	el, ok := c.entries[query]
	if !ok {
		return nil, false
	}
	c.order.MoveToFront(el)
	return el.Value.(*queryCacheEntry).vec, true
}

// put records vec as query's embedding, evicting the least-recently-used
// entry first if the cache is already at capacity. Re-inserting a query
// already present refreshes its value and its recency without growing
// the cache.
func (c *queryCache) put(query string, vec []float32) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if el, ok := c.entries[query]; ok {
		el.Value.(*queryCacheEntry).vec = vec
		c.order.MoveToFront(el)
		return
	}

	el := c.order.PushFront(&queryCacheEntry{query: query, vec: vec})
	c.entries[query] = el

	if c.order.Len() > c.capacity {
		oldest := c.order.Back()
		c.order.Remove(oldest)
		delete(c.entries, oldest.Value.(*queryCacheEntry).query)
	}
}

// embedCached returns query's embedding, consulting cache first and
// calling embedder.Embed only on a miss — the seam this package's
// querycache_test.go exercises hermetically with a counting fake
// Embedder, and the one Searcher.embedQuery uses for real Semantic
// retrieval.
//
// An error from embedder.Embed is returned as-is, uncached: a transient
// failure must not poison the cache with a missing entry that looks
// identical to "never asked", and must surface to the caller rather than
// silently falling back to no results (see Semantic's own doc comment).
func embedCached(ctx context.Context, cache *queryCache, embedder embed.Embedder, query string) ([]float32, error) {
	if vec, ok := cache.get(query); ok {
		return vec, nil
	}

	vecs, err := embedder.Embed(ctx, []string{query})
	if err != nil {
		return nil, err
	}
	if len(vecs) != 1 {
		return nil, fmt.Errorf("search: embedder returned %d vectors for 1 input query", len(vecs))
	}

	cache.put(query, vecs[0])
	return vecs[0], nil
}
