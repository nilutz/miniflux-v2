// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// This file implements the sidecar's authentication and authorisation
// gates: the sidecar's data endpoints (GET /api/search, /api/similar and
// /api/article) accept ANY credential Miniflux already issues -- an API
// key or a browser's web session cookie -- or the fork's own service
// credential (below), and its control endpoints and status page
// additionally require an END-USER credential whose user is a Miniflux
// administrator.
//
// Three credentials, resolved by resolveCredential below:
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
//   - Service credential (X-Sidecar-Service-Token; see serviceTokenHeader
//     below), a single shared secret configured on both the fork
//     (SEARCH_SIDECAR_TOKEN) and this sidecar (SIDECAR_SERVICE_TOKEN),
//     for the fork's own internal/searchclient -- the ONLY caller with no
//     credential of its own to forward: it renders search/similar
//     results on behalf of whichever Miniflux user is browsing, a user
//     who has no API key or session cookie the fork could hand to a
//     second HTTP service. Unlike the two credentials above, presenting
//     this secret identifies no user by itself -- it authorises the
//     caller to ASSERT a user id via the request's own "user" parameter,
//     and only that: it is never accepted by requireAdmin (see that
//     function's own doc comment), so knowing the secret can never reach
//     a control endpoint or the status page, admin user id or not.
//     resolveCredential compares it in constant time and refuses to
//     authenticate ANY caller through this path when s.serviceToken is
//     unset -- an unset secret disables the header entirely rather than
//     matching an empty value, which is what "fail closed" requires here.
//
// There is no configuration flag that disables the API-key/session check;
// one Miniflux credential (of either kind) works against both services.
//
// Authorisation: the status page, GET /api/status, and every control
// endpoint (backfill pause/resume/config, the embedder-switch endpoints)
// require requireAdmin below, not just requireAuthenticatedUser -- ANY
// valid API-key/session credential authenticates a caller, but only one
// whose user is_admin (AdminChecker / store.Store.IsAdmin) may reach
// these operator surfaces, and the service credential never reaches this
// far at all. The three data endpoints admit any valid credential
// (including the service one) and scope the request to a user via
// requireAuthenticatedUser -- see resolveAuthenticatedUserID in
// search_handlers.go and handleArticle in article_handler.go.
package web // import "miniflux.app/v2/sidecar/internal/web"

import (
	"context"
	"crypto/subtle"
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

// serviceTokenHeader is the header internal/searchclient (the fork's own
// HTTP client for this sidecar) sends the shared service secret in --
// deliberately a DIFFERENT header from authTokenHeader, so a service
// credential can never be confused with, or accidentally satisfy a check
// written for, an end-user API key. Must match
// internal/searchclient/client.go's own constant exactly.
const serviceTokenHeader = "X-Sidecar-Service-Token"

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

const (
	// authenticatedUserIDKey is the context key requireAuthenticatedUser
	// and requireAdmin store the validated caller's user id under. For a
	// service credential this is 0 -- see credentialKind's own doc
	// comment -- and resolveAuthenticatedUserID (search_handlers.go) is
	// what actually reconciles that against the request's own "user"
	// parameter.
	authenticatedUserIDKey contextKey = iota

	// credentialKindKey is the context key requireAuthenticatedUser and
	// requireAdmin store the resolved credential's kind under (see
	// credentialKind below) -- resolveAuthenticatedUserID reads this to
	// tell a service credential (which may assert any user id) apart from
	// an end-user one (which may not).
	credentialKindKey
)

// credentialKind distinguishes how a request's caller identity was
// established -- see this file's own package doc comment for what each
// one is and what it may do.
type credentialKind int

const (
	credentialNone    credentialKind = iota // resolveCredential's ok=false case; never stored in context
	credentialAPIKey                        // X-Auth-Token: a real end-user Miniflux API key
	credentialSession                       // a Miniflux web session cookie
	credentialService                       // X-Sidecar-Service-Token: the fork's own shared secret, no inherent user
)

// authenticatedUserID returns the user id the current request's
// credential resolved to -- for an API key or session cookie, its
// owner; for a service credential, 0 (see credentialKind), reconciled
// against the request's own "user" parameter by
// resolveAuthenticatedUserID rather than here. ok is false only if this
// is called from a handler that neither requireAuthenticatedUser nor
// requireAdmin wrapped -- every protected handler in this package is
// always wrapped by one of the two, so callers should treat a false here
// as a programming error (see resolveAuthenticatedUserID and
// handleArticle's own defensive handling of it) rather than a normal
// outcome.
func authenticatedUserID(r *http.Request) (int64, bool) {
	id, ok := r.Context().Value(authenticatedUserIDKey).(int64)
	return id, ok
}

// authenticatedCredentialKind returns the kind of credential the current
// request authenticated with (see credentialKind and authenticatedUserID,
// which it mirrors exactly, including the "unreachable except via a
// programming error" contract for ok=false).
func authenticatedCredentialKind(r *http.Request) (credentialKind, bool) {
	kind, ok := r.Context().Value(credentialKindKey).(credentialKind)
	return kind, ok
}

// errAuthValidatorUnavailable is resolveCredential's sentinel for "the
// credential kind this request presented has no validator configured" --
// a misconfiguration (cmd/sidecar always supplies both), never a normal
// outcome. Kept distinct from a validator's own errors so callers can
// always map it to 500 without inspecting message text.
var errAuthValidatorUnavailable = errors.New("web: no validator configured for the presented credential")

// serviceTokenMatches reports whether presented is exactly s.serviceToken,
// in constant time (crypto/subtle.ConstantTimeCompare) so a byte-by-byte
// timing difference can never leak how much of a guessed secret was
// correct. An unconfigured secret (s.serviceToken == "") always reports
// false, whatever presented is -- including the empty string -- which is
// what makes an unset SIDECAR_SERVICE_TOKEN disable this credential
// entirely rather than accept every caller that also sends nothing:
// ConstantTimeCompare on two empty byte slices reports equal, so that
// check has to come first, not be relied on to fall out of the compare
// itself.
func (s *Server) serviceTokenMatches(presented string) bool {
	if s.serviceToken == "" || presented == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(presented), []byte(s.serviceToken)) == 1
}

// resolveCredential resolves the current request's caller identity from
// whichever credential it presents (see this file's own package doc
// comment for what each one is): the X-Auth-Token API key header,
// checked first, the X-Sidecar-Service-Token shared secret next, or the
// MinifluxSessionID browser cookie (session_auth.go) when neither header
// is present. requireAuthenticatedUser and requireAdmin both call this;
// it is the single place any credential is actually validated.
//
// ok=false with err=nil covers every case that must look identical to a
// caller (§ Behaviour's "do not leak whether it exists", extended from
// API keys to sessions and to the service secret): no credential
// presented at all, an API key header naming an unknown token, a service
// token header that does not match (or an unconfigured secret -- see
// serviceTokenMatches), or a session cookie that fails to validate for
// any reason (malformed, unknown session id, wrong secret, not bound to
// a user, or deleted -- store.ValidateWebSessionCookie's own doc comment
// collapses all of these the same way ValidateAPIKey does).
//
// A non-nil err means fail closed with 500: either errAuthValidatorUnavailable
// (s.keys or s.sessions is nil for the credential kind presented) or a
// genuine validator error (a database hiccup, or -- see websession.go's
// own doc comment on the coupling risk -- an upstream schema change this
// query can no longer satisfy).
func (s *Server) resolveCredential(r *http.Request) (userID int64, kind credentialKind, ok bool, err error) {
	if token := r.Header.Get(authTokenHeader); token != "" {
		if s.keys == nil {
			return 0, credentialNone, false, errAuthValidatorUnavailable
		}
		userID, ok, err = s.keys.ValidateAPIKey(r.Context(), token)
		return userID, credentialAPIKey, ok, err
	}

	if token := r.Header.Get(serviceTokenHeader); token != "" {
		if !s.serviceTokenMatches(token) {
			return 0, credentialNone, false, nil
		}
		// No inherent owner: the caller (internal/searchclient) asserts
		// one via the "user" parameter, reconciled by
		// resolveAuthenticatedUserID -- see credentialService's own doc
		// comment for why that is safe (never accepted by requireAdmin).
		return 0, credentialService, true, nil
	}

	if cookie, cerr := r.Cookie(minifluxSessionCookieName); cerr == nil && cookie.Value != "" {
		if s.sessions == nil {
			return 0, credentialNone, false, errAuthValidatorUnavailable
		}
		userID, ok, err = s.sessions.ValidateWebSessionCookie(r.Context(), cookie.Value)
		return userID, credentialSession, ok, err
	}

	return 0, credentialNone, false, nil
}

// missingCredentialMessage is requireAuthenticatedUser and requireAdmin's
// shared 401 body for "no credential presented at all". It names
// authTokenHeader and the session cookie alternative -- the two credentials
// an operator or an end-user request would ever present -- deliberately
// omitting the service credential, which is internal/searchclient's own
// and not something a caller reading this message could productively act
// on.
var missingCredentialMessage = fmt.Sprintf(
	"authentication required: send a %s header (a Miniflux API key) or sign in to Miniflux in this browser (a %s session cookie)",
	authTokenHeader, minifluxSessionCookieName,
)

// requireAuthenticatedUser wraps next with the sidecar's data-endpoint
// authentication gate -- see resolveCredential and this file's own
// package doc comment. ANY valid credential passes, service credential
// included: unlike requireAdmin, this makes no is_admin check
// ("data endpoints: any valid credential, scoped to a user"). A valid
// credential's resolved user id and kind are attached to the request's
// context (authenticatedUserID, authenticatedCredentialKind) for next to
// read -- deriving search/article scope from the CREDENTIAL (and, for a
// service credential only, the request's own explicit assertion) rather
// than from anything an end-user credential's own caller could freely
// claim closes the door on a caller claiming a different user's id; see
// resolveAuthenticatedUserID in search_handlers.go and handleArticle in
// article_handler.go.
//
// Fails CLOSED, never open, on every error path (missing credential,
// unrecognised credential, or resolveCredential erroring): no path,
// including a misconfiguration, may end up equivalent to a flag disabling
// authentication.
func (s *Server) requireAuthenticatedUser(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, kind, ok, err := s.resolveCredential(r)
		if err != nil {
			slog.Error("web: unable to resolve the request's credential", slog.Any("error", err))
			writeAPIError(w, http.StatusInternalServerError, "authentication unavailable")
			return
		}
		if !ok {
			writeAPIError(w, http.StatusUnauthorized, missingCredentialMessage)
			return
		}

		ctx := context.WithValue(r.Context(), authenticatedUserIDKey, userID)
		ctx = context.WithValue(ctx, credentialKindKey, kind)
		next(w, r.WithContext(ctx))
	}
}

// requireAdmin wraps next with the authorisation gate for the sidecar's
// control endpoints and status page: it accepts the same end-user
// credentials requireAuthenticatedUser does (API key, session cookie),
// but additionally requires the resolved user to be a Miniflux
// administrator (AdminChecker / store.Store.IsAdmin, read-only against
// public.users.is_admin); see server.go's own doc comment on New for why
// these endpoints require admin rather than staying unauthenticated or
// accepting any valid credential.
//
// The service credential is REJECTED outright, before any IsAdmin
// lookup, however it might be paired with a "user" parameter -- it
// authorises asserting a user id on the three data endpoints and nothing
// else (see credentialService's own doc comment). Checking IsAdmin
// against an asserted id here would let anyone who merely knows the
// shared secret AND a real administrator's user id reach every control
// endpoint as that administrator, without ever presenting that
// administrator's own API key or session -- exactly the privilege
// escalation this rejection exists to close, and precisely what "must
// not grant admin" means for this credential.
//
// No credential at all -> 401 (missingCredentialMessage, same as
// requireAuthenticatedUser). A credential that resolves to a real,
// non-admin user, or a service credential -> 403 -- never 401, since the
// credential itself was valid; conflating the two would make it
// impossible for an operator to tell "you are not signed in" from "you
// are signed in but not an admin". s.admins == nil fails CLOSED with 500,
// the same "no flag disables authentication" invariant
// requireAuthenticatedUser's s.keys==nil path already enforces.
func (s *Server) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, kind, ok, err := s.resolveCredential(r)
		if err != nil {
			slog.Error("web: unable to resolve the request's credential", slog.Any("error", err))
			writeAPIError(w, http.StatusInternalServerError, "authentication unavailable")
			return
		}
		if !ok {
			writeAPIError(w, http.StatusUnauthorized, missingCredentialMessage)
			return
		}
		if kind == credentialService {
			// See this function's own doc comment: a service credential
			// can never be an administrator, by construction, regardless
			// of any user id it might assert.
			writeAPIError(w, http.StatusForbidden, "administrator access required")
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

		ctx := context.WithValue(r.Context(), authenticatedUserIDKey, userID)
		ctx = context.WithValue(ctx, credentialKindKey, kind)
		next(w, r.WithContext(ctx))
	}
}
