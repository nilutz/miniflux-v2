// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package passage // import "miniflux.app/v2/sidecar/internal/passage"

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSplitOffsetsRoundTripExactly(t *testing.T) {
	text := strings.TrimSpace(strings.Repeat("This is a sentence about search. ", 200))

	for _, p := range Split(text, DefaultSplitOptions()) {
		if p.CharStart < 0 || p.CharEnd > len(text) || p.CharStart >= p.CharEnd {
			t.Fatalf("offsets out of range: %d..%d (len %d)", p.CharStart, p.CharEnd, len(text))
		}
		if got := text[p.CharStart:p.CharEnd]; got != p.Text {
			t.Fatalf("offset mismatch:\n slice: %q\n  text: %q", got, p.Text)
		}
	}
}

func TestSplitCoversTheWholeText(t *testing.T) {
	text := strings.TrimSpace(strings.Repeat("Sentences make up a document. ", 200))

	passages := Split(text, DefaultSplitOptions())
	if len(passages) < 2 {
		t.Fatalf("expected several passages, got %d", len(passages))
	}
	if passages[0].CharStart != 0 {
		t.Fatalf("first passage should start at 0, got %d", passages[0].CharStart)
	}
	if last := passages[len(passages)-1]; last.CharEnd != len(text) {
		t.Fatalf("last passage should end at %d, got %d", len(text), last.CharEnd)
	}
}

func TestSplitConsecutivePassagesOverlap(t *testing.T) {
	text := strings.TrimSpace(strings.Repeat("Overlap keeps context across boundaries. ", 200))

	passages := Split(text, DefaultSplitOptions())
	if len(passages) < 2 {
		t.Fatalf("expected several passages, got %d", len(passages))
	}
	for i := 1; i < len(passages); i++ {
		if passages[i].CharStart >= passages[i-1].CharEnd {
			t.Fatalf("passage %d does not overlap its predecessor", i)
		}
	}
}

func TestSplitShortTextYieldsOnePassage(t *testing.T) {
	text := "A single short sentence."

	passages := Split(text, DefaultSplitOptions())
	if len(passages) != 1 {
		t.Fatalf("expected 1 passage, got %d", len(passages))
	}
	if passages[0].Text != text {
		t.Fatalf("got %q", passages[0].Text)
	}
}

func TestSplitEmptyTextYieldsNothing(t *testing.T) {
	if got := Split("", DefaultSplitOptions()); len(got) != 0 {
		t.Fatalf("expected no passages, got %d", len(got))
	}
	if got := Split("   \n  ", DefaultSplitOptions()); len(got) != 0 {
		t.Fatalf("whitespace-only text should yield no passages, got %d", len(got))
	}
}

func TestSplitBreaksOnSentenceBoundaries(t *testing.T) {
	text := strings.TrimSpace(strings.Repeat("Alpha beta gamma delta. ", 300))

	for _, p := range Split(text, DefaultSplitOptions()) {
		trimmed := strings.TrimSpace(p.Text)
		if trimmed == "" {
			t.Fatal("passage is blank")
		}
		// Every passage but the last should end at a sentence terminator.
		if p.CharEnd < len(text) && !strings.HasSuffix(trimmed, ".") {
			t.Fatalf("passage does not end on a sentence boundary: %q", trimmed)
		}
	}
}

func TestSplitHandlesMultiByteUTF8Offsets(t *testing.T) {
	// Beyond the brief: sentences packed with multi-byte UTF-8 runes (café,
	// naïve, Zürich, 日本語) exercise the same offset machinery as the ASCII
	// round-trip test above, but only this one would catch a byte offset
	// landing mid-rune.
	sentence := "The café in Zürich serves naïve travelers 日本語 pastries. "
	text := strings.TrimSpace(strings.Repeat(sentence, 200))

	passages := Split(text, DefaultSplitOptions())
	if len(passages) < 2 {
		t.Fatalf("expected several passages, got %d", len(passages))
	}
	for _, p := range passages {
		if !utf8.ValidString(p.Text) {
			t.Fatalf("passage text is not valid UTF-8: %q", p.Text)
		}
		if got := text[p.CharStart:p.CharEnd]; got != p.Text {
			t.Fatalf("offset mismatch:\n slice: %q\n  text: %q", got, p.Text)
		}
		if !utf8.RuneStart(text[p.CharStart]) {
			t.Fatalf("CharStart %d does not fall on a rune boundary", p.CharStart)
		}
		if p.CharEnd < len(text) && !utf8.RuneStart(text[p.CharEnd]) {
			t.Fatalf("CharEnd %d does not fall on a rune boundary", p.CharEnd)
		}
	}
	if passages[0].CharStart != 0 {
		t.Fatalf("first passage should start at 0, got %d", passages[0].CharStart)
	}
	if last := passages[len(passages)-1]; last.CharEnd != len(text) {
		t.Fatalf("last passage should end at %d, got %d", len(text), last.CharEnd)
	}
}
