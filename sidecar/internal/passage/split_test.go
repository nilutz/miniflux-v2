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
	// Sentences packed with multi-byte UTF-8 runes (café, naïve, Zürich,
	// 日本語) exercise the same offset machinery as the ASCII round-trip
	// test above, but only this one would catch a byte offset landing
	// mid-rune.
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

// TestSplitHonoursInjectedTokenCounter proves Split actually consults
// opts.TokenCounter when one is set, rather than silently falling back to
// the word-based estimate regardless of what was injected — the failure
// mode a "the units are honest now" claim would be cosmetic without a
// test that discriminates against it.
//
// This package cannot exercise a real tokenizer-backed TokenCounter
// itself (see TokenCounter's own doc comment for why); this test proves
// the general mechanism with a synthetic one, hermetically. See
// cmd/sidecar's TestSplitPassageAtCapNeverExceedsNomicsRealTokenLimit for
// the counterpart that injects the real nomic tokenizer and proves a
// passage at the cap does not exceed the model's real token limit.
func TestSplitHonoursInjectedTokenCounter(t *testing.T) {
	// Every "sentence" is the single word "Word" plus its terminator, so
	// the word-based default estimator (one word = one "token") would
	// count each sentence as costing 1 token — and, at MaxTokens=40,
	// would happily pack all 100 into one passage. heavyCounter instead
	// reports 10 tokens per sentence, simulating a real subword tokenizer
	// finding far more tokens in dense or rare text than a word count
	// would ever suggest. The two disagree sharply on cost per sentence
	// while agreeing on sentence count, which is exactly what makes this
	// test able to tell "Split used my counter" apart from "Split used
	// the word estimate and got lucky".
	text := strings.TrimSpace(strings.Repeat("Word. ", 100))

	var calls int
	heavyCounter := func(s string) int {
		calls++
		return 10
	}

	opts := SplitOptions{TargetTokens: 30, MaxTokens: 40, OverlapTokens: 10, TokenCounter: heavyCounter}
	passages := Split(text, opts)

	if calls == 0 {
		t.Fatal("TokenCounter was never called — Split is not using the injected counter at all")
	}
	if len(passages) < 2 {
		t.Fatalf("expected several passages (100 sentences at 10 tokens each, capped at 40), got %d", len(passages))
	}

	// MaxTokens=40 at 10 tokens/sentence permits at most 4 sentences per
	// passage. Under the word-based default (1 token/sentence for this
	// text) the same text would pack far more than 4 per passage — a
	// regression back to the word estimate would show up here as
	// oversized passages, not as a crash.
	const maxSentencesPerPassage = 4
	for i, p := range passages {
		if n := strings.Count(p.Text, "."); n > maxSentencesPerPassage {
			t.Fatalf("passage %d has %d sentences (%d tokens under the injected counter), exceeding MaxTokens=%d — "+
				"Split is not enforcing the cap using the injected TokenCounter:\n%q",
				i, n, n*10, opts.MaxTokens, p.Text)
		}
	}
}
