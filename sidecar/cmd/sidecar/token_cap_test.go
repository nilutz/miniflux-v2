// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build ORT

package main // import "miniflux.app/v2/sidecar/cmd/sidecar"

// This file requires -tags ORT because, unlike every other file in this
// package, it imports github.com/daulet/tokenizers DIRECTLY -- the exact
// native, CGO, ~37MB-libtokenizers.a dependency internal/passage exists
// specifically to keep uninvolved packages away from (see
// passage.TokenCounter's own doc comment). hugot itself only pulls that
// same import in under the identical `cgo && (ORT || XLA || ALL)`
// constraint (see hugot's backends/tokenizer_rust.go): without this file's
// own matching tag, a plain `go build ./...` / `go vet ./...` -- what the
// sidecar CI workflow runs, deliberately without -tags ORT, so it never
// needs libtokenizers.a to exist -- would break on a machine that has
// never built with -tags ORT at all.
//
// Run with (mirrors cmd/sidecar/eval_test.go's own invocation):
//
//	CGO_LDFLAGS="-L<dir-with-libtokenizers.a>" \
//	DYLD_LIBRARY_PATH=/opt/homebrew/opt/onnxruntime/lib \
//	SIDECAR_MODEL_PATH=<path-to-nomic-embed-text-v1.5/onnx/model_quantized.onnx> \
//	  go test -tags ORT ./cmd/sidecar/ -run TestSplitPassageAtCapNeverExceedsNomicsRealTokenLimit -v
//
// Skips (not fails) without SIDECAR_MODEL_PATH, exactly like every other
// model-backed test in this module.

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daulet/tokenizers"

	"miniflux.app/v2/sidecar/internal/passage"
)

// nomicRealTokenLimit is nomic-embed-text-v1.5's real context window --
// NOT config.json's misleading max_position_embeddings: 2048 (nomic
// migration plan, Global Constraints), but the tokenizer's own
// model_max_length and the rotary export's empirically confirmed ceiling.
// The migration's own spike pushed a passage to 8,120 real tokens with no
// truncation and no error (spike-nomic/FINDINGS.md, step 6) -- within ~1%
// of this number, not the 2048 config.json states.
const nomicRealTokenLimit = 8192

// documentPrefix is nomic's mandatory prefix for text being indexed
// (internal/embed/onnx's modelPromptPrefixes) -- included here because
// it's what actually reaches the tokenizer for a real passage, and the
// spike measured real token counts the identical way (main.go step 7:
// `tok.Encode("search_document: "+longPassage, true)`).
const documentPrefix = "search_document: "

// TestSplitPassageAtCapNeverExceedsNomicsRealTokenLimit is task 3's
// stated deliverable: proof that a passage Split produces at the cap does
// not exceed the model's REAL token limit -- not an estimate of it. It
// exercises passage.Split with a real TokenCounter backed by nomic's own
// tokenizer.json (nomic migration plan, task 3 brief; superseded task 13
// brief's "count real tokens" approach), the one internal/passage cannot
// build for itself (see passage.TokenCounter's doc comment for why, and
// TestSplitHonoursInjectedTokenCounter in internal/passage for the
// hermetic counterpart that proves the injection mechanism itself without
// a real tokenizer).
//
// The input text is deliberately adversarial: every "word" is a random,
// dictionary-free string, so WordPiece has no whole-token match for any
// of them and must fall back to several short subword pieces each --
// costing far more real tokens per word than ordinary English prose's
// ~1.3 (estimateTokens' own documented ratio). Split is configured with
// MaxTokens set to nomic's real 8192-token ceiling itself and TargetTokens
// comfortably below it, so this is not merely "the small default cap
// (512 words) stays safely under 8192 either way" (which would pass
// trivially regardless of whether the injected counter is honoured at
// all) -- it is the literal claim task 3 asks for: a passage AT THE CAP
// does not exceed the model's real limit. If Split silently used the
// word-based estimator instead of the injected real counter (the
// regression this test exists to catch -- see its own discrimination
// proof in the task report), packing "until ~8192 words" of this
// adversarial text would produce passages of tens of thousands of real
// tokens, comfortably caught by the assertions below.
func TestSplitPassageAtCapNeverExceedsNomicsRealTokenLimit(t *testing.T) {
	modelPath := os.Getenv("SIDECAR_MODEL_PATH")
	if modelPath == "" {
		t.Skip("SIDECAR_MODEL_PATH is not set, skipping model test")
	}

	tokenizerPath, err := findTokenizerJSON(modelPath)
	if err != nil {
		t.Fatalf("locating tokenizer.json next to %s: %v", modelPath, err)
	}

	tok, err := tokenizers.FromFile(tokenizerPath)
	if err != nil {
		t.Fatalf("loading tokenizer from %s: %v", tokenizerPath, err)
	}
	defer tok.Close()

	realTokenCount := func(s string) int {
		ids, _ := tok.Encode(documentPrefix+s, true)
		return len(ids)
	}

	text := adversarialText(800, 6) // ~4,800 gibberish "words" in ~800 short sentences

	opts := passage.SplitOptions{
		TargetTokens:  6000,
		MaxTokens:     nomicRealTokenLimit,
		OverlapTokens: 200,
		TokenCounter:  realTokenCount,
	}

	passages := passage.Split(text, opts)
	if len(passages) < 2 {
		t.Fatalf("expected several passages out of this much adversarial text, got %d -- test setup is not exercising the cap", len(passages))
	}

	for i, p := range passages {
		got := realTokenCount(p.Text)
		if got > nomicRealTokenLimit {
			t.Fatalf("passage %d: %d real tokens exceeds nomic-embed-text-v1.5's %d-token limit -- "+
				"this passage would be silently truncated at embed time with no error (text length %d bytes)",
				i, got, nomicRealTokenLimit, len(p.Text))
		}
		if got > opts.MaxTokens {
			t.Errorf("passage %d: %d real tokens exceeds configured MaxTokens=%d even though a real TokenCounter was injected -- "+
				"Split is not enforcing the cap using the counter it was given", i, got, opts.MaxTokens)
		}
	}
}

// findTokenizerJSON walks up from an ONNX model file to find tokenizer.json
// alongside it, mirroring internal/embed/onnx's own resolveModelRoot (kept
// unexported there, so duplicated here in miniature rather than exported
// just for this one test).
func findTokenizerJSON(onnxPath string) (string, error) {
	dir := filepath.Dir(onnxPath)
	for range 3 {
		candidate := filepath.Join(dir, "tokenizer.json")
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", fmt.Errorf("no tokenizer.json found within 3 directories above %s", onnxPath)
}

// adversarialText builds nSentences pseudo-random "sentences" of
// wordsPerSentence dictionary-free words each, deterministically (a fixed
// seed, so a failing run reproduces exactly). Every word is 8-16 random
// lowercase letters -- long enough, and unlike any real vocabulary entry,
// so a WordPiece tokenizer must decompose almost all of them into several
// subword tokens rather than matching a single whole-word token, which is
// exactly the gap between "one word" and "one token" this test needs to
// be wide open.
func adversarialText(nSentences, wordsPerSentence int) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz"
	rng := rand.New(rand.NewSource(42))

	var sb strings.Builder
	for s := 0; s < nSentences; s++ {
		for w := 0; w < wordsPerSentence; w++ {
			if w > 0 {
				sb.WriteByte(' ')
			}
			n := 8 + rng.Intn(9) // 8-16 letters
			for range n {
				sb.WriteByte(alphabet[rng.Intn(len(alphabet))])
			}
		}
		sb.WriteString(". ")
	}
	return strings.TrimSpace(sb.String())
}
