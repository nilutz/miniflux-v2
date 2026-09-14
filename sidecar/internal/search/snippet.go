// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package search // import "miniflux.app/v2/sidecar/internal/search"

import (
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"miniflux.app/v2/sidecar/internal/passage"
	"miniflux.app/v2/sidecar/internal/store"
)

// Range is a half-open byte span [Start, End) into a Snippet's own Text,
// marking one location a caller should render as highlighted. Both ends
// always fall on a UTF-8 rune boundary of Text and never exceed
// len(Text): every Range BuildSnippet returns comes from locating a query
// term inside Text itself (see locateHighlights), never from reusing a
// PassageHit's CharStart/CharEnd beyond the one guarded slice described
// on BuildSnippet's own doc comment.
type Range struct {
	Start, End int
}

// Snippet is a rendering-ready fragment of an entry's plaintext, plus the
// spans within Text a caller should render as highlighted matches for
// the search query that produced it.
type Snippet struct {
	Text       string
	Highlights []Range
}

// snippetWindowChars bounds how much surrounding context BuildSnippet
// returns when it has to re-locate a query term inside a freshly
// re-derived source rather than trust a PassageHit's stored offsets (see
// the "degraded path" in BuildSnippet's own doc comment). It is sized for
// a search-result snippet, not a full excerpt.
const snippetWindowChars = 320

// minHighlightTermRunes is the shortest query term BuildSnippet will
// look for and highlight. Below this, a one- or two-letter "term" (an
// article, a stray initial) would light up so much of ordinary prose
// that the highlight stops meaning anything.
const minHighlightTermRunes = 3

// BuildSnippet renders hit as a highlighted Snippet, re-deriving the
// plaintext hit's offsets index into rather than ever trusting a stored
// copy of it — because none exists. char_start/char_end are only ever
// meaningful relative to a plaintext that search.passages itself never
// persists; this package's own hazard (see package doc comment) is
// slicing the wrong string, or a string that no longer exists, with
// them. BuildSnippet is the one place that risk is retired.
//
// entry is the entry's CURRENT state, exactly as store.EntryForIndexing
// returns it — so entry.ContentHash reflects store.PipelineVersion, the
// current title and the current content, recomputed fresh. indexed is
// the state recorded the last time this entry was actually indexed,
// exactly as store.EntryIndexState returns it, or nil if it has never
// been indexed at all.
//
// The source string hit's offsets index into depends on hit.Source
// exactly as it does when search.passages is written: "title" indexes
// into entry.Title verbatim; anything else ("content") indexes into
// passage.ExtractText(entry.Content) — the same derivation the indexer
// itself runs, and the only correct way to get back the string
// char_start/char_end are relative to. Slicing entry.Content directly
// with these offsets would land in raw, unextracted HTML: a different
// string, of a different length, and never the right one. BuildSnippet
// never does that (see sourceText).
//
// entry.ContentHash == indexed.ContentHash is the one signal available
// from outside package store that hit's stored offsets are still safe
// to slice by: that hash folds in store.PipelineVersion, the title and
// the content together (see store's own unexported contentHash), so it
// can only still match if none of those three have changed since hit
// was indexed. When it does not match — indexed is nil, the pipeline
// version was bumped since, or the entry's title or content was edited
// — BuildSnippet never slices by the stored offsets, because they may
// point past the end of the current string, or land mid-rune, or simply
// land on the wrong words with no error to show for it. Instead it
// re-locates the query's own terms directly inside the freshly
// re-derived source and returns a window around the first match
// (relocate), or, failing to find any of them at all, hit's own
// already-stored Text with no highlights (the last-resort fallback at
// the end of this function). Both are safe degradations; slicing stale
// offsets into a re-derived string of a different length is the one
// outcome this function is built to never produce.
func BuildSnippet(hit PassageHit, entry store.Entry, indexed *store.IndexState, query string) Snippet {
	source := sourceText(hit, entry)

	if indexed != nil && indexed.ContentHash == entry.ContentHash {
		if text, ok := safeSlice(source, hit.CharStart, hit.CharEnd); ok {
			return Snippet{Text: text, Highlights: locateHighlights(text, query)}
		}
		// Defensive only: a matching content hash means these offsets
		// were valid when written and nothing has changed since, so
		// safeSlice succeeding is not merely expected, it is guaranteed
		// by construction. If it somehow does not, fall through to
		// exactly the same degraded path a real mismatch takes below,
		// rather than ever guessing at a slice that failed its own
		// bounds check.
	}

	if text, highlights, ok := relocate(source, query); ok {
		return Snippet{Text: text, Highlights: highlights}
	}

	return Snippet{Text: hit.Text, Highlights: nil}
}

// sourceText returns the string hit's CharStart/CharEnd are meant to
// index into, per hit.Source, re-derived from entry's CURRENT state —
// never a stored or cached copy, because none is ever persisted (see
// BuildSnippet's own doc comment).
func sourceText(hit PassageHit, entry store.Entry) string {
	if hit.Source == "title" {
		return entry.Title
	}
	return passage.ExtractText(entry.Content)
}

// safeSlice returns source[start:end] and true only when that range
// lies entirely inside source and both ends fall on a UTF-8 rune
// boundary — so a caller can never slice mid-rune, nor past the end of
// a string that may be shorter than whatever produced start/end.
func safeSlice(source string, start, end int) (string, bool) {
	if start < 0 || end < start || end > len(source) {
		return "", false
	}
	if start > 0 && !utf8.RuneStart(source[start]) {
		return "", false
	}
	if end < len(source) && !utf8.RuneStart(source[end]) {
		return "", false
	}
	return source[start:end], true
}

// relocate finds query's own terms directly inside source — ignoring
// any stored offset entirely — and returns a snippetWindowChars-wide
// window around the first match, snapped to rune boundaries on both
// ends, with Highlights located fresh inside that window (never carried
// over from anywhere else). ok is false when none of query's terms
// appear in source at all, which BuildSnippet treats as "nothing safe
// to show a located match for" and falls back further still.
func relocate(source, query string) (string, []Range, bool) {
	best := -1
	for _, term := range queryTerms(query) {
		for _, r := range findTerm(source, term) {
			if best == -1 || r.Start < best {
				best = r.Start
			}
		}
	}
	if best == -1 {
		return "", nil, false
	}

	start := snapForward(source, max(0, best-snippetWindowChars/2))
	end := snapBackward(source, min(len(source), start+snippetWindowChars))
	if end <= start {
		end = len(source)
	}

	text := source[start:end]
	return text, locateHighlights(text, query), true
}

// snapForward returns the smallest rune-boundary index >= i in s (never
// past len(s)).
func snapForward(s string, i int) int {
	for i < len(s) && !utf8.RuneStart(s[i]) {
		i++
	}
	return i
}

// snapBackward returns the largest rune-boundary index <= i in s (never
// below 0).
func snapBackward(s string, i int) int {
	for i > 0 && i < len(s) && !utf8.RuneStart(s[i]) {
		i--
	}
	return i
}

// locateHighlights finds every occurrence of every one of query's terms
// inside text, case-insensitively, and returns them merged into
// non-overlapping, Start-ascending Ranges. It never consults anything
// but text and query themselves — no stored offset ever reaches this
// function — so its output can never point outside text or at the
// wrong string.
//
// Matching is a plain, case-insensitive substring search, not a stemmed
// one: paradedb.match (lexical.go) and the embedding model (semantic.go)
// can both match a passage on a term that never appears in it verbatim
// — a stem, a synonym, a paraphrase — and this function does not
// highlight those. It only marks literal, case-folded occurrences of
// the query's own words. That is a deliberately narrower guarantee than
// "why this passage matched": it is "where these exact words appear",
// which is what a reader actually wants underlined, and it never risks
// marking the wrong span to chase a fuzzier match.
func locateHighlights(text, query string) []Range {
	var ranges []Range
	for _, term := range queryTerms(query) {
		ranges = append(ranges, findTerm(text, term)...)
	}
	return mergeRanges(ranges)
}

// queryTerms splits query into the words BuildSnippet looks for: runs
// of letters/digits, with case folding happening later in findTerm.
// Terms shorter than minHighlightTermRunes are dropped — see that
// constant's own doc comment.
func queryTerms(query string) []string {
	fields := strings.FieldsFunc(query, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	terms := fields[:0]
	for _, f := range fields {
		if utf8.RuneCountInString(f) >= minHighlightTermRunes {
			terms = append(terms, f)
		}
	}
	return terms
}

// findTerm returns every occurrence of term in text, matched
// case-insensitively rune by rune (caseFoldPrefixLen), scanning one rune
// of text at a time so every returned Range starts and ends on one of
// text's own rune boundaries regardless of how case-folding affects
// byte width on either side.
func findTerm(text, term string) []Range {
	var out []Range
	for i := 0; i < len(text); {
		_, size := utf8.DecodeRuneInString(text[i:])
		if size == 0 {
			break
		}
		if n, ok := caseFoldPrefixLen(text[i:], term); ok {
			out = append(out, Range{Start: i, End: i + n})
		}
		i += size
	}
	return out
}

// caseFoldPrefixLen reports whether s begins with term under simple,
// per-rune case folding (unicode.ToLower on each side), and if so, the
// exact byte length of that prefix IN s. That length equals len(term)
// whenever case-folding never changes a rune's UTF-8 width, which is
// the overwhelmingly common case; walking rune-by-rune rather than
// allocating strings.ToLower on either side means the returned length
// is always an exact multiple of whole runes of s regardless, so a
// Range built from it can never land mid-rune even on the rare input
// where a folded rune's width differs.
func caseFoldPrefixLen(s, term string) (int, bool) {
	si, ti := 0, 0
	for ti < len(term) {
		if si >= len(s) {
			return 0, false
		}
		sr, ssize := utf8.DecodeRuneInString(s[si:])
		tr, tsize := utf8.DecodeRuneInString(term[ti:])
		if unicode.ToLower(sr) != unicode.ToLower(tr) {
			return 0, false
		}
		si += ssize
		ti += tsize
	}
	return si, true
}

// mergeRanges sorts ranges by Start and merges any that overlap or
// touch, so a query with a repeated or overlapping term never produces
// two highlights covering the same text.
func mergeRanges(ranges []Range) []Range {
	if len(ranges) == 0 {
		return nil
	}
	sort.Slice(ranges, func(i, j int) bool {
		if ranges[i].Start != ranges[j].Start {
			return ranges[i].Start < ranges[j].Start
		}
		return ranges[i].End < ranges[j].End
	})

	merged := []Range{ranges[0]}
	for _, r := range ranges[1:] {
		last := &merged[len(merged)-1]
		if r.Start <= last.End {
			if r.End > last.End {
				last.End = r.End
			}
			continue
		}
		merged = append(merged, r)
	}
	return merged
}
