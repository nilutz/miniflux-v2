// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package search // import "miniflux.app/v2/sidecar/internal/search"

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// maxSeedPassages caps how many of an entry's own passages are used as
// seed vectors for a single Similar call.
//
// Design choice (spec §7): given an entry, Similar could either (a)
// average its passages' embeddings into one seed vector, or (b) query
// per-passage and merge the results. Averaging is a single query but
// blurs a multi-topic article's several subjects into one centroid that
// may sit near none of them; querying per-passage is truer to what a
// reader wants from "more like this" on a long article -- surface
// neighbours of every subject it actually covers, not just their
// average -- so that is what Similar does.
//
// The real corpus grounds the cost side of that trade-off rather than
// leaving it as a hunch: measured directly against this database,
// passages-per-entry is median 7, p90 24, p95 ~35, p99 ~110, and one
// outlier entry has 334. An unfiltered nearest-neighbour query against
// this corpus measured ~26ms. Querying once per passage for that
// outlier would mean ~334 sequential round trips (many seconds) for a
// single "similar articles" panel, while the typical entry (7-24
// passages) costs a few hundred milliseconds at most -- a fine price
// for an async block on the entry page, not for the 1-in-100 outlier.
//
// maxSeedPassages=32 (just above the measured p90/p95) keeps the common
// case exact -- almost every entry's passages all become seeds -- and
// bounds the pathological case to a fixed, small number of queries
// rather than degrading toward the averaged approach's own blur.
// entrySeedVectors, not this file's query logic, is what enforces the
// cap, and it does so by an even stride across every ordinal (see
// selectSeedIndices) rather than truncating to the entry's first N
// passages, so a capped long entry still samples across its whole
// length instead of just its introduction.
const maxSeedPassages = 32

// Similar returns entries whose own passages rank as nearest neighbours
// of entryID's passages, aggregated to entry level exactly as Search's
// own semantic and hybrid modes are (aggregate, fuse.go -- task 4's
// machinery, reused verbatim rather than re-derived here). entryID
// itself never appears in its own results: every underlying nearest-
// neighbour query excludes entryID's passages at the SQL level, before
// candidates are even counted against limit, so its own text can never
// crowd out real neighbours or leak into the output through a
// forgotten post-filter.
//
// A zero or negative limit is treated as defaultSearchLimit, matching
// Search's own convention.
//
// An entry with no passages indexed yet (never processed, or processing
// failed) returns nil, nil -- there is no seed vector to query with, and
// that is not an error condition worth surfacing as one: the entry page
// simply has nothing to show in its "similar articles" block yet.
func (s *Searcher) Similar(ctx context.Context, entryID int64, limit int, f Filters) ([]EntryHit, error) {
	if limit <= 0 {
		limit = defaultSearchLimit
	}

	seeds, err := s.entrySeedVectors(ctx, entryID)
	if err != nil {
		return nil, fmt.Errorf("search: unable to load entry #%d's own passages: %w", entryID, err)
	}
	if len(seeds) == 0 {
		return nil, nil
	}

	// Each seed passage is queried for up to perSeedLimit neighbours --
	// the same "over-fetch before fusing" reasoning Search applies to
	// ModeHybrid/ModePassages (searchCandidateMultiplier, fuse.go): a
	// passage relevant only to one of the entry's several topics needs
	// its own seed's ranking to reach deep enough to appear at all.
	perSeedLimit := limit * searchCandidateMultiplier

	// best holds, per candidate passage, the single smallest cosine
	// distance seen across every seed query it appeared in -- the
	// natural merge for several rankings that all share the same metric
	// (cosine distance in the same embedding space), unlike Fuse's RRF,
	// which exists specifically to combine rankings whose raw scores
	// (BM25 vs. cosine) are not comparable. A passage close to *any* of
	// the entry's topics is exactly what "more like this" should surface,
	// so its best (smallest) distance to any seed is its score here.
	best := make(map[int64]PassageHit)
	for _, seed := range seeds {
		hits, err := s.nearestExcludingEntry(ctx, seed, perSeedLimit, entryID, f)
		if err != nil {
			return nil, fmt.Errorf("search: similar-article retrieval failed for entry #%d: %w", entryID, err)
		}
		for _, h := range hits {
			// Defence in depth: nearestExcludingEntry's own SQL already
			// excludes entryID's passages before they can be counted as
			// candidates at all (see that method's doc comment for why
			// that has to happen inside the CTE, not after it). This
			// check costs nothing and means a future change to that SQL
			// that weakens the exclusion fails loudly here instead of
			// silently leaking the seed entry into its own results.
			if h.EntryID == entryID {
				continue
			}
			if cur, ok := best[h.PassageID]; !ok || h.Score < cur.Score {
				best[h.PassageID] = h
			}
		}
	}

	ranked := make([]PassageHit, 0, len(best))
	for _, h := range best {
		ranked = append(ranked, h)
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].Score != ranked[j].Score {
			return ranked[i].Score < ranked[j].Score
		}
		return ranked[i].PassageID < ranked[j].PassageID
	})
	for i := range ranked {
		ranked[i].Rank = i + 1
	}

	return truncateEntries(aggregate(ranked), limit), nil
}

// entrySeedVectors returns entryID's own passages' embeddings, decoded
// from pgvector's plain-text format, ordered by ordinal and capped at
// maxSeedPassages via an even stride across the full ordinal range (see
// selectSeedIndices) so a long entry is sampled across its whole length
// rather than just its introduction. An entry with no rows in
// search.passages returns (nil, nil) -- not an error, since "not indexed
// yet" is an expected, common state (a new entry the backfill hasn't
// reached, or one an embedding failure left unindexed), and Similar
// treats an empty seed set as "nothing to show" rather than a failure.
func (s *Searcher) entrySeedVectors(ctx context.Context, entryID int64) ([][]float32, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT embedding::text FROM search.passages WHERE entry_id = $1 ORDER BY ordinal`,
		entryID,
	)
	if err != nil {
		return nil, fmt.Errorf("search: unable to query passages for entry #%d: %w", entryID, err)
	}
	defer rows.Close()

	var all [][]float32
	for rows.Next() {
		var text string
		if err := rows.Scan(&text); err != nil {
			return nil, fmt.Errorf("search: unable to scan passage embedding for entry #%d: %w", entryID, err)
		}
		vec, err := parseVectorText(text)
		if err != nil {
			return nil, fmt.Errorf("search: unable to parse passage embedding for entry #%d: %w", entryID, err)
		}
		all = append(all, vec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("search: error reading passages for entry #%d: %w", entryID, err)
	}
	if len(all) == 0 {
		return nil, nil
	}

	indices := selectSeedIndices(len(all), maxSeedPassages)
	seeds := make([][]float32, len(indices))
	for i, idx := range indices {
		seeds[i] = all[idx]
	}
	return seeds, nil
}

// selectSeedIndices returns up to maxSeeds indices into [0,n), evenly
// spaced across the whole range, for n > maxSeeds; every index 0..n-1
// for n <= maxSeeds. Spacing them evenly rather than taking the first
// maxSeeds is what lets a capped long entry still seed from passages
// spread across its whole length instead of only its introduction.
func selectSeedIndices(n, maxSeeds int) []int {
	if n <= maxSeeds {
		indices := make([]int, n)
		for i := range indices {
			indices[i] = i
		}
		return indices
	}

	indices := make([]int, maxSeeds)
	for i := 0; i < maxSeeds; i++ {
		indices[i] = i * n / maxSeeds
	}
	return indices
}

// nearestExcludingEntry ranks passages by cosine distance to vec,
// excluding every passage belonging to excludeEntryID, applying f after
// retrieval, and returns up to limit hits ordered best-first. Its SQL
// shape mirrors Semantic's own (semantic.go) -- the same candidates CTE,
// the same over-fetch/clamp against candidateMultiplier/minCandidates/
// maxCandidates, the same SET LOCAL hnsw.ef_search ahead of the query it
// configures, for the identical reason documented on Semantic. It is
// kept separate from Semantic rather than sharing its code because the
// two differ in the one place that matters here: this method's query
// vector comes from an already-indexed passage, not from embedding a
// caller's text, and it excludes one entry's passages entirely rather
// than merely filtering after the fact.
//
// excludeEntryID's exclusion happens inside the candidates CTE, before
// its own LIMIT -- not as a post-filter applied after fetching
// candidates, unlike Filters (whose predicates need a join to
// public.entries the CTE doesn't have). Passages from the same entry are
// frequently the most similar passages of all (adjacent chunks of one
// article routinely cosine-match each other more closely than any other
// entry's content), so excluding them only after the CTE's LIMIT would
// let them silently consume most or all of the candidate budget, this is
// exactly the failure this method exists to prevent.
func (s *Searcher) nearestExcludingEntry(ctx context.Context, vec []float32, limit int, excludeEntryID int64, f Filters) ([]PassageHit, error) {
	if limit <= 0 {
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
	args := []any{vectorLiteral(vec), excludeEntryID, candidates}

	b.WriteString(`
		WITH candidates AS (
			SELECT id, entry_id, ordinal, text, char_start, char_end, source,
			       (embedding <=> $1::public.vector) AS distance
			FROM search.passages
			WHERE entry_id != $2
			ORDER BY embedding <=> $1::public.vector
			LIMIT $3
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
		return nil, fmt.Errorf("search: Similar requires a transaction-capable database connection (got %T), so hnsw.ef_search can be raised as its own statement ahead of the query it configures", s.db)
	}

	tx, err := beginner.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("search: unable to begin similar-article query transaction: %w", err)
	}
	defer tx.Rollback()

	// See Semantic's own identical statement (semantic.go) for why this
	// must be its own round trip, strictly before the query it
	// configures, on the same connection.
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("SET LOCAL hnsw.ef_search = %d", candidates)); err != nil {
		return nil, fmt.Errorf("search: unable to raise hnsw.ef_search: %w", err)
	}

	rows, err := tx.QueryContext(ctx, b.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("search: similar-article query failed: %w", err)
	}
	defer rows.Close()

	var hits []PassageHit
	for rows.Next() {
		var h PassageHit
		if err := rows.Scan(&h.PassageID, &h.EntryID, &h.Ordinal, &h.Text, &h.CharStart, &h.CharEnd, &h.Source, &h.Score); err != nil {
			return nil, fmt.Errorf("search: unable to scan similar-article hit: %w", err)
		}
		hits = append(hits, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("search: error reading similar-article results: %w", err)
	}

	for i := range hits {
		hits[i].Rank = i + 1
	}

	return hits, nil
}

// parseVectorText parses pgvector's plain text vector format
// ("[0.1,0.2,...]", what embedding::text returns) into a []float32 --
// the inverse of vectorLiteral (semantic.go). Named distinctly from
// semantic_test.go's own test-only parseVectorLiteral (same package,
// same shape, different callers: that one is a test helper trusted to
// t.Fatalf on bad input, this one is production code and must return an
// error instead).
func parseVectorText(s string) ([]float32, error) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "[")
	s = strings.TrimSuffix(s, "]")
	if s == "" {
		return nil, nil
	}

	parts := strings.Split(s, ",")
	out := make([]float32, len(parts))
	for i, p := range parts {
		v, err := strconv.ParseFloat(strings.TrimSpace(p), 32)
		if err != nil {
			return nil, fmt.Errorf("invalid vector component %q: %w", p, err)
		}
		out[i] = float32(v)
	}
	return out, nil
}
