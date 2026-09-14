// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package search // import "miniflux.app/v2/sidecar/internal/search"

import (
	"context"
	"fmt"
	"strings"

	"github.com/lib/pq"

	"miniflux.app/v2/sidecar/internal/embed"
	"miniflux.app/v2/sidecar/internal/store"
)

// candidateMultiplier is how many times limit's worth of candidates are
// pulled from passages_bm25_idx (and, in semantic.go/similar.go, from
// passages_embedding_idx) before Filters is applied. The index covers
// only (id, text) — it cannot push a feed/category/date/status predicate
// into its own scan — so a candidate set that already matches the final
// limit exactly would come back short after filtering removes some of
// it. 5x is generous enough that a filter excluding up to 80% of an
// unfiltered ranking still leaves a full page of results, without
// pulling in enough of the corpus's long BM25 tail to matter for latency.
//
// BEWARE THE AMPLIFICATION. This is the inner of two independent 5x
// multipliers, and they COMPOUND. A caller's Request.Limit is multiplied
// by searchCandidateMultiplier (fuse.go, 5x) at the fusion boundary
// before it is handed to Lexical/Semantic as *their* limit, which then
// multiplies it by candidateMultiplier (5x) again to size its own index
// scan. The total amplification from Request.Limit to rows asked of an
// index is therefore 25x, not 5x:
//
//	Request.Limit=10  ->  50 per channel  ->  250 index candidates
//	Request.Limit=40  ->  200 per channel ->  1000 index candidates
//	Request.Limit=100 -> 500 per channel  -> 2500 index candidates (clamped)
//
// Each multiplier's comment used to describe only its own layer, which
// is exactly how a Critical went unnoticed for a full review cycle: the
// semantic path feeds this product into `SET LOCAL hnsw.ef_search`,
// which pgvector caps at 1000 (see maxVectorCandidates in semantic.go),
// so every hybrid search asking for more than 40 results failed
// outright. Any change to either constant must be reasoned about as a
// change to the product.
const candidateMultiplier = 5

// minCandidates is a floor under candidateMultiplier*limit so that a
// small limit (say, limit=1) still overfetches enough candidates for a
// narrow filter to have something left to return.
const minCandidates = 50

// maxCandidates caps how many candidates a single Lexical call will ever
// ask ParadeDB to rank and return, regardless of how large limit or
// candidateMultiplier's product gets — a defensive ceiling against a
// caller-supplied limit turning one query into a full-table scan.
//
// This ceiling is LEXICAL-ONLY and is purely self-imposed: ParadeDB has
// no server-side limit of its own that 2000 corresponds to. The vector
// path must NOT reuse it — pgvector imposes a real, database-enforced
// ceiling on hnsw.ef_search that is half this value, so semantic.go and
// similar.go clamp against maxVectorCandidates instead. Reusing this
// constant there is what caused the Critical documented on
// candidateMultiplier above.
const maxCandidates = 2000

// Searcher runs retrieval queries against search.passages and
// public.entries through a store.Reader. Beyond that connection, its only
// state is what Semantic needs to embed a query — an embed.Embedder and
// its query cache, both optional (nil when NewSearcher is called without
// WithEmbedder) — so a single Searcher is safe to share and reuse across
// concurrent requests: Lexical opens its own query with its own arguments
// every call, and Semantic's cache is itself safe for concurrent use.
//
// It holds store.Reader, not *store.Store: retrieval only ever reads, and
// the narrower type keeps that true at compile time rather than by
// convention — nothing in this package can Exec or Begin its way around
// store's ownership of writes to search.passages.
type Searcher struct {
	db       store.Reader
	embedder embed.Embedder
	cache    *queryCache
}

// SearcherOption configures a Searcher with dependencies beyond the store
// every retrieval method needs. WithEmbedder is the only option today.
type SearcherOption func(*Searcher)

// WithEmbedder attaches e as the Searcher's query embedder, backed by a
// query cache of defaultQueryCacheSize entries (spec §6.5), so that
// Semantic has something to embed a query with. A Searcher built without
// this option can still run Lexical; calling Semantic on it fails with a
// clear error rather than a nil-pointer panic.
func WithEmbedder(e embed.Embedder) SearcherOption {
	return func(s *Searcher) {
		s.embedder = e
		s.cache = newQueryCache(defaultQueryCacheSize)
	}
}

// NewSearcher builds a Searcher over an already-migrated Store. Pass
// WithEmbedder to also enable Semantic; Lexical needs no options.
func NewSearcher(s *store.Store, opts ...SearcherOption) *Searcher {
	searcher := &Searcher{db: s.Reader()}
	for _, opt := range opts {
		opt(searcher)
	}
	return searcher
}

// Lexical ranks passages by BM25 (paradedb.score) against query, applying
// f after retrieval, and returns up to limit hits ordered best-first.
//
// query is never interpolated into pg_search's query-string syntax: it is
// passed as the value argument to paradedb.match('text', query), which
// tokenizes and matches it as data. `text @@@ $1` — a naked parameterized
// string implicitly cast to paradedb.searchqueryinput — was measured
// directly against this database to invoke the same Tantivy query-string
// parser as a literal `text @@@ '...'`, and errors on unbalanced
// parentheses, bare boolean keywords, and other syntax a real search bar
// will receive from real users. paradedb.match sidesteps that parser
// entirely, so no input can produce a SQL/pg_search syntax error here —
// only zero matches.
//
// An empty (after trimming) query or a non-positive limit returns no
// hits and no error without touching the database: there is nothing a
// blank search bar or a zero-result page size could usefully ask
// ParadeDB for.
//
// Caveat for callers of a filtered search: f is applied after Lexical
// pulls its BM25 candidate set (passages_bm25_idx covers only (id, text)
// and cannot push a Filters predicate into its own scan), so a filter
// narrow enough to exclude more than roughly the top candidateMultiplier
// fraction of the unfiltered ranking can legitimately return fewer than
// limit hits even when more matching passages exist further down BM25's
// ranking. This is not a bug to chase — it is the accepted cost of an
// index that cannot filter for itself — but it is worth knowing before
// diagnosing a short result page as a retrieval defect.
func (s *Searcher) Lexical(ctx context.Context, query string, limit int, f Filters) ([]PassageHit, error) {
	if strings.TrimSpace(query) == "" || limit <= 0 {
		return nil, nil
	}

	candidates := limit * candidateMultiplier
	if candidates < minCandidates {
		candidates = minCandidates
	}
	if candidates > maxCandidates {
		candidates = maxCandidates
	}

	var b strings.Builder
	args := []any{query, candidates}

	b.WriteString(`
		WITH candidates AS (
			SELECT id, entry_id, ordinal, text, char_start, char_end, source,
			       paradedb.score(id) AS score
			FROM search.passages
			WHERE text @@@ paradedb.match('text', $1)
			ORDER BY paradedb.score(id) DESC, id ASC
			LIMIT $2
		)
		SELECT c.id, c.entry_id, c.ordinal, c.text, c.char_start, c.char_end, c.source, c.score
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

	b.WriteString(fmt.Sprintf(" ORDER BY c.score DESC, c.id ASC LIMIT $%d", len(args)+1))
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, b.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("search: lexical query failed: %w", err)
	}
	defer rows.Close()

	var hits []PassageHit
	for rows.Next() {
		var h PassageHit
		if err := rows.Scan(&h.PassageID, &h.EntryID, &h.Ordinal, &h.Text, &h.CharStart, &h.CharEnd, &h.Source, &h.Score); err != nil {
			return nil, fmt.Errorf("search: unable to scan lexical hit: %w", err)
		}
		hits = append(hits, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("search: error reading lexical results: %w", err)
	}

	for i := range hits {
		hits[i].Rank = i + 1
	}

	return hits, nil
}

// whereClause builds the dynamic filter predicate against the "e" alias
// (public.entries) that every retrieval method in this package applies
// after fetching its own candidate set — see Filters' own doc comment for
// why filtering can't happen inside either index's scan. argStart is the
// 1-based placeholder number the first generated predicate should use;
// callers append their own leading arguments (the query text, a
// candidate LIMIT, an embedding) before this clause's.
//
// It returns "" with a nil arg slice when f is empty, so a caller can
// skip both the JOIN and the WHERE entirely for the common unfiltered
// case rather than filtering against an always-true predicate.
func (f Filters) whereClause(argStart int) (string, []any) {
	if f.empty() {
		return "", nil
	}

	var conds []string
	var args []any
	idx := argStart

	if len(f.FeedIDs) > 0 {
		conds = append(conds, fmt.Sprintf("e.feed_id = ANY($%d)", idx))
		args = append(args, pq.Array(f.FeedIDs))
		idx++
	}
	if len(f.CategoryIDs) > 0 {
		conds = append(conds, fmt.Sprintf("e.feed_id IN (SELECT id FROM feeds WHERE category_id = ANY($%d))", idx))
		args = append(args, pq.Array(f.CategoryIDs))
		idx++
	}
	if !f.Since.IsZero() {
		conds = append(conds, fmt.Sprintf("e.published_at >= $%d", idx))
		args = append(args, f.Since)
		idx++
	}
	if !f.Until.IsZero() {
		conds = append(conds, fmt.Sprintf("e.published_at <= $%d", idx))
		args = append(args, f.Until)
		idx++
	}
	if f.UnreadOnly {
		conds = append(conds, "e.status = 'unread'")
	}
	if f.StarredOnly {
		conds = append(conds, "e.starred = true")
	}

	return strings.Join(conds, " AND "), args
}
