// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package web // import "miniflux.app/v2/sidecar/internal/web"

import "context"

// minifluxSessionCookieName is the cookie Miniflux's own web UI sets and
// reads for a signed-in browser session -- internal/ui/auth.go's
// sessionCookieName constant, matched here EXACTLY, by name, so a browser
// already signed into Miniflux carries a cookie this package recognises
// with no separate sidecar login.
//
// This is the one place in the web package that names the cookie; the
// value itself is validated by store.Store.ValidateWebSessionCookie
// (sidecar/internal/store/websession.go), which has its own doc comment
// on the upstream files it mirrors and the coupling risk that creates
// across the module boundary. If Miniflux ever renames this cookie, every
// browser request simply stops carrying it at all (resolveCredential's
// r.Cookie lookup returns http.ErrNoCookie, indistinguishable from "not
// signed in") -- there is no error to surface for that specific failure
// mode, which is why this constant is named plainly and kept in sync by
// inspection, the same as authTokenHeader in auth.go.
const minifluxSessionCookieName = "MinifluxSessionID"

// SessionValidator validates the raw value of Miniflux's own
// MinifluxSessionID cookie, read-only, against public.web_sessions -- the
// same table Miniflux's own web_session_middleware.go reads (see
// store/websession.go's doc comment for exactly which upstream files this
// mirrors). Defined as an interface, not *store.Store directly, for the
// same hermetic-tests reason APIKeyValidator is (see that type's own doc
// comment in auth.go): this package's suite must be able to run with no
// database. *store.Store's ValidateWebSessionCookie satisfies this with
// no adaptation.
//
// ok is false, with a nil error, for every cookie that must produce an
// identical response to an HTTP caller: malformed, unknown session id,
// wrong secret, a session not yet bound to any user, or one whose
// underlying row Miniflux has since deleted (sign-out, or its own
// cleanup sweep) -- see ValidateWebSessionCookie's own doc comment.
type SessionValidator interface {
	ValidateWebSessionCookie(ctx context.Context, cookieValue string) (userID int64, ok bool, err error)
}

// AdminChecker reports whether userID is a Miniflux administrator,
// read-only, against public.users.is_admin -- the authorisation check for
// the control endpoints and status page (requireAdmin, auth.go). Defined
// as an interface for the same hermetic-tests reason every other
// narrow store dependency in this package is. *store.Store's IsAdmin
// satisfies this with no adaptation.
type AdminChecker interface {
	IsAdmin(ctx context.Context, userID int64) (bool, error)
}
