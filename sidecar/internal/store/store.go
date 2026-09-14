// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package store // import "miniflux.app/v2/sidecar/internal/store"

import (
	"context"
	"database/sql"
	"fmt"

	_ "github.com/lib/pq"
)

// Store owns the `search` schema and reads Miniflux's `public` schema
// read-only.
type Store struct {
	db *sql.DB
}

// Reader is the narrow read-only subset of *sql.DB that a package needs
// to build its own dynamic queries — QueryContext and QueryRowContext
// only. Exec and Begin are deliberately absent: store is meant to be the
// sole writer to search.passages and the only thing that touches
// public.entries beyond a plain SELECT, and a full *sql.DB handle would
// hand any future consumer of Reader a way around that boundary. *sql.DB
// already implements this interface, so Store.Reader needs no wrapper.
type Reader interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
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

// Reader returns a read-only view of the underlying connection for
// packages that need to build dynamic queries beyond Store's own
// fixed-shape methods.
//
// package search is the motivating case: its retrieval queries build a
// variable WHERE clause per call (an arbitrary combination of feed,
// category, date-range, unread and starred filters, per search.Filters)
// joined against a BM25 or HNSW candidate set — that doesn't fit the
// fixed-shape query pattern the rest of this package's methods use for
// passage writes and index-state bookkeeping. Returning the narrow Reader
// interface rather than *sql.DB keeps that query-building logic in the
// package that owns the retrieval behaviour, without also handing it
// Exec or Begin: store stays the only writer to search.passages, and the
// only thing that writes to public.entries beyond a plain SELECT.
func (s *Store) Reader() Reader { return s.db }
