// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// This file implements task 17: guarding the sidecar's data endpoints
// (GET /api/search, /api/similar and /api/article) with the same Miniflux
// API keys Miniflux's own REST API already issues and validates. It reads
// the same header Miniflux reads (authTokenHeader below, matching
// internal/api/middleware.go:41's `r.Header.Get("X-Auth-Token")` on the
// fork exactly, in name and casing -- internal/sidecarclient already sends
// this same header on every request cmd/mcp makes, so a working Miniflux
// key works end to end with no client-side change) and validates it with
// a single, read-only, indexed lookup against public.api_keys
// (APIKeyValidator / store.Store.ValidateAPIKey) -- the same table
// Miniflux's own middleware reads. There is no second credential system:
// one Miniflux key works against both services, and no configuration flag
// disables this check.
//
// The status page, GET /api/status, and every control endpoint (backfill
// pause/resume/config, the Task 9 embedder-switch endpoints) are
// deliberately NOT gated by this file -- see server.go's own doc comment
// on New for the reasoning. This file's own scope is the three data
// endpoints, and the search HTTP API's user-scoping fix: a protected
// request's search.Filters.UserID (or GET /api/article's owning-user
// check) is now derived from the validated key, never from a "user" query
// parameter the caller could otherwise use to assert somebody else's
// identity -- see resolveAuthenticatedUserID in search_handlers.go and
// handleArticle in article_handler.go.
package web // import "miniflux.app/v2/sidecar/internal/web"

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
)

// authTokenHeader is the header Miniflux's own REST API reads
// (internal/api/middleware.go:41, `r.Header.Get("X-Auth-Token")`) --
// matched here exactly, by name and casing, so one Miniflux API key works
// against both services. Kept as a constant, mirroring
// internal/sidecarclient's own authTokenHeader, so the two never risk
// drifting from the production header name by way of a typo in a string
// literal.
const authTokenHeader = "X-Auth-Token"

// APIKeyValidator looks up a Miniflux API key's owning user, read-only,
// against public.api_keys -- the same table and header
// internal/api/middleware.go already validates on the fork's own REST
// API. Defined as an interface, not *store.Store directly, for the same
// hermetic-tests reason every other store dependency in this package is
// (see BackfillController's own doc comment in server.go): this package's
// suite must be able to run with no database. *store.Store's
// ValidateAPIKey satisfies this with no adaptation.
//
// ok is false, with a nil error, for a token that resolves to no row --
// never issued, or a revoked key whose row Miniflux deleted. Both must
// produce the identical response to an HTTP caller (§ Behaviour: "do not
// leak whether a token exists"), which is exactly what a single boolean
// return collapses them into.
type APIKeyValidator interface {
	ValidateAPIKey(ctx context.Context, token string) (userID int64, ok bool, err error)
}

// contextKey is a private type for this package's own context keys, so a
// key set here can never collide with one set by net/http or any other
// package sharing the same request context.
type contextKey int

// authenticatedUserIDKey is the context key requireAPIKey stores the
// validated caller's user id under.
const authenticatedUserIDKey contextKey = iota

// authenticatedUserID returns the user id the current request's API key
// resolved to. ok is false only if this is called from a handler that
// requireAPIKey did not wrap -- every protected handler in this package
// is always wrapped, so callers should treat a false here as a
// programming error (see resolveAuthenticatedUserID and handleArticle's
// own defensive handling of it) rather than a normal outcome.
func authenticatedUserID(r *http.Request) (int64, bool) {
	id, ok := r.Context().Value(authenticatedUserIDKey).(int64)
	return id, ok
}

// requireAPIKey wraps next with task 17's authentication gate. Missing or
// unknown token -> 401 with a body that says which header is expected,
// but never says whether the token exists at all (§ Behaviour). A valid
// token's resolved user id is attached to the request's context via
// authenticatedUserID for next to read -- deriving search/article scope
// from the KEY rather than from anything the caller separately asserts is
// this task's "second problem" fix; see this file's own package doc
// comment.
//
// s.keys == nil fails CLOSED (500, "authentication unavailable"), never
// open: this task's brief explicitly rules out any path, including a
// misconfiguration, that ends up equivalent to a flag disabling
// authentication. cmd/sidecar always supplies a validator; only a test
// harness exercising routes this middleware does not wrap ever leaves it
// nil.
func (s *Server) requireAPIKey(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := r.Header.Get(authTokenHeader)
		if token == "" {
			writeAPIError(w, http.StatusUnauthorized, fmt.Sprintf("missing %s header: a Miniflux API key is required", authTokenHeader))
			return
		}

		if s.keys == nil {
			slog.Error("web: a protected endpoint was called with no APIKeyValidator configured")
			writeAPIError(w, http.StatusInternalServerError, "authentication unavailable")
			return
		}

		userID, ok, err := s.keys.ValidateAPIKey(r.Context(), token)
		if err != nil {
			slog.Error("web: unable to validate API key", slog.Any("error", err))
			writeAPIError(w, http.StatusInternalServerError, "authentication failed")
			return
		}
		if !ok {
			writeAPIError(w, http.StatusUnauthorized, fmt.Sprintf("invalid %s: unrecognised API key", authTokenHeader))
			return
		}

		next(w, r.WithContext(context.WithValue(r.Context(), authenticatedUserIDKey, userID)))
	}
}
