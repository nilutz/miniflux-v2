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

// SplitOptions controls passage granularity. Sizes are in approximate tokens,
// estimated as words — close enough for chunking, and far cheaper than running
// the real tokenizer over the whole corpus twice.
type SplitOptions struct {
	TargetTokens  int // aim for this many tokens per passage
	MaxTokens     int // never exceed this
	OverlapTokens int // repeat this much of the previous passage
}

// DefaultSplitOptions returns the sizes used for indexing.
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

	var passages []Passage
	i := 0
	for i < len(sentences) {
		groupStart := i
		j := groupStart
		tokens := 0
		for j < len(sentences) {
			stoks := wordCount(text[sentences[j].start:sentences[j].end])
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
			overlapTokens += wordCount(text[sentences[newStart].start:sentences[newStart].end])
			newStart--
		}
		i = newStart + 1
	}

	return passages
}

func wordCount(s string) int {
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
