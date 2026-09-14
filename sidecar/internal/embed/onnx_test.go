// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package embed // import "miniflux.app/v2/sidecar/internal/embed"

import (
	"context"
	"math"
	"os"
	"testing"
)

// TestBackendIsORT fails when the binary was built without -tags ORT, which
// silently substitutes a far slower backend instead of erroring.
func TestBackendIsORT(t *testing.T) {
	if got := BackendName(); got != "ORT" {
		t.Fatalf("built with the %q backend; rebuild with -tags ORT (see spec §6.7)", got)
	}
}

func testEmbedder(t *testing.T) Embedder {
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

	vectors, err := e.Embed(context.Background(), []string{"hello world"})
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

	vectors, err := e.Embed(context.Background(), []string{
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

	vectors, err := e.Embed(context.Background(), nil)
	if err != nil {
		t.Fatalf("an empty batch should be a no-op, got error: %v", err)
	}
	if len(vectors) != 0 {
		t.Fatalf("expected no vectors, got %d", len(vectors))
	}
}
