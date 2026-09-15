// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package search // import "miniflux.app/v2/sidecar/internal/search"

import (
	"strings"
	"testing"
	"unicode/utf8"

	"miniflux.app/v2/sidecar/internal/passage"
	"miniflux.app/v2/sidecar/internal/store"
)

// extractedRange finds needle inside plaintext and returns its
// [start,end) byte range, failing the test if it is not present exactly
// once — a small helper so fixtures below compute real offsets from the
// same derivation the indexer itself runs, rather than hand-counting
// bytes (which is exactly the kind of arithmetic that goes silently
// wrong on multi-byte content).
func extractedRange(t *testing.T, plaintext, needle string) (int, int) {
	t.Helper()
	i := strings.Index(plaintext, needle)
	if i < 0 {
		t.Fatalf("fixture bug: %q not found in %q", needle, plaintext)
	}
	if strings.Index(plaintext[i+1:], needle) >= 0 {
		t.Fatalf("fixture bug: %q appears more than once in %q", needle, plaintext)
	}
	return i, i + len(needle)
}

// highlightedText returns the substrings of snippetText covered by each
// Highlight, in order — what a caller would actually render as
// emphasised.
func highlightedText(snippetText string, highlights []Range) []string {
	out := make([]string, len(highlights))
	for i, h := range highlights {
		out[i] = snippetText[h.Start:h.End]
	}
	return out
}

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// TestBuildSnippetHighlightsQueryTermsInReDerivedPlaintext is the
// hazard's core case: a content-sourced passage's offsets index into
// passage.ExtractText(entry.Content), NEVER the raw HTML, and the
// snippet's highlights must land on the query's own words inside that
// re-derived plaintext.
func TestBuildSnippetHighlightsQueryTermsInReDerivedPlaintext(t *testing.T) {
	content := "<p>The quick brown fox jumps over the lazy dog.</p>"
	plaintext := passage.ExtractText(content)
	if strings.Contains(plaintext, "<p>") {
		t.Fatalf("fixture bug: extracted plaintext still contains markup: %q", plaintext)
	}

	start, end := extractedRange(t, plaintext, "brown fox jumps")

	hit := PassageHit{
		Source:    "content",
		Text:      "brown fox jumps",
		CharStart: start,
		CharEnd:   end,
	}
	entry := store.Entry{Title: "Irrelevant title", Content: content, ContentHash: "same-hash"}
	indexed := &store.IndexState{ContentHash: "same-hash"}

	snip := BuildSnippet(hit, entry, indexed, "brown fox")

	if snip.Text != "brown fox jumps" {
		t.Fatalf("Text = %q, want the re-derived slice %q", snip.Text, "brown fox jumps")
	}
	if strings.Contains(snip.Text, "<") {
		t.Fatalf("snippet text contains raw markup, meaning raw HTML was sliced: %q", snip.Text)
	}

	got := highlightedText(snip.Text, snip.Highlights)
	if !containsString(got, "brown") || !containsString(got, "fox") {
		t.Fatalf("highlights = %v (from %v), want spans covering %q and %q", got, snip.Highlights, "brown", "fox")
	}
	for _, h := range snip.Highlights {
		if h.Start < 0 || h.End > len(snip.Text) || h.Start > h.End {
			t.Fatalf("highlight %+v out of bounds for text of length %d", h, len(snip.Text))
		}
	}
}

// TestBuildSnippetTitleSourceSlicesTitleNotBody guards a specific trap:
// a title passage's offsets index into the title, a completely
// different string from the body plaintext, and source distinguishes
// them. If BuildSnippet ever sliced the body
// instead, this test's title text ("Widgets") does not appear anywhere
// in the body, so the mistake would be visible immediately rather than
// coincidentally passing.
func TestBuildSnippetTitleSourceSlicesTitleNotBody(t *testing.T) {
	title := "Widgets and Gadgets"
	content := "<p>This article body never mentions the word at all, only gizmos and sprockets.</p>"

	start, end := extractedRange(t, title, "Widgets")

	hit := PassageHit{
		Source:    "title",
		Text:      "Widgets",
		CharStart: start,
		CharEnd:   end,
	}
	entry := store.Entry{Title: title, Content: content, ContentHash: "same-hash"}
	indexed := &store.IndexState{ContentHash: "same-hash"}

	snip := BuildSnippet(hit, entry, indexed, "widgets")

	if snip.Text != "Widgets" {
		t.Fatalf("Text = %q, want the title slice %q (body must never be sliced for a title hit)", snip.Text, "Widgets")
	}
	got := highlightedText(snip.Text, snip.Highlights)
	if !containsString(got, "Widgets") {
		t.Fatalf("highlights = %v, want a span covering %q", got, "Widgets")
	}
}

// TestBuildSnippetDegradesGracefullyOnPipelineVersionMismatch proves the
// central hazard is actually retired, not just reasoned about: it hands
// BuildSnippet offsets that are flatly WRONG for the query it also
// passes (CharStart/CharEnd pointing at "The quick", not "brown fox"),
// paired with a content-hash mismatch simulating exactly what a
// store.PipelineVersion bump does to an entry's recorded state (folded
// into the hash per store's own contentHash — see entries.go). A buggy
// implementation that trusted the offsets anyway would highlight "The
// quick"; this test fails unless the degraded path is actually taken and
// the query's real terms are re-located instead.
func TestBuildSnippetDegradesGracefullyOnPipelineVersionMismatch(t *testing.T) {
	content := "<p>The quick brown fox jumps over the lazy dog.</p>"
	plaintext := passage.ExtractText(content)

	wrongStart, wrongEnd := extractedRange(t, plaintext, "The quick")

	hit := PassageHit{
		Source:    "content",
		Text:      "brown fox jumps", // what was actually indexed
		CharStart: wrongStart,        // but these offsets are stale/wrong
		CharEnd:   wrongEnd,
	}
	entry := store.Entry{Title: "T", Content: content, ContentHash: "hash-under-current-pipeline-version"}
	indexed := &store.IndexState{ContentHash: "hash-recorded-under-an-older-pipeline-version"}

	snip := BuildSnippet(hit, entry, indexed, "brown fox")

	if snip.Text == "The quick" {
		t.Fatalf("BuildSnippet trusted stale offsets across a pipeline-version mismatch and highlighted the wrong span: %q", snip.Text)
	}

	got := highlightedText(snip.Text, snip.Highlights)
	if containsString(got, "The") || containsString(got, "quick") {
		t.Fatalf("highlights = %v landed on the wrong words (%q), want no trace of the stale offset's span", got, snip.Text)
	}
	// Graceful degradation means either: no highlight at all, or the
	// query's own terms correctly re-located. Both are acceptable; a
	// wrong highlight is not.
	if len(snip.Highlights) > 0 && !containsString(got, "brown") && !containsString(got, "fox") {
		t.Fatalf("highlights = %v, want either none or the query's real terms (brown/fox)", got)
	}
}

// TestBuildSnippetOffsetsNeverRunPastChangedContent covers an entry
// whose content shrank since it was indexed: the stored CharEnd is far
// beyond the length of the current (re-derived) plaintext. This must
// never panic, and no Highlight may reference a position past the end
// of the returned Snippet.Text.
func TestBuildSnippetOffsetsNeverRunPastChangedContent(t *testing.T) {
	shortContent := "<p>Hi there.</p>"
	shortPlaintext := passage.ExtractText(shortContent)

	hit := PassageHit{
		Source:    "content",
		Text:      "a much longer passage that used to exist in this entry before it was edited down",
		CharStart: 0,
		CharEnd:   len(shortPlaintext) + 500, // stale: indexed against much longer content
	}
	entry := store.Entry{Title: "T", Content: shortContent, ContentHash: "hash-of-short-content"}
	indexed := &store.IndexState{ContentHash: "hash-of-the-old-much-longer-content"}

	snip := BuildSnippet(hit, entry, indexed, "there")

	if len(snip.Text) > len(shortPlaintext)+len(hit.Text) {
		t.Fatalf("snippet text implausibly long (%d bytes): offsets were not bounded", len(snip.Text))
	}
	for _, h := range snip.Highlights {
		if h.Start < 0 || h.End > len(snip.Text) || h.Start > h.End {
			t.Fatalf("highlight %+v out of bounds for text of length %d", h, len(snip.Text))
		}
	}
}

// TestBuildSnippetHighlightsMultiByteUTF8Content reuses the fixture
// style established in internal/passage (café, naïve, Zürich, 日本語):
// several multi-byte runes of different widths in the same short
// passage, which is exactly what would expose a highlight Range landing
// mid-rune.
func TestBuildSnippetHighlightsMultiByteUTF8Content(t *testing.T) {
	content := "<p>The café in Zürich serves naïve travelers 日本語 pastries.</p>"
	plaintext := passage.ExtractText(content)

	start, end := extractedRange(t, plaintext, "café in Zürich serves naïve travelers 日本語 pastries")

	hit := PassageHit{
		Source:    "content",
		Text:      plaintext[start:end],
		CharStart: start,
		CharEnd:   end,
	}
	entry := store.Entry{Title: "T", Content: content, ContentHash: "same-hash"}
	indexed := &store.IndexState{ContentHash: "same-hash"}

	snip := BuildSnippet(hit, entry, indexed, "café 日本語 pastries")

	if !utf8.ValidString(snip.Text) {
		t.Fatalf("snippet text is not valid UTF-8: %q", snip.Text)
	}

	got := highlightedText(snip.Text, snip.Highlights)
	if !containsString(got, "café") {
		t.Fatalf("highlights = %v, want a span covering %q", got, "café")
	}
	if !containsString(got, "日本語") {
		t.Fatalf("highlights = %v, want a span covering %q", got, "日本語")
	}
	if !containsString(got, "pastries") {
		t.Fatalf("highlights = %v, want a span covering %q", got, "pastries")
	}

	for _, h := range snip.Highlights {
		if !utf8.RuneStart(snip.Text[h.Start]) {
			t.Fatalf("highlight %+v starts mid-rune in %q", h, snip.Text)
		}
		if h.End < len(snip.Text) && !utf8.RuneStart(snip.Text[h.End]) {
			t.Fatalf("highlight %+v ends mid-rune in %q", h, snip.Text)
		}
	}
}

// TestBuildSnippetFallsBackToStoredTextWhenNoTermRelocates covers the
// last-resort degraded path: content hash mismatched AND none of the
// query's terms can be found anywhere in the freshly re-derived source
// (the entry was rewritten so thoroughly that even a fresh search finds
// nothing). BuildSnippet must still return something displayable — the
// passage's own already-stored Text — rather than an empty result or a
// panic, and it must carry no highlights rather than a guessed one.
func TestBuildSnippetFallsBackToStoredTextWhenNoTermRelocates(t *testing.T) {
	content := "<p>Completely different words now, unrelated to anything.</p>"

	hit := PassageHit{
		Source:    "content",
		Text:      "brown fox jumps",
		CharStart: 0,
		CharEnd:   9999,
	}
	entry := store.Entry{Title: "T", Content: content, ContentHash: "new-hash"}
	indexed := &store.IndexState{ContentHash: "old-hash"}

	snip := BuildSnippet(hit, entry, indexed, "zzyzxnonexistentqueryterm")

	if snip.Text != hit.Text {
		t.Fatalf("Text = %q, want the stored passage text %q as a last resort", snip.Text, hit.Text)
	}
	if len(snip.Highlights) != 0 {
		t.Fatalf("Highlights = %v, want none when nothing could be safely located", snip.Highlights)
	}
}
