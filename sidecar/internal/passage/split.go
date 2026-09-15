// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package passage // import "miniflux.app/v2/sidecar/internal/passage"

import "strings"

// Passage is a contiguous slice of a plaintext article. CharStart and
// CharEnd are byte offsets into the string that was passed to Split, and
// Text is always exactly that slice — text[CharStart:CharEnd] == Text holds
// by construction, never by luck, since Text is never rebuilt by
// concatenation. A later phase uses these offsets to highlight the matched
// passage inside the original article, so an off-by-one here would silently
// highlight the wrong words.
type Passage struct {
	Text      string
	CharStart int
	CharEnd   int
}

// TokenCounter measures how many tokens a single sentence costs against
// TargetTokens/MaxTokens/OverlapTokens below. Split calls it once per
// sentence and only ever sums and compares the results — it never
// interprets what "token" means beyond that arithmetic, so any counter
// that consistently answers the same question for the same input is safe
// to inject.
//
// DefaultSplitOptions leaves this nil, which makes Split fall back to
// estimateTokens — a word count, not a real token count; see its own doc
// comment for the ratio and its error bars. That default is a cheap,
// day-to-day budget, not a hard ceiling. Inject a counter backed by the
// actual embedding model's real tokenizer wherever MaxTokens has to be a
// genuine guarantee against a model's real context window rather than an
// estimate of one — doing so via an injected function value keeps this
// package free of any tokenizer import.
//
// internal/passage cannot supply a real TokenCounter itself: the actual
// tokenizer lives behind internal/embed/onnx, which links ~37MB of
// native libtokenizers.a / ONNX Runtime, and this package exists
// specifically so packages that never do inference — this one included —
// never have to link that (see the package doc comment). A real counter
// is therefore always built by a caller that already pays that cost and
// handed in here as a plain function value — see cmd/sidecar's
// TestSplitPassageAtCapNeverExceedsNomicsRealTokenLimit for the one that
// actually proves a passage at the cap fits the model's real limit.
type TokenCounter func(sentence string) int

// SplitOptions controls passage granularity. TargetTokens/MaxTokens/
// OverlapTokens are counted by TokenCounter — see its own doc comment for
// what "tokens" means when TokenCounter is left nil, as
// DefaultSplitOptions leaves it.
type SplitOptions struct {
	TargetTokens  int // aim for this many tokens per passage
	MaxTokens     int // never exceed this
	OverlapTokens int // repeat this much of the previous passage

	// TokenCounter is how the three fields above are actually measured.
	// Nil (DefaultSplitOptions' value) means "estimated by word count",
	// not "exactly counted" — see TokenCounter's own doc comment.
	TokenCounter TokenCounter
}

// DefaultSplitOptions returns the sizes used for indexing.
//
// These are still WORD counts by default (TokenCounter left nil), not
// real token counts — see estimateTokens for the ratio and its error
// bars. Retuning these values for nomic-embed-text-v1.5's 8192-token
// context is a measurement to make with cmd/sidecar's TestChunkingSweep,
// not a guess to make here; they have not been retuned yet.
func DefaultSplitOptions() SplitOptions {
	return SplitOptions{TargetTokens: 320, MaxTokens: 512, OverlapTokens: 64}
}

// sentenceSpan is a byte range [start, end) within the source text: from the
// first non-whitespace byte of a sentence up to and including its
// terminator, with no trailing whitespace.
type sentenceSpan struct {
	start, end int
}

// Split breaks text into overlapping passages. Sentence boundaries (`.`,
// `!`, `?` followed by whitespace or end of string) are found first, then
// sentences are accumulated into a passage until TargetTokens is reached
// (never exceeding MaxTokens), after which the next passage backs up
// OverlapTokens worth of sentences and continues. A single sentence longer
// than MaxTokens is emitted alone rather than split mid-sentence. Passage
// text is always sliced directly from text, never rebuilt, so offsets
// round-trip exactly.
func Split(text string, opts SplitOptions) []Passage {
	sentences := findSentences(text)
	if len(sentences) == 0 {
		return nil
	}

	// count is the TokenCounter actually used for this call: the one the
	// caller injected, or estimateTokens (the word-based proxy) when they
	// left TokenCounter nil. Resolved once, here, so every sentence in
	// this call is measured the same way rather than risking a nil
	// dereference deeper in the loop below.
	count := opts.TokenCounter
	if count == nil {
		count = estimateTokens
	}

	var passages []Passage
	i := 0
	for i < len(sentences) {
		groupStart := i
		j := groupStart
		tokens := 0
		for j < len(sentences) {
			stoks := count(text[sentences[j].start:sentences[j].end])
			if j > groupStart && tokens+stoks > opts.MaxTokens {
				break
			}
			tokens += stoks
			j++
			if tokens >= opts.TargetTokens {
				break
			}
		}
		groupEnd := j - 1

		passages = append(passages, Passage{
			Text:      text[sentences[groupStart].start:sentences[groupEnd].end],
			CharStart: sentences[groupStart].start,
			CharEnd:   sentences[groupEnd].end,
		})

		if j >= len(sentences) {
			break
		}

		// Step back OverlapTokens worth of sentences from the end of the
		// group just emitted, so the next passage repeats that much
		// context. newStart lands on the last sentence NOT included in the
		// overlap; the next group starts right after it.
		newStart := groupEnd
		overlapTokens := 0
		for newStart > groupStart {
			if overlapTokens >= opts.OverlapTokens {
				break
			}
			overlapTokens += count(text[sentences[newStart].start:sentences[newStart].end])
			newStart--
		}
		i = newStart + 1
	}

	return passages
}

// estimateTokens is the default TokenCounter: a plain word count, used as
// a cheap proxy for what a real subword tokenizer would report. For
// English prose one word averages roughly 1.3 real BERT/WordPiece
// tokens, so this UNDER-counts real tokens by roughly that factor on
// ordinary prose, and can under-count far more sharply on text with many
// rare or non-dictionary "words" (each one costing several WordPiece
// subword tokens instead of close to one). It is a rough, cheap proxy,
// never a substitute for actually asking a tokenizer — see TokenCounter's
// own doc comment for how to inject one that is exact.
func estimateTokens(s string) int {
	return len(strings.Fields(s))
}

func isSpaceByte(b byte) bool {
	switch b {
	case ' ', '\t', '\n', '\r', '\v', '\f':
		return true
	default:
		return false
	}
}

// findSentences locates sentence spans in text. Splitting only ever occurs
// right after an ASCII '.', '!' or '?' byte, which is always a safe UTF-8
// rune boundary — continuation and lead bytes of multi-byte runes are all
// >= 0x80 and can never be mistaken for these terminators or for ASCII
// whitespace, so this byte-oriented scan is UTF-8 safe without decoding.
func findSentences(text string) []sentenceSpan {
	n := len(text)

	i := 0
	for i < n && isSpaceByte(text[i]) {
		i++
	}
	start := i

	var spans []sentenceSpan
	for i < n {
		c := text[i]
		if (c == '.' || c == '!' || c == '?') && (i+1 == n || isSpaceByte(text[i+1])) {
			end := i + 1
			spans = append(spans, sentenceSpan{start: start, end: end})
			i = end
			for i < n && isSpaceByte(text[i]) {
				i++
			}
			start = i
			continue
		}
		i++
	}

	// Trailing text with no terminator still counts as one final sentence.
	if start < n {
		end := n
		for end > start && isSpaceByte(text[end-1]) {
			end--
		}
		if end > start {
			spans = append(spans, sentenceSpan{start: start, end: end})
		}
	}

	return spans
}
