// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package store // import "miniflux.app/v2/sidecar/internal/store"

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// IsAdmin reports whether userID is a Miniflux administrator, read-only,
// against public.users.is_admin (internal/database/migrations.go's
// initial migration: "is_admin bool default 'f'" -- a plain, non-nullable
// boolean column, unlike web_sessions' shape; there is no comparable
// coupling risk here to call out the way there is in websession.go).
//
// This is task 18's authorisation check for the sidecar's control
// endpoints and status page: any valid Miniflux credential (API key or
// web session) identifies a user, but only an is_admin user may reach an
// operator surface -- see sidecar/internal/web/auth.go's requireAdmin.
//
// A userID with no matching row (a stale session or key whose user was
// deleted, which foreign keys ON DELETE CASCADE should prevent, but this
// stays defensive) reports false, not an error: fail closed, never open,
// on a caller this table has nothing to say is an administrator.
func (s *Store) IsAdmin(ctx context.Context, userID int64) (bool, error) {
	var isAdmin bool
	err := s.db.QueryRowContext(ctx, `SELECT is_admin FROM users WHERE id = $1`, userID).Scan(&isAdmin)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: unable to check admin status: %w", err)
	}
	return isAdmin, nil
}
