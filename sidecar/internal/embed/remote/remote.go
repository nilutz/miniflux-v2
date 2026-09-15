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
//	{"texts": ["first passage", "second passage"], "task": "document"}
//
// "task" is "document" (embed.TaskDocument) or "query" (embed.TaskQuery),
// set by which of EmbedDocuments/EmbedQuery the caller invoked. It exists
// because an asymmetric model — nomic-embed-text-v1.5, the model the
// nomic migration plan's task 1 added this for — must embed a document
// differently than it embeds a query for the identical string (it
// requires "search_document: "/"search_query: " prepended respectively),
// and only the remote knows what, if anything, the model it is actually
// running needs done with that distinction: this client deliberately
// does not hardcode either prefix itself, the same way the local ONNX
// implementation gates its own prefixing by model rather than by a
// blanket rule. A server whose configured model needs no such
// distinction (bge-small-en-v1.5) is free to ignore this field entirely.
//
// texts may be empty — used internally by New to probe the remote's
// identity and dimensions without embedding anything real; task is still
// sent (as "document", arbitrarily — it has no effect on an empty batch)
// so every request this client ever sends has the same shape. A
// conforming server must still populate "model" in that case. Response,
// 200 only:
//
//	{
//	  "vectors": [[0.01, -0.02, ...], [0.03, 0.04, ...]],
//	  "model": {"name": "bge-small-en-v1.5", "revision": "abc123", "dimensions": 384}
//	}
//
// "vectors" has exactly one entry per input text, in the same order,
// each of "model.dimensions" length. "model" is returned on every
// response, not only the first, describing whatever model actually
// produced that batch's vectors — EmbedDocuments/EmbedQuery compare it
// against what New learned at startup on every call, so a remote that
// swaps models mid-run is caught on the next batch rather than silently
// mixing two models' vectors in one HNSW graph (spec §13.1).
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
	Texts []string   `json:"texts"`
	Task  embed.Task `json:"task"`
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
// embedWithTask re-checks the identity it gets back on every call rather
// than trusting it stays put for the life of the process — see the
// package doc's note on model swaps mid-run.
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

	// The task sent here is arbitrary — an empty batch embeds nothing —
	// but must still be a valid one, so every request this client ever
	// sends (including this one-time startup probe) has the same shape.
	_, model, err := e.doEmbed(context.Background(), nil, embed.TaskDocument)
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

// EmbedDocuments implements embed.Embedder.
func (e *remoteEmbedder) EmbedDocuments(ctx context.Context, texts []string) ([][]float32, error) {
	return e.embedWithTask(ctx, texts, embed.TaskDocument)
}

// EmbedQuery implements embed.Embedder.
func (e *remoteEmbedder) EmbedQuery(ctx context.Context, text string) ([]float32, error) {
	vectors, err := e.embedWithTask(ctx, []string{text}, embed.TaskQuery)
	if err != nil {
		return nil, err
	}
	if len(vectors) != 1 {
		return nil, fmt.Errorf("embed/remote: expected 1 vector for 1 query text, got %d", len(vectors))
	}
	return vectors[0], nil
}

// embedWithTask is EmbedDocuments and EmbedQuery's shared implementation:
// every request/response mechanic — the POST itself, the per-call
// identity re-check below, the vector-count check — lives here exactly
// once, so the only thing that differs between the two public methods is
// which embed.Task each hardcodes into its own call, not two
// independently maintained copies of the mechanics that could drift out
// of step with each other.
func (e *remoteEmbedder) embedWithTask(ctx context.Context, texts []string, task embed.Task) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}

	vectors, model, err := e.doEmbed(ctx, texts, task)
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
	// contain. It ALSO wraps embed.ErrRequiresRestart, which
	// ErrUnavailable does not: nothing on this side will make the remote
	// change back, so unlike a network outage this can never clear
	// itself no matter how long the lane keeps retrying — an operator
	// has to restart the sidecar (see the message below) with matching
	// configuration before it clears. The indexer's classification and
	// the lane's pause/resume machinery still treat this exactly like a
	// network outage for the pause/retry mechanics themselves (pause,
	// leave entries pending, keep retrying); ErrRequiresRestart only
	// changes what the admin page tells an operator to DO about it.
	if got := embed.Identity(model.Name, model.Revision, model.Dimensions); got != e.identity {
		return nil, fmt.Errorf(
			"%w: %w: embed/remote: remote identity changed mid-run, from %q to %q — refusing to mix vectors from two models; restart the sidecar to pick up the new model (spec §13.1)",
			embed.ErrUnavailable, embed.ErrRequiresRestart, e.identity, got,
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
// single endpoint, sending task alongside texts so the remote knows
// which side of an asymmetric model's distinction this batch is on (see
// the package doc). It backs both embedWithTask (non-empty texts) and
// New's startup probe (an empty batch, to learn the remote's identity
// and dimensions without embedding anything real) — embedWithTask
// short-circuits before reaching here on an empty input, so this is the
// only path that ever actually talks to the network.
func (e *remoteEmbedder) doEmbed(ctx context.Context, texts []string, task embed.Task) ([][]float32, modelInfo, error) {
	if texts == nil {
		texts = []string{}
	}

	body, err := json.Marshal(embedRequest{Texts: texts, Task: task})
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
