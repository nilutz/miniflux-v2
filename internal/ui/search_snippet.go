// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui // import "miniflux.app/v2/internal/ui"

import (
	"sort"

	"miniflux.app/v2/internal/searchclient"
)

// snippetSegment is one piece of a rendered search snippet, split from a
// searchclient.Snippet's Text at its Highlights' byte offsets.
//
// Text is a plain string, never template.HTML: the snippet is article
// content pulled from the open internet and must go through
// html/template's ordinary auto-escaping like any other untrusted text.
// The <mark> wrapping around a Highlight segment is decided structurally
// by the template (see search.html), which ranges over these segments
// and wraps the ones marked Highlight in a literal <mark> element from
// the template source - never by splicing markup into Text itself.
type snippetSegment struct {
	Text      string
	Highlight bool
}

// buildSnippetSegments splits sn.Text into segments at sn.Highlights'
// byte offsets so a template can mark the highlighted spans without ever
// handling raw HTML. It returns nil for an empty snippet - the fallback
// search path has no snippet at all, and the template treats a nil
// Segments the same as "nothing to show here".
func buildSnippetSegments(sn searchclient.Snippet) []snippetSegment {
	if sn.Text == "" {
		return nil
	}

	ranges := sanitizeHighlightRanges(sn.Highlights, len(sn.Text))
	if len(ranges) == 0 {
		return []snippetSegment{{Text: sn.Text}}
	}

	segments := make([]snippetSegment, 0, len(ranges)*2+1)
	pos := 0
	for _, rg := range ranges {
		if rg.Start > pos {
			segments = append(segments, snippetSegment{Text: sn.Text[pos:rg.Start]})
		}
		segments = append(segments, snippetSegment{Text: sn.Text[rg.Start:rg.End], Highlight: true})
		pos = rg.End
	}
	if pos < len(sn.Text) {
		segments = append(segments, snippetSegment{Text: sn.Text[pos:]})
	}
	return segments
}

// sanitizeHighlightRanges clamps each range into [0, textLen], drops any
// that are empty or inverted after clamping, sorts by start, and merges
// overlapping or touching ranges. The sidecar is a separate, network-
// facing process; buildSnippetSegments must never trust its offsets
// enough to slice sn.Text out of bounds or double-highlight a byte.
func sanitizeHighlightRanges(ranges []searchclient.Range, textLen int) []searchclient.Range {
	clamped := make([]searchclient.Range, 0, len(ranges))
	for _, r := range ranges {
		start, end := r.Start, r.End
		if start < 0 {
			start = 0
		}
		if end > textLen {
			end = textLen
		}
		if start >= end {
			continue
		}
		clamped = append(clamped, searchclient.Range{Start: start, End: end})
	}
	if len(clamped) == 0 {
		return nil
	}

	sort.Slice(clamped, func(i, j int) bool { return clamped[i].Start < clamped[j].Start })

	merged := clamped[:1]
	for _, r := range clamped[1:] {
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
