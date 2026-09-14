// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package search implements retrieval over search.passages: BM25 lexical
// ranking (task 2), vector similarity (task 3), and their Reciprocal Rank
// Fusion plus entry-level aggregation (task 4, spec §6.3-6.4), tied
// together behind (*Searcher).Search. It reads Miniflux's public.entries
// read-only, alongside the sidecar-owned search schema, and never writes
// to either.
package search // import "miniflux.app/v2/sidecar/internal/search"

import (
	"fmt"
	"time"
)

// PassageHit is one retrieved passage, carrying enough of its own identity
// and position for a caller to render, highlight, or re-rank it without a
// second round trip.
//
// Rank and Score are both returned deliberately: Rank is this retrieval's
// 1-based position (1 = best), which is what Reciprocal Rank Fusion (task
// 4, spec §6.3) actually fuses on; Score is the raw ranking signal behind
// it (paradedb.score() here, cosine distance for semantic retrieval) kept
// for display and debugging. Returning both here means task 4 never has
// to re-derive rank from a list it didn't produce.
type PassageHit struct {
	PassageID int64
	EntryID   int64
	Ordinal   int
	Source    string // "title" | "content"
	Text      string
	CharStart int
	CharEnd   int
	Rank      int
	Score     float64
}

// Filters narrows a retrieval to a subset of entries. Every field is
// optional: its zero value (nil slice, zero time, false bool) means "no
// constraint from this field". Filters are plain predicates against
// public.entries, per spec §6.4, applied to a passage's owning entry.
//
// Both passages_bm25_idx and passages_embedding_idx cover only the
// passage's own columns (id/text, and embedding respectively) — neither
// can push a Filters predicate into its index scan. Every retrieval
// method in this package therefore over-fetches a generous candidate set
// from its index first and applies Filters afterward, in a join against
// public.entries; see each method's own candidateMultiplier for the
// factor used and why.
type Filters struct {
	// FeedIDs restricts results to entries from these feeds. Empty means
	// no restriction.
	FeedIDs []int64

	// CategoryIDs restricts results to entries whose feed belongs to one
	// of these categories. Empty means no restriction.
	CategoryIDs []int64

	// Since and Until bound entries.published_at, inclusive on both
	// ends. A zero time.Time on either means that bound is unset.
	Since, Until time.Time

	// UnreadOnly restricts results to entries with status = 'unread'.
	UnreadOnly bool

	// StarredOnly restricts results to starred entries.
	StarredOnly bool
}

// empty reports whether f imposes no constraint at all — the common case
// of an unfiltered search, checked once per query so the SQL builder can
// skip the join to public.entries entirely rather than filtering against
// an always-true predicate.
func (f Filters) empty() bool {
	return len(f.FeedIDs) == 0 &&
		len(f.CategoryIDs) == 0 &&
		f.Since.IsZero() &&
		f.Until.IsZero() &&
		!f.UnreadOnly &&
		!f.StarredOnly
}

// EntryHit is one entry-level search result (task 4, spec §6.4): an entry
// ranked by its own best-scoring passage, which is carried both as Score
// (what Search ranks entries by) and as Best (what a caller renders as the
// result's snippet). Passages carries every one of that entry's passages
// that also matched, best-first, including Best itself at index 0 — kept
// for callers that need the full per-entry set rather than only the one
// chosen as the snippet (see aggregate in fuse.go).
//
// DO NOT SORT, THRESHOLD, OR COMPARE Score ACROSS MODES. Score is
// copied verbatim from Best.Score, and Best.Score's units AND ITS
// DIRECTION both depend on which Mode produced the result:
//
//	Mode          Score is                       Better is
//	------------  -----------------------------  -----------
//	ModeKeyword   paradedb.score() (BM25)        LARGER
//	ModeSemantic  cosine distance, 0..2          SMALLER
//	ModeHybrid    RRF fused score, ~0..2/(k+1)   LARGER
//
// So `sort.Slice(hits, byScoreDescending)` is correct for two of the
// three modes and silently inverts the ranking for the third, and a
// threshold like `Score > 0.5` means three unrelated things. The results
// Search returns are ALREADY in the correct order for their mode —
// aggregate (fuse.go) preserves the order of the ranked passage list it
// is given, whichever direction that list was sorted in — so a caller
// should treat slice position as the ranking and Score as a display and
// debugging value only.
//
// PassageHit's own comment has carried this warning since task 2;
// EntryHit's did not, which left the entry-level type — the one an HTTP
// handler actually serialises — as the easy place to get it wrong.
type EntryHit struct {
	EntryID int64

	// Score is Best.Score verbatim. Its direction is mode-dependent:
	// see the type's own comment above before sorting or thresholding
	// on it.
	Score float64

	Best     PassageHit
	Passages []PassageHit
}

// Mode selects which retrieval channel(s) a Search call draws from and how
// its results are aggregated — the search bar's mode picker "over the same
// machinery" (spec §6.3). The zero value is ModeHybrid, so a Request built
// without explicitly setting Mode gets the spec's own default rather than
// silently running lexical-only.
type Mode int

const (
	// ModeHybrid runs both Lexical and Semantic retrieval, fuses them with
	// Fuse (Reciprocal Rank Fusion), and aggregates the result to entry
	// level. Spec §6.3's default.
	ModeHybrid Mode = iota

	// ModeKeyword runs Lexical retrieval only ("BM25 only", spec §6.3).
	ModeKeyword

	// ModeSemantic runs Semantic retrieval only ("vector only", spec §6.3).
	ModeSemantic

	// ModePassages runs the same fused retrieval as ModeHybrid but returns
	// a flat, passage-first ranked list instead of aggregating to entry
	// level — "same retrieval, passage-first presentation" (spec §6.3),
	// the extractive answer to "RAG" (spec §6.4).
	ModePassages
)

// String names m for logging. An unrecognised value names itself
// explicitly rather than silently formatting as a bare integer, so a
// caller-constructed invalid Mode is visible in logs rather than looking
// like a typo'd but valid one.
func (m Mode) String() string {
	switch m {
	case ModeHybrid:
		return "hybrid"
	case ModeKeyword:
		return "keyword"
	case ModeSemantic:
		return "semantic"
	case ModePassages:
		return "passages"
	default:
		return fmt.Sprintf("Mode(%d)", int(m))
	}
}

// Request is one Search call's parameters. Query and Mode select what to
// retrieve and how; Limit bounds how many entries (or, in ModePassages,
// passages) come back, defaulting to defaultSearchLimit when left at its
// zero value; Filters narrows the search to a subset of entries exactly
// as it does for Lexical and Semantic.
type Request struct {
	Query   string
	Mode    Mode
	Limit   int
	Filters Filters
}

// Response is one Search call's results. Mode echoes the Request's own
// Mode so a caller can tell which of Entries or Passages was populated
// without inspecting which is non-nil: Entries holds results for
// ModeKeyword, ModeSemantic and ModeHybrid; Passages holds them for
// ModePassages, and is nil for every other mode (and vice versa).
type Response struct {
	Mode     Mode
	Entries  []EntryHit
	Passages []PassageHit
}
