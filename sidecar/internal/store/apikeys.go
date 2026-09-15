// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package store // import "miniflux.app/v2/sidecar/internal/store"

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ValidateAPIKey looks up token in Miniflux's own public.api_keys table
// and returns the id of the user that owns it. It is the ONLY thing that
// makes an HTTP caller "who they say they are" from the sidecar's point
// of view -- see internal/web's auth middleware, which derives every
// protected request's search scope from this return value rather than
// from anything the caller separately asserts (a "user" query parameter,
// say).
//
// ok is false, with a nil error, for a token this table has no row for --
// a token that was never issued, or a revoked key whose row Miniflux
// itself deleted. Both cases must look identical to a caller, to avoid
// leaking whether a token exists at all: collapsing "unknown" and
// "revoked" into the same (0, false, nil) return does exactly that.
//
// This is a single indexed lookup (api_keys.token is UNIQUE, per
// internal/database/migrations.go:333 on the fork) against a table the
// sidecar already shares a database connection with -- the same table and
// header (X-Auth-Token) internal/api/middleware.go already validates on
// the fork's own REST API, so one Miniflux key works against both
// services.
//
// Deliberately READ-ONLY: it never writes api_keys.last_used_at, unlike
// the fork's own validateAPIKeyAuth. That column belongs to Miniflux; a
// write here would make the sidecar a writer of upstream state for the
// first time, which is deliberately avoided. A key that is only ever used
// against the sidecar therefore never advances its last_used_at in
// Miniflux's own UI -- an accepted, documented trade-off (see
// sidecar/README.md), not an oversight.
func (s *Store) ValidateAPIKey(ctx context.Context, token string) (userID int64, ok bool, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT user_id FROM api_keys WHERE token = $1`, token).Scan(&userID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("store: unable to validate api key: %w", err)
	}
	return userID, true, nil
}
