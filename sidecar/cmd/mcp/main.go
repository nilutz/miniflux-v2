// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Command mcp is the sidecar's MCP server (task 15): a stdio child process
// Claude Code spawns locally, exposing three tools -- search, similar and
// fetch_article -- that let an agent search and read this reader's
// article corpus. It is a thin wrapper: every tool call becomes one or
// more HTTP requests to a running sidecar's read-only search API
// (internal/mcpserver, internal/sidecarclient); this binary holds no
// database connection and does no retrieval of its own.
//
// It runs over stdio, not as another HTTP service, and deliberately does
// NOT live on the sidecar's own admin port. The sidecar's HTTP API has no
// authentication today (see internal/web/search_handlers.go's own package
// doc comment: "run the remote on a private network, the same trust
// boundary Postgres itself is given") -- serving MCP from it would expose
// an unauthenticated search surface on the network. A stdio child process
// Claude Code spawns and owns the pipes of adds no network surface at
// all and needs none.
//
// This binary is CGO-free and pulls in none of the sidecar's ONNX
// Runtime / libtokenizers dependency: it imports internal/mcpserver and
// internal/sidecarclient only, neither of which imports internal/embed,
// internal/embed/onnx, internal/indexer or internal/store. It builds
// (see the Makefile's build-mcp target) with CGO_ENABLED=0 and no
// CGO_LDFLAGS, unlike cmd/sidecar, which requires -tags ORT and a linked
// libtokenizers.a. That matters because this is the binary meant to run
// on an operator's own laptop, next to Claude Code, not inside the
// container that runs cmd/sidecar.
//
// MCP's stdio transport reserves stdout for the JSON-RPC protocol
// stream -- anything else written there corrupts every message after it.
// This program therefore sends all its own logging to stderr (see
// newLogger below) and must never use fmt.Println/os.Stdout directly.
package main // import "miniflux.app/v2/sidecar/cmd/mcp"

import (
	"context"
	"log/slog"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"miniflux.app/v2/sidecar/internal/mcpserver"
	"miniflux.app/v2/sidecar/internal/sidecarclient"
)

// version identifies this binary in MCP's initialize handshake. It is
// intentionally independent of the sidecar binary's own versioning
// (cmd/sidecar has none either, as of task 15) -- this is a separate
// artifact with a separate release cadence.
const version = "0.1.0"

// sidecarURLEnv is read for the sidecar's base URL. Documented in
// sidecar/README.md alongside the exact `claude mcp add` invocation.
const sidecarURLEnv = "SIDECAR_URL"

// apiKeyEnv is read for the Miniflux API key sent as X-Auth-Token on
// every request to the sidecar (internal/sidecarclient's doc comment
// explains why unconditionally, even though the sidecar does not check it
// yet). Named after Miniflux's own API key, not the sidecar, because it
// is the SAME credential: a follow-on task has the sidecar validate this
// token against public.api_keys, the table and header
// internal/api/middleware.go already reads on the main fork's REST API.
const apiKeyEnv = "MINIFLUX_API_KEY"

func main() {
	logger := newLogger()
	slog.SetDefault(logger)

	baseURL := os.Getenv(sidecarURLEnv)
	if baseURL == "" {
		baseURL = sidecarclient.DefaultBaseURL
	}

	apiKey := os.Getenv(apiKeyEnv)
	if apiKey == "" {
		// Not fatal: the sidecar does not enforce authentication today,
		// so every tool call will still work. It is a warning, not an
		// error, precisely because that will stop being true once the
		// follow-on task lands -- see this file's own doc comment and
		// internal/sidecarclient's.
		logger.Warn(apiKeyEnv+" is not set; requests to the sidecar will carry no X-Auth-Token header",
			slog.String("effect_today", "none -- the sidecar does not yet require authentication"),
			slog.String("effect_once_sidecar_auth_lands", "every tool call will be rejected with HTTP 401 until this is set"),
		)
	}

	logger.Info("mcp: starting",
		slog.String("sidecar_url", baseURL),
		slog.Bool("api_key_configured", apiKey != ""),
		slog.String("version", version),
	)

	server := mcpserver.New(baseURL, apiKey, version)

	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		logger.Error("mcp: server exited with error", slog.Any("error", err))
		os.Exit(1)
	}
}

// newLogger builds the process-wide slog.Logger. It writes exclusively to
// stderr -- see this file's own package doc comment for why stdout must
// carry nothing but the MCP JSON-RPC stream.
func newLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
}
