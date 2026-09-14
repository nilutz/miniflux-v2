// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package indexer // import "miniflux.app/v2/sidecar/internal/indexer"

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	_ "github.com/lib/pq"

	"miniflux.app/v2/sidecar/internal/store"
)

// fakeEmbedder is a deterministic, call-counting stand-in for the real ONNX
// Embedder (Task 2). This task tests pipeline wiring, not inference: the
// call count is what makes "an unchanged/skipped entry is not re-embedded"
// assertions possible at all. calls is an atomic.Int64, not a plain int:
// Backfill's worker pool can call Embed from multiple goroutines
// concurrently, and an unsynchronised counter there is only safe by
// accident of whatever MaxWorkers a given test happens to pin (fix round
// 1, finding 12).
type fakeEmbedder struct {
	calls atomic.Int64
	err   error
}

func (f *fakeEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	f.calls.Add(1)
	if f.err != nil {
		return nil, f.err
	}
	out := make([][]float32, len(texts))
	for i := range texts {
		v := make([]float32, 384)
		v[0] = float32(len(texts[i])) // deterministic, and distinguishes passages
		out[i] = v
	}
	return out, nil
}

func (f *fakeEmbedder) Dimensions() int { return 384 }
func (f *fakeEmbedder) Close() error    { return nil }

// batchRecordingEmbedder records how many texts each Embed call received,
// so a test can observe the actual runtime batch size IndexEntry used
// (fix round 2, finding 2).
type batchRecordingEmbedder struct {
	mu         sync.Mutex
	batchSizes []int
}

func (b *batchRecordingEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	b.mu.Lock()
	b.batchSizes = append(b.batchSizes, len(texts))
	b.mu.Unlock()

	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = make([]float32, 384)
	}
	return out, nil
}

func (b *batchRecordingEmbedder) Dimensions() int { return 384 }
func (b *batchRecordingEmbedder) Close() error    { return nil }

func (b *batchRecordingEmbedder) sizes() []int {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]int, len(b.batchSizes))
	copy(out, b.batchSizes)
	return out
}

// testEnv opens both a *store.Store (the code under test) and a raw *sql.DB
// (for fixture setup — package store's own db field is unexported and this
// is a different package) against the same real dev database, and migrates
// the search schema. Skips when SIDECAR_DATABASE_URL is unset, keeping the
// default suite hermetic; this must never require SIDECAR_MODEL_PATH, since
// no real ONNX model is loaded here.
func testEnv(t *testing.T) (*store.Store, *sql.DB) {
	t.Helper()

	dsn := os.Getenv("SIDECAR_DATABASE_URL")
	if dsn == "" {
		t.Skip("SIDECAR_DATABASE_URL is not set, skipping database test")
	}

	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("unable to open database: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	s, err := store.New(dsn)
	if err != nil {
		t.Fatalf("unable to open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	if err := s.Migrate(); err != nil {
		t.Fatalf("migrate failed: %v", err)
	}

	return s, db
}

// createTestEntry inserts a user, category, feed and entry with the given
// content into the real Miniflux tables, mirroring the P0 fork's own test
// fixtures (see internal/storage/fork_full_text_test.go at the repo root).
// Everything, including any search schema rows a test leaves behind, is
// removed when the test finishes.
func createTestEntry(t *testing.T, db *sql.DB, username, content string) int64 {
	t.Helper()

	var userID int64
	if err := db.QueryRow(
		`INSERT INTO users (username, password) VALUES ($1, 'x') RETURNING id`,
		username,
	).Scan(&userID); err != nil {
		t.Fatalf("unable to create user: %v", err)
	}
	t.Cleanup(func() {
		db.Exec(`DELETE FROM users WHERE id=$1`, userID)
	})

	var categoryID int64
	if err := db.QueryRow(
		`INSERT INTO categories (user_id, title) VALUES ($1, 'Test') RETURNING id`,
		userID,
	).Scan(&categoryID); err != nil {
		t.Fatalf("unable to create category: %v", err)
	}

	var feedID int64
	if err := db.QueryRow(
		`INSERT INTO feeds (feed_url, site_url, title, category_id, user_id)
		 VALUES ($1, $1, 'Test feed', $2, $3) RETURNING id`,
		"https://example.org/"+username+".xml", categoryID, userID,
	).Scan(&feedID); err != nil {
		t.Fatalf("unable to create feed: %v", err)
	}

	var entryID int64
	if err := db.QueryRow(
		`INSERT INTO entries (title, hash, url, published_at, changed_at, user_id, feed_id, content)
		 VALUES ('Test entry', $1, 'https://example.org/'||$2, now(), now(), $3, $4, $5)
		 RETURNING id`,
		"hash-"+username, username, userID, feedID, content,
	).Scan(&entryID); err != nil {
		t.Fatalf("unable to create entry: %v", err)
	}
	t.Cleanup(func() {
		db.Exec(`DELETE FROM search.passages WHERE entry_id=$1`, entryID)
		db.Exec(`DELETE FROM search.entry_index_state WHERE entry_id=$1`, entryID)
	})

	return entryID
}

func updateEntryContent(t *testing.T, db *sql.DB, entryID int64, content string) {
	t.Helper()

	if _, err := db.Exec(`UPDATE entries SET content=$1, changed_at=now() WHERE id=$2`, content, entryID); err != nil {
		t.Fatalf("unable to update entry content: %v", err)
	}
}

// 1. A plain entry indexes: passages are written, each with a non-null
// 384-dim embedding, and entry_index_state.status = 'ok'.
func TestIndexEntryIndexesAPlainEntry(t *testing.T) {
	s, db := testEnv(t)
	entryID := createTestEntry(t, db, "index-plain",
		"<p>The quick brown fox jumps over the lazy dog. It was a fine day for testing indexing.</p>")

	idx := New(s, &fakeEmbedder{})
	if err := idx.IndexEntry(context.Background(), entryID); err != nil {
		t.Fatalf("IndexEntry failed: %v", err)
	}

	rows, err := db.Query(
		`SELECT ordinal, vector_dims(embedding) FROM search.passages WHERE entry_id=$1 ORDER BY ordinal`,
		entryID,
	)
	if err != nil {
		t.Fatalf("unable to query passages: %v", err)
	}
	defer rows.Close()

	count := 0
	for rows.Next() {
		var ordinal, dims int
		if err := rows.Scan(&ordinal, &dims); err != nil {
			t.Fatalf("unable to scan passage: %v", err)
		}
		if dims != 384 {
			t.Fatalf("passage %d: expected a 384-dim embedding, got %d", ordinal, dims)
		}
		count++
	}
	if count == 0 {
		t.Fatal("expected at least one passage to be written")
	}

	var status string
	if err := db.QueryRow(
		`SELECT status FROM search.entry_index_state WHERE entry_id=$1`, entryID,
	).Scan(&status); err != nil {
		t.Fatalf("unable to read index state: %v", err)
	}
	if status != "ok" {
		t.Fatalf("expected status 'ok', got %q", status)
	}
}

// 2. An entry with no usable text is skipped: no passages written,
// status='skipped', reason non-empty, and a second IndexEntry call does not
// re-embed it; the entry also stays out of the pending set (not retried).
func TestIndexEntryWithNoUsableTextIsSkipped(t *testing.T) {
	s, db := testEnv(t)
	entryID := createTestEntry(t, db, "index-empty",
		"<script>var x = 1;</script><style>p { color: red; }</style>")

	fe := &fakeEmbedder{}
	idx := New(s, fe)

	if err := idx.IndexEntry(context.Background(), entryID); err != nil {
		t.Fatalf("IndexEntry failed: %v", err)
	}

	var count int
	if err := db.QueryRow(`SELECT count(*) FROM search.passages WHERE entry_id=$1`, entryID).Scan(&count); err != nil {
		t.Fatalf("unable to count passages: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected no passages, got %d", count)
	}

	var status, reason string
	if err := db.QueryRow(
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
	if fe.calls.Load() != 0 {
		t.Fatalf("expected the embedder not to be called, got %d calls", fe.calls.Load())
	}

	if err := idx.IndexEntry(context.Background(), entryID); err != nil {
		t.Fatalf("second IndexEntry failed: %v", err)
	}
	if fe.calls.Load() != 0 {
		t.Fatalf("expected the embedder still not to be called after a second run, got %d calls", fe.calls.Load())
	}

	ids, err := s.PendingEntryIDs(entryID-1, 100)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, id := range ids {
		if id == entryID {
			t.Fatalf("expected skipped entry #%d to not be retried (excluded from the pending set)", entryID)
		}
	}
}

// 3. Re-indexing is atomic: index an entry, change its content, re-index,
// and the passage set matches the new content exactly — no orphans from the
// first pass.
func TestIndexEntryReplacesPassagesOnContentChange(t *testing.T) {
	s, db := testEnv(t)
	entryID := createTestEntry(t, db, "index-reindex",
		"<p>Original content sentence number one. Original content sentence number two.</p>")

	fe := &fakeEmbedder{}
	idx := New(s, fe)

	if err := idx.IndexEntry(context.Background(), entryID); err != nil {
		t.Fatalf("first IndexEntry failed: %v", err)
	}

	updateEntryContent(t, db, entryID,
		"<p>Completely different text after the edit, an unrelated sentence for the second version.</p>")

	if err := idx.IndexEntry(context.Background(), entryID); err != nil {
		t.Fatalf("second IndexEntry failed: %v", err)
	}

	rows, err := db.Query(`SELECT text FROM search.passages WHERE entry_id=$1 ORDER BY ordinal`, entryID)
	if err != nil {
		t.Fatalf("unable to query passages: %v", err)
	}
	defer rows.Close()

	var texts []string
	for rows.Next() {
		var text string
		if err := rows.Scan(&text); err != nil {
			t.Fatalf("unable to scan passage: %v", err)
		}
		texts = append(texts, text)
	}
	if len(texts) == 0 {
		t.Fatal("expected passages after re-indexing")
	}
	for _, text := range texts {
		if strings.Contains(text, "Original content") {
			t.Fatalf("found an orphaned passage from the first pass: %q", text)
		}
		if !strings.Contains(text, "Completely different") && !strings.Contains(text, "second version") {
			t.Fatalf("passage does not look like it came from the new content: %q", text)
		}
	}

	var status, hash string
	if err := db.QueryRow(
		`SELECT status, content_hash FROM search.entry_index_state WHERE entry_id=$1`, entryID,
	).Scan(&status, &hash); err != nil {
		t.Fatalf("unable to read index state: %v", err)
	}
	if status != "ok" {
		t.Fatalf("expected status 'ok' after re-index, got %q", status)
	}

	entry, err := s.EntryForIndexing(entryID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if hash != entry.ContentHash {
		t.Fatalf("expected stored content_hash to match the new content's hash, got %q want %q", hash, entry.ContentHash)
	}
}

// 4. An unchanged entry is not re-embedded: same content_hash means the
// fake Embedder sees zero additional calls on the second run.
func TestIndexEntryUnchangedIsNotReembedded(t *testing.T) {
	s, db := testEnv(t)
	entryID := createTestEntry(t, db, "index-unchanged",
		"<p>Stable content that never changes between the two runs at all.</p>")

	fe := &fakeEmbedder{}
	idx := New(s, fe)

	if err := idx.IndexEntry(context.Background(), entryID); err != nil {
		t.Fatalf("first IndexEntry failed: %v", err)
	}
	callsAfterFirst := fe.calls.Load()
	if callsAfterFirst == 0 {
		t.Fatal("expected the embedder to be called on the first run")
	}

	if err := idx.IndexEntry(context.Background(), entryID); err != nil {
		t.Fatalf("second IndexEntry failed: %v", err)
	}
	if fe.calls.Load() != callsAfterFirst {
		t.Fatalf("expected no additional embed calls for an unchanged entry, got %d -> %d", callsAfterFirst, fe.calls.Load())
	}
}

// 5. Embedding failure marks the entry failed and retryable: status='failed',
// reason non-empty, no passages written, and the entry still appears in the
// pending set.
func TestIndexEntryEmbeddingFailureMarksFailedAndRetryable(t *testing.T) {
	s, db := testEnv(t)
	entryID := createTestEntry(t, db, "index-failed",
		"<p>Some content that will fail to embed because of a fake backend error.</p>")

	fe := &fakeEmbedder{err: errors.New("embedding backend unavailable")}
	idx := New(s, fe)

	err := idx.IndexEntry(context.Background(), entryID)
	if err == nil {
		t.Fatal("expected IndexEntry to return an error")
	}

	var status, reason string
	if err := db.QueryRow(
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

	var count int
	if err := db.QueryRow(`SELECT count(*) FROM search.passages WHERE entry_id=$1`, entryID).Scan(&count); err != nil {
		t.Fatalf("unable to count passages: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected no passages written on a failed embed, got %d", count)
	}

	ids, err := s.PendingEntryIDs(entryID-1, 100)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	found := false
	for _, id := range ids {
		if id == entryID {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected failed entry #%d to still be in the pending set (retryable)", entryID)
	}
}

// 6. (Fix round 2, finding 2.) SetBatchSize is the runtime-editable knob
// spec §9.2 actually names ("batch size — passages per forward pass; the
// main lever on CPU efficiency"), distinct from Backfill.SetPageSize
// (entry ids fetched per pagination page). It clamps to
// [MinBatchSize, MaxBatchSize] — spec §6.7's measured sweet spot, where
// batch 64 was slower than batch 8 — and takes effect on the next
// IndexEntry call.
func TestIndexerSetBatchSizeClampsAndTakesEffect(t *testing.T) {
	s, db := testEnv(t)

	// Enough short sentences that Split produces well over MaxBatchSize
	// passages, so the runtime batch size is directly observable via how
	// many texts each Embed call receives.
	var sb strings.Builder
	sb.WriteString("<p>")
	for i := 0; i < 1000; i++ {
		sb.WriteString(fmt.Sprintf("Sentence number %d for the batch size test. ", i))
	}
	sb.WriteString("</p>")
	entryID := createTestEntry(t, db, "batchsize-test", sb.String())

	be := &batchRecordingEmbedder{}
	idx := New(s, be)

	if got := idx.BatchSize(); got != DefaultBatchSize {
		t.Fatalf("expected a fresh Indexer to start at DefaultBatchSize=%d, got %d", DefaultBatchSize, got)
	}

	idx.SetBatchSize(1_000_000) // absurdly high; must clamp down
	if got := idx.BatchSize(); got != MaxBatchSize {
		t.Fatalf("expected SetBatchSize to clamp down to MaxBatchSize=%d, got %d", MaxBatchSize, got)
	}

	idx.SetBatchSize(0) // absurdly low (and the zero value); must clamp up
	if got := idx.BatchSize(); got != MinBatchSize {
		t.Fatalf("expected SetBatchSize to clamp up to MinBatchSize=%d, got %d", MinBatchSize, got)
	}

	idx.SetBatchSize(MinBatchSize) // a valid in-range value takes effect exactly
	if got := idx.BatchSize(); got != MinBatchSize {
		t.Fatalf("expected SetBatchSize(%d) to take effect exactly, got %d", MinBatchSize, got)
	}

	if err := idx.IndexEntry(context.Background(), entryID); err != nil {
		t.Fatalf("IndexEntry failed: %v", err)
	}

	sizes := be.sizes()
	if len(sizes) < 2 {
		t.Fatalf("test setup invalid: expected multiple embed batches with this much content, got %d", len(sizes))
	}
	for _, n := range sizes {
		if n > MinBatchSize {
			t.Fatalf("expected every Embed call to receive at most %d texts (the batch size in effect), got %d in %v",
				MinBatchSize, n, sizes)
		}
	}
}
