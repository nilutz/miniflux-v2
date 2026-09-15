// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package store // import "miniflux.app/v2/sidecar/internal/store"

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// This file exists so a rebase conflict is findable in one place: it is
// the ONLY thing in the sidecar that reads Miniflux's
// web_sessions table, and it deliberately mirrors -- by name, hash
// scheme and cookie format, not by re-deriving them -- four upstream
// files on the fork this module cannot import (a Go module boundary
// separates sidecar/ from the rest of this repository, so a change to
// any of the four below breaks ValidateWebSessionCookie with NO compile
// error; see this function's own doc comment for what an operator would
// see and how to diagnose it):
//
//   - internal/database/migrations.go (search for "CREATE TABLE
//     web_sessions"): the table shape this file's SELECT depends on --
//     id text, secret_hash bytea, user_id int (nullable).
//   - internal/model/web_session.go: the hashing scheme. hashWebSessionSecret
//     is sha256.Sum256 of the raw secret with no salt, and VerifySecret
//     compares with crypto/subtle.ConstantTimeCompare -- both mirrored
//     exactly below.
//   - internal/ui/auth.go: sessionCookieName = "MinifluxSessionID" --
//     mirrored as minifluxSessionCookieName in this package's caller,
//     sidecar/internal/web/session_auth.go.
//   - internal/ui/web_session_middleware.go: loadWebSessionFromCookie's
//     cookie VALUE format, "<session id>.<raw secret>", split with
//     strings.Cut on the first ".".

// ValidateWebSessionCookie validates the raw value of Miniflux's own
// MinifluxSessionID cookie (see sidecar/internal/web/session_auth.go for
// where that cookie is read from an incoming request) and returns the id
// of the user it is bound to.
//
// This is the sidecar's "no second login" path: a browser
// already signed into Miniflux carries this cookie, and reusing it here
// -- read-only, exactly as Miniflux's own web_session_middleware.go reads
// it -- is what lets that same browser open the sidecar's admin page
// with no separate credential.
//
// ok is false, with a nil error, for every case that must look identical
// to a caller (the same "do not leak whether it exists" rule
// store.ValidateAPIKey's own doc comment documents for API keys, extended
// here to sessions): a malformed cookie value, a session id no row
// matches, a secret that does not hash to the stored secret_hash, and a
// session that exists but is not bound to any user (state.userID IS NULL
// -- an anonymous, not-yet-logged-in browser session; see
// model.WebSession.IsAuthenticated's own doc comment on the fork).
//
// Deliberately READ-ONLY, matching ValidateAPIKey: it never writes
// web_sessions in any way -- no rotation, no UpdateWebSession, nothing.
// Miniflux owns that table exclusively; only its own middleware ever
// creates, rotates or expires a session row.
//
// If upstream ever changes the cookie's value format, the hash function,
// or the table's column names, this query either errors outright (a
// renamed/missing column -- surfaced to the caller as err != nil, a 401
// with "authentication unavailable" logged server-side, NOT a silent
// bypass) or, more dangerously, starts returning ok=false for every
// session that used to validate (a changed hash scheme) -- indistinguishable
// from "no browser has any valid Miniflux session," which is why this
// package's test suite includes TestValidateWebSessionCookieRejectsAKnownGoodSessionIfTheSchemaShapeChanges:
// see that test's own doc comment for the operator-facing diagnosis this
// mismatch produces.
func (s *Store) ValidateWebSessionCookie(ctx context.Context, cookieValue string) (userID int64, ok bool, err error) {
	sessionID, secret, found := strings.Cut(cookieValue, ".")
	if !found || sessionID == "" || secret == "" {
		return 0, false, nil
	}

	var secretHash []byte
	var nullUserID sql.NullInt64
	err = s.db.QueryRowContext(ctx,
		`SELECT secret_hash, user_id FROM web_sessions WHERE id = $1`,
		sessionID,
	).Scan(&secretHash, &nullUserID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("store: unable to validate web session cookie: %w", err)
	}

	sum := sha256.Sum256([]byte(secret))
	if subtle.ConstantTimeCompare(sum[:], secretHash) != 1 {
		return 0, false, nil
	}

	if !nullUserID.Valid {
		return 0, false, nil
	}

	return nullUserID.Int64, true, nil
}
