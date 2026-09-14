// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package search // import "miniflux.app/v2/sidecar/internal/search"

import (
	"context"
	"testing"

	"miniflux.app/v2/sidecar/internal/store"
)

// hasEntryIDInEntries mirrors lexical_test.go's hasEntryID (which checks
// []PassageHit) but for the entry-level results (*Searcher).Search
// actually returns.
func hasEntryIDInEntries(hits []EntryHit, entryID int64) bool {
	for _, h := range hits {
		if h.EntryID == entryID {
			return true
		}
	}
	return false
}

// TestSearchFiltersNarrowResultsThroughFullRequestPath exercises Filters
// through the whole public entry point, (*Searcher).Search(ctx,
// Request{...}) — not just Lexical/Semantic directly, which
// lexical_test.go and semantic_test.go already cover on their own. This
// is the "filters narrow results through the full request path"
// requirement: a Request carrying FeedIDs must reach Search's dispatch,
// Lexical underneath it, and public.entries' join, all the way to
// Response.Entries.
//
// ModeKeyword is used deliberately so this test needs no embedder and no
// ONNX runtime, matching this package's own "hermetic where possible"
// posture — it still requires SIDECAR_TEST_DATABASE_URL, like every other
// fixture-backed test in this package, and skips cleanly without it.
func TestSearchFiltersNarrowResultsThroughFullRequestPath(t *testing.T) {
	s := testStore(t)
	db := testDB(t)
	searcher := NewSearcher(s)

	fxA := createFixtureEntry(t, db, "reqpath-a", "Zzyzx Request Path A", "<p>irrelevant</p>")
	writePassages(t, s, fxA.EntryID, []store.PassageRow{
		{Ordinal: 0, Text: "zzyzxreqpathterm appears in feed a", CharStart: 0, CharEnd: 35, Source: "content", Embedding: zeroEmbedding()},
	})

	fxB := createFixtureEntry(t, db, "reqpath-b", "Zzyzx Request Path B", "<p>irrelevant</p>")
	writePassages(t, s, fxB.EntryID, []store.PassageRow{
		{Ordinal: 0, Text: "zzyzxreqpathterm appears in feed b", CharStart: 0, CharEnd: 35, Source: "content", Embedding: zeroEmbedding()},
	})

	unfiltered, err := searcher.Search(context.Background(), Request{
		Query: "zzyzxreqpathterm",
		Mode:  ModeKeyword,
	})
	if err != nil {
		t.Fatalf("Search: unexpected error: %v", err)
	}
	if !hasEntryIDInEntries(unfiltered.Entries, fxA.EntryID) || !hasEntryIDInEntries(unfiltered.Entries, fxB.EntryID) {
		t.Fatalf("expected both fixtures unfiltered, got %+v", unfiltered.Entries)
	}

	filtered, err := searcher.Search(context.Background(), Request{
		Query:   "zzyzxreqpathterm",
		Mode:    ModeKeyword,
		Filters: Filters{FeedIDs: []int64{fxA.FeedID}},
	})
	if err != nil {
		t.Fatalf("Search: unexpected error: %v", err)
	}
	if !hasEntryIDInEntries(filtered.Entries, fxA.EntryID) {
		t.Fatalf("expected feed A's entry to survive the feed filter through Search(), got %+v", filtered.Entries)
	}
	if hasEntryIDInEntries(filtered.Entries, fxB.EntryID) {
		t.Fatalf("expected feed B's entry to be excluded by the feed filter through Search(), got %+v", filtered.Entries)
	}
}

// TestSearchPassageModeFiltersToo mirrors the above for ModePassages
// (spec §6.4's "same retrieval, passage-first presentation"): Filters
// must narrow the flat passage list exactly as it narrows the
// entry-aggregated modes, since both dispatch through the same Lexical/
// Semantic calls underneath Search — and passage mode is the one that
// populates Response.Passages instead of Response.Entries, so it needs
// its own assertion against the right field.
func TestSearchPassageModeFiltersToo(t *testing.T) {
	s := testStore(t)
	db := testDB(t)

	// ModePassages fuses Lexical AND Semantic, so this needs a working
	// embedder even though only the lexical half is what actually
	// matters to this test's assertions — a fixedVectorEmbedder mapping
	// this fixture's own query string to a real, nonzero anchor vector
	// (mirroring semantic_test.go's own pattern), so neither channel
	// errors: an all-zero embedding is rejected by pgvector's cosine
	// operator as having no defined direction.
	anchor := anchorEmbedding(t, db)
	const passageModeQuery = "zzyzxpassagespathterm"
	searcher := NewSearcher(s, WithEmbedder(&fixedVectorEmbedder{vectors: map[string][]float32{
		passageModeQuery: anchor,
	}}))

	fxA := createFixtureEntry(t, db, "reqpath-passages-a", "Zzyzx Passages Path A", "<p>irrelevant</p>")
	writePassages(t, s, fxA.EntryID, []store.PassageRow{
		{Ordinal: 0, Text: "zzyzxpassagespathterm appears in feed a", CharStart: 0, CharEnd: 40, Source: "content", Embedding: anchor},
	})

	fxB := createFixtureEntry(t, db, "reqpath-passages-b", "Zzyzx Passages Path B", "<p>irrelevant</p>")
	writePassages(t, s, fxB.EntryID, []store.PassageRow{
		{Ordinal: 0, Text: "zzyzxpassagespathterm appears in feed b", CharStart: 0, CharEnd: 40, Source: "content", Embedding: anchor},
	})

	// Scoped to both fixtures' own feeds (rather than left wide open) so
	// this "both present" check isn't at the mercy of how the rest of
	// the real, 5,681-passage corpus happens to rank against this
	// fixture's query and embedding -- the point being tested here is
	// narrowing behaviour, not an unfiltered top-10.
	unfiltered, err := searcher.Search(context.Background(), Request{
		Query:   passageModeQuery,
		Mode:    ModePassages,
		Filters: Filters{FeedIDs: []int64{fxA.FeedID, fxB.FeedID}},
	})
	if err != nil {
		t.Fatalf("Search: unexpected error: %v", err)
	}
	if !hasEntryID(unfiltered.Passages, fxA.EntryID) || !hasEntryID(unfiltered.Passages, fxB.EntryID) {
		t.Fatalf("expected both fixtures under the shared two-feed filter in passage mode, got %+v", unfiltered.Passages)
	}

	filtered, err := searcher.Search(context.Background(), Request{
		Query:   "zzyzxpassagespathterm",
		Mode:    ModePassages,
		Filters: Filters{FeedIDs: []int64{fxB.FeedID}},
	})
	if err != nil {
		t.Fatalf("Search: unexpected error: %v", err)
	}
	if hasEntryID(filtered.Passages, fxA.EntryID) {
		t.Fatalf("expected feed A's entry to be excluded by the feed filter in passage mode, got %+v", filtered.Passages)
	}
	if !hasEntryID(filtered.Passages, fxB.EntryID) {
		t.Fatalf("expected feed B's entry to survive the feed filter in passage mode, got %+v", filtered.Passages)
	}
	if filtered.Mode != ModePassages || filtered.Entries != nil {
		t.Fatalf("passage-mode Response must populate Passages, not Entries: %+v", filtered)
	}
}

// --- corpus-scale regression tests ---------------------------------------
//
// The two tests below run against whatever real corpus
// SIDECAR_TEST_DATABASE_URL points at, rather than against a hand-written
// fixture, because both bugs they guard are bugs of *scale*: one needs a
// large Limit, the other needs an entry that owns several matching
// passages. A two-passage fixture cannot exhibit either.

// corpusQuery is a plain English word chosen to match real content in the
// corpus lexically. It is mapped to an already-indexed embedding (see
// anchorEmbedding) for the semantic half, so these tests need no ONNX
// runtime while still exercising both retrieval channels against real
// data.
const corpusQuery = "camera"

func corpusEmbedder(queryVec []float32) *fixedVectorEmbedder {
	return &fixedVectorEmbedder{vectors: map[string][]float32{
		corpusQuery: queryVec,
	}}
}

// TestSearchHybridAtLargeLimitStaysWithinPgvectorsEfSearchCeiling is a
// regression guard for a Critical found in review: pgvector 0.8.4 caps
// hnsw.ef_search at 1000 (pg_settings: min_val 1, max_val 1000), but the
// candidate count handed to SET LOCAL compounded two 5x multipliers
// (searchCandidateMultiplier at the fusion boundary, candidateMultiplier
// inside Semantic) for an effective 25x amplification of Request.Limit,
// on top of a maxCandidates ceiling of 2000 borrowed from lexical.go
// where no such database-side limit exists.
//
// The measured effect was that every hybrid search asking for more than
// 40 results failed outright -- Limit=41 computed 41*5*5 = 1025 and
// errored, Limit=100 computed 2000 (the clamp) and errored -- which made
// a second page of results a hard failure. Worse, on a pooled connection
// where pgvector's module had not yet loaded, hnsw.ef_search is a
// placeholder GUC: the out-of-range SET LOCAL was *accepted* with only a
// WARNING (which lib/pq surfaces solely through a NoticeHandler that is
// not configured here), then silently discarded when the module loaded
// mid-query, reinstating exactly the silent-truncation bug the SET LOCAL
// work existed to eliminate.
//
// Limit = 100 is deliberate. The pre-existing regression test
// (TestSemanticRaisesEfSearchForTheOverfetchWindow) uses limit = 40,
// which sits one single unit below the break -- which is precisely why
// this survived review once already.
func TestSearchHybridAtLargeLimitStaysWithinPgvectorsEfSearchCeiling(t *testing.T) {
	s := testStore(t)
	db := testDB(t)
	searcher := NewSearcher(s, WithEmbedder(corpusEmbedder(anchorEmbedding(t, db))))

	const limit = 100

	resp, err := searcher.Search(context.Background(), Request{
		Query: corpusQuery,
		Mode:  ModeHybrid,
		Limit: limit,
	})
	if err != nil {
		t.Fatalf("Search(ModeHybrid, Limit=%d): unexpected error: %v -- a candidate count above pgvector's hnsw.ef_search ceiling of 1000 must be clamped, not passed through to SET LOCAL", limit, err)
	}
	if len(resp.Entries) == 0 {
		t.Fatalf("Search(ModeHybrid, Limit=%d) returned no entries at all against a real corpus -- this is the silent variant of the same bug: an out-of-range SET LOCAL accepted as a placeholder GUC and then discarded mid-query, leaving the scan at the default ef_search of 40", limit)
	}
	t.Logf("ModeHybrid Limit=%d returned %d entries with no error", limit, len(resp.Entries))
}

// TestSearchEveryModeFillsTheRequestedLimit guards the measurement-
// invalidating bug found in review: ModeKeyword and ModeSemantic passed
// Request.Limit straight through to retrieval and only then aggregated
// passages into entries, so every entry contributing more than one
// matching passage collapsed several retrieved rows into a single
// result. Asking for ten entries returned four (keyword) and three
// (semantic) against this corpus, while ModeHybrid -- which already
// over-fetched by searchCandidateMultiplier before aggregating --
// returned ten.
//
// Beyond being a plain user-visible bug, this structurally confounded the
// task 4 evaluation: hybrid's recall@10 was compared against keyword and
// semantic baselines that could not fill more than three or four of their
// ten slots, so spec §6.3's central claim had never actually been tested.
//
// The assertion is deliberately "exactly limit", not "at least some":
// under-filling is the entire failure mode, and a mode that returns
// limit-1 entries when the corpus can supply limit is still under-filling.
func TestSearchEveryModeFillsTheRequestedLimit(t *testing.T) {
	s := testStore(t)
	db := testDB(t)
	searcher := NewSearcher(s, WithEmbedder(corpusEmbedder(anchorEmbedding(t, db))))

	const limit = 10

	for _, mode := range []Mode{ModeKeyword, ModeSemantic, ModeHybrid, ModePassages} {
		t.Run(mode.String(), func(t *testing.T) {
			resp, err := searcher.Search(context.Background(), Request{
				Query: corpusQuery,
				Mode:  mode,
				Limit: limit,
			})
			if err != nil {
				t.Fatalf("Search(%v): unexpected error: %v", mode, err)
			}

			got := len(resp.Entries)
			if mode == ModePassages {
				got = len(resp.Passages)
			}
			if got != limit {
				t.Fatalf("Search(%v, Limit=%d) returned %d results, want %d -- the corpus can supply them (verified directly: %q matches 19 distinct entries within the lexical top 50, and the anchor embedding's top 50 neighbours span 28 distinct entries), so anything short means retrieval under-fetched before aggregating passages into entries", mode, limit, got, limit, corpusQuery)
			}
			t.Logf("%v Limit=%d returned %d results", mode, limit, got)
		})
	}
}
