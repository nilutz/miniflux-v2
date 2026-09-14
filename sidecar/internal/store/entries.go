// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package store // import "miniflux.app/v2/sidecar/internal/store"

import (
	"context"
	"crypto/md5"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
)

// Entry is the subset of a Miniflux entry the indexer needs: its title and
// raw content, and a hash covering both, used to detect edits that require
// a full re-index.
type Entry struct {
	ID          int64
	Title       string
	Content     string
	ContentHash string
}

// PipelineVersion identifies the derivation pipeline whose output is
// currently stored in search.passages: internal/passage's ExtractText (HTML
// -> plaintext) and Split (plaintext -> passages with char_start/char_end
// offsets into that plaintext).
//
// It exists because content_hash alone is not enough to decide whether a
// stored passage set is still valid. The offsets index into a plaintext
// that is never stored anywhere — P1b has to re-derive it by calling
// ExtractText again — so ANY change to ExtractText or Split silently
// invalidates every stored offset and passage boundary, with the entry's
// HTML, and therefore its content hash, completely unchanged. Nothing
// would detect that, and there would be no way to force a re-index short
// of TRUNCATE plus a 4-41 hour re-run (spec §9.3).
//
// Folding it into the hash rather than adding a pipeline_version column
// keeps exactly one value to compare, on both sides, with no migration and
// no second predicate to keep in step: a bump changes every entry's
// recorded hash, so PendingEntryIDs' existing content_hash comparison
// re-offers the whole corpus by itself.
//
// **Bump this whenever internal/passage's extraction or splitting output
// changes for any input.** internal/passage's TestPipelineOutputDigestIsStable
// exists to fail loudly and remind you.
//
//	1 — initial pipeline (task 3).
//	2 — ExtractText collapses Unicode whitespace (U+00A0, the rest of
//	    \p{Z}, and U+FEFF), not only ASCII \s.
//	3 — (task 1.5) the entry's title is now indexed as its own passage
//	    (source='title', ordinal 0) alongside its body passages
//	    (source='content'), and contentHash now covers the title as well
//	    as the content -- so a title-only edit re-offers the entry too,
//	    not just a body edit. Bumping this re-derives every already-
//	    indexed entry's hash from the new formula on its own, but the
//	    version is still bumped explicitly per this file's own rule: what
//	    search.passages stores changed, independently of whether
//	    ExtractText/Split's output for a given content string changed.
const PipelineVersion = "3"

// pipelineVersion is the value actually used by contentHash and by the
// pending-set predicate. It is a var, not the constant directly, purely so
// this package's own tests can bump it at runtime and observe that
// previously-indexed entries become pending again; production never
// changes it.
var pipelineVersion = PipelineVersion

// modelIdentity is the identity (embed.Embedder.Identity(), per spec
// §13.1) of the embedding model currently configured to produce vectors,
// folded into contentHash exactly like pipelineVersion. Before this
// existed, contentHash covered only the pipeline version, title and
// content — switching embedding models changed no hash, marked nothing
// pending, and left search.passages holding vectors from two different
// models in one HNSW graph, where cosine similarity between them is
// meaningless. This is the same mechanism PipelineVersion already uses,
// extended to cover the component that actually produces the vectors.
//
// A package-level var, not a parameter threaded through EntryForIndexing,
// PendingEntryIDs and PendingEntryCount, for the same reason
// pipelineVersion is one: exactly one value compared on both the Go side
// and the SQL side, set once via SetModelIdentity by whoever constructs
// the configured Embedder (indexer.New), and otherwise left alone.
// Production sets it once at startup and never mutates it afterwards;
// this package's own tests bump it directly, exactly like
// pipelineVersion, to exercise the "model changed" path.
//
// Deliberately not exported as a var itself (SetModelIdentity is the only
// way to change it from outside this package) so that nothing outside
// this file can set it to something that does not actually come from an
// Embedder's own Identity().
var modelIdentity string

// SetModelIdentity records the identity of the embedding model currently
// configured to produce vectors (embed.Embedder.Identity()), so that
// switching models — a different remote host, a different revision, a
// different width entirely — marks every entry pending on its own,
// exactly as a PipelineVersion bump already does (spec §13.1). Call it
// once at startup with the configured Embedder's Identity(); leaving it
// unset is indistinguishable from every configured model sharing the same
// (empty) identity, which reintroduces the exact gap this exists to close.
func SetModelIdentity(identity string) {
	modelIdentity = identity
}

// contentHash returns the hash recorded in
// search.entry_index_state.content_hash for a given entry title and
// content. It covers the pipeline version, the configured embedding
// model's identity, the title and the content, so a pipeline change, a
// model change, a title edit, or a content edit each invalidate stored
// passages exactly like one another (see PipelineVersion and
// modelIdentity) — a title-only edit must re-offer the entry too, since it
// changes what the (task 1.5) title passage should contain.
//
// It must agree, byte for byte, with the hash PendingEntryIDs and
// PendingEntryCount compute in SQL, md5(pipeline version || model identity
// || coalesce(entry title, ”) || coalesce(entry content, ”)) — both sides
// hash the empty string for a NULL/absent column — or a changed entry
// could be silently missed, or an unchanged one endlessly re-queued. Title
// is never NULL in practice (public.entries.title is NOT NULL), but
// coalescing it on the SQL side costs nothing and keeps the two sides
// symmetric with how content is already handled.
func contentHash(title, content string) string {
	sum := md5.Sum([]byte(pipelineVersion + modelIdentity + title + content))
	return hex.EncodeToString(sum[:])
}

// EntryForIndexing loads one entry's title and content from public.entries
// (read-only) and computes its content hash.
//
// It takes a context because its callers are not all background work: the
// search API builds a snippet per result through this, on the synchronous
// render path of a page someone is waiting for, and a reader who navigates
// away should not leave a detoasted full-content read running behind them.
func (s *Store) EntryForIndexing(ctx context.Context, entryID int64) (*Entry, error) {
	var title string
	var content sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT title, content FROM entries WHERE id=$1`, entryID).Scan(&title, &content)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("store: entry #%d does not exist: %w", entryID, err)
	}
	if err != nil {
		return nil, fmt.Errorf("store: unable to fetch entry #%d: %w", entryID, err)
	}

	return &Entry{
		ID:          entryID,
		Title:       title,
		Content:     content.String,
		ContentHash: contentHash(title, content.String),
	}, nil
}

// PendingEntryIDs returns up to limit entry ids greater than afterID that
// need (re-)indexing: entries absent from entry_index_state, entries whose
// last attempt failed, or entries whose title or content has changed since
// they were last indexed (which also covers a pipeline or model-identity
// change — see contentHash — since that changes every entry's expected
// hash without touching the entry itself). Ascending id order lets the
// caller checkpoint on the last id it processed.
//
// Deliberately has no "AND NOT e.hidden" or equivalent: spec §13.3 requires
// a hidden entry (one a reader marked "not interested") to remain
// searchable, so it must remain eligible for (re-)indexing here too. Do
// not add a hidden filter to this predicate or to PendingEntryCount's copy
// below.
func (s *Store) PendingEntryIDs(afterID int64, limit int) ([]int64, error) {
	query := `
		SELECT e.id
		FROM entries e
		LEFT JOIN search.entry_index_state s ON s.entry_id = e.id
		WHERE e.id > $1
		  AND (
		    s.entry_id IS NULL
		    OR s.status = 'failed'
		    OR s.content_hash <> md5($3 || $4 || coalesce(e.title, '') || coalesce(e.content, ''))
		  )
		ORDER BY e.id ASC
		LIMIT $2
	`

	rows, err := s.db.Query(query, afterID, limit, pipelineVersion, modelIdentity)
	if err != nil {
		return nil, fmt.Errorf("store: unable to fetch pending entry ids: %w", err)
	}
	defer rows.Close()

	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: unable to scan pending entry id: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: unable to read pending entry ids: %w", err)
	}

	return ids, nil
}

// PendingEntryCount returns how many entries with id > afterID currently
// need (re-)indexing, by the same criteria as PendingEntryIDs (absent from
// entry_index_state, last attempt failed, or content changed since it was
// last indexed) but without a limit — the total remaining backlog size,
// used to report progress and estimate an ETA (spec §9.4) rather than
// leaving every caller to build this query itself. Pass 0 to count the
// whole table.
//
// Unlike PendingEntryIDs, this has no LIMIT to stop early at, and its
// content_hash comparison must detoast and MD5 every candidate row's full
// body — there is no index that helps evaluate it. The afterID parameter
// is the only lever this query has to bound that cost: it lets a caller
// that already knows it is only responsible for ids above some point (a
// backfill lane scoped to its own starting cursor, say) skip
// content-hashing everything at or below it entirely, via the primary key
// index, rather than scanning and hashing the whole table on every call.
// Passing 0 still scans and hashes everything, same as before — this is a
// partial mitigation for callers that can bound themselves, not a fix for
// the worst case.
//
// Nothing on a hot path calls this. It is deliberately kept as the
// REFERENCE definition of "pending", sharing its predicate with
// PendingEntryIDs, and it is what PendingEntryCountApprox is pinned
// against in the tests — an approximation with no exact counterpart to
// check it is an approximation of nothing. Progress and ETA display go
// through PendingEntryCountApprox instead.
//
// Deliberately has no "AND NOT e.hidden" or equivalent — see
// PendingEntryIDs' doc comment; the same spec §13.3 requirement applies
// here too.
func (s *Store) PendingEntryCount(afterID int64) (int64, error) {
	var count int64
	err := s.db.QueryRow(`
		SELECT count(*)
		FROM entries e
		LEFT JOIN search.entry_index_state s ON s.entry_id = e.id
		WHERE e.id > $1
		  AND (
		    s.entry_id IS NULL
		    OR s.status = 'failed'
		    OR s.content_hash <> md5($2 || $3 || coalesce(e.title, '') || coalesce(e.content, ''))
		  )
	`, afterID, pipelineVersion, modelIdentity).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("store: unable to count pending entries: %w", err)
	}
	return count, nil
}

// PendingEntryCountApprox returns an approximation of PendingEntryCount
// that no Postgres plan has to detoast or MD5 anything to answer: the
// number of entries above afterID, minus the number of index-state rows
// above afterID that are not in the retryable 'failed' state.
//
// It exists because the exact count is genuinely expensive and is asked
// for on a timer. PendingEntryCount has no LIMIT to stop early at and must
// content-hash every candidate row's full body; on a large corpus that can
// run for minutes, and the admin page polls it (spec §9.4) against the
// same database Miniflux itself is serving from. An ETA does not need
// exactness, and the parts of the exact predicate that are expensive are
// exactly the parts that do not move the number much.
//
// How it differs from the exact count, in both directions:
//
//   - It UNDER-counts entries whose content changed since they were last
//     indexed: those have an 'ok' row and are subtracted here, but the
//     exact predicate's content_hash comparison still calls them pending.
//     At steady state that is a handful of re-scraped entries, not a
//     fraction of the corpus.
//   - It does NOT under-count for index-state rows whose entry no longer
//     exists (a deleted entry's orphan row): the EXISTS clause below
//     excludes those. That clause is the one join here, and it is a
//     primary-key probe per state row — still no detoast and no MD5. It
//     matters because nothing collects orphan rows today, so on a
//     long-lived instance they accumulate, and subtracting them blindly
//     would drag Remaining toward zero and the ETA with it. The result is
//     clamped at zero regardless.
//
// Use PendingEntryCount where the number has to be right; use this one for
// progress and ETA display.
func (s *Store) PendingEntryCountApprox(afterID int64) (int64, error) {
	var count int64
	err := s.db.QueryRow(`
		SELECT
			(SELECT count(*) FROM entries WHERE id > $1)
			- (SELECT count(*) FROM search.entry_index_state s
			   WHERE s.entry_id > $1 AND s.status <> 'failed'
			     AND EXISTS (SELECT 1 FROM entries e WHERE e.id = s.entry_id))
	`, afterID).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("store: unable to approximate the pending entry count: %w", err)
	}
	if count < 0 {
		count = 0
	}
	return count, nil
}

// MaxEntryID returns the highest entry id currently in public.entries, or 0
// if the table is empty. It exists so the live lane can snapshot "the
// newest entry that already existed" at startup and start its cursor
// there, rather than at 0 — see RunLive's doc comment for why: without it,
// the live lane and the backfill lane both begin at the very start of the
// entire historical backlog and race each other over it.
func (s *Store) MaxEntryID() (int64, error) {
	var id int64
	if err := s.db.QueryRow(`SELECT coalesce(max(id), 0) FROM entries`).Scan(&id); err != nil {
		return 0, fmt.Errorf("store: unable to determine the maximum entry id: %w", err)
	}
	return id, nil
}
