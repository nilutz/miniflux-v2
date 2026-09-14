// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package search // import "miniflux.app/v2/sidecar/internal/search"

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"miniflux.app/v2/sidecar/internal/store"
)

// --- fakes shared by this file's tests -------------------------------------

// normalizeVector L2-normalises v. Real embeddings in this corpus are all
// L2-normalised (confirmed against the live database before writing this
// file: every row's <#> with itself is ~-1) — cosine distance is only the
// well-behaved, bounded [0, 2] geometry <=> assumes when every vector
// already has unit length, so every fixture vector this file builds goes
// through this before being written or used as a query embedding.
func normalizeVector(v []float32) []float32 {
	var sumSq float64
	for _, x := range v {
		sumSq += float64(x) * float64(x)
	}
	norm := float32(math.Sqrt(sumSq))
	out := make([]float32, len(v))
	for i, x := range v {
		out[i] = x / norm
	}
	return out
}

// orthogonalTo returns a unit vector orthogonal to base (assumed already
// unit length), via Gram-Schmidt against the standard basis vector e0 —
// or e1, on the astronomically unlikely chance base already lies almost
// entirely along e0.
func orthogonalTo(base []float32) []float32 {
	e := make([]float32, len(base))
	idx := 0
	if math.Abs(float64(base[0])) > 0.9 {
		idx = 1
	}
	e[idx] = 1

	var dot float64
	for i := range base {
		dot += float64(e[i]) * float64(base[i])
	}
	for i := range e {
		e[i] -= float32(dot) * base[i]
	}
	return normalizeVector(e)
}

// atDistance returns the unit vector cos(theta)*base + sin(theta)*orth,
// where theta is chosen so that its cosine distance to base is exactly
// distance. base and orth must already be orthonormal (orth =
// orthogonalTo(base)); the result is then unit length by construction,
// with an exact, checkable-by-hand cosine distance from base — the same
// reasoning as the package's own <=> operator, just run in reverse to
// build a fixture instead of to score one.
func atDistance(base, orth []float32, distance float64) []float32 {
	cosTheta := 1 - distance
	sinTheta := math.Sqrt(math.Max(0, 1-cosTheta*cosTheta))
	out := make([]float32, len(base))
	for i := range base {
		out[i] = float32(cosTheta)*base[i] + float32(sinTheta)*orth[i]
	}
	return out
}

// parseVectorLiteral parses pgvector's plain text vector format
// ("[0.1,0.2,...]", what embedding::text below returns) into a
// []float32 — the inverse of store's own vectorLiteral.
func parseVectorLiteral(t *testing.T, s string) []float32 {
	t.Helper()
	s = strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(s), "["), "]")
	parts := strings.Split(s, ",")
	out := make([]float32, len(parts))
	for i, p := range parts {
		f, err := strconv.ParseFloat(strings.TrimSpace(p), 32)
		if err != nil {
			t.Fatalf("unable to parse vector literal component %q: %v", p, err)
		}
		out[i] = float32(f)
	}
	return out
}

// anchorEmbedding returns an actual, already-indexed passage's real
// embedding from this dev database's corpus, to use as the base direction
// every fixture in this file's database-backed tests is built relative
// to.
//
// Two false starts led here, both worth recording:
//
//  1. Picking an arbitrary axis-aligned vector (e.g. all weight on
//     dimension 0) as "the query" does not work: real sentence embeddings
//     are anisotropic (they cluster around a shared dominant direction
//     rather than spreading uniformly over the unit sphere — measured
//     directly against this corpus, avg(embedding) has norm ~0.70 out of
//     a possible 1.0), so an arbitrary axis is close to unrelated real
//     passages more often than a hand-picked "45 degrees away" or
//     "orthogonal" fixture is, silently invalidating an ordering
//     assertion.
//  2. Deliberately picking the *opposite* of that dominant direction
//     (-mean) to dodge problem 1 above instead breaks pgvector's HNSW
//     index in a different way: an approximate-nearest-neighbour search
//     starting from a query that lies in a region of the vector space
//     with no nearby indexed points can fail to find *any* candidates at
//     the index's default search width, confirmed directly against this
//     corpus and this exact index (passages_embedding_idx) with EXPLAIN
//     ANALYZE — a plain, unfiltered `ORDER BY embedding <=> $1 LIMIT 10`
//     against a -mean query returned zero rows outright, and returned the
//     expected nearest neighbours again only once hnsw.ef_search was
//     raised well above its default.
//
// A real, already-indexed embedding has neither problem: it is exactly
// where the HNSW graph expects a query to be (it's a graph node), so
// approximate search behaves normally, and every fixture built as a small
// rotation away from it (via orthogonalTo + atDistance) can be given an
// exact, known distance that this file picks with a comfortable margin
// below the corpus's own measured nearest-neighbour distances at anchor
// (0.125 to the anchor's own next-closest real passage, 0.354 to the
// closest *other* real entry) — see each test's own comment for its
// chosen margins.
func anchorEmbedding(t *testing.T, db *sql.DB) []float32 {
	t.Helper()
	var text string
	if err := db.QueryRow(`SELECT embedding::text FROM search.passages ORDER BY id LIMIT 1`).Scan(&text); err != nil {
		t.Fatalf("unable to fetch an anchor embedding: %v", err)
	}
	return normalizeVector(parseVectorLiteral(t, text))
}

// fixedVectorEmbedder maps specific, known query strings to specific,
// known vectors, so a test can pick the exact query embedding used in the
// SQL below and reason about the resulting cosine distances by hand. Any
// text without a fixture entry is a test-author mistake, not a runtime
// condition to handle gracefully, so it errors loudly rather than
// returning a zero vector that would silently match nothing.
type fixedVectorEmbedder struct {
	vectors map[string][]float32
}

func (f *fixedVectorEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v, ok := f.vectors[t]
		if !ok {
			return nil, errors.New("fixedVectorEmbedder: no fixture vector for " + t)
		}
		out[i] = v
	}
	return out, nil
}

func (f *fixedVectorEmbedder) Dimensions() int { return 384 }
func (f *fixedVectorEmbedder) Close() error    { return nil }

// erroringEmbedder always fails, simulating an inference backend that is
// down or misconfigured.
type erroringEmbedder struct{ err error }

func (e *erroringEmbedder) Embed(_ context.Context, _ []string) ([][]float32, error) {
	return nil, e.err
}
func (e *erroringEmbedder) Dimensions() int { return 384 }
func (e *erroringEmbedder) Close() error    { return nil }

// countingReader is a store.Reader whose QueryContext/QueryRowContext
// count how many times they were invoked and then fail, so a test can
// assert the database was never reached — e.g. an empty query, or an
// embedding failure, must short-circuit before any SQL runs.
type countingReader struct {
	queryCalls atomic.Int64
}

func (c *countingReader) QueryContext(_ context.Context, _ string, _ ...any) (*sql.Rows, error) {
	c.queryCalls.Add(1)
	return nil, errors.New("countingReader: QueryContext must not be called in this test")
}

func (c *countingReader) QueryRowContext(_ context.Context, _ string, _ ...any) *sql.Row {
	c.queryCalls.Add(1)
	return nil
}

// --- hermetic tests: no database, no SIDECAR_DATABASE_URL required --------

// TestSemanticEmptyQueryDoesNotReachDatabase mirrors Lexical's own
// contract for a blank query: nothing for the database or the embedder to
// usefully do with it, so neither must be touched. Both dependencies here
// are wired to fail loudly if invoked, proving the short-circuit rather
// than merely asserting an empty result (which an over-eager query that
// happens to match nothing would also produce).
func TestSemanticEmptyQueryDoesNotReachDatabase(t *testing.T) {
	reader := &countingReader{}
	embedder := &erroringEmbedder{err: errors.New("embedder must not be called for an empty query")}
	s := &Searcher{db: reader, embedder: embedder, cache: newQueryCache(defaultQueryCacheSize)}

	hits, err := s.Semantic(context.Background(), "   ", 10, Filters{})
	if err != nil {
		t.Fatalf("Semantic: expected no error for an empty query, got: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("Semantic: expected no hits for an empty query, got %+v", hits)
	}
	if calls := reader.queryCalls.Load(); calls != 0 {
		t.Fatalf("Semantic: expected the database never to be queried for an empty query, got %d calls", calls)
	}
}

// TestSemanticEmbeddingFailureSurfacesAsError is the bug class the task
// brief calls out by name: a broken embedder returning nothing would look
// exactly like "no matches" unless the failure is surfaced as an error.
func TestSemanticEmbeddingFailureSurfacesAsError(t *testing.T) {
	reader := &countingReader{}
	wantErr := errors.New("embedder is down")
	s := &Searcher{db: reader, embedder: &erroringEmbedder{err: wantErr}, cache: newQueryCache(defaultQueryCacheSize)}

	hits, err := s.Semantic(context.Background(), "a real query", 10, Filters{})
	if err == nil {
		t.Fatal("Semantic: expected an error when the embedder fails, got nil")
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("Semantic: expected the error to wrap the embedder's own error, got: %v", err)
	}
	if hits != nil {
		t.Fatalf("Semantic: expected nil hits alongside the error, got %+v", hits)
	}
	if calls := reader.queryCalls.Load(); calls != 0 {
		t.Fatalf("Semantic: expected the database never to be queried after an embedding failure, got %d calls", calls)
	}
}

// TestSemanticWithoutAnEmbedderConfiguredFailsClearly checks the other
// half of the same defensiveness: a Searcher built via plain NewSearcher(s)
// (no WithEmbedder) must refuse Semantic with a clear error, not a
// nil-pointer panic.
func TestSemanticWithoutAnEmbedderConfiguredFailsClearly(t *testing.T) {
	reader := &countingReader{}
	s := &Searcher{db: reader}

	_, err := s.Semantic(context.Background(), "a real query", 10, Filters{})
	if err == nil {
		t.Fatal("Semantic: expected an error when no embedder is configured, got nil")
	}
}

// --- database-backed tests: skip without SIDECAR_DATABASE_URL -------------
//
// These reuse testStore/testDB/createFixtureEntry/writePassages from
// lexical_test.go: same fixture conventions, same "zzyzx" vocabulary
// isolation, same cleanup discipline. Only the embedder differs — a
// fixedVectorEmbedder standing in for the real ONNX model, mapping this
// file's query text to a hand-picked unit vector so cosine distance
// against each fixture passage's own (directly written) embedding is
// exact, checkable arithmetic.

// semanticQuery is the one query string every database-backed test in
// this file embeds; each test maps it to its own anchorEmbedding(t, db),
// fetched fresh against whatever is in search.passages at the time.
const semanticQuery = "zzyzx-semantic-fixture-query"

func semanticEmbedder(queryVec []float32) *fixedVectorEmbedder {
	return &fixedVectorEmbedder{vectors: map[string][]float32{
		semanticQuery: queryVec,
	}}
}

// TestSemanticOrdersByCosineDistanceAscendingWithConsistentRank writes
// three passages whose embeddings are, by construction, at increasing,
// exact cosine distances from the query vector (anchorEmbedding(t, db) —
// see that helper's doc comment for the two approaches that don't work
// against this real, 5,681-passage corpus): 0, ~0.05, and ~0.25. Both
// nonzero distances stay comfortably under the anchor's own measured
// nearest-real-neighbour distances (0.125 to its closest real sibling
// passage, 0.354 to the closest passage belonging to a different real
// entry), so all three fixtures rank ahead of every unrelated real
// passage regardless of what the corpus's actual content is.
//
// The query is additionally filtered to this fixture's own feed. Without
// that, the query's own anchor row — a real passage already in the
// corpus, at cosine distance exactly 0 from itself — would sit in the
// results too, which is fine for presence checks but pollutes an exact
// "hits[i].Rank == i+1 for these 3 fixtures precisely" assertion; scoping
// to the fixture's feed keeps this test about Semantic's ordering, not
// about how many other passages tie at distance 0.
func TestSemanticOrdersByCosineDistanceAscendingWithConsistentRank(t *testing.T) {
	s := testStore(t)
	db := testDB(t)
	anchor := anchorEmbedding(t, db)
	orth := orthogonalTo(anchor)
	searcher := NewSearcher(s, WithEmbedder(semanticEmbedder(anchor)))

	fx := createFixtureEntry(t, db, "semantic-order", "Zzyzx Semantic Order Fixture", "<p>irrelevant</p>")
	if err := s.ReplacePassages(fx.EntryID, "hash-semantic-order", []store.PassageRow{
		{Ordinal: 0, Text: "zzyzx passage identical to the query direction", CharStart: 0, CharEnd: 10, Source: "content", Embedding: anchor},
		{Ordinal: 1, Text: "zzyzx passage a little further from the query", CharStart: 10, CharEnd: 20, Source: "content", Embedding: atDistance(anchor, orth, 0.05)},
		{Ordinal: 2, Text: "zzyzx passage the furthest from the query", CharStart: 20, CharEnd: 30, Source: "content", Embedding: atDistance(anchor, orth, 0.25)},
	}); err != nil {
		t.Fatalf("unable to write fixture passages: %v", err)
	}

	hits, err := searcher.Semantic(context.Background(), semanticQuery, 10, Filters{FeedIDs: []int64{fx.FeedID}})
	if err != nil {
		t.Fatalf("Semantic: unexpected error: %v", err)
	}
	if len(hits) != 3 {
		t.Fatalf("expected exactly the 3 fixture passages once filtered to their own feed, got %+v", hits)
	}

	for i := 0; i < len(hits)-1; i++ {
		if hits[i].Score > hits[i+1].Score {
			t.Fatalf("expected non-decreasing cosine distance order, got hits[%d].Score=%v > hits[%d].Score=%v", i, hits[i].Score, i+1, hits[i+1].Score)
		}
	}

	wantOrdinalOrder := []int{0, 1, 2}
	for i, h := range hits {
		if h.Ordinal != wantOrdinalOrder[i] {
			t.Fatalf("expected fixture ordinal order %v by ascending distance, got hits=%+v", wantOrdinalOrder, hits)
		}
		if h.Rank != i+1 {
			t.Fatalf("expected hits[%d].Rank == %d, got %d", i, i+1, h.Rank)
		}
	}

	if math.Abs(hits[0].Score-0) > 1e-4 {
		t.Fatalf("expected ordinal 0's distance to be ~0 (identical direction), got %v", hits[0].Score)
	}
	if math.Abs(hits[1].Score-0.05) > 1e-3 {
		t.Fatalf("expected ordinal 1's distance to be ~0.05, got %v", hits[1].Score)
	}
	if math.Abs(hits[2].Score-0.25) > 1e-3 {
		t.Fatalf("expected ordinal 2's distance to be ~0.25, got %v", hits[2].Score)
	}
}

// TestSemanticHonoursLimit writes four fixture passages at increasing,
// exact cosine distances from the query (0, ~0.02, ~0.1, ~0.28 — again
// anchored on anchorEmbedding, and again scoped to this fixture's own
// feed so the query's own anchor row doesn't occupy one of the 2 slots a
// limit of 2 allows) and checks that limit=2 returns exactly the two
// closest, not merely "2 of the 4".
func TestSemanticHonoursLimit(t *testing.T) {
	s := testStore(t)
	db := testDB(t)
	anchor := anchorEmbedding(t, db)
	orth := orthogonalTo(anchor)
	searcher := NewSearcher(s, WithEmbedder(semanticEmbedder(anchor)))

	fx := createFixtureEntry(t, db, "semantic-limit", "Zzyzx Semantic Limit Fixture", "<p>irrelevant</p>")
	if err := s.ReplacePassages(fx.EntryID, "hash-semantic-limit", []store.PassageRow{
		{Ordinal: 0, Text: "zzyzx limit passage zero", CharStart: 0, CharEnd: 10, Source: "content", Embedding: anchor},
		{Ordinal: 1, Text: "zzyzx limit passage one", CharStart: 10, CharEnd: 20, Source: "content", Embedding: atDistance(anchor, orth, 0.02)},
		{Ordinal: 2, Text: "zzyzx limit passage two", CharStart: 20, CharEnd: 30, Source: "content", Embedding: atDistance(anchor, orth, 0.10)},
		{Ordinal: 3, Text: "zzyzx limit passage three", CharStart: 30, CharEnd: 40, Source: "content", Embedding: atDistance(anchor, orth, 0.28)},
	}); err != nil {
		t.Fatalf("unable to write fixture passages: %v", err)
	}

	hits, err := searcher.Semantic(context.Background(), semanticQuery, 2, Filters{FeedIDs: []int64{fx.FeedID}})
	if err != nil {
		t.Fatalf("Semantic: unexpected error: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("expected exactly 2 hits with limit=2, got %d: %+v", len(hits), hits)
	}
	for i, h := range hits {
		if h.Rank != i+1 {
			t.Fatalf("expected hits[%d].Rank == %d, got %d", i, i+1, h.Rank)
		}
	}
	// The two closest fixture passages by construction are ordinals 0 and
	// 1; a short candidate cutoff must not let a farther passage crowd
	// them out.
	if hits[0].Ordinal != 0 || hits[1].Ordinal != 1 {
		t.Fatalf("expected limit=2 to return the two closest fixture passages in order (ordinals 0, 1), got hits=%+v", hits)
	}
}

// TestSemanticFiltersNarrowResultsByFeed mirrors Lexical's own feed-filter
// test: two entries in two different feeds, both with a passage embedded
// identically to the query, and a filter to one feed must exclude the
// other's entry even though both are equally close matches.
func TestSemanticFiltersNarrowResultsByFeed(t *testing.T) {
	s := testStore(t)
	db := testDB(t)
	anchor := anchorEmbedding(t, db)
	searcher := NewSearcher(s, WithEmbedder(semanticEmbedder(anchor)))

	fxA := createFixtureEntry(t, db, "semantic-filter-feed-a", "Zzyzx Semantic Filter Feed A", "<p>irrelevant</p>")
	if err := s.ReplacePassages(fxA.EntryID, "hash-semantic-filter-a", []store.PassageRow{
		{Ordinal: 0, Text: "zzyzx semantic filter feed a passage", CharStart: 0, CharEnd: 10, Source: "content", Embedding: anchor},
	}); err != nil {
		t.Fatalf("unable to write fixture A passages: %v", err)
	}

	fxB := createFixtureEntry(t, db, "semantic-filter-feed-b", "Zzyzx Semantic Filter Feed B", "<p>irrelevant</p>")
	if err := s.ReplacePassages(fxB.EntryID, "hash-semantic-filter-b", []store.PassageRow{
		{Ordinal: 0, Text: "zzyzx semantic filter feed b passage", CharStart: 0, CharEnd: 10, Source: "content", Embedding: anchor},
	}); err != nil {
		t.Fatalf("unable to write fixture B passages: %v", err)
	}

	unfiltered, err := searcher.Semantic(context.Background(), semanticQuery, 10, Filters{})
	if err != nil {
		t.Fatalf("Semantic: unexpected error: %v", err)
	}
	if !hasEntryID(unfiltered, fxA.EntryID) || !hasEntryID(unfiltered, fxB.EntryID) {
		t.Fatalf("expected both fixtures unfiltered, got %+v", unfiltered)
	}

	filtered, err := searcher.Semantic(context.Background(), semanticQuery, 10, Filters{FeedIDs: []int64{fxA.FeedID}})
	if err != nil {
		t.Fatalf("Semantic: unexpected error: %v", err)
	}
	if !hasEntryID(filtered, fxA.EntryID) {
		t.Fatalf("expected feed A's entry to survive the feed filter, got %+v", filtered)
	}
	if hasEntryID(filtered, fxB.EntryID) {
		t.Fatalf("expected feed B's entry to be excluded by the feed filter, got %+v", filtered)
	}
	for i, h := range filtered {
		if h.Rank != i+1 {
			t.Fatalf("expected filtered hits[%d].Rank == %d (contiguous after filtering), got %d", i, i+1, h.Rank)
		}
	}
}

// TestSemanticRaisesEfSearchForTheOverfetchWindow is a regression guard
// for a real bug found in review: pgvector's default hnsw.ef_search (40)
// silently truncates the HNSW index scan's result set once a query asks
// for more candidates than that — not an error, just quietly fewer rows
// than LIMIT asked for.
//
// A first fix attempt (a MATERIALIZED CTE calling set_config, joined
// into the candidates CTE so the planner couldn't inline it away) looked
// right but did not take effect: EXPLAIN ANALYZE against this exact query
// showed the planner was free to put that CTE on the *inner* side of a
// nested loop whose *outer* side was the very index scan it was meant to
// configure, so it only ran *after* the scan — once per outer row a
// nested loop had already produced, never before it. MATERIALIZED
// prevents inlining; it does not order execution.
//
// This test reproduces the bug's own repro directly: a real,
// already-indexed embedding as the query (anchorEmbedding — see its own
// doc comment for why an arbitrary or adversarial vector doesn't
// reproduce this cleanly), unfiltered, with limit=40 so the computed
// candidate count is candidateMultiplier(5)*40 = 200 — comfortably above
// the default ef_search of 40. The live corpus has 5,681 passages, far
// more than 200, so a correctly configured search has no reason to
// return fewer than the full limit; a scan whose ef_search elevation
// silently didn't take effect does.
func TestSemanticRaisesEfSearchForTheOverfetchWindow(t *testing.T) {
	s := testStore(t)
	db := testDB(t)
	anchor := anchorEmbedding(t, db)
	searcher := NewSearcher(s, WithEmbedder(semanticEmbedder(anchor)))

	const limit = 40 // candidateMultiplier(5) * 40 = 200 candidates requested, well above the default ef_search of 40

	hits, err := searcher.Semantic(context.Background(), semanticQuery, limit, Filters{})
	if err != nil {
		t.Fatalf("Semantic: unexpected error: %v", err)
	}
	if len(hits) != limit {
		t.Fatalf("expected exactly %d hits from a 5,681-passage corpus with hnsw.ef_search actually raised to cover the %d-candidate overfetch window, got %d — this is exactly the silent-truncation failure mode: ef_search defaults to 40, and a scan that never actually raised it returns far fewer rows than requested without erroring", limit, limit*candidateMultiplier, len(hits))
	}
}
