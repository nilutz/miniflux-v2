// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package onnx // import "miniflux.app/v2/sidecar/internal/embed/onnx"

import (
	"os"
	"path/filepath"
	"testing"
)

// TestResolveModelRoot exercises resolveModelRoot directly. It needs no
// SIDECAR_MODEL_PATH, no CGO and no ORT session — only os and path/filepath
// — so it runs unconditionally in the default hermetic suite. A regression
// here should fail loudly at this level rather than surface only as a
// confusing hugot error deep inside a model-gated test that ordinary CI
// skips.
func TestResolveModelRoot(t *testing.T) {
	writeTokenizer := func(t *testing.T, dir string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, "tokenizer.json"), []byte("{}"), 0o644); err != nil {
			t.Fatalf("write tokenizer.json: %v", err)
		}
	}

	t.Run("model file beside tokenizer.json", func(t *testing.T) {
		root := t.TempDir()
		writeTokenizer(t, root)
		onnxPath := filepath.Join(root, "model.onnx")

		gotRoot, gotFile := resolveModelRoot(onnxPath)
		if gotRoot != root {
			t.Errorf("root = %q, want %q", gotRoot, root)
		}
		if gotFile != "model.onnx" {
			t.Errorf("onnxFilename = %q, want %q", gotFile, "model.onnx")
		}
	})

	t.Run("model one directory below tokenizer.json (the real onnx/ layout)", func(t *testing.T) {
		root := t.TempDir()
		writeTokenizer(t, root)
		onnxDir := filepath.Join(root, "onnx")
		if err := os.Mkdir(onnxDir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		onnxPath := filepath.Join(onnxDir, "model_quantized.onnx")

		gotRoot, gotFile := resolveModelRoot(onnxPath)
		if gotRoot != root {
			t.Errorf("root = %q, want %q", gotRoot, root)
		}
		if gotFile != "model_quantized.onnx" {
			t.Errorf("onnxFilename = %q, want %q", gotFile, "model_quantized.onnx")
		}
	})

	t.Run("model two directories below tokenizer.json", func(t *testing.T) {
		root := t.TempDir()
		writeTokenizer(t, root)
		nested := filepath.Join(root, "variant", "onnx")
		if err := os.MkdirAll(nested, 0o755); err != nil {
			t.Fatalf("mkdir all: %v", err)
		}
		onnxPath := filepath.Join(nested, "model.onnx")

		gotRoot, gotFile := resolveModelRoot(onnxPath)
		if gotRoot != root {
			t.Errorf("root = %q, want %q", gotRoot, root)
		}
		if gotFile != "model.onnx" {
			t.Errorf("onnxFilename = %q, want %q", gotFile, "model.onnx")
		}
	})

	t.Run("no tokenizer.json anywhere up the chain falls back to the file's own directory", func(t *testing.T) {
		root := t.TempDir()
		onnxDir := filepath.Join(root, "onnx")
		if err := os.Mkdir(onnxDir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		onnxPath := filepath.Join(onnxDir, "model.onnx")

		gotRoot, gotFile := resolveModelRoot(onnxPath)
		if gotRoot != onnxDir {
			t.Errorf("root = %q, want fallback %q", gotRoot, onnxDir)
		}
		if gotFile != "model.onnx" {
			t.Errorf("onnxFilename = %q, want %q", gotFile, "model.onnx")
		}
	})

	t.Run("path at the filesystem root terminates via the parent==dir guard", func(t *testing.T) {
		// "/model.onnx" puts dir at "/" on the first check; filepath.Dir("/")
		// is "/" again, so the loop must hit the parent==dir break on its
		// first iteration rather than trying to climb past the root. The
		// fixed 3-iteration loop bound means this can never hang, but the
		// early break is still the behaviour under test: without it this
		// would keep re-checking "/" for the full 3 iterations instead of
		// stopping as soon as it's clear there's nowhere further to go.
		gotRoot, gotFile := resolveModelRoot("/model.onnx")
		if gotRoot != "/" {
			t.Errorf("root = %q, want %q", gotRoot, "/")
		}
		if gotFile != "model.onnx" {
			t.Errorf("onnxFilename = %q, want %q", gotFile, "model.onnx")
		}
	})
}
