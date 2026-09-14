// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package embed // import "miniflux.app/v2/sidecar/internal/embed"

import (
	"context"
	"fmt"
)

// Embedder turns text into dense vectors. The ONNX implementation is the only
// one today; the interface exists so a local inference service or a pure-Go
// backend can be substituted without touching the pipeline (spec §6.6).
type Embedder interface {
	// Embed returns one vector per input text, each of Dimensions() length.
	// An empty input returns no vectors and no error.
	Embed(ctx context.Context, texts []string) ([][]float32, error)

	// Dimensions is the fixed width of every vector this Embedder returns.
	Dimensions() int

	// Identity is a stable string identifying the model actually producing
	// vectors: its name, revision and dimensions combined, changing if and
	// only if any of those three changes (spec §13.1). It exists so
	// store.contentHash can fold the model into the hash it compares
	// against: without it, switching models changes no hash, marks nothing
	// pending, and leaves search.passages holding vectors from two
	// different models in one HNSW graph, where cosine similarity across
	// them is meaningless — the same class of silent corruption
	// PipelineVersion already exists to prevent, for the component that
	// actually produces the vectors.
	//
	// The ONNX implementation derives this from its configured model file;
	// a future remote/HTTP implementation reports what the remote says it
	// is running. Build it with the Identity helper below so every
	// implementation formats it the same way.
	Identity() string

	// Close releases the underlying session.
	Close() error
}

// Identity formats the three components spec §13.1 requires — model name,
// revision, and dimensions — into the single stable string an Embedder's
// Identity() method returns. It is a free function, not a method, so every
// implementation (ONNX today, a remote HTTP one later) builds its identity
// the same way rather than each inventing its own format.
//
// Dimensions is folded in explicitly, not left to be implied by name and
// revision, because two models of different width that happened to share a
// name would otherwise hash identically here — and pgvector's fixed-width
// vector(384) column would then reject one of them at insert time with a
// confusing type error, rather than this being caught as the model change
// it is.
func Identity(name, revision string, dimensions int) string {
	return fmt.Sprintf("%s@%s#%d", name, revision, dimensions)
}
