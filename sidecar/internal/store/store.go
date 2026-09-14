// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package store // import "miniflux.app/v2/sidecar/internal/store"

import (
	"database/sql"
	"fmt"

	_ "github.com/lib/pq"
)

// Store owns the `search` schema and reads Miniflux's `public` schema
// read-only.
type Store struct {
	db *sql.DB
}

func New(dsn string) (*Store, error) {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: unable to open database: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Ping() error { return s.db.Ping() }
