// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui // import "miniflux.app/v2/internal/ui"

import (
	"reflect"
	"testing"

	"miniflux.app/v2/internal/searchclient"
)

// TestBuildSnippetSegments_NoHighlightsIsOneSegment proves a snippet with
// no highlight ranges renders as a single, unmarked segment.
func TestBuildSnippetSegments_NoHighlightsIsOneSegment(t *testing.T) {
	got := buildSnippetSegments(searchclient.Snippet{Text: "the best coffee in town"})
	want := []snippetSegment{{Text: "the best coffee in town"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

// TestBuildSnippetSegments_EmptyTextIsNil proves an empty snippet (the
// fallback path never populates one) produces no segments at all, so the
// template's "{{ if .Segments }}" renders nothing.
func TestBuildSnippetSegments_EmptyTextIsNil(t *testing.T) {
	if got := buildSnippetSegments(searchclient.Snippet{}); got != nil {
		t.Fatalf("expected nil segments for an empty snippet, got %+v", got)
	}
}

// TestBuildSnippetSegments_SplitsAroundHighlight proves the snippet is
// split into plain/highlighted/plain segments at the highlight's byte
// offsets, as three PLAIN GO STRINGS - never as a single string with HTML
// spliced in. This is the property the security constraint in the task
// brief depends on: the caller (search.html) ranges over these segments
// and wraps only the Highlight ones in a literal <mark> from the
// template source, so html/template's normal auto-escaping still applies
// to every byte of article content.
func TestBuildSnippetSegments_SplitsAroundHighlight(t *testing.T) {
	sn := searchclient.Snippet{
		Text:       "the best coffee in town",
		Highlights: []searchclient.Range{{Start: 4, End: 8}},
	}
	got := buildSnippetSegments(sn)
	want := []snippetSegment{
		{Text: "the "},
		{Text: "best", Highlight: true},
		{Text: " coffee in town"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	for _, seg := range got {
		if seg.Text == "" {
			t.Fatalf("did not expect an empty segment in %+v", got)
		}
	}
}

// TestBuildSnippetSegments_MergesOverlappingHighlights proves two
// overlapping ranges are merged into one highlighted segment rather than
// producing a malformed or double-nested result.
func TestBuildSnippetSegments_MergesOverlappingHighlights(t *testing.T) {
	sn := searchclient.Snippet{
		Text: "abcdefghij",
		Highlights: []searchclient.Range{
			{Start: 6, End: 9},
			{Start: 2, End: 5},
			{Start: 4, End: 7}, // overlaps both neighbours once sorted
		},
	}
	got := buildSnippetSegments(sn)
	want := []snippetSegment{
		{Text: "ab"},
		{Text: "cdefghi", Highlight: true},
		{Text: "j"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

// TestBuildSnippetSegments_ClampsOutOfBoundsRanges proves a highlight
// range outside the snippet's own text - which would panic a raw
// text[start:end] slice - is clamped rather than trusted. The sidecar is
// a separate, network-facing process; its offsets must never be assumed
// well-formed.
func TestBuildSnippetSegments_ClampsOutOfBoundsRanges(t *testing.T) {
	sn := searchclient.Snippet{
		Text: "short",
		Highlights: []searchclient.Range{
			{Start: -5, End: 2},
			{Start: 3, End: 999},
			{Start: 10, End: 20}, // entirely out of bounds: must be dropped, not panic
		},
	}
	got := buildSnippetSegments(sn) // must not panic
	want := []snippetSegment{
		{Text: "sh", Highlight: true},
		{Text: "o"},
		{Text: "rt", Highlight: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

// TestBuildSnippetSegments_DropsInvertedRange proves a Start >= End range
// (however it arose) is dropped rather than used, since it would slice
// backwards or highlight nothing meaningfully.
func TestBuildSnippetSegments_DropsInvertedRange(t *testing.T) {
	sn := searchclient.Snippet{
		Text:       "hello",
		Highlights: []searchclient.Range{{Start: 3, End: 3}, {Start: 4, End: 2}},
	}
	got := buildSnippetSegments(sn)
	want := []snippetSegment{{Text: "hello"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}
