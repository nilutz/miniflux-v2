// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package search // import "miniflux.app/v2/sidecar/internal/search"

import (
	"context"
	"fmt"
	"sort"
)

// rrfK is Reciprocal Rank Fusion's damping constant, k, from the plan
// preamble's validated RRF query shape (`1.0/(60 + rank)`) and spec §6.3.
// It is what makes a rank-1 hit only modestly better than a rank-2 hit
// (1/61 vs 1/62) rather than dominating the fused score outright; 60 is
// the conventional value in the RRF literature and is kept as a named
// constant rather than a literal so its provenance is visible at every
// call site.
const rrfK = 60

// Fuse merges lexical and semantic passage-retrieval results via
// Reciprocal Rank Fusion. Each passage's fused score is the sum, over
// every input list it appears in, of 1/(rrfK + that list's 1-based Rank
// for it) -- a function of Rank alone. Fuse never reads a hit's Score:
// lexical BM25 scores and semantic cosine distances are not on a
// comparable scale, and normalising them onto one is exactly where hybrid
// search usually goes wrong (spec §6.3) -- fusing on rank position sidesteps
// that problem entirely rather than solving it.
//
// A passage present in only one of the two lists is scored from that list
// alone; one present in both sums both contributions, which is what lets
// cross-channel agreement outrank a strong showing in a single channel
// (TestRRFRewardsAppearingInBothLists). Either argument may be nil or
// empty, in which case fusion degenerates to the other list's own
// ranking with a rewritten Score, though no production caller relies on
// that: Fuse has exactly two callers, ModeHybrid and ModePassages, and
// both always pass two real lists (ModeKeyword and ModeSemantic
// aggregate their single channel's raw hits directly -- see Search below
// -- and never call Fuse at all).
//
// The returned hits are the union of both inputs by PassageID, ordered by
// fused score descending, each carrying that fused score as its own Score
// and a freshly assigned, contiguous 1-based Rank -- overwriting whatever
// Rank/Score the passage carried in its source list, since those were
// meaningful only within that list. Ties are broken by PassageID
// ascending, so identical inputs always fuse to identical output order
// (TestFuseIsStableForEqualScores) regardless of Go's map iteration order
// or either input slice's own relative ordering beyond Rank.
func Fuse(lexical, semantic []PassageHit) []PassageHit {
	scores := make(map[int64]float64, len(lexical)+len(semantic))
	byID := make(map[int64]PassageHit, len(lexical)+len(semantic))
	order := make([]int64, 0, len(lexical)+len(semantic))

	accumulate := func(hits []PassageHit) {
		for _, h := range hits {
			if _, seen := byID[h.PassageID]; !seen {
				order = append(order, h.PassageID)
			}
			byID[h.PassageID] = h
			scores[h.PassageID] += 1.0 / float64(rrfK+h.Rank)
		}
	}
	accumulate(lexical)
	accumulate(semantic)

	fused := make([]PassageHit, 0, len(order))
	for _, id := range order {
		h := byID[id]
		h.Score = scores[id]
		fused = append(fused, h)
	}

	sort.Slice(fused, func(i, j int) bool {
		if fused[i].Score != fused[j].Score {
			return fused[i].Score > fused[j].Score
		}
		return fused[i].PassageID < fused[j].PassageID
	})

	for i := range fused {
		fused[i].Rank = i + 1
	}
	return fused
}

// aggregate groups a ranked passage list (as returned by Lexical, Semantic
// or Fuse -- anything already ordered best-first) into one EntryHit per
// distinct entry, per spec §6.4: "entries ranked by their best-scoring
// passage". Because hits is already globally ordered best-first, the
// first time an entry id is encountered in it is, by construction, that
// entry's own best-scoring passage -- so a single pass both identifies
// each entry's Best and preserves the correct relative order between
// entries, with no second sort needed: whichever entry's best passage
// appears earlier in hits necessarily has the higher score.
//
// Passages holds every passage belonging to that entry, in the same
// relative (best-first) order they appeared in hits -- not just Best.
// This is deliberate: it is what lets a caller building a passage-mode
// response (or a future per-entry "other matches" view) reuse this same
// aggregation rather than re-deriving it from the raw hit list.
func aggregate(hits []PassageHit) []EntryHit {
	byEntry := make(map[int64]*EntryHit, len(hits))
	order := make([]int64, 0, len(hits))

	for _, h := range hits {
		e, seen := byEntry[h.EntryID]
		if !seen {
			e = &EntryHit{EntryID: h.EntryID, Score: h.Score, Best: h}
			byEntry[h.EntryID] = e
			order = append(order, h.EntryID)
		}
		e.Passages = append(e.Passages, h)
	}

	result := make([]EntryHit, 0, len(order))
	for _, id := range order {
		result = append(result, *byEntry[id])
	}
	return result
}

// defaultSearchLimit is the entry/passage count Search returns when
// Request.Limit is left at its zero value. 10 matches this package's own
// evaluation harness (recall@10) and is a reasonable single-page default
// for a search-bar result list.
const defaultSearchLimit = 10

// searchCandidateMultiplier is how many times Request.Limit's worth of
// passages EVERY mode retrieves before aggregating or fusing.
//
// Two distinct things need it, for two distinct reasons:
//
//   - ModeHybrid and ModePassages retrieve this many from EACH channel
//     before fusing. Retrieving only Limit's worth from each would
//     silently starve Fuse: a passage strong in only one channel needs
//     that channel's own ranking to reach deep enough for it to appear
//     at all, or it never gets the chance to be rewarded (or even
//     considered) by fusion.
//
//   - ModeKeyword and ModeSemantic retrieve this many before
//     aggregating. Retrieval is passage-level and results are
//     entry-level, so several passages of one entry collapse into a
//     single EntryHit: N retrieved passages yield at most N entries and
//     routinely far fewer. Asking retrieval for exactly Limit passages
//     therefore cannot fill Limit entries except in the degenerate case
//     where no entry matches twice.
//
// Missing the second reason would not just be a user-visible "ask for
// ten, get three" bug: it would structurally confound any recall
// evaluation that compares a full-fat ModeHybrid against ModeKeyword and
// ModeSemantic baselines, which could then only fill three or four of
// their ten slots. Applying one multiplier uniformly across all four
// modes is what makes the modes comparable at all.
//
// BEWARE THE AMPLIFICATION: this multiplier COMPOUNDS with
// candidateMultiplier (lexical.go, also 5x), which Lexical and Semantic
// apply again to whatever limit they are handed here. One Request.Limit
// becomes 25x that many rows asked of an index -- see candidateMultiplier's
// own comment for the worked numbers and for why that matters in the
// semantic path, where it is bounded by pgvector's hnsw.ef_search
// ceiling (maxVectorCandidates, semantic.go).
const searchCandidateMultiplier = 5

// Search runs q against this Searcher's retrieval, dispatching on q.Mode
// (spec §6.3's mode picker): ModeKeyword and ModeSemantic each run their
// single retrieval channel and aggregate to entry level; ModeHybrid fuses
// both channels with Fuse before aggregating; ModePassages runs the same
// fused retrieval as ModeHybrid but returns a flat, passage-first ranked
// list instead (spec §6.4's "extractive answer to RAG").
//
// Every mode retrieves searchCandidateMultiplier * q.Limit passages
// before aggregating or fusing, so every mode can actually return
// q.Limit entries when the corpus can supply them. This uniformity is
// load-bearing twice over: it is what makes "ask for ten, get ten" true
// in the single-channel modes, and it is what makes a comparison between
// the modes (the recall evaluation) measure retrieval quality rather
// than measuring which mode was allowed to fill its result page.
//
// A zero or negative q.Limit is treated as defaultSearchLimit rather than
// returning nothing -- unlike Lexical/Semantic's own zero-limit handling,
// Limit here means "how many results the caller wants back", and 0 is a
// caller that didn't set it, not a caller asking for an empty page.
func (s *Searcher) Search(ctx context.Context, q Request) (Response, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = defaultSearchLimit
	}

	// Every mode over-fetches by the same factor before aggregating or
	// fusing, because every mode retrieves passages and (except
	// ModePassages) reports entries. See searchCandidateMultiplier.
	candidates := limit * searchCandidateMultiplier

	switch q.Mode {
	case ModeKeyword:
		hits, err := s.Lexical(ctx, q.Query, candidates, q.Filters)
		if err != nil {
			return Response{}, fmt.Errorf("search: keyword search failed: %w", err)
		}
		return Response{Mode: q.Mode, Entries: truncateEntries(aggregate(hits), limit)}, nil

	case ModeSemantic:
		hits, err := s.Semantic(ctx, q.Query, candidates, q.Filters)
		if err != nil {
			return Response{}, fmt.Errorf("search: semantic search failed: %w", err)
		}
		return Response{Mode: q.Mode, Entries: truncateEntries(aggregate(hits), limit)}, nil

	case ModeHybrid, ModePassages:
		lexHits, err := s.Lexical(ctx, q.Query, candidates, q.Filters)
		if err != nil {
			return Response{}, fmt.Errorf("search: hybrid search's lexical half failed: %w", err)
		}
		semHits, err := s.Semantic(ctx, q.Query, candidates, q.Filters)
		if err != nil {
			return Response{}, fmt.Errorf("search: hybrid search's semantic half failed: %w", err)
		}
		fused := Fuse(lexHits, semHits)

		if q.Mode == ModePassages {
			if len(fused) > limit {
				fused = fused[:limit]
			}
			return Response{Mode: q.Mode, Passages: fused}, nil
		}
		return Response{Mode: q.Mode, Entries: truncateEntries(aggregate(fused), limit)}, nil

	default:
		return Response{}, fmt.Errorf("search: unknown Mode %v", q.Mode)
	}
}

// truncateEntries returns entries' first limit elements, or entries
// itself unchanged when it already has limit or fewer.
func truncateEntries(entries []EntryHit, limit int) []EntryHit {
	if len(entries) > limit {
		return entries[:limit]
	}
	return entries
}
