// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package store // import "miniflux.app/v2/sidecar/internal/store"

import (
	"testing"
)

func TestReplacePassagesWritesPassagesAndOKState(t *testing.T) {
	s := testStore(t)
	if err := s.Migrate(); err != nil {
		t.Fatalf("migrate failed: %v", err)
	}

	entryID := createTestEntry(t, s, "replace-basic", "<p>Some content.</p>")

	embedding := make([]float32, 384)
	embedding[0] = 0.5
	if err := s.ReplacePassages(entryID, "hash-1", []PassageRow{
		{Ordinal: 0, Text: "Some content.", CharStart: 0, CharEnd: 13, Embedding: embedding},
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var count int
	if err := s.db.QueryRow(`SELECT count(*) FROM search.passages WHERE entry_id=$1`, entryID).Scan(&count); err != nil {
		t.Fatalf("unable to count passages: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected 1 passage, got %d", count)
	}

	var dims int
	if err := s.db.QueryRow(
		`SELECT vector_dims(embedding) FROM search.passages WHERE entry_id=$1 AND ordinal=0`, entryID,
	).Scan(&dims); err != nil {
		t.Fatalf("unable to read embedding dims: %v", err)
	}
	if dims != 384 {
		t.Fatalf("expected a 384-dim embedding, got %d", dims)
	}

	var status, hash string
	if err := s.db.QueryRow(
		`SELECT status, content_hash FROM search.entry_index_state WHERE entry_id=$1`, entryID,
	).Scan(&status, &hash); err != nil {
		t.Fatalf("unable to read index state: %v", err)
	}
	if status != "ok" {
		t.Fatalf("expected status 'ok', got %q", status)
	}
	if hash != "hash-1" {
		t.Fatalf("expected content_hash 'hash-1', got %q", hash)
	}
}

func TestReplacePassagesReplacesOldPassages(t *testing.T) {
	s := testStore(t)
	if err := s.Migrate(); err != nil {
		t.Fatalf("migrate failed: %v", err)
	}

	entryID := createTestEntry(t, s, "replace-swap", "<p>First version.</p>")

	if err := s.ReplacePassages(entryID, "hash-old", []PassageRow{
		{Ordinal: 0, Text: "old passage one", CharStart: 0, CharEnd: 16, Embedding: make([]float32, 384)},
		{Ordinal: 1, Text: "old passage two", CharStart: 16, CharEnd: 32, Embedding: make([]float32, 384)},
	}); err != nil {
		t.Fatalf("unable to write first passage set: %v", err)
	}

	if err := s.ReplacePassages(entryID, "hash-new", []PassageRow{
		{Ordinal: 0, Text: "new passage", CharStart: 0, CharEnd: 11, Embedding: make([]float32, 384)},
	}); err != nil {
		t.Fatalf("unable to write second passage set: %v", err)
	}

	rows, err := s.db.Query(`SELECT ordinal, text FROM search.passages WHERE entry_id=$1 ORDER BY ordinal`, entryID)
	if err != nil {
		t.Fatalf("unable to query passages: %v", err)
	}
	defer rows.Close()

	var texts []string
	for rows.Next() {
		var ordinal int
		var text string
		if err := rows.Scan(&ordinal, &text); err != nil {
			t.Fatalf("unable to scan passage: %v", err)
		}
		texts = append(texts, text)
	}

	if len(texts) != 1 || texts[0] != "new passage" {
		t.Fatalf("expected exactly the new passage set, got %v", texts)
	}
}

// TestReplacePassagesRollsBackEntirelyOnFailure is the atomicity proof: it
// forces a failure partway through the write (a duplicate ordinal violates
// search.passages' UNIQUE(entry_id, ordinal) constraint on the second insert
// of the batch) and asserts that NOTHING from the failed attempt is visible
// afterward — neither the first, successfully-inserted row of the failed
// batch, nor a changed entry_index_state row. This actually exercises
// rollback rather than trusting that "one transaction" in the source was
// enough.
func TestReplacePassagesRollsBackEntirelyOnFailure(t *testing.T) {
	s := testStore(t)
	if err := s.Migrate(); err != nil {
		t.Fatalf("migrate failed: %v", err)
	}

	entryID := createTestEntry(t, s, "replace-atomic", "<p>Stable content.</p>")

	// Establish a known-good prior state so we can prove it survives the
	// failed replacement untouched.
	if err := s.ReplacePassages(entryID, "hash-good", []PassageRow{
		{Ordinal: 0, Text: "prior passage", CharStart: 0, CharEnd: 13, Embedding: make([]float32, 384)},
	}); err != nil {
		t.Fatalf("unable to write the prior good state: %v", err)
	}

	// Two rows sharing ordinal 0 trip the UNIQUE(entry_id, ordinal)
	// constraint on the second INSERT, after the DELETE and the first
	// INSERT of this call have already run on the same transaction.
	err := s.ReplacePassages(entryID, "hash-bad", []PassageRow{
		{Ordinal: 0, Text: "conflicting one", CharStart: 0, CharEnd: 15, Embedding: make([]float32, 384)},
		{Ordinal: 0, Text: "conflicting two", CharStart: 0, CharEnd: 15, Embedding: make([]float32, 384)},
	})
	if err == nil {
		t.Fatal("expected an error from a duplicate-ordinal batch")
	}

	// The DELETE that ran before the failing INSERT must not have stuck:
	// the prior good passage must still be exactly there.
	rows, qerr := s.db.Query(`SELECT ordinal, text FROM search.passages WHERE entry_id=$1 ORDER BY ordinal`, entryID)
	if qerr != nil {
		t.Fatalf("unable to query passages: %v", qerr)
	}
	defer rows.Close()

	var texts []string
	for rows.Next() {
		var ordinal int
		var text string
		if err := rows.Scan(&ordinal, &text); err != nil {
			t.Fatalf("unable to scan passage: %v", err)
		}
		texts = append(texts, text)
	}
	if len(texts) != 1 || texts[0] != "prior passage" {
		t.Fatalf("expected the prior passage set to survive the failed replacement untouched, got %v", texts)
	}

	// entry_index_state must still show the prior good hash — not
	// "hash-bad", and not some half-applied value.
	var status, hash string
	if err := s.db.QueryRow(
		`SELECT status, content_hash FROM search.entry_index_state WHERE entry_id=$1`, entryID,
	).Scan(&status, &hash); err != nil {
		t.Fatalf("unable to read index state: %v", err)
	}
	if status != "ok" || hash != "hash-good" {
		t.Fatalf("expected the prior state (ok, hash-good) to survive untouched, got (%s, %s)", status, hash)
	}
}

func TestMarkEntrySkippedRecordsReasonAndNoPassages(t *testing.T) {
	s := testStore(t)
	if err := s.Migrate(); err != nil {
		t.Fatalf("migrate failed: %v", err)
	}

	entryID := createTestEntry(t, s, "skip-basic", "")

	if err := s.MarkEntrySkipped(entryID, "hash-empty", "no usable text extracted"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var count int
	if err := s.db.QueryRow(`SELECT count(*) FROM search.passages WHERE entry_id=$1`, entryID).Scan(&count); err != nil {
		t.Fatalf("unable to count passages: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected no passages, got %d", count)
	}

	var status, reason string
	if err := s.db.QueryRow(
		`SELECT status, reason FROM search.entry_index_state WHERE entry_id=$1`, entryID,
	).Scan(&status, &reason); err != nil {
		t.Fatalf("unable to read index state: %v", err)
	}
	if status != "skipped" {
		t.Fatalf("expected status 'skipped', got %q", status)
	}
	if reason == "" {
		t.Fatal("expected a non-empty reason")
	}
}

func TestMarkEntryFailedLeavesExistingPassagesInPlace(t *testing.T) {
	s := testStore(t)
	if err := s.Migrate(); err != nil {
		t.Fatalf("migrate failed: %v", err)
	}

	entryID := createTestEntry(t, s, "fail-keeps-old", "<p>Still here.</p>")

	if err := s.ReplacePassages(entryID, "hash-good", []PassageRow{
		{Ordinal: 0, Text: "still here", CharStart: 0, CharEnd: 10, Embedding: make([]float32, 384)},
	}); err != nil {
		t.Fatalf("unable to write the prior good state: %v", err)
	}

	if err := s.MarkEntryFailed(entryID, "hash-good", "embedding backend unavailable"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var count int
	if err := s.db.QueryRow(`SELECT count(*) FROM search.passages WHERE entry_id=$1`, entryID).Scan(&count); err != nil {
		t.Fatalf("unable to count passages: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected the previously-indexed passage to survive a failed re-index, got %d", count)
	}

	var status, reason string
	if err := s.db.QueryRow(
		`SELECT status, reason FROM search.entry_index_state WHERE entry_id=$1`, entryID,
	).Scan(&status, &reason); err != nil {
		t.Fatalf("unable to read index state: %v", err)
	}
	if status != "failed" {
		t.Fatalf("expected status 'failed', got %q", status)
	}
	if reason == "" {
		t.Fatal("expected a non-empty reason")
	}
}
