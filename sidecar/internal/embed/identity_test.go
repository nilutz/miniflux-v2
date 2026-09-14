// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package embed // import "miniflux.app/v2/sidecar/internal/embed"

import "testing"

// TestIdentityIsStableForTheSameInputs pins Identity's basic contract: the
// same name, revision and dimensions must always produce the same string,
// with no hidden dependence on call order or process state.
func TestIdentityIsStableForTheSameInputs(t *testing.T) {
	a := Identity("bge-small-en-v1.5", "abc123", 384)
	b := Identity("bge-small-en-v1.5", "abc123", 384)
	if a != b {
		t.Fatalf("Identity is not deterministic: %q vs %q", a, b)
	}
}

// TestIdentityDiffersOnName is the "different model" half of spec §13.1:
// a name change alone must produce a different identity.
func TestIdentityDiffersOnName(t *testing.T) {
	a := Identity("bge-small-en-v1.5", "abc123", 384)
	b := Identity("gte-small", "abc123", 384)
	if a == b {
		t.Fatalf("expected different names to produce different identities, both were %q", a)
	}
}

// TestIdentityDiffersOnRevision is the "same model, different weights"
// half: a revision change alone (a re-quantization, a newer checkpoint
// under the same name) must produce a different identity too, or a silent
// weight swap would leave two models' vectors mixed in one HNSW graph.
func TestIdentityDiffersOnRevision(t *testing.T) {
	a := Identity("bge-small-en-v1.5", "abc123", 384)
	b := Identity("bge-small-en-v1.5", "def456", 384)
	if a == b {
		t.Fatalf("expected different revisions to produce different identities, both were %q", a)
	}
}

// TestIdentityDiffersOnDimensionsAlone is the case spec §13.1 calls out by
// name: two models that share a name and revision string but differ only
// in width must still be distinguished, because pgvector's fixed-width
// column would otherwise reject one of them at insert time with a
// confusing error rather than this being caught as a model change.
func TestIdentityDiffersOnDimensionsAlone(t *testing.T) {
	a := Identity("same-name", "same-revision", 384)
	b := Identity("same-name", "same-revision", 768)
	if a == b {
		t.Fatalf("expected different dimensions to produce different identities even with the same name and revision, both were %q", a)
	}
}
