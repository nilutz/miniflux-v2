// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package search // import "miniflux.app/v2/sidecar/internal/search"

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
)

// txBeginner is the one capability Semantic needs beyond store.Reader's
// plain QueryContext/QueryRowContext: a real, explicit transaction, so
// that "raise hnsw.ef_search" and "run the query it configures" are two
// ordered round trips on the same connection, guaranteed by the wire
// protocol rather than by planner behaviour (see the SET LOCAL comment
// inside Semantic for why that distinction is the whole fix).
//
// This is intentionally not added to store.Reader itself, which stays
// exactly the narrow QueryContext/QueryRowContext surface it always was
// (see that interface's own doc comment in package store): every
// production Searcher's db is in fact backed by *sql.DB — store.Store.Reader
// returns it directly — which already implements BeginTx, so the type
// assertion in Semantic succeeds for every real caller. Only a hand-rolled
// store.Reader fake without BeginTx (as this package's own hermetic tests
// use for the empty-query and embedder-failure cases) fails it, and
// those tests return before Semantic ever reaches this code path.
type txBeginner interface {
	BeginTx(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error)
}

// Semantic ranks passages by cosine distance (embedding <=> query vector,
// public.vector_cosine_ops — the same operator class passages_embedding_idx
// is built on) against query's own embedding, applying f after retrieval,
// and returns up to limit hits ordered best-first (smallest distance
// first). Score on each hit is that raw cosine distance: 0 for an
// identical direction, 1 for orthogonal, 2 for opposite — never a
// similarity, so a smaller Score is always a better match here, the
// opposite sense from Lexical's Score. Rank, as with Lexical, is always
// this retrieval's own 1-based, contiguous position and is safe to fuse on
// without re-deriving it.
//
// query is embedded once per call through the Searcher's configured
// embedder (see WithEmbedder), consulting its query cache first (spec
// §6.5) — repeating the same query text costs one cache lookup, not
// another forward pass.
//
// An empty (after trimming) query or a non-positive limit returns no hits
// and no error without touching the embedder or the database, mirroring
// Lexical exactly: there is nothing useful to embed or search for on
// behalf of a blank search bar or a zero-result page size.
//
// Two failure modes are deliberately never silent, because either one
// returning zero hits would look exactly like "no matches" and could hide
// for months:
//
//   - Semantic called on a Searcher with no embedder configured (NewSearcher
//     without WithEmbedder) returns an error rather than a nil-pointer
//     panic or empty results.
//   - A failing embedder (model unavailable, request timeout, etc.) is
//     returned as an error, not swallowed into an empty hit list.
//
// Caveat for callers of a filtered search: identical to Lexical's own —
// passages_embedding_idx covers only the embedding column and cannot push a
// Filters predicate into its HNSW scan, so f is applied after Semantic
// pulls its candidate set. A filter narrow enough to exclude more than
// roughly the top candidateMultiplier fraction of the unfiltered ranking
// can legitimately return fewer than limit hits even though more matching
// passages exist further down the corpus's vector ranking.
func (s *Searcher) Semantic(ctx context.Context, query string, limit int, f Filters) ([]PassageHit, error) {
	if strings.TrimSpace(query) == "" || limit <= 0 {
		return nil, nil
	}

	vec, err := s.embedQuery(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("search: unable to embed query: %w", err)
	}

	candidates := limit * candidateMultiplier
	if candidates < minCandidates {
		candidates = minCandidates
	}
	if candidates > maxCandidates {
		candidates = maxCandidates
	}

	var b strings.Builder
	args := []any{vectorLiteral(vec), candidates}

	b.WriteString(`
		WITH candidates AS (
			SELECT id, entry_id, ordinal, text, char_start, char_end, source,
			       (embedding <=> $1::public.vector) AS distance
			FROM search.passages
			ORDER BY embedding <=> $1::public.vector
			LIMIT $2
		)
		SELECT c.id, c.entry_id, c.ordinal, c.text, c.char_start, c.char_end, c.source, c.distance
		FROM candidates c
	`)

	if !f.empty() {
		b.WriteString(" JOIN entries e ON e.id = c.entry_id")
	}

	where, whereArgs := f.whereClause(len(args) + 1)
	if where != "" {
		b.WriteString(" WHERE ")
		b.WriteString(where)
		args = append(args, whereArgs...)
	}

	b.WriteString(fmt.Sprintf(" ORDER BY c.distance ASC, c.id ASC LIMIT $%d", len(args)+1))
	args = append(args, limit)

	beginner, ok := s.db.(txBeginner)
	if !ok {
		return nil, fmt.Errorf("search: Semantic requires a transaction-capable database connection (got %T), so hnsw.ef_search can be raised as its own statement ahead of the query it configures", s.db)
	}

	tx, err := beginner.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("search: unable to begin semantic query transaction: %w", err)
	}
	// Read-only transaction: Rollback is always a safe way to end it,
	// whether the query below succeeds or fails, and there is never a
	// data change here to lose by not committing.
	defer tx.Rollback()

	// hnsw.ef_search (pgvector's HNSW search-time candidate list, default
	// 40) must be raised to at least candidates — the LIMIT the query
	// below asks its own index scan for — or the scan silently returns
	// fewer rows than requested, not an error, just quietly short. This
	// has to be its own statement, executed strictly before the query it
	// configures, on the same connection: an earlier version folded
	// set_config into a MATERIALIZED CTE inside the same query, which
	// looked right but did not take effect (found in review) — EXPLAIN
	// ANALYZE showed the planner was free to place that CTE on the
	// *inner* side of a nested loop whose *outer* side was the very
	// index scan it was meant to configure, so it ran *after* the scan,
	// once per already-produced outer row, never before it.
	// MATERIALIZED stops a CTE being inlined away; it does not order
	// execution. SET LOCAL, run here as its own round trip inside this
	// explicit transaction, is ordered by the wire protocol instead:
	// this statement completes before the next one is even sent. It is
	// also scoped to this transaction alone (never leaks to whatever
	// query the pooled connection serves next once this one ends).
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("SET LOCAL hnsw.ef_search = %d", candidates)); err != nil {
		return nil, fmt.Errorf("search: unable to raise hnsw.ef_search: %w", err)
	}

	rows, err := tx.QueryContext(ctx, b.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("search: semantic query failed: %w", err)
	}
	defer rows.Close()

	var hits []PassageHit
	for rows.Next() {
		var h PassageHit
		if err := rows.Scan(&h.PassageID, &h.EntryID, &h.Ordinal, &h.Text, &h.CharStart, &h.CharEnd, &h.Source, &h.Score); err != nil {
			return nil, fmt.Errorf("search: unable to scan semantic hit: %w", err)
		}
		hits = append(hits, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("search: error reading semantic results: %w", err)
	}

	for i := range hits {
		hits[i].Rank = i + 1
	}

	return hits, nil
}

// embedQuery returns query's embedding via the Searcher's cache, calling
// the configured embedder only on a cache miss. It fails clearly — rather
// than dereferencing a nil embedder — when NewSearcher was never given
// WithEmbedder, since that is a caller wiring mistake Semantic must never
// paper over with empty results.
func (s *Searcher) embedQuery(ctx context.Context, query string) ([]float32, error) {
	if s.embedder == nil {
		return nil, fmt.Errorf("search: Semantic requires an embedder; construct the Searcher with search.WithEmbedder")
	}
	return embedCached(ctx, s.cache, s.embedder, query)
}

// vectorLiteral formats an embedding using pgvector's plain text input
// format ("[0.1,0.2,...]"), the format understood by an explicit ::vector
// cast. Mirrors store's own unexported helper of the same name and
// purpose (package store cannot export it for this package to reuse
// without widening store's own public surface for a single call site).
func vectorLiteral(embedding []float32) string {
	var sb strings.Builder
	sb.WriteByte('[')
	for i, v := range embedding {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(strconv.FormatFloat(float64(v), 'g', -1, 32))
	}
	sb.WriteByte(']')
	return sb.String()
}
