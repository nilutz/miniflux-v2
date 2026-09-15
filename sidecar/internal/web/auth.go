// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// This file implements the sidecar's authentication and authorisation
// gates: the sidecar's data endpoints (GET /api/search, /api/similar and
// /api/article) accept ANY credential Miniflux already issues -- an API
// key or a browser's web session cookie -- and its control endpoints and
// status page additionally require that credential's user to be a
// Miniflux administrator.
//
// Two credentials, both already Miniflux's, resolved by resolveCredential
// below:
//
//   - API key (X-Auth-Token). Reads the same header Miniflux reads
//     (authTokenHeader below, matching internal/api/middleware.go:41's
//     `r.Header.Get("X-Auth-Token")` on the fork exactly, in name and
//     casing -- internal/sidecarclient already sends this same header on
//     every request cmd/mcp makes, so a working Miniflux key works end to
//     end with no client-side change) and validates it with a single,
//     read-only, indexed lookup against public.api_keys (APIKeyValidator /
//     store.Store.ValidateAPIKey) -- the same table Miniflux's own
//     middleware reads.
//   - Miniflux web session cookie -- see session_auth.go for the cookie
//     name and SessionValidator, and store/websession.go for the
//     read-only validation this couples to across the module boundary
//     with Miniflux's own internal/ui and internal/model packages. This is
//     what delivers "no login": a browser already signed into Miniflux
//     carries this cookie, so the sidecar's admin page just works.
//
// There is no second credential system and no configuration flag disables
// either check -- one Miniflux credential (of either kind) works against
// both services.
//
// Authorisation: the status page, GET /api/status, and every control
// endpoint (backfill pause/resume/config, the embedder-switch endpoints)
// require requireAdmin below, not just requireAuthenticatedUser -- ANY
// valid credential authenticates a caller, but only one whose user
// is_admin (AdminChecker / store.Store.IsAdmin) may reach these operator
// surfaces. The three data endpoints admit any valid credential and scope
// the request to that credential's user via requireAuthenticatedUser --
// see resolveAuthenticatedUserID in search_handlers.go and handleArticle
// in article_handler.go.
package web // import "miniflux.app/v2/sidecar/internal/web"

import (
	"context"
	"errors"
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

// authenticatedUserIDKey is the context key requireAuthenticatedUser and
// requireAdmin store the validated caller's user id under.
const authenticatedUserIDKey contextKey = iota

// authenticatedUserID returns the user id the current request's
// credential (API key or session cookie) resolved to. ok is false only if
// this is called from a handler that neither requireAuthenticatedUser nor
// requireAdmin wrapped -- every protected handler in this package is
// always wrapped by one of the two, so callers should treat a false here
// as a programming error (see resolveAuthenticatedUserID and
// handleArticle's own defensive handling of it) rather than a normal
// outcome.
func authenticatedUserID(r *http.Request) (int64, bool) {
	id, ok := r.Context().Value(authenticatedUserIDKey).(int64)
	return id, ok
}

// errAuthValidatorUnavailable is resolveCredential's sentinel for "the
// credential kind this request presented has no validator configured" --
// a misconfiguration (cmd/sidecar always supplies both), never a normal
// outcome. Kept distinct from a validator's own errors so callers can
// always map it to 500 without inspecting message text.
var errAuthValidatorUnavailable = errors.New("web: no validator configured for the presented credential")

// resolveCredential resolves the current request's caller identity from
// either credential Miniflux issues (see this file's own package doc
// comment): the X-Auth-Token API key header, checked first, or the
// MinifluxSessionID browser cookie (session_auth.go) when no header is
// present. Both requireAuthenticatedUser and
// requireAdmin call this; it is the single place either credential is
// actually validated.
//
// ok=false with err=nil covers every case that must look identical to a
// caller (§ Behaviour's "do not leak whether it exists", extended from
// API keys to sessions): no credential presented at all, an API key
// header naming an unknown token, or a session cookie that fails to
// validate for any reason (malformed, unknown session id, wrong secret,
// not bound to a user, or deleted -- store.ValidateWebSessionCookie's own
// doc comment collapses all of these the same way ValidateAPIKey does).
//
// A non-nil err means fail closed with 500: either errAuthValidatorUnavailable
// (s.keys or s.sessions is nil for the credential kind presented) or a
// genuine validator error (a database hiccup, or -- see websession.go's
// own doc comment on the coupling risk -- an upstream schema change this
// query can no longer satisfy).
func (s *Server) resolveCredential(r *http.Request) (userID int64, ok bool, err error) {
	if token := r.Header.Get(authTokenHeader); token != "" {
		if s.keys == nil {
			return 0, false, errAuthValidatorUnavailable
		}
		return s.keys.ValidateAPIKey(r.Context(), token)
	}

	if cookie, cerr := r.Cookie(minifluxSessionCookieName); cerr == nil && cookie.Value != "" {
		if s.sessions == nil {
			return 0, false, errAuthValidatorUnavailable
		}
		return s.sessions.ValidateWebSessionCookie(r.Context(), cookie.Value)
	}

	return 0, false, nil
}

// missingCredentialMessage is requireAuthenticatedUser and requireAdmin's
// shared 401 body for "no credential presented at all". It names both
// authTokenHeader and the session cookie alternative -- a caller reading
// this message can tell which credential is missing without already
// knowing the sidecar accepts two kinds.
var missingCredentialMessage = fmt.Sprintf(
	"authentication required: send a %s header (a Miniflux API key) or sign in to Miniflux in this browser (a %s session cookie)",
	authTokenHeader, minifluxSessionCookieName,
)

// requireAuthenticatedUser wraps next with the sidecar's data-endpoint
// authentication gate -- see resolveCredential and this file's own
// package doc comment. ANY valid credential passes: unlike requireAdmin,
// this makes no is_admin check ("data endpoints: any valid credential,
// scoped to that user"). A valid credential's resolved user id is
// attached to the request's context via authenticatedUserID for next to
// read -- deriving search/article scope from the CREDENTIAL rather than
// from anything the caller separately asserts closes the door on a
// caller claiming a different user's id; see resolveAuthenticatedUserID
// in search_handlers.go and handleArticle in article_handler.go.
//
// Fails CLOSED, never open, on every error path (missing credential,
// unrecognised credential, or resolveCredential erroring): no path,
// including a misconfiguration, may end up equivalent to a flag disabling
// authentication.
func (s *Server) requireAuthenticatedUser(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok, err := s.resolveCredential(r)
		if err != nil {
			slog.Error("web: unable to resolve the request's credential", slog.Any("error", err))
			writeAPIError(w, http.StatusInternalServerError, "authentication unavailable")
			return
		}
		if !ok {
			writeAPIError(w, http.StatusUnauthorized, missingCredentialMessage)
			return
		}

		next(w, r.WithContext(context.WithValue(r.Context(), authenticatedUserIDKey, userID)))
	}
}

// requireAdmin wraps next with the authorisation gate for the sidecar's
// control endpoints and status page: it accepts the same two credentials
// requireAuthenticatedUser does, but additionally requires the resolved
// user to be a Miniflux administrator (AdminChecker / store.Store.IsAdmin,
// read-only against public.users.is_admin); see server.go's own doc
// comment on New for why these endpoints require admin rather than
// staying unauthenticated or accepting any valid credential.
//
// No credential at all -> 401 (missingCredentialMessage, same as
// requireAuthenticatedUser). A credential that resolves to a real,
// non-admin user -> 403 -- never 401, since the credential itself was
// valid; conflating the two would make it impossible for an operator to
// tell "you are not signed in" from "you are signed in but not an admin".
// s.admins == nil fails CLOSED with 500, the same "no flag disables
// authentication" invariant requireAuthenticatedUser's s.keys==nil path
// already enforces.
func (s *Server) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok, err := s.resolveCredential(r)
		if err != nil {
			slog.Error("web: unable to resolve the request's credential", slog.Any("error", err))
			writeAPIError(w, http.StatusInternalServerError, "authentication unavailable")
			return
		}
		if !ok {
			writeAPIError(w, http.StatusUnauthorized, missingCredentialMessage)
			return
		}

		if s.admins == nil {
			slog.Error("web: an admin-only endpoint was called with no AdminChecker configured")
			writeAPIError(w, http.StatusInternalServerError, "authorisation unavailable")
			return
		}

		isAdmin, err := s.admins.IsAdmin(r.Context(), userID)
		if err != nil {
			slog.Error("web: unable to check admin status", slog.Any("error", err))
			writeAPIError(w, http.StatusInternalServerError, "authorisation failed")
			return
		}
		if !isAdmin {
			writeAPIError(w, http.StatusForbidden, "administrator access required")
			return
		}

		next(w, r.WithContext(context.WithValue(r.Context(), authenticatedUserIDKey, userID)))
	}
}
