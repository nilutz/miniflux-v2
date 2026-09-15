// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package store // import "miniflux.app/v2/sidecar/internal/store"

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// EmbedderSettings is the operator's persisted embedder choice (Task 9,
// spec §13.1): which kind of embedder ("local" or "remote") and, for
// "remote", which URL. See search.embedder_settings' own migration
// comment for why this is a singleton row rather than a general-purpose
// settings table.
type EmbedderSettings struct {
	Kind      string
	RemoteURL string
	UpdatedAt time.Time
}

// GetEmbedderSettings returns the persisted embedder choice, or nil (and
// no error) if an operator has never switched embedders through the admin
// page -- a distinct, meaningful state from any row's own content: it is
// what lets cmd/sidecar fall back to the SIDECAR_EMBEDDER/
// SIDECAR_REMOTE_EMBEDDER_URL environment variables as the *initial*
// default (spec §13.1: "the persisted row wins; the environment variables
// are the initial default used only when no row exists"). Callers must
// not confuse a nil result with an error.
func (s *Store) GetEmbedderSettings(ctx context.Context) (*EmbedderSettings, error) {
	var row EmbedderSettings
	err := s.db.QueryRowContext(ctx, `
		SELECT kind, remote_url, updated_at
		FROM search.embedder_settings
		WHERE id = 1
	`).Scan(&row.Kind, &row.RemoteURL, &row.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: unable to read embedder settings: %w", err)
	}
	return &row, nil
}

// SetEmbedderSettings persists kind/remoteURL as the operator's embedder
// choice, overwriting whatever (if anything) was recorded before. It is
// the only way an operator's switch (Task 9's admin page) survives a
// restart -- without it, the compose stack's `restart: unless-stopped`
// would silently revert to the environment variables' default the next
// time the container restarts, changing the model identity back with
// nobody asking.
//
// remoteURL is stored as given even when kind is "local" (where it is
// meaningless) -- deliberately, so switching local -> remote -> local
// does not lose the remote URL an operator may want to switch back to,
// and so the stored row always reflects exactly the form fields the
// admin page last submitted, with no silent server-side clearing.
func (s *Store) SetEmbedderSettings(ctx context.Context, kind, remoteURL string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO search.embedder_settings (id, kind, remote_url, updated_at)
		VALUES (1, $1, $2, now())
		ON CONFLICT (id) DO UPDATE
		SET kind = EXCLUDED.kind, remote_url = EXCLUDED.remote_url, updated_at = EXCLUDED.updated_at
	`, kind, remoteURL)
	if err != nil {
		return fmt.Errorf("store: unable to persist embedder settings: %w", err)
	}
	return nil
}
