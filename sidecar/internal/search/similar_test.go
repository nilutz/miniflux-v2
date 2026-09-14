// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package search // import "miniflux.app/v2/sidecar/internal/search"

import (
	"context"
	"database/sql"
	"testing"

	"miniflux.app/v2/sidecar/internal/store"
)

// secondAnchorEmbedding returns a second, distinct real indexed
// embedding from the opposite end of the corpus (highest id) so a test
// can construct two genuinely separated real "topics" that are both
// graph-dense HNSW nodes -- see anchorEmbedding's own doc comment
// (semantic_test.go) for why an arbitrary, out-of-distribution vector
// doesn't reliably work as an ANN query against this corpus.
func secondAnchorEmbedding(t *testing.T, db *sql.DB) []float32 {
	t.Helper()
	var text string
	if err := db.QueryRow(`SELECT embedding::text FROM search.passages ORDER BY id DESC LIMIT 1`).Scan(&text); err != nil {
		t.Fatalf("unable to fetch a second anchor embedding: %v", err)
	}
	return normalizeVector(parseVectorLiteral(t, text))
}

// hasEntryHit reports whether hits contains an EntryHit for entryID.
func hasEntryHit(hits []EntryHit, entryID int64) bool {
	for _, h := range hits {
		if h.EntryID == entryID {
			return true
		}
	}
	return false
}

// TestSimilarExcludesSeedEntryFromItsOwnResults is the requirement the
// spec calls out by name (§7): a seed entry's own passages must never
// appear as their own "similar" result. fxA's only passage sits exactly
// on the query direction (distance 0 to itself) -- the strongest
// possible case for it to wrongly appear first if self-exclusion were
// ever missing or accidentally scoped wrong -- while fxB sits a small,
// genuine distance away and must still be found.
func TestSimilarExcludesSeedEntryFromItsOwnResults(t *testing.T) {
	s := testStore(t)
	db := testDB(t)
	anchor := anchorEmbedding(t, db)
	orth := orthogonalTo(anchor)
	searcher := NewSearcher(s)

	fxA := createFixtureEntry(t, db, "similar-self-a", "Zzyzx Similar Self A", "<p>seed</p>")
	writePassages(t, s, fxA.EntryID, []store.PassageRow{
		{Ordinal: 0, Text: "zzyzx similar self seed passage", CharStart: 0, CharEnd: 10, Source: "content", Embedding: anchor},
	})

	fxB := createFixtureEntry(t, db, "similar-self-b", "Zzyzx Similar Self B", "<p>neighbour</p>")
	writePassages(t, s, fxB.EntryID, []store.PassageRow{
		{Ordinal: 0, Text: "zzyzx similar self neighbour passage", CharStart: 0, CharEnd: 10, Source: "content", Embedding: atDistance(anchor, orth, 0.05)},
	})

	hits, err := searcher.Similar(context.Background(), fxA.EntryID, 10, Filters{FeedIDs: []int64{fxA.FeedID, fxB.FeedID}})
	if err != nil {
		t.Fatalf("Similar: unexpected error: %v", err)
	}
	if hasEntryHit(hits, fxA.EntryID) {
		t.Fatalf("expected the seed entry #%d never to appear in its own results, got %+v", fxA.EntryID, hits)
	}
	if !hasEntryHit(hits, fxB.EntryID) {
		t.Fatalf("expected entry #%d (a genuine near neighbour) among results, got %+v", fxB.EntryID, hits)
	}
}

// TestSimilarUsesEveryPassageAsASeed builds a seed entry covering two
// well-separated real topics (anchor1, anchor2 -- both already-indexed
// embeddings, so ANN search behaves normally for either) across its two
// passages, and one small, unrelated entry near each topic. If Similar
// only used one of the seed entry's passages (or blurred them into a
// single average, which for two far-apart directions would land far
// from both), one of the two neighbours would be missed. Finding both
// confirms every passage contributed as its own seed.
func TestSimilarUsesEveryPassageAsASeed(t *testing.T) {
	s := testStore(t)
	db := testDB(t)
	anchor1 := anchorEmbedding(t, db)
	anchor2 := secondAnchorEmbedding(t, db)
	orth1 := orthogonalTo(anchor1)
	orth2 := orthogonalTo(anchor2)
	searcher := NewSearcher(s)

	fxSeed := createFixtureEntry(t, db, "similar-multi-seed", "Zzyzx Similar Multi Seed", "<p>two topics</p>")
	writePassages(t, s, fxSeed.EntryID, []store.PassageRow{
		{Ordinal: 0, Text: "zzyzx similar multi topic one passage", CharStart: 0, CharEnd: 10, Source: "content", Embedding: anchor1},
		{Ordinal: 1, Text: "zzyzx similar multi topic two passage", CharStart: 10, CharEnd: 20, Source: "content", Embedding: anchor2},
	})

	fxNearTopicOne := createFixtureEntry(t, db, "similar-multi-near1", "Zzyzx Similar Multi Near Topic One", "<p>near topic one</p>")
	writePassages(t, s, fxNearTopicOne.EntryID, []store.PassageRow{
		{Ordinal: 0, Text: "zzyzx similar multi near topic one passage", CharStart: 0, CharEnd: 10, Source: "content", Embedding: atDistance(anchor1, orth1, 0.02)},
	})

	fxNearTopicTwo := createFixtureEntry(t, db, "similar-multi-near2", "Zzyzx Similar Multi Near Topic Two", "<p>near topic two</p>")
	writePassages(t, s, fxNearTopicTwo.EntryID, []store.PassageRow{
		{Ordinal: 0, Text: "zzyzx similar multi near topic two passage", CharStart: 0, CharEnd: 10, Source: "content", Embedding: atDistance(anchor2, orth2, 0.02)},
	})

	hits, err := searcher.Similar(context.Background(), fxSeed.EntryID, 10, Filters{FeedIDs: []int64{fxSeed.FeedID, fxNearTopicOne.FeedID, fxNearTopicTwo.FeedID}})
	if err != nil {
		t.Fatalf("Similar: unexpected error: %v", err)
	}
	if hasEntryHit(hits, fxSeed.EntryID) {
		t.Fatalf("expected the seed entry #%d never to appear in its own results, got %+v", fxSeed.EntryID, hits)
	}
	if !hasEntryHit(hits, fxNearTopicOne.EntryID) {
		t.Fatalf("expected the entry near topic one (seeded by ordinal 0) among results, got %+v", hits)
	}
	if !hasEntryHit(hits, fxNearTopicTwo.EntryID) {
		t.Fatalf("expected the entry near topic two (seeded by ordinal 1) among results, got %+v", hits)
	}
}

// TestSimilarEntryWithNoPassagesReturnsEmpty covers an entry that exists
// in public.entries but has never been indexed (or whose indexing
// failed before writing any passages): Similar must return an empty
// result, not an error, and must not panic on an empty seed set.
func TestSimilarEntryWithNoPassagesReturnsEmpty(t *testing.T) {
	s := testStore(t)
	db := testDB(t)
	searcher := NewSearcher(s)

	fx := createFixtureEntry(t, db, "similar-no-passages", "Zzyzx Similar No Passages", "<p>never indexed</p>")

	hits, err := searcher.Similar(context.Background(), fx.EntryID, 10, Filters{})
	if err != nil {
		t.Fatalf("Similar: expected no error for an entry with no passages, got: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("expected no hits for an entry with no passages, got %+v", hits)
	}
}

// TestSimilarRanksByAggregateSimilarity writes three unrelated entries
// at increasing, exact cosine distances (0.02, 0.08, 0.20) from a
// single-passage seed entry, and checks both that they come back in
// that order and that EntryHit.Score (cosine distance -- smaller is
// better, same convention as Semantic) is non-decreasing across the
// returned list.
func TestSimilarRanksByAggregateSimilarity(t *testing.T) {
	s := testStore(t)
	db := testDB(t)
	anchor := anchorEmbedding(t, db)
	orth := orthogonalTo(anchor)
	searcher := NewSearcher(s)

	fxSeed := createFixtureEntry(t, db, "similar-rank-seed", "Zzyzx Similar Rank Seed", "<p>seed</p>")
	writePassages(t, s, fxSeed.EntryID, []store.PassageRow{
		{Ordinal: 0, Text: "zzyzx similar rank seed passage", CharStart: 0, CharEnd: 10, Source: "content", Embedding: anchor},
	})

	fx0 := createFixtureEntry(t, db, "similar-rank-0", "Zzyzx Similar Rank Zero", "<p>closest</p>")
	writePassages(t, s, fx0.EntryID, []store.PassageRow{
		{Ordinal: 0, Text: "zzyzx similar rank passage zero", CharStart: 0, CharEnd: 10, Source: "content", Embedding: atDistance(anchor, orth, 0.02)},
	})

	fx1 := createFixtureEntry(t, db, "similar-rank-1", "Zzyzx Similar Rank One", "<p>middle</p>")
	writePassages(t, s, fx1.EntryID, []store.PassageRow{
		{Ordinal: 0, Text: "zzyzx similar rank passage one", CharStart: 0, CharEnd: 10, Source: "content", Embedding: atDistance(anchor, orth, 0.08)},
	})

	fx2 := createFixtureEntry(t, db, "similar-rank-2", "Zzyzx Similar Rank Two", "<p>farthest</p>")
	writePassages(t, s, fx2.EntryID, []store.PassageRow{
		{Ordinal: 0, Text: "zzyzx similar rank passage two", CharStart: 0, CharEnd: 10, Source: "content", Embedding: atDistance(anchor, orth, 0.20)},
	})

	hits, err := searcher.Similar(context.Background(), fxSeed.EntryID, 10, Filters{FeedIDs: []int64{fx0.FeedID, fx1.FeedID, fx2.FeedID}})
	if err != nil {
		t.Fatalf("Similar: unexpected error: %v", err)
	}
	if len(hits) != 3 {
		t.Fatalf("expected exactly the 3 fixture entries once filtered to their own feeds, got %+v", hits)
	}

	wantOrder := []int64{fx0.EntryID, fx1.EntryID, fx2.EntryID}
	for i, h := range hits {
		if h.EntryID != wantOrder[i] {
			t.Fatalf("expected entries ranked closest-first %v, got order %+v", wantOrder, hits)
		}
	}
	for i := 0; i < len(hits)-1; i++ {
		if hits[i].Score > hits[i+1].Score {
			t.Fatalf("expected non-decreasing distance across ranked hits, got hits[%d].Score=%v > hits[%d].Score=%v", i, hits[i].Score, i+1, hits[i+1].Score)
		}
	}
}

// TestSimilarFiltersNarrowResultsByFeed mirrors Semantic's own feed-
// filter test (semantic_test.go): two candidate entries in two
// different feeds, both equally close to the seed, and a filter to one
// feed must exclude the other even though it is an equally good match.
func TestSimilarFiltersNarrowResultsByFeed(t *testing.T) {
	s := testStore(t)
	db := testDB(t)
	anchor := anchorEmbedding(t, db)
	searcher := NewSearcher(s)

	fxSeed := createFixtureEntry(t, db, "similar-filter-seed", "Zzyzx Similar Filter Seed", "<p>seed</p>")
	writePassages(t, s, fxSeed.EntryID, []store.PassageRow{
		{Ordinal: 0, Text: "zzyzx similar filter seed passage", CharStart: 0, CharEnd: 10, Source: "content", Embedding: anchor},
	})

	fxA := createFixtureEntry(t, db, "similar-filter-a", "Zzyzx Similar Filter Feed A", "<p>feed a</p>")
	writePassages(t, s, fxA.EntryID, []store.PassageRow{
		{Ordinal: 0, Text: "zzyzx similar filter feed a passage", CharStart: 0, CharEnd: 10, Source: "content", Embedding: anchor},
	})

	fxB := createFixtureEntry(t, db, "similar-filter-b", "Zzyzx Similar Filter Feed B", "<p>feed b</p>")
	writePassages(t, s, fxB.EntryID, []store.PassageRow{
		{Ordinal: 0, Text: "zzyzx similar filter feed b passage", CharStart: 0, CharEnd: 10, Source: "content", Embedding: anchor},
	})

	unfiltered, err := searcher.Similar(context.Background(), fxSeed.EntryID, 10, Filters{})
	if err != nil {
		t.Fatalf("Similar: unexpected error: %v", err)
	}
	if !hasEntryHit(unfiltered, fxA.EntryID) || !hasEntryHit(unfiltered, fxB.EntryID) {
		t.Fatalf("expected both fixtures unfiltered, got %+v", unfiltered)
	}

	filtered, err := searcher.Similar(context.Background(), fxSeed.EntryID, 10, Filters{FeedIDs: []int64{fxA.FeedID}})
	if err != nil {
		t.Fatalf("Similar: unexpected error: %v", err)
	}
	if !hasEntryHit(filtered, fxA.EntryID) {
		t.Fatalf("expected feed A's entry to survive the feed filter, got %+v", filtered)
	}
	if hasEntryHit(filtered, fxB.EntryID) {
		t.Fatalf("expected feed B's entry to be excluded by the feed filter, got %+v", filtered)
	}
}

// TestSelectSeedIndicesCoversEveryPassageUnderTheCap checks the trivial
// n<=maxSeeds branch: no subsampling should happen at all.
func TestSelectSeedIndicesCoversEveryPassageUnderTheCap(t *testing.T) {
	got := selectSeedIndices(5, 32)
	want := []int{0, 1, 2, 3, 4}
	if len(got) != len(want) {
		t.Fatalf("expected %v, got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("expected %v, got %v", want, got)
		}
	}
}

// TestSelectSeedIndicesSpreadsEvenlyOverTheCap checks the subsampling
// branch: exactly maxSeeds indices, strictly increasing, spanning close
// to the full [0,n) range rather than clustering near the start (which
// would bias a capped long entry's seeds toward its own introduction).
func TestSelectSeedIndicesSpreadsEvenlyOverTheCap(t *testing.T) {
	const n = 334
	const maxSeeds = 32

	got := selectSeedIndices(n, maxSeeds)
	if len(got) != maxSeeds {
		t.Fatalf("expected exactly %d indices, got %d: %v", maxSeeds, len(got), got)
	}
	for i, idx := range got {
		if idx < 0 || idx >= n {
			t.Fatalf("index %d out of range [0,%d): %v", idx, n, got)
		}
		if i > 0 && idx <= got[i-1] {
			t.Fatalf("expected strictly increasing indices, got %v", got)
		}
	}
	if last := got[len(got)-1]; last < n-1-(n/maxSeeds) {
		t.Fatalf("expected the last seed index to reach within one stride of the end of the range (n=%d), got %d: %v", n, last, got)
	}
}
