// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package search implements retrieval over search.passages: BM25 lexical
// ranking (task 2), vector similarity (task 3), and their fusion (task 4).
// It reads Miniflux's public.entries read-only, alongside the sidecar-owned
// search schema, and never writes to either.
package search // import "miniflux.app/v2/sidecar/internal/search"

import "time"

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
