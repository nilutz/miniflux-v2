// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Command mcp is the sidecar's MCP server: a stdio child process Claude
// Code spawns locally, exposing three tools -- search, similar and
// fetch_article -- that let an agent search and read this reader's
// article corpus. It is a thin wrapper: every tool call becomes one or
// more HTTP requests to a running sidecar's read-only search API
// (internal/mcpserver, internal/sidecarclient); this binary holds no
// database connection and does no retrieval of its own.
//
// It runs over stdio, not as another HTTP service, and deliberately does
// NOT live on the sidecar's own admin port. The sidecar's data endpoints
// (GET /api/search, /api/similar, /api/article -- see internal/web/auth.go)
// require a Miniflux API key, but the admin server's status page and
// control endpoints still do not (see internal/web/server.go's own doc
// comment on Server for why); running MCP as a second HTTP service on
// that same port would still mean opening a new network surface where
// none is otherwise needed. A stdio child process Claude Code spawns and
// owns the pipes of adds no network surface at all, authenticated or not.
//
// This binary is CGO-free and pulls in none of the sidecar's ONNX
// Runtime / libtokenizers dependency: it imports internal/mcpserver and
// internal/sidecarclient only, neither of which imports internal/embed,
// internal/embed/onnx, internal/indexer or internal/store. It builds
// (see the Makefile's build-mcp target) with CGO_ENABLED=0 and no
// CGO_LDFLAGS, unlike cmd/sidecar, which requires -tags ORT and a linked
// libtokenizers.a: this is the binary meant to run on an operator's own
// laptop, next to Claude Code, not inside the container that runs
// cmd/sidecar.
//
// MCP's stdio transport reserves stdout for the JSON-RPC protocol
// stream -- anything else written there corrupts every message after it.
// This program therefore sends all its own logging to stderr (see
// newLogger below) and must never use fmt.Println/os.Stdout directly.
package main // import "miniflux.app/v2/sidecar/cmd/mcp"

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"miniflux.app/v2/sidecar/internal/mcpserver"
	"miniflux.app/v2/sidecar/internal/sidecarclient"
)

// version identifies this binary in MCP's initialize handshake. It is
// intentionally independent of the sidecar binary's own versioning
// (cmd/sidecar has none either) -- this is a separate artifact with a
// separate release cadence.
const version = "0.1.0"

// sidecarURLEnv is read for the sidecar's base URL. Documented in
// sidecar/README.md alongside the exact `claude mcp add` invocation.
const sidecarURLEnv = "SIDECAR_URL"

// apiKeyEnv is read for the Miniflux API key sent as X-Auth-Token on
// every request to the sidecar (internal/sidecarclient's doc comment
// explains why unconditionally). Named after Miniflux's own API key, not
// the sidecar, because it is the SAME credential: the sidecar validates
// this token against public.api_keys, the table and header
// internal/api/middleware.go already reads on the main fork's own REST
// API -- so a real Miniflux key works against both services with no
// separate key to manage.
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
		// Not fatal here -- main() still starts, and connecting to a
		// sidecar whose admin/control endpoints remain unauthenticated
		// (server.go's own doc comment on Server explains which do and do
		// not) still succeeds. But GET /api/search, /api/similar and
		// /api/article all require this key: every search/similar/
		// fetch_article tool call will fail with an AuthError ("sidecar
		// rejected the API key") until it is set.
		logger.Warn(apiKeyEnv+" is not set; requests to the sidecar will carry no X-Auth-Token header",
			slog.String("effect", "every search/similar/fetch_article tool call will fail with HTTP 401 until this is set to a valid Miniflux API key"),
		)
	}

	logger.Info("mcp: starting",
		slog.String("sidecar_url", baseURL),
		slog.Bool("api_key_configured", apiKey != ""),
		slog.String("version", version),
	)

	server := mcpserver.New(baseURL, apiKey, version)

	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		if isNormalStdioTermination(err) {
			// EOF on stdin is how a client *normally* stops a stdio MCP
			// server -- Claude Code closes stdin on shutdown -- so this is
			// a clean exit, not a crash. Logging it at Error with exit 1
			// made every ordinary shutdown look like a failure in the
			// logs; see isNormalStdioTermination's own doc comment for
			// why this can't be a plain errors.Is(err, io.EOF) check.
			logger.Info("mcp: stdio closed by the client; shutting down", slog.Any("detail", err))
			return
		}
		logger.Error("mcp: server exited with error", slog.Any("error", err))
		os.Exit(1)
	}
}

// isNormalStdioTermination reports whether err represents nothing more
// than the far end (Claude Code, or any other stdio MCP client) closing
// stdin -- the ordinary way a stdio server like this one is stopped, not
// a crash. Genuine errors (a malformed message, a transport failure that
// isn't a plain close) must still be reported and still exit 1.
//
// The MCP SDK (github.com/modelcontextprotocol/go-sdk v1.6.0) does not
// make this a plain errors.Is(err, io.EOF) check: its jsonrpc2 connection
// filters out a bare io.EOF read/write error internally (conn.go's
// wait()), but a clean stdin close can still surface here wrapped as
// jsonrpc2's own unexported ErrServerClosing formatted with "%v" (not
// "%w") around the underlying io.EOF -- e.g. "server is closing: EOF" --
// which does NOT chain through errors.Is because the %v verb breaks the
// wrapping. ErrServerClosing itself lives in the SDK's internal/jsonrpc2
// package and cannot be imported here, so this falls back to matching the
// io.EOF text ("EOF") that SDK always appends after a colon in that case.
// An exact io.EOF (never wrapped at all) is still caught directly via
// errors.Is first.
func isNormalStdioTermination(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.EOF) {
		return true
	}
	// Deliberately ": EOF" (colon-space before it), not a bare "EOF"
	// suffix: io.ErrUnexpectedEOF's own text is "unexpected EOF", which
	// ends in "EOF" too but is a genuine error (a truncated read), not a
	// clean close, and must not be swallowed here.
	return strings.HasSuffix(err.Error(), ": "+io.EOF.Error())
}

// newLogger builds the process-wide slog.Logger. It writes exclusively to
// stderr -- see this file's own package doc comment for why stdout must
// carry nothing but the MCP JSON-RPC stream.
func newLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
}
