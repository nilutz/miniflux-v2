// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package store // import "miniflux.app/v2/sidecar/internal/store"

import (
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// PassageRow is one passage ready to be written to search.passages: its
// text, its byte offsets into the entry's extracted plaintext, and its
// already-computed embedding. It is the store package's own row type so
// that package store never imports package passage — the indexer converts
// between passage.Passage and PassageRow.
type PassageRow struct {
	Ordinal   int
	Text      string
	CharStart int
	CharEnd   int
	Embedding []float32
}

// IndexState is the currently recorded indexing outcome for one entry.
type IndexState struct {
	ContentHash string
	Status      string
}

// EntryIndexState returns the currently recorded index state for an entry,
// or nil if the entry has never been indexed (no row in
// search.entry_index_state yet).
func (s *Store) EntryIndexState(entryID int64) (*IndexState, error) {
	var st IndexState
	err := s.db.QueryRow(
		`SELECT content_hash, status FROM search.entry_index_state WHERE entry_id=$1`,
		entryID,
	).Scan(&st.ContentHash, &st.Status)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: unable to fetch index state for entry #%d: %w", entryID, err)
	}

	return &st, nil
}

// ReplacePassages atomically replaces every passage of an entry and records
// it as successfully indexed at contentHash. The delete, the inserts, and
// the entry_index_state upsert all happen inside a single transaction: an
// error partway through rolls everything back, rather than leaving the
// entry half-indexed — old passages gone, new ones missing or incomplete —
// and silently under-searchable. Never call this with a partial passage
// set.
func (s *Store) ReplacePassages(entryID int64, contentHash string, passages []PassageRow) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: unable to begin transaction: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`DELETE FROM search.passages WHERE entry_id=$1`, entryID); err != nil {
		return fmt.Errorf("store: unable to delete existing passages for entry #%d: %w", entryID, err)
	}

	stmt, err := tx.Prepare(`
		INSERT INTO search.passages (entry_id, ordinal, text, char_start, char_end, embedding)
		VALUES ($1, $2, $3, $4, $5, $6::public.vector)
	`)
	if err != nil {
		return fmt.Errorf("store: unable to prepare passage insert: %w", err)
	}
	defer stmt.Close()

	for _, p := range passages {
		if _, err := stmt.Exec(entryID, p.Ordinal, p.Text, p.CharStart, p.CharEnd, vectorLiteral(p.Embedding)); err != nil {
			return fmt.Errorf("store: unable to insert passage %d for entry #%d: %w", p.Ordinal, entryID, err)
		}
	}

	if err := upsertIndexState(tx, entryID, contentHash, "ok", ""); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: unable to commit passage replacement for entry #%d: %w", entryID, err)
	}

	return nil
}

// MarkEntrySkipped records that an entry has no usable text to index, so it
// is not retried by PendingEntryIDs until its content changes. Any stale
// passages left over from a previous, since-edited version of the entry are
// removed in the same transaction as the state upsert, for the same
// atomicity reason as ReplacePassages.
func (s *Store) MarkEntrySkipped(entryID int64, contentHash, reason string) error {
	return s.markEntry(entryID, contentHash, "skipped", reason, true)
}

// MarkEntryFailed records that indexing an entry failed and should be
// retried later. Existing passages, if any, are left untouched: a
// transient embedding failure must not make a previously-indexed entry any
// less searchable than it already was.
func (s *Store) MarkEntryFailed(entryID int64, contentHash, reason string) error {
	return s.markEntry(entryID, contentHash, "failed", reason, false)
}

func (s *Store) markEntry(entryID int64, contentHash, status, reason string, deletePassages bool) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: unable to begin transaction: %w", err)
	}
	defer tx.Rollback()

	if deletePassages {
		if _, err := tx.Exec(`DELETE FROM search.passages WHERE entry_id=$1`, entryID); err != nil {
			return fmt.Errorf("store: unable to delete stale passages for entry #%d: %w", entryID, err)
		}
	}

	if err := upsertIndexState(tx, entryID, contentHash, status, reason); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: unable to commit status %q for entry #%d: %w", status, entryID, err)
	}

	return nil
}

func upsertIndexState(tx *sql.Tx, entryID int64, contentHash, status, reason string) error {
	var reasonArg any
	if reason != "" {
		reasonArg = reason
	}

	_, err := tx.Exec(`
		INSERT INTO search.entry_index_state (entry_id, content_hash, indexed_at, status, reason)
		VALUES ($1, $2, now(), $3, $4)
		ON CONFLICT (entry_id) DO UPDATE SET
			content_hash = excluded.content_hash,
			indexed_at   = excluded.indexed_at,
			status       = excluded.status,
			reason       = excluded.reason
	`, entryID, contentHash, status, reasonArg)
	if err != nil {
		return fmt.Errorf("store: unable to upsert index state for entry #%d: %w", entryID, err)
	}

	return nil
}

// vectorLiteral formats an embedding using pgvector's plain text input
// format ("[0.1,0.2,...]"), the format understood by an explicit ::vector
// cast. Preferred over adding github.com/pgvector/pgvector-go as a
// dependency for the sake of a single type used in one query.
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
