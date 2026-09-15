// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package passage // import "miniflux.app/v2/sidecar/internal/passage"

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"testing"
)

// pipelineFixtures is a small corpus chosen to touch every part of the
// derivation pipeline whose output search.passages stores: block-element
// separation, skipped subtrees, entity decoding, whitespace collapsing,
// sentence-boundary splitting and overlap.
var pipelineFixtures = []string{
	"<p>First sentence here. Second sentence follows it.</p><div>Third block.</div>",
	"<h1>Title</h1><p>Body&nbsp;text with a non-breaking space. And more.</p><script>ignored()</script>",
	"<ul><li>One.</li><li>Two.</li></ul><blockquote>Quoted material, at some length, to be split.</blockquote>",
	"<p>" + longFixtureBody() + "</p>",
}

func longFixtureBody() string {
	var out string
	for i := 0; i < 120; i++ {
		out += fmt.Sprintf("Sentence number %d carries enough words to push the splitter past a passage boundary. ", i)
	}
	return out
}

// TestPipelineOutputDigestIsStable pins the exact output of ExtractText +
// Split over a fixed corpus.
//
// It is not here to assert the pipeline is *correct* — the extraction and
// splitting tests next to it do that. It is here to fail loudly when the
// pipeline's output CHANGES, because search.passages stores char_start /
// char_end offsets into a plaintext that is never persisted anywhere: it
// has to be re-derived by calling ExtractText again. A change to either
// function therefore silently invalidates every stored offset and passage
// boundary in the corpus, with the entries' own HTML — and so their
// content hash — completely unchanged.
//
// If this test fails because you deliberately changed the pipeline, that
// is fine and expected: bump store.PipelineVersion (internal/store/
// entries.go) in the SAME change, which folds into the recorded content
// hash and re-offers the whole corpus for re-indexing, then update the
// digest below.
func TestPipelineOutputDigestIsStable(t *testing.T) {
	const wantDigest = "2da0a12371619d2f8b8d50e50bdd7f59b5ffa1096221d82153262604922c3806"

	h := sha256.New()
	for _, input := range pipelineFixtures {
		text := ExtractText(input)
		fmt.Fprintf(h, "TEXT\x00%s\x00", text)
		for _, p := range Split(text, DefaultSplitOptions()) {
			fmt.Fprintf(h, "P\x00%d\x00%d\x00%s\x00", p.CharStart, p.CharEnd, p.Text)
		}
	}
	got := hex.EncodeToString(h.Sum(nil))

	if got != wantDigest {
		t.Fatalf("the extraction/splitting pipeline's output changed.\n"+
			"  got  digest %s\n  want digest %s\n"+
			"If this change was deliberate, bump store.PipelineVersion in the same commit "+
			"(otherwise every already-indexed entry keeps offsets into a plaintext this "+
			"pipeline no longer produces) and update wantDigest above.", got, wantDigest)
	}
}
