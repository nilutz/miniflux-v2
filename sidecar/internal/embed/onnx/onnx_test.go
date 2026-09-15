// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package onnx // import "miniflux.app/v2/sidecar/internal/embed/onnx"

import (
	"context"
	"math"
	"os"
	"testing"

	"miniflux.app/v2/sidecar/internal/embed"
)

// TestBackendIsORT fails when the binary was built without -tags ORT, which
// silently substitutes a far slower backend instead of erroring.
func TestBackendIsORT(t *testing.T) {
	if got := BackendName(); got != "ORT" {
		t.Fatalf("built with the %q backend; rebuild with -tags ORT (see spec §6.7)", got)
	}
}

func testEmbedder(t *testing.T) embed.Embedder {
	t.Helper()

	modelPath := os.Getenv("SIDECAR_MODEL_PATH")
	if modelPath == "" {
		t.Skip("SIDECAR_MODEL_PATH is not set, skipping model test")
	}

	e, err := NewONNX(ONNXConfig{
		ModelPath:      modelPath,
		ONNXLibraryDir: os.Getenv("SIDECAR_ONNX_LIB_DIR"),
	})
	if err != nil {
		t.Fatalf("unable to create embedder: %v", err)
	}
	t.Cleanup(func() { e.Close() })

	return e
}

func cosine(a, b []float32) float64 {
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

func TestEmbedReturns384Dimensions(t *testing.T) {
	e := testEmbedder(t)

	vectors, err := e.EmbedDocuments(context.Background(), []string{"hello world"})
	if err != nil {
		t.Fatalf("embed failed: %v", err)
	}
	if len(vectors) != 1 {
		t.Fatalf("expected 1 vector, got %d", len(vectors))
	}
	if len(vectors[0]) != 384 {
		t.Fatalf("expected 384 dimensions, got %d", len(vectors[0]))
	}
	if e.Dimensions() != 384 {
		t.Fatalf("expected Dimensions() == 384, got %d", e.Dimensions())
	}
}

// TestEmbedSeparatesParaphraseFromUnrelated is the sanity check from spec §6.7.
// It catches a wrong model, wrong pooling, or missing normalisation — all of
// which yield vectors that look fine but rank meaninglessly.
func TestEmbedSeparatesParaphraseFromUnrelated(t *testing.T) {
	e := testEmbedder(t)

	vectors, err := e.EmbedDocuments(context.Background(), []string{
		"The cat sat quietly on the warm windowsill in the afternoon sun.",
		"A cat was resting peacefully on the sunny windowsill that afternoon.",
		"The stock market fell sharply after the central bank raised interest rates.",
	})
	if err != nil {
		t.Fatalf("embed failed: %v", err)
	}

	paraphrase := cosine(vectors[0], vectors[1])
	unrelated := cosine(vectors[0], vectors[2])

	// The spike measured 0.87 against 0.31. A margin of 0.2 catches a broken
	// pipeline without being brittle about model revisions.
	if paraphrase <= unrelated+0.2 {
		t.Fatalf("paraphrase %.4f should clearly exceed unrelated %.4f", paraphrase, unrelated)
	}
}

func TestEmbedRejectsEmptyBatch(t *testing.T) {
	e := testEmbedder(t)

	vectors, err := e.EmbedDocuments(context.Background(), nil)
	if err != nil {
		t.Fatalf("an empty batch should be a no-op, got error: %v", err)
	}
	if len(vectors) != 0 {
		t.Fatalf("expected no vectors, got %d", len(vectors))
	}
}

// --- promptPrefixesForModel: hermetic, no model/CGO required -------------

// TestPromptPrefixesForModel pins the lookup table itself: nomic gets its
// two documented prefixes, bge-small (and anything else unrecognized) gets
// neither. This runs unconditionally — no SIDECAR_MODEL_PATH, no ORT
// session — so a table edit that breaks either case fails immediately in
// the default hermetic suite, rather than only in a model-gated test
// everyday CI skips.
func TestPromptPrefixesForModel(t *testing.T) {
	nomic := promptPrefixesForModel("nomic-embed-text-v1.5")
	if nomic.document != "search_document: " || nomic.query != "search_query: " {
		t.Fatalf("nomic-embed-text-v1.5: got prefixes %+v, want document=%q query=%q",
			nomic, "search_document: ", "search_query: ")
	}

	for _, name := range []string{"bge-small-en-v1.5", "", "some-future-model-nobody-added-yet"} {
		got := promptPrefixesForModel(name)
		if got != (promptPrefixes{}) {
			t.Fatalf("%q: expected no prefixes for an unlisted model, got %+v", name, got)
		}
	}
}

// TestWithPrefixDoesNotMutateCallersSlice guards the one correctness
// property withPrefix's own doc comment promises beyond string
// concatenation: texts (the indexer's own batch slice, on the production
// path) must come back from EmbedDocuments/EmbedQuery unchanged, never
// rewritten in place.
func TestWithPrefixDoesNotMutateCallersSlice(t *testing.T) {
	texts := []string{"a", "b"}
	original := append([]string(nil), texts...)

	out := withPrefix("search_document: ", texts)

	if !equalStrings(texts, original) {
		t.Fatalf("withPrefix mutated the caller's slice: got %v, want unchanged %v", texts, original)
	}
	want := []string{"search_document: a", "search_document: b"}
	if !equalStrings(out, want) {
		t.Fatalf("withPrefix result = %v, want %v", out, want)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// --- nomic prompt-prefix wiring: model-gated, real tokenizer boundary ---

// nomicModelPath returns SIDECAR_NOMIC_MODEL_PATH, skipping the test if
// unset. This is deliberately a second, separate variable from
// SIDECAR_MODEL_PATH: this task (nomic migration plan, task 1) adds
// nomic's prefix table ahead of task 2's actual model swap, so the
// model under test everywhere else in this file stays bge-small — this
// test alone needs an actual nomic-embed-text-v1.5 model file on disk,
// at a path whose directory is named exactly "nomic-embed-text-v1.5"
// (promptPrefixesForModel keys off that directory basename, the same
// string embed.Identity's "name" component is built from).
func nomicModelPath(t *testing.T) string {
	t.Helper()
	path := os.Getenv("SIDECAR_NOMIC_MODEL_PATH")
	if path == "" {
		t.Skip("SIDECAR_NOMIC_MODEL_PATH is not set, skipping nomic prompt-prefix test")
	}
	return path
}

// TestEmbedAppliesDistinctPromptPrefixesForNomic is this task's flagship
// discrimination test, at the boundary that actually matters: not what a
// fake embedder was handed, but what reaches the real tokenizer. Rather
// than only comparing EmbedDocuments against EmbedQuery to each other
// (which a bug dropping BOTH prefixes identically would still pass —
// their outputs would still agree, just wrongly), it pins each method
// against the literal prefixed string it is supposed to send: it embeds
// the same sentence three ways through a real nomic-embed-text-v1.5
// session — via EmbedDocuments, via EmbedQuery, and via the package's own
// unexported embed() called directly with the prefix hand-written in —
// and asserts EmbedDocuments's output is bit-for-bit what embed() produces
// for "search_document: "+sentence, and EmbedQuery's is what embed()
// produces for "search_query: "+sentence. This is only possible from
// inside package onnx (a black-box test could not reach the unexported
// embed method), which is exactly why this file, not a public API
// consumer, is where the real-tokenizer proof belongs.
//
// If either public method's prefix is ever dropped, transposed, or
// hardcoded somewhere it doesn't apply, its output stops matching the
// hand-prefixed reference and this test fails loudly — the exact
// silent-degradation hazard this task exists to close, which otherwise
// has no symptom anywhere in production.
func TestEmbedAppliesDistinctPromptPrefixesForNomic(t *testing.T) {
	modelPath := nomicModelPath(t)

	e, err := NewONNX(ONNXConfig{
		ModelPath:      modelPath,
		ONNXLibraryDir: os.Getenv("SIDECAR_ONNX_LIB_DIR"),
	})
	if err != nil {
		t.Fatalf("unable to create embedder: %v", err)
	}
	t.Cleanup(func() { e.Close() })

	oe, ok := e.(*onnxEmbedder)
	if !ok {
		t.Fatalf("NewONNX returned %T, want *onnxEmbedder", e)
	}

	const sentence = "The cat sat quietly on the warm windowsill in the afternoon sun."

	gotDoc, err := oe.EmbedDocuments(context.Background(), []string{sentence})
	if err != nil {
		t.Fatalf("EmbedDocuments: %v", err)
	}
	wantDoc, err := oe.embed(context.Background(), []string{"search_document: " + sentence})
	if err != nil {
		t.Fatalf("embed(hand-prefixed document text): %v", err)
	}
	if !vectorsAlmostEqual(gotDoc[0], wantDoc[0]) {
		t.Fatalf("EmbedDocuments(sentence) does not match embed(%q): the document prefix was not "+
			"applied as expected", "search_document: "+sentence)
	}

	gotQuery, err := oe.EmbedQuery(context.Background(), sentence)
	if err != nil {
		t.Fatalf("EmbedQuery: %v", err)
	}
	wantQuery, err := oe.embed(context.Background(), []string{"search_query: " + sentence})
	if err != nil {
		t.Fatalf("embed(hand-prefixed query text): %v", err)
	}
	if !vectorsAlmostEqual(gotQuery, wantQuery[0]) {
		t.Fatalf("EmbedQuery(sentence) does not match embed(%q): the query prefix was not "+
			"applied as expected", "search_query: "+sentence)
	}

	// Belt-and-braces sanity check restating the spike's own finding
	// (0.8855): the two really are distinct vectors, not the same value
	// twice over.
	if got := cosine(gotDoc[0], gotQuery); got >= 0.999 {
		t.Fatalf("cosine(EmbedDocuments(x), EmbedQuery(x)) = %.6f, expected clearly below 1.0 "+
			"for genuinely different search_document:/search_query: prefixes", got)
	}
}

// vectorsAlmostEqual reports whether a and b are equal to within a tight
// per-element tolerance — not exact bit-for-bit equality, which would be
// too strict against any nondeterminism ONNX Runtime's multithreaded
// execution could in principle introduce across two separate RunPipeline
// calls, but tight enough that two calls embedding genuinely identical
// input text cannot be mistaken for two calls embedding different,
// differently-prefixed text (see the spike's measured 0.8855 for that
// case — many orders of magnitude looser than this).
func vectorsAlmostEqual(a, b []float32) bool {
	if len(a) != len(b) {
		return false
	}
	const epsilon = 1e-4
	for i := range a {
		diff := float64(a[i]) - float64(b[i])
		if diff < 0 {
			diff = -diff
		}
		if diff > epsilon {
			return false
		}
	}
	return true
}

// TestEmbedDocumentsAndEmbedQueryAgreeWithoutPrefixes is
// TestEmbedAppliesDistinctPromptPrefixesForNomic's counterpart for
// bge-small-en-v1.5 (the model actually configured as of this task): a
// model absent from modelPromptPrefixes must produce IDENTICAL vectors
// from EmbedDocuments and EmbedQuery for the same text, proving
// withPrefix's empty-prefix branch is a true no-op at the real tokenizer
// boundary, not just in TestPromptPrefixesForModel's pure-function
// check. If a future change hardcoded a prefix into the generic path
// instead of gating it by model — the mistake this task's brief
// explicitly warns against — this is the test that would catch it for
// the one model that must never see one.
func TestEmbedDocumentsAndEmbedQueryAgreeWithoutPrefixes(t *testing.T) {
	e := testEmbedder(t) // SIDECAR_MODEL_PATH: bge-small-en-v1.5, no prefixes

	const text = "A cat was resting peacefully on the sunny windowsill that afternoon."

	docVectors, err := e.EmbedDocuments(context.Background(), []string{text})
	if err != nil {
		t.Fatalf("EmbedDocuments: %v", err)
	}
	queryVector, err := e.EmbedQuery(context.Background(), text)
	if err != nil {
		t.Fatalf("EmbedQuery: %v", err)
	}

	got := cosine(docVectors[0], queryVector)
	if got < 0.999999 {
		t.Fatalf("cosine(EmbedDocuments(x), EmbedQuery(x)) = %.8f for a model with no prompt "+
			"prefixes, want ~1.0 (identical input, deterministic embedding)", got)
	}
}
