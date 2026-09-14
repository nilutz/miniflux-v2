// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package embed // import "miniflux.app/v2/sidecar/internal/embed"

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/knights-analytics/hugot"
	"github.com/knights-analytics/hugot/options"
	"github.com/knights-analytics/hugot/pipelines"
)

// dimensions is the fixed output width of the pinned bge-small-en-v1.5
// model. It is not derived at runtime: this package is validated against
// exactly this model (see TestEmbedSeparatesParaphraseFromUnrelated), and
// swapping models is a decision that needs re-validating, not a config knob.
const dimensions = 384

// ONNXConfig configures an ONNX-backed Embedder.
type ONNXConfig struct {
	// ModelPath is the filesystem path to the quantized ONNX model file
	// itself (e.g. ".../bge-small-en-v1.5/onnx/model_quantized.onnx"). The
	// surrounding model directory — tokenizer.json, config.json and
	// friends, which hugot requires alongside the .onnx file — is located
	// by walking up from this path; see resolveModelRoot.
	ModelPath string

	// ONNXLibraryDir is the directory containing the native ONNX Runtime
	// shared library (libonnxruntime.dylib on macOS, .so on Linux). hugot
	// defaults to /usr/lib, a Linux-only path, so this must be set
	// explicitly on macOS. Ignored when empty.
	ONNXLibraryDir string
}

// onnxEmbedder is an Embedder backed by a hugot ONNX Runtime session running
// the pinned bge-small-en-v1.5 model. One session and one pipeline are
// shared across every Embed call: the spike measured its best throughput
// (33.9 passages/sec) with two goroutines calling RunPipeline concurrently
// against a single unconstrained session, so Embed is safe to call from
// multiple goroutines and deliberately does no additional locking of its
// own — the controller in a later task depends on that.
type onnxEmbedder struct {
	session  *hugot.Session
	pipeline *pipelines.FeatureExtractionPipeline
}

// NewONNX creates an Embedder that runs the bge-small-en-v1.5 model through
// ONNX Runtime via hugot.
//
// This requires a binary built with -tags ORT. Without that tag,
// hugot.NewORTSession itself returns an error rather than silently
// substituting a different backend — the silent-substitution case this
// package guards against is a plain, tagless `go build` of the *sidecar*
// binary, which BackendName and TestBackendIsORT catch (spec §6.7).
func NewONNX(cfg ONNXConfig) (Embedder, error) {
	modelRoot, onnxFilename := resolveModelRoot(cfg.ModelPath)

	var sessionOpts []options.WithOption
	if cfg.ONNXLibraryDir != "" {
		sessionOpts = append(sessionOpts, options.WithOnnxLibraryPath(cfg.ONNXLibraryDir))
	}

	// Deliberately not setting WithIntraOpNumThreads / WithInterOpNumThreads
	// here. The spike measured 33.9 passages/sec leaving ORT's default
	// threading unconstrained, against 11.7 when following hugot's own
	// README advice of pinning each worker to one intra-op/one inter-op
	// thread — three times slower. Do not add thread pinning without
	// re-measuring on the target hardware first.
	session, err := hugot.NewORTSession(context.Background(), sessionOpts...)
	if err != nil {
		return nil, fmt.Errorf("embed: create ORT session: %w", err)
	}

	pipelineConfig := hugot.FeatureExtractionConfig{
		ModelPath:    modelRoot,
		Name:         "sidecar-embedder",
		OnnxFilename: onnxFilename,
		Options: []hugot.FeatureExtractionOption{
			pipelines.WithNormalization(),
		},
	}
	pipeline, err := hugot.NewPipeline(session, pipelineConfig)
	if err != nil {
		_ = session.Destroy()
		return nil, fmt.Errorf("embed: create feature-extraction pipeline: %w", err)
	}

	slog.Info("sidecar: embedder ready",
		slog.String("backend", BackendName()),
		slog.String("model_root", modelRoot),
		slog.String("onnx_filename", onnxFilename),
		slog.Int("dimensions", dimensions),
	)

	return &onnxEmbedder{session: session, pipeline: pipeline}, nil
}

// resolveModelRoot walks up from an ONNX model file to the directory hugot
// expects as ModelPath: the one containing tokenizer.json alongside the
// onnx/ subdirectory the model file itself lives in. This lets callers
// (and SIDECAR_MODEL_PATH) name the .onnx file directly instead of having
// to know hugot's directory-layout convention.
func resolveModelRoot(onnxPath string) (root, onnxFilename string) {
	onnxFilename = filepath.Base(onnxPath)
	dir := filepath.Dir(onnxPath)

	for range 3 {
		if _, err := os.Stat(filepath.Join(dir, "tokenizer.json")); err == nil {
			return dir, onnxFilename
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}

	// No ancestor had tokenizer.json; fall back to the file's own directory
	// and let hugot produce the real error.
	return filepath.Dir(onnxPath), onnxFilename
}

// Embed implements Embedder.
func (e *onnxEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}

	result, err := e.pipeline.RunPipeline(ctx, texts)
	if err != nil {
		return nil, fmt.Errorf("embed: run pipeline: %w", err)
	}
	return result.Embeddings, nil
}

// Dimensions implements Embedder.
func (e *onnxEmbedder) Dimensions() int { return dimensions }

// Close implements Embedder.
func (e *onnxEmbedder) Close() error { return e.session.Destroy() }
