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
// posture — it still requires SIDECAR_DATABASE_URL, like every other
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
