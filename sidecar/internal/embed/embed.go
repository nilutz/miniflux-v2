// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package embed // import "miniflux.app/v2/sidecar/internal/embed"

import "context"

// Embedder turns text into dense vectors. The ONNX implementation is the only
// one today; the interface exists so a local inference service or a pure-Go
// backend can be substituted without touching the pipeline (spec §6.6).
type Embedder interface {
	// Embed returns one vector per input text, each of Dimensions() length.
	// An empty input returns no vectors and no error.
	Embed(ctx context.Context, texts []string) ([][]float32, error)

	// Dimensions is the fixed width of every vector this Embedder returns.
	Dimensions() int

	// Close releases the underlying session.
	Close() error
}
