// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package remote implements embed.Embedder by delegating to an HTTP
// service running on another machine — a GPU host, most often — instead
// of embedding in-process (spec §13.1). It has no CGO and does not import
// internal/embed/onnx, so packages that link this package never pull in
// the native ONNX Runtime / libtokenizers libraries that one requires.
//
// # Wire protocol
//
// One endpoint, POST {URL}/embed. Request:
//
//	{"texts": ["first passage", "second passage"]}
//
// texts may be empty — used internally by New to probe the remote's
// identity and dimensions without embedding anything real. A conforming
// server must still populate "model" in that case. Response, 200 only:
//
//	{
//	  "vectors": [[0.01, -0.02, ...], [0.03, 0.04, ...]],
//	  "model": {"name": "bge-small-en-v1.5", "revision": "abc123", "dimensions": 384}
//	}
//
// "vectors" has exactly one entry per input text, in the same order,
// each of "model.dimensions" length. "model" is returned on every
// response, not only the first, describing whatever model actually
// produced that batch's vectors — Embed compares it against what New
// learned at startup on every call, so a remote that swaps models
// mid-run is caught on the next batch rather than silently mixing two
// models' vectors in one HNSW graph (spec §13.1).
//
// Anything other than a 200 status is an error; the response body, if
// any, is included in the error message on a best-effort basis. There is
// no authentication in this protocol — run the remote on a private
// network, the same trust boundary Postgres itself is given.
package remote // import "miniflux.app/v2/sidecar/internal/embed/remote"

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"miniflux.app/v2/sidecar/internal/embed"
)

// wantDimensions is the fixed width search.passages.embedding requires
// (public.vector(384); spec §13.1). A remote reporting anything else is
// a schema mismatch and must fail construction, not the first insert —
// pgvector's fixed-width column would otherwise reject it there with a
// confusing type error instead of this being caught as the model change
// it is.
const wantDimensions = 384

// DefaultTimeout is used for every request, including New's own startup
// probe, when Config.Timeout is zero.
const DefaultTimeout = 30 * time.Second

// Config configures a remote Embedder.
type Config struct {
	// URL is the remote service's base URL, e.g. "http://gpu-host:9000".
	// The client POSTs to "<URL>/embed". Required.
	URL string

	// Timeout bounds every HTTP request to the remote. Zero uses
	// DefaultTimeout.
	Timeout time.Duration
}

// embedRequest is this package's wire request. See the package doc.
type embedRequest struct {
	Texts []string `json:"texts"`
}

// modelInfo is the model-identity half of the wire response. See the
// package doc.
type modelInfo struct {
	Name       string `json:"name"`
	Revision   string `json:"revision"`
	Dimensions int    `json:"dimensions"`
}

// embedResponse is this package's wire response. See the package doc.
type embedResponse struct {
	Vectors [][]float32 `json:"vectors"`
	Model   modelInfo   `json:"model"`
}

// remoteEmbedder is an Embedder that delegates every call to an HTTP
// service. identity and dims are fixed at construction (New's probe);
// Embed re-checks the identity it gets back on every call rather than
// trusting it stays put for the life of the process — see the package
// doc's note on model swaps mid-run.
type remoteEmbedder struct {
	endpoint string
	client   *http.Client
	identity string
	dims     int
}

// New creates an Embedder that delegates to a remote HTTP service. It
// probes the remote once, synchronously, with an empty batch: this is
// the only way to learn what model the remote is actually running, and
// a dimension mismatch against the fixed schema width is reported here,
// loudly, at construction — not as a confusing insert error later (spec
// §13.1).
func New(cfg Config) (embed.Embedder, error) {
	base := strings.TrimSuffix(strings.TrimSpace(cfg.URL), "/")
	if base == "" {
		return nil, fmt.Errorf("embed/remote: Config.URL is required")
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}

	e := &remoteEmbedder{
		endpoint: base + "/embed",
		client:   &http.Client{Timeout: timeout},
	}

	_, model, err := e.doEmbed(context.Background(), nil)
	if err != nil {
		return nil, fmt.Errorf("embed/remote: probe %s: %w", e.endpoint, err)
	}
	if model.Dimensions != wantDimensions {
		return nil, fmt.Errorf(
			"embed/remote: remote %s reports %d dimensions, but search.passages requires %d — a dimension change needs an explicit, operator-initiated re-index, not a config swap (spec §13.1)",
			e.endpoint, model.Dimensions, wantDimensions,
		)
	}
	if err := validateIdentityComponents(model.Name, model.Revision); err != nil {
		return nil, err
	}

	e.dims = model.Dimensions
	e.identity = embed.Identity(model.Name, model.Revision, model.Dimensions)

	slog.Info("sidecar: remote embedder ready",
		slog.String("endpoint", e.endpoint),
		slog.Int("dimensions", e.dims),
		slog.String("identity", e.identity),
	)

	return e, nil
}

// validateIdentityComponents rejects name/revision strings that would
// make embed.Identity's "%s@%s#%d" format ambiguous. embed.Identity does
// no escaping of '@' or '#' within its inputs; that is safe for the ONNX
// implementation (a resolved directory basename and a hex digest, never
// containing those characters) but not here, where name and revision are
// a remote server's self-reported strings — a far less controlled input.
//
// This rejects outright rather than silently stripping or substituting
// the characters: the identity string stays exactly what the remote
// reported (still legible in logs and the admin page), and a genuine
// collision is fixed at the source — the remote's own naming — rather
// than papered over in this client.
func validateIdentityComponents(name, revision string) error {
	if name == "" {
		return fmt.Errorf("embed/remote: remote reported an empty model name")
	}
	if strings.ContainsAny(name, "@#") || strings.ContainsAny(revision, "@#") {
		return fmt.Errorf(
			"embed/remote: model name %q / revision %q must not contain '@' or '#' — those are embed.Identity's own separators and this pair would format ambiguously; rename the model on the remote",
			name, revision,
		)
	}
	return nil
}

// Embed implements embed.Embedder.
func (e *remoteEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}

	vectors, model, err := e.doEmbed(ctx, texts)
	if err != nil {
		return nil, err
	}

	// Re-derive the identity from THIS response and compare against what
	// New's probe recorded. A mismatch means the remote started serving a
	// different model without a restart on this side — exactly the
	// silent-corruption case spec §13.1 exists to prevent, so this
	// refuses the batch rather than returning vectors under a stale
	// identity.
	//
	// This wraps embed.ErrUnavailable, the same sentinel as a genuine
	// network outage below, not a bespoke "model changed" one: it is not
	// this batch's entry that is at fault, and mixing vectors from two
	// models into one HNSW graph is corruption, not a recoverable
	// per-entry error the existing retry/backoff path is built to
	// contain. It happens not to be transient the way a network blip is
	// — nothing on this side will make the remote change back — but the
	// indexer's classification and the lane's pause/resume machinery
	// treat both the same way: pause, leave entries pending, and let the
	// operator's own restart (named in the message below) be what
	// clears it, exactly like a health check clearing once connectivity
	// returns.
	if got := embed.Identity(model.Name, model.Revision, model.Dimensions); got != e.identity {
		return nil, fmt.Errorf(
			"%w: embed/remote: remote identity changed mid-run, from %q to %q — refusing to mix vectors from two models; restart the sidecar to pick up the new model (spec §13.1)",
			embed.ErrUnavailable, e.identity, got,
		)
	}
	if len(vectors) != len(texts) {
		return nil, fmt.Errorf("embed/remote: remote returned %d vectors for %d input texts", len(vectors), len(texts))
	}
	return vectors, nil
}

// Dimensions implements embed.Embedder.
func (e *remoteEmbedder) Dimensions() int { return e.dims }

// Identity implements embed.Embedder. It reports what New's probe
// learned at construction — the value store.contentHash folds in, so it
// must reflect what actually produced the corpus's vectors rather than
// anything reconfigured locally.
func (e *remoteEmbedder) Identity() string { return e.identity }

// Close implements embed.Embedder. There is no persistent session to
// release here, only pooled HTTP connections.
func (e *remoteEmbedder) Close() error {
	e.client.CloseIdleConnections()
	return nil
}

// doEmbed performs one request/response round trip against the remote's
// single endpoint. It backs both the public Embed (non-empty texts) and
// New's startup probe (an empty batch, to learn the remote's identity
// and dimensions without embedding anything real) — Embed short-circuits
// before reaching here on an empty input, so this is the only path that
// ever actually talks to the network.
func (e *remoteEmbedder) doEmbed(ctx context.Context, texts []string) ([][]float32, modelInfo, error) {
	if texts == nil {
		texts = []string{}
	}

	body, err := json.Marshal(embedRequest{Texts: texts})
	if err != nil {
		return nil, modelInfo{}, fmt.Errorf("embed/remote: encode request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, modelInfo{}, fmt.Errorf("embed/remote: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	// Everything below this point is "the remote did not give us something
	// usable" rather than "this input was bad" — a dropped connection, a
	// non-2xx status, a malformed body, or a body that doesn't even match
	// its own claimed input count are all properties of the SERVICE, not
	// of texts. Each wraps embed.ErrUnavailable so the indexer classifies
	// it as lane-level (spec §13.1): pause and retry, never
	// store.MarkEntryFailed for whatever entry happened to be embedding
	// when the service dropped out.
	resp, err := e.client.Do(req)
	if err != nil {
		return nil, modelInfo{}, fmt.Errorf("%w: embed/remote: request failed: %w", embed.ErrUnavailable, err)
	}
	defer resp.Body.Close()

	const maxErrorBody = 4 << 10
	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		return nil, modelInfo{}, fmt.Errorf("%w: embed/remote: %s returned %s: %s", embed.ErrUnavailable, e.endpoint, resp.Status, bytes.TrimSpace(snippet))
	}

	var parsed embedResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, modelInfo{}, fmt.Errorf("%w: embed/remote: decode response from %s: %w", embed.ErrUnavailable, e.endpoint, err)
	}
	if len(parsed.Vectors) != len(texts) {
		return nil, modelInfo{}, fmt.Errorf("%w: embed/remote: response has %d vectors for %d input texts", embed.ErrUnavailable, len(parsed.Vectors), len(texts))
	}

	return parsed.Vectors, parsed.Model, nil
}
