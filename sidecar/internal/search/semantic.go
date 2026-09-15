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

// maxEfSearch is pgvector's own hard ceiling on hnsw.ef_search, read
// directly off the live database rather than assumed:
//
//	SELECT name, min_val, max_val FROM pg_settings WHERE name = 'hnsw.ef_search';
//	      name       | min_val | max_val
//	-----------------+---------+---------
//	 hnsw.ef_search  | 1       | 1000
//
// (pgvector 0.8.4.) Passing anything above it to SET LOCAL fails the
// statement outright — `ERROR: 1005 is outside the valid range for
// parameter "hnsw.ef_search" (1 .. 1000)` — which is why this ceiling is
// a constant the candidate arithmetic is clamped against rather than a
// value the code hopes it never reaches.
//
// There is a second, nastier reason this clamp exists. hnsw.ef_search
// only becomes a real, range-checked
// GUC once pgvector's module has actually been loaded into the backend.
// On a freshly handed-out pooled connection it is still a *placeholder*
// GUC, and PostgreSQL accepts any value for a placeholder. Verified
// against this database:
//
//	BEGIN;
//	SET LOCAL hnsw.ef_search = 1005;      -- WARNING, not ERROR; "SET" returned
//	SELECT current_setting('hnsw.ef_search');   -- '1005'
//	SELECT count(*) FROM search.passages;       -- loads the module
//	SELECT current_setting('hnsw.ef_search');   -- '40' — silently discarded
//
// lib/pq surfaces a WARNING only through a NoticeHandler, and none is
// configured on this driver, so on a cold connection an out-of-range
// value produced no error at all: the setting was quietly reverted to
// the default of 40 mid-query and the scan truncated to ~40 rows. That
// is precisely the silent-truncation failure the SET LOCAL work exists
// to eliminate, re-entering through a different door. An in-range value
// has no such problem — it survives the module load intact (verified the
// same way with 1000) — so clamping is a complete fix for both variants
// and no NoticeHandler is required.
const maxEfSearch = 1000

// maxVectorCandidates caps how many candidates a single vector retrieval
// (Semantic, or similar.go's nearestExcludingEntry) will ask
// passages_embedding_idx for. It is deliberately equal to maxEfSearch
// and deliberately NOT lexical.go's maxCandidates.
//
// The two must agree because they describe one thing from two sides: the
// candidates CTE's LIMIT is how many rows the index scan is asked to
// produce, and hnsw.ef_search is how wide that scan searches in order to
// produce them. Letting the LIMIT exceed what ef_search can cover is the
// original silent-truncation bug (the scan just returns fewer rows, with
// no error); clamping them to the same number keeps the request and the
// search width honest with each other at every limit.
//
// Consequence worth knowing at the call site: above a per-channel limit
// of 200, the effective over-fetch ratio degrades below
// candidateMultiplier's nominal 5x (at limit 500 it is 2x, at 1000 it is
// 1x), so a Filters predicate narrow enough to exclude most of the
// unfiltered ranking can under-fill a very large page. That is a
// deliberate trade against the alternative — asking the index for more
// rows than its search width can find, and silently getting fewer.
const maxVectorCandidates = maxEfSearch

// vectorCandidates returns how many candidates a vector retrieval should
// ask passages_embedding_idx for, given the caller's own limit: limit
// over-fetched by candidateMultiplier, floored at minCandidates so a
// tiny limit still leaves a narrow filter something to work with, and
// ceilinged at maxVectorCandidates so the value can always be handed
// verbatim to SET LOCAL hnsw.ef_search.
//
// Semantic and nearestExcludingEntry share this rather than repeating
// the three-line clamp, so the invariant "the candidate LIMIT and
// hnsw.ef_search are the same number, and that number is always within
// pgvector's range" holds in exactly one place instead of two.
func vectorCandidates(limit int) int {
	candidates := limit * candidateMultiplier
	if candidates < minCandidates {
		candidates = minCandidates
	}
	if candidates > maxVectorCandidates {
		candidates = maxVectorCandidates
	}
	return candidates
}

// Semantic ranks passages by cosine distance (embedding <=> query vector,
// public.vector_cosine_ops — the same operator class passages_embedding_idx
// is built on) against query's own embedding, applying f within its own
// candidate scan, and returns up to limit hits ordered best-first
// (smallest distance first). Score on each hit is that raw cosine distance: 0 for an
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
// f is applied inside the candidates CTE, beneath its LIMIT, exactly as
// in Lexical (see Filters' doc comment). One caveat remains specific to
// this path: passages_embedding_idx cannot evaluate the predicate itself,
// so the planner chooses between an HNSW scan whose results are then
// filtered — which can still under-fill, since an HNSW scan yields at
// most hnsw.ef_search tuples — and an exact filtered scan sorted by
// distance. At this corpus's size it picks the exact plan and the
// candidate set is complete; on a much larger corpus with a narrow
// filter, pgvector's hnsw.iterative_scan is the lever for that case.
func (s *Searcher) Semantic(ctx context.Context, query string, limit int, f Filters) ([]PassageHit, error) {
	if strings.TrimSpace(query) == "" || limit <= 0 {
		return nil, nil
	}

	vec, err := s.embedQuery(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("search: unable to embed query: %w", err)
	}

	candidates := vectorCandidates(limit)

	var b strings.Builder
	args := []any{vectorLiteral(vec), candidates}
	where, whereArgs := f.whereClause(len(args) + 1)
	args = append(args, whereArgs...)

	b.WriteString(`
		WITH candidates AS (
			SELECT p.id, p.entry_id, p.ordinal, p.text, p.char_start, p.char_end, p.source,
			       (p.embedding <=> $1::public.vector) AS distance
			FROM search.passages p
	`)
	if where != "" {
		b.WriteString(" JOIN entries e ON e.id = p.entry_id WHERE ")
		b.WriteString(where)
	}
	b.WriteString(`
			ORDER BY p.embedding <=> $1::public.vector
			LIMIT $2
		)
		SELECT c.id, c.entry_id, c.ordinal, c.text, c.char_start, c.char_end, c.source, c.distance
		FROM candidates c
	`)

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
	// 40) must be raised to at least candidates (never above maxEfSearch,
	// which vectorCandidates already guarantees) before the query below
	// runs its index scan, or the scan silently returns fewer rows than
	// requested — not an error, just quietly short. This has to be its
	// own statement, executed strictly before the query it configures, on
	// the same connection: folding set_config into a MATERIALIZED CTE in
	// the same query looks right but does not take effect — MATERIALIZED
	// only stops the CTE being inlined away, it does not order execution,
	// and the planner is free to place it on the inner side of a nested
	// loop whose outer side is the very index scan it was meant to
	// configure, running it after the scan instead of before. SET LOCAL,
	// run here as its own round trip inside this explicit transaction, is
	// ordered by the wire protocol instead: this statement completes
	// before the next one is even sent, and it is scoped to this
	// transaction alone (never leaks to whatever query the pooled
	// connection serves next once this one ends).
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
