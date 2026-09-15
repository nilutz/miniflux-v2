// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package embed // import "miniflux.app/v2/sidecar/internal/embed"

import (
	"context"
	"errors"
	"fmt"
)

// ErrUnavailable is the sentinel an Embedder implementation wraps into any
// error Embed returns that means "I cannot currently serve requests at
// all" — a network outage, a remote returning 5xxs, a malformed response,
// or (see internal/embed/remote) a remote that starts serving a different
// model mid-run — as opposed to "this specific input failed to embed".
//
// internal/indexer classifies an Embed error by errors.Is(err,
// ErrUnavailable): wrapping it turns the failure into a LANE-level
// condition (the backfill and live lanes pause themselves, and leave the
// entry's index state untouched so it stays pending — spec §13.1) rather
// than an entry-level one (store.MarkEntryFailed, retried later with
// backoff). Conflating the two is exactly the bug spec §13.1 exists to
// prevent: a transient outage must not mark thousands of entries failed
// individually, and an entry that is genuinely bad must not silently
// stall the whole lane forever.
//
// An error that does NOT wrap ErrUnavailable is treated as entry-level —
// this is deliberate, not an oversight, and is the safe default for a new
// Embedder implementation to fall into. The in-process ONNX embedder
// (internal/embed/onnx) has no notion of "service unavailable" at all —
// every error it can return is about the specific input or a fatal
// process-level condition, never a recoverable, wait-and-it-comes-back
// outage — so its errors must keep classifying as entry-level exactly as
// they always have. Defaulting an UNCLASSIFIED error to lane-level
// instead would mean a single bad entry from any embedder that forgets to
// wrap ErrUnavailable freezes progress on every OTHER entry too, silently,
// for as long as that one entry keeps being retried — a worse failure
// mode than the one this sentinel exists to fix, since the existing
// entry-level path already has mature, visible containment (per-entry
// retry backoff, and the admin page's failures-by-cause breakdown). Only
// an embedder that genuinely can be unreachable — a networked one — should
// ever wrap this, and it must do so explicitly at each call site that can
// fail that way.
var ErrUnavailable = errors.New("embedder unavailable")

// ErrRequiresRestart further qualifies an ErrUnavailable-wrapping error:
// wrap BOTH together (fmt.Errorf supports multiple %w) when the condition
// cannot clear itself no matter how long a caller keeps retrying — the
// only case today is internal/embed/remote's mid-run model-identity
// change, where the remote will keep reporting the new identity until an
// operator restarts the sidecar with matching configuration.
//
// This is deliberately a second, independent sentinel rather than a third
// value in some enum, and deliberately NOT a replacement for
// ErrUnavailable: every ErrRequiresRestart-wrapping error is still, and
// must still be classified as, ErrUnavailable — pause the lane, leave the
// entry pending, keep retrying automatically — because it is just as much
// "not this entry's fault" as a plain network outage is. What differs is
// only what an operator watching the admin page should DO about it: a
// plain ErrUnavailable resolves itself the moment the network/remote come
// back, so "wait" is a reasonable response; an ErrRequiresRestart-wrapping
// one never will, no matter how long the lane keeps trying, so "wait" is
// the wrong answer and the page must say so distinctly rather than
// leaving that distinction buried in prose an operator has to read.
var ErrRequiresRestart = errors.New("embedder requires a sidecar restart to recover")

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
//
// The format has no escaping of "@" or "#" within name/revision, so two
// distinct (name, revision) pairs could in principle format identically if
// one embedded the other's separator. Not reachable today — the ONNX
// implementation's name is a resolved directory basename and its revision
// a hex digest — but a remote implementation reporting a name string the
// far end supplies is a less controlled input, and would need to sanitise
// or otherwise guarantee uniqueness before calling this.
func Identity(name, revision string, dimensions int) string {
	return fmt.Sprintf("%s@%s#%d", name, revision, dimensions)
}
