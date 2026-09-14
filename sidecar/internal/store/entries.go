// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package store // import "miniflux.app/v2/sidecar/internal/store"

import (
	"crypto/md5"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
)

// Entry is the subset of a Miniflux entry the indexer needs: its raw
// content and a hash of that content, used to detect edits that require a
// full re-index.
type Entry struct {
	ID          int64
	Content     string
	ContentHash string
}

// contentHash returns the hash recorded in
// search.entry_index_state.content_hash for a given piece of entry content.
// It must agree, byte for byte, with the hash PendingEntryIDs computes in
// SQL, md5(coalesce(entry content, empty string)) — both sides hash the
// empty string for a NULL/absent content column — or a changed entry could
// be silently missed, or an unchanged one endlessly re-queued.
func contentHash(content string) string {
	sum := md5.Sum([]byte(content))
	return hex.EncodeToString(sum[:])
}

// EntryForIndexing loads one entry's content from public.entries (read-only)
// and computes its content hash.
func (s *Store) EntryForIndexing(entryID int64) (*Entry, error) {
	var content sql.NullString
	err := s.db.QueryRow(`SELECT content FROM entries WHERE id=$1`, entryID).Scan(&content)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("store: entry #%d does not exist: %w", entryID, err)
	}
	if err != nil {
		return nil, fmt.Errorf("store: unable to fetch entry #%d: %w", entryID, err)
	}

	return &Entry{
		ID:          entryID,
		Content:     content.String,
		ContentHash: contentHash(content.String),
	}, nil
}

// PendingEntryIDs returns up to limit entry ids greater than afterID that
// need (re-)indexing: entries absent from entry_index_state, entries whose
// last attempt failed, or entries whose content has changed since it was
// last indexed. Ascending id order lets the caller checkpoint on the last id
// it processed.
func (s *Store) PendingEntryIDs(afterID int64, limit int) ([]int64, error) {
	query := `
		SELECT e.id
		FROM entries e
		LEFT JOIN search.entry_index_state s ON s.entry_id = e.id
		WHERE e.id > $1
		  AND (
		    s.entry_id IS NULL
		    OR s.status = 'failed'
		    OR s.content_hash <> md5(coalesce(e.content, ''))
		  )
		ORDER BY e.id ASC
		LIMIT $2
	`

	rows, err := s.db.Query(query, afterID, limit)
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

// PendingEntryCount returns how many entries currently need (re-)indexing,
// by the same criteria as PendingEntryIDs (absent from entry_index_state,
// last attempt failed, or content changed since it was last indexed) but
// without an afterID cursor or a limit — the total remaining backlog size,
// used to report progress and estimate an ETA (spec §9.4) rather than
// leaving every caller to build this query itself.
func (s *Store) PendingEntryCount() (int64, error) {
	var count int64
	err := s.db.QueryRow(`
		SELECT count(*)
		FROM entries e
		LEFT JOIN search.entry_index_state s ON s.entry_id = e.id
		WHERE (
		  s.entry_id IS NULL
		  OR s.status = 'failed'
		  OR s.content_hash <> md5(coalesce(e.content, ''))
		)
	`).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("store: unable to count pending entries: %w", err)
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
