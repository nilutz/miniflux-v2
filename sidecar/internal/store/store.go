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

// DB returns the underlying database handle for packages that need direct
// SQL access beyond Store's own narrow, single-purpose methods.
//
// package search is the motivating case: its retrieval queries build a
// variable WHERE clause per call (an arbitrary combination of feed,
// category, date-range, unread and starred filters, per search.Filters)
// joined against a BM25 or HNSW candidate set — that doesn't fit the
// fixed-shape query pattern the rest of this package's methods use for
// passage writes and index-state bookkeeping. Exposing db directly here
// keeps that query-building logic in the package that owns the retrieval
// behaviour, rather than growing Store an ever-widening set of retrieval
// methods it has no other reason to know about.
func (s *Store) DB() *sql.DB { return s.db }
