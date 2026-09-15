// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package indexer // import "miniflux.app/v2/sidecar/internal/indexer"

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	_ "github.com/lib/pq"

	"miniflux.app/v2/sidecar/internal/embed"
	"miniflux.app/v2/sidecar/internal/passage"
	"miniflux.app/v2/sidecar/internal/store"
	"miniflux.app/v2/sidecar/internal/testdb"
)

// testModelIdentity is the Identity() every fake Embedder in this package's
// tests returns. These fakes stand in for one configured model across a
// test — including across a simulated process restart, where a test
// deliberately swaps one fake Go type for another (e.g.
// TestBackfillResumesFromCheckpointAfterInterruption's blockOnMarkerEmbedder
// then countingEmbedder) purely for orchestration, not to represent an
// actual model change. Giving every fake a distinct identity would make
// New's store.SetModelIdentity call (spec §13.1) see that swap as a real
// model change and mark already-indexed entries pending again — exactly
// the mechanism these tests are not exercising. Model-change behavior
// itself is covered at the store level, where the test controls modelIdentity
// directly (see internal/store's TestModelIdentityChangeMakesIndexedEntriesPendingAgain).
const testModelIdentity = "fake-embedder@test#768"

// fakeEmbedder is a deterministic, call-counting stand-in for the real ONNX
// Embedder, testing pipeline wiring, not inference: the
// call count is what makes "an unchanged/skipped entry is not re-embedded"
// assertions possible at all. calls is an atomic.Int64, not a plain int:
// Backfill's worker pool can call Embed from multiple goroutines
// concurrently, and an unsynchronised counter there is only safe by
// accident of whatever MaxWorkers a given test happens to pin.
type fakeEmbedder struct {
	calls atomic.Int64
	err   error
}

func (f *fakeEmbedder) EmbedDocuments(_ context.Context, texts []string) ([][]float32, error) {
	f.calls.Add(1)
	if f.err != nil {
		return nil, f.err
	}
	out := make([][]float32, len(texts))
	for i := range texts {
		v := make([]float32, 768)
		v[0] = float32(len(texts[i])) // deterministic, and distinguishes passages
		out[i] = v
	}
	return out, nil
}

// EmbedQuery is never called on this path: IndexEntry only ever embeds
// documents (see indexer.go). Panicking rather than returning a fake
// vector makes the two call paths' separation a property this test
// suite proves, not merely assumes -- if IndexEntry were ever changed to
// call EmbedQuery by mistake, this fake would fail loudly instead of
// silently returning a plausible-looking vector under the wrong task.
func (f *fakeEmbedder) EmbedQuery(context.Context, string) ([]float32, error) {
	panic("fakeEmbedder: EmbedQuery should never be called by internal/indexer")
}

func (f *fakeEmbedder) Dimensions() int  { return 768 }
func (f *fakeEmbedder) Identity() string { return testModelIdentity }
func (f *fakeEmbedder) Close() error     { return nil }

// unavailableEmbedder is a fake standing in for a networked embedder
// (internal/embed/remote) that is currently unreachable: every
// Embed call fails with an error wrapping embed.ErrUnavailable, exactly
// how remote.go's own error sites are wrapped. It exists to prove
// IndexEntry classifies THIS kind of error as lane-level (spec §13.1) --
// distinct from fakeEmbedder's plain, unwrapped err, which must keep
// classifying as an ordinary per-entry failure.
type unavailableEmbedder struct {
	calls atomic.Int64
}

func (u *unavailableEmbedder) EmbedDocuments(_ context.Context, texts []string) ([][]float32, error) {
	u.calls.Add(1)
	return nil, fmt.Errorf("%w: simulated: remote unreachable", embed.ErrUnavailable)
}

// EmbedQuery is never called by internal/indexer; see fakeEmbedder's
// EmbedQuery for why this panics rather than faking a result.
func (u *unavailableEmbedder) EmbedQuery(context.Context, string) ([]float32, error) {
	panic("unavailableEmbedder: EmbedQuery should never be called by internal/indexer")
}

func (u *unavailableEmbedder) Dimensions() int  { return 768 }
func (u *unavailableEmbedder) Identity() string { return testModelIdentity }
func (u *unavailableEmbedder) Close() error     { return nil }

// batchRecordingEmbedder records how many texts each Embed call received,
// so a test can observe the actual runtime batch size IndexEntry used.
type batchRecordingEmbedder struct {
	mu         sync.Mutex
	batchSizes []int
}

func (b *batchRecordingEmbedder) EmbedDocuments(_ context.Context, texts []string) ([][]float32, error) {
	b.mu.Lock()
	b.batchSizes = append(b.batchSizes, len(texts))
	b.mu.Unlock()

	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = make([]float32, 768)
	}
	return out, nil
}

// EmbedQuery is never called by internal/indexer; see fakeEmbedder's
// EmbedQuery for why this panics rather than faking a result.
func (b *batchRecordingEmbedder) EmbedQuery(context.Context, string) ([]float32, error) {
	panic("batchRecordingEmbedder: EmbedQuery should never be called by internal/indexer")
}

func (b *batchRecordingEmbedder) Dimensions() int  { return 768 }
func (b *batchRecordingEmbedder) Identity() string { return testModelIdentity }
func (b *batchRecordingEmbedder) Close() error     { return nil }

// distinctIdentityEmbedder is a minimal Embedder whose Identity() is
// whatever the test sets, unlike every other fake in this package (which
// deliberately share testModelIdentity so a simulated restart doesn't look
// like a model change). It exists solely for
// TestNewWiresEmbedderIdentityIntoContentHash, which needs two fakes with
// genuinely different identities to prove that New's
// store.SetModelIdentity(e.Identity()) call -- the only production path
// connecting a configured embedder to contentHash (spec §13.1) -- actually
// runs, rather than assuming it does because the store-level tests already
// exercise the hash math by setting the unexported var directly.
type distinctIdentityEmbedder struct {
	identity string
}

func (d *distinctIdentityEmbedder) EmbedDocuments(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = make([]float32, 768)
	}
	return out, nil
}

// EmbedQuery is never called by internal/indexer; see fakeEmbedder's
// EmbedQuery for why this panics rather than faking a result.
func (d *distinctIdentityEmbedder) EmbedQuery(context.Context, string) ([]float32, error) {
	panic("distinctIdentityEmbedder: EmbedQuery should never be called by internal/indexer")
}

func (d *distinctIdentityEmbedder) Dimensions() int  { return 768 }
func (d *distinctIdentityEmbedder) Identity() string { return d.identity }
func (d *distinctIdentityEmbedder) Close() error     { return nil }

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
// the search schema. Skips when SIDECAR_TEST_DATABASE_URL is unset (and
// refuses outright if it names the same database as SIDECAR_DATABASE_URL —
// see package testdb; this suite sweeps the whole entries table), keeping the
// default suite hermetic; this must never require SIDECAR_MODEL_PATH, since
// no real ONNX model is loaded here.
func testEnv(t *testing.T) (*store.Store, *sql.DB) {
	t.Helper()

	dsn := testdb.DSN(t)

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
	return createTestEntryWithTitle(t, db, username, "Test entry", content)
}

// createTestEntryWithTitle is createTestEntry with a caller-chosen title,
// for tests exercising title indexing itself: an empty title,
// a title distinct from the body, a title edited independently of content.
func createTestEntryWithTitle(t *testing.T, db *sql.DB, username, title, content string) int64 {
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
		 VALUES ($1, $2, 'https://example.org/'||$3, now(), now(), $4, $5, $6)
		 RETURNING id`,
		title, "hash-"+username, username, userID, feedID, content,
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

func updateEntryTitleAndContent(t *testing.T, db *sql.DB, entryID int64, title, content string) {
	t.Helper()

	if _, err := db.Exec(`UPDATE entries SET title=$1, content=$2, changed_at=now() WHERE id=$3`, title, content, entryID); err != nil {
		t.Fatalf("unable to update entry title/content: %v", err)
	}
}

// passageRow is what these tests read back from search.passages to check
// the indexer's output, source and offsets included.
type passageRow struct {
	Ordinal   int
	Text      string
	CharStart int
	CharEnd   int
	Source    string
	Dims      int
}

func passagesFor(t *testing.T, db *sql.DB, entryID int64) []passageRow {
	t.Helper()

	rows, err := db.Query(
		`SELECT ordinal, text, char_start, char_end, source, vector_dims(embedding)
		 FROM search.passages WHERE entry_id=$1 ORDER BY ordinal`,
		entryID,
	)
	if err != nil {
		t.Fatalf("unable to query passages: %v", err)
	}
	defer rows.Close()

	var out []passageRow
	for rows.Next() {
		var p passageRow
		if err := rows.Scan(&p.Ordinal, &p.Text, &p.CharStart, &p.CharEnd, &p.Source, &p.Dims); err != nil {
			t.Fatalf("unable to scan passage: %v", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("unable to read passages: %v", err)
	}
	return out
}

// 1. A plain entry indexes: passages are written, each with a non-null
// 768-dim embedding, and entry_index_state.status = 'ok'.
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
		if dims != 768 {
			t.Fatalf("passage %d: expected a 768-dim embedding, got %d", ordinal, dims)
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

// TestIndexEntryOnlyEverCallsEmbedDocuments is the nomic migration plan's
// call-path proof for internal/indexer: IndexEntry indexes
// passages, so it must call embed.Embedder.EmbedDocuments and must never
// call EmbedQuery — an asymmetric model (nomic-embed-text-v1.5) applies a
// different, incompatible prefix to each, and passages embedded under the
// query prefix would be silently wrong, with no error anywhere (see
// embed.Embedder's doc comment).
//
// fakeEmbedder's EmbedQuery panics rather than returning a fake vector
// (see its own doc comment), so this is a genuine discrimination test,
// not an assertion against what a permissive fake merely recorded:
// change indexer.go's IndexEntry to call EmbedQuery instead of
// EmbedDocuments and this test panics.
func TestIndexEntryOnlyEverCallsEmbedDocuments(t *testing.T) {
	s, db := testEnv(t)
	entryID := createTestEntry(t, db, "index-only-embeds-documents",
		"<p>The quick brown fox jumps over the lazy dog. It was a fine day for testing indexing.</p>")

	fe := &fakeEmbedder{}
	idx := New(s, fe)
	if err := idx.IndexEntry(context.Background(), entryID); err != nil {
		t.Fatalf("IndexEntry failed: %v", err)
	}

	if calls := fe.calls.Load(); calls == 0 {
		t.Fatal("expected IndexEntry to have called EmbedDocuments at least once")
	}
}

// 1a. Indexing an entry produces a title passage: ordinal 0,
// source='title', text exactly the entry's title, offsets spanning the
// whole title -- and the body's passages follow it as source='content'
// with ordinals from 1. This is the regression this guards against:
// before it, a query matching only the title (e.g. "Why Raft is hard" for
// a post whose body never repeats "Raft") had nothing to find.
func TestIndexEntryCreatesATitlePassageAndContentPassages(t *testing.T) {
	s, db := testEnv(t)
	const title = "Why Raft is hard"
	entryID := createTestEntryWithTitle(t, db, "index-title-basic", title,
		"<p>Distributed consensus protocols are notoriously difficult to implement correctly in practice.</p>")

	idx := New(s, &fakeEmbedder{})
	if err := idx.IndexEntry(context.Background(), entryID); err != nil {
		t.Fatalf("IndexEntry failed: %v", err)
	}

	passages := passagesFor(t, db, entryID)
	if len(passages) < 2 {
		t.Fatalf("expected at least a title passage and a content passage, got %d: %+v", len(passages), passages)
	}

	titlePassage := passages[0]
	if titlePassage.Ordinal != 0 {
		t.Fatalf("expected the title passage at ordinal 0, got %d", titlePassage.Ordinal)
	}
	if titlePassage.Source != "title" {
		t.Fatalf("expected source 'title', got %q", titlePassage.Source)
	}
	if titlePassage.Text != title {
		t.Fatalf("expected the title passage's text to be exactly the title %q, got %q", title, titlePassage.Text)
	}
	if titlePassage.CharStart != 0 || titlePassage.CharEnd != len(title) {
		t.Fatalf("expected title offsets [0,%d), got [%d,%d)", len(title), titlePassage.CharStart, titlePassage.CharEnd)
	}
	if titlePassage.Dims != 768 {
		t.Fatalf("expected the title passage to be embedded (768 dims), got %d", titlePassage.Dims)
	}

	for _, p := range passages[1:] {
		if p.Source != "content" {
			t.Fatalf("expected every passage after the title to have source 'content', got %q at ordinal %d", p.Source, p.Ordinal)
		}
		if p.Ordinal < 1 {
			t.Fatalf("expected content ordinals to start from 1 when a title passage exists, got %d", p.Ordinal)
		}
		if p.Text == title {
			t.Fatalf("a content passage must not duplicate the title's text")
		}
	}
}

// 1b. An entry with an empty (or whitespace-only) title produces
// no title passage at all -- not an empty one that would waste an ordinal
// and an embedding call on nothing.
func TestIndexEntryEmptyTitleProducesNoTitlePassage(t *testing.T) {
	s, db := testEnv(t)
	entryID := createTestEntryWithTitle(t, db, "index-title-empty", "",
		"<p>Body text for an entry whose title is empty.</p>")

	idx := New(s, &fakeEmbedder{})
	if err := idx.IndexEntry(context.Background(), entryID); err != nil {
		t.Fatalf("IndexEntry failed: %v", err)
	}

	passages := passagesFor(t, db, entryID)
	if len(passages) == 0 {
		t.Fatal("expected content passages to still be written")
	}
	for _, p := range passages {
		if p.Source == "title" {
			t.Fatalf("expected no title passage for an entry with an empty title, got one: %+v", p)
		}
	}
}

func TestIndexEntryWhitespaceOnlyTitleProducesNoTitlePassage(t *testing.T) {
	s, db := testEnv(t)
	entryID := createTestEntryWithTitle(t, db, "index-title-whitespace", "   \t  ",
		"<p>Body text for an entry whose title is only whitespace.</p>")

	idx := New(s, &fakeEmbedder{})
	if err := idx.IndexEntry(context.Background(), entryID); err != nil {
		t.Fatalf("IndexEntry failed: %v", err)
	}

	passages := passagesFor(t, db, entryID)
	for _, p := range passages {
		if p.Source == "title" {
			t.Fatalf("expected no title passage for a whitespace-only title, got one: %+v", p)
		}
	}
}

// 1c. THE TRAP: a title passage's char_start/char_end must index
// into the title itself, never into the body's extracted plaintext. The
// title here is deliberately multi-byte (accented characters), and the
// content is deliberately a very different length, so an implementation
// that accidentally reused the content's plaintext or its length for the
// title's offsets is caught rather than coincidentally passing.
func TestIndexEntryTitleOffsetsIndexIntoTitleNotContentPlaintext(t *testing.T) {
	s, db := testEnv(t)
	const title = "Café Étude: naïve façade déjà vu" // multi-byte UTF-8, byte len != rune len
	const contentHTML = "<p>This paragraph is unrelated in both length and vocabulary to the headline above, deliberately so that any offset computed against it instead of the title would be immediately, visibly wrong rather than passing by coincidence.</p>"
	entryID := createTestEntryWithTitle(t, db, "index-title-trap", title, contentHTML)

	idx := New(s, &fakeEmbedder{})
	if err := idx.IndexEntry(context.Background(), entryID); err != nil {
		t.Fatalf("IndexEntry failed: %v", err)
	}

	passages := passagesFor(t, db, entryID)
	if len(passages) < 2 {
		t.Fatalf("expected a title passage and at least one content passage, got %d", len(passages))
	}

	titlePassage := passages[0]
	if titlePassage.Source != "title" {
		t.Fatalf("expected passages[0] to be the title passage, got source %q", titlePassage.Source)
	}
	if titlePassage.CharStart != 0 || titlePassage.CharEnd != len(title) {
		t.Fatalf("expected title offsets [0,%d) (byte length of the title), got [%d,%d) -- "+
			"if this equals a content-derived length instead, offsets were computed against the wrong source string",
			len(title), titlePassage.CharStart, titlePassage.CharEnd)
	}
	// The offsets must be a valid, exact slice of the title itself.
	if title[titlePassage.CharStart:titlePassage.CharEnd] != titlePassage.Text {
		t.Fatalf("title[%d:%d] = %q, want the stored text %q",
			titlePassage.CharStart, titlePassage.CharEnd, title[titlePassage.CharStart:titlePassage.CharEnd], titlePassage.Text)
	}

	// And content passages must be valid, exact slices of the content's
	// OWN extracted plaintext -- not the title.
	contentText := passage.ExtractText(contentHTML)
	for _, p := range passages[1:] {
		if p.CharEnd > len(contentText) {
			t.Fatalf("content passage offsets [%d,%d) exceed the extracted plaintext's length %d", p.CharStart, p.CharEnd, len(contentText))
		}
		if contentText[p.CharStart:p.CharEnd] != p.Text {
			t.Fatalf("content plaintext[%d:%d] = %q, want the stored text %q",
				p.CharStart, p.CharEnd, contentText[p.CharStart:p.CharEnd], p.Text)
		}
	}
}

// 1d. An entry whose body has no usable text at all (e.g. a
// link post with only a script tag) still indexes -- by its title alone --
// rather than being skipped outright, now that the title carries its own
// searchable passage.
func TestIndexEntryTitleOnlyEntryWithNoUsableBodyIndexesJustTheTitle(t *testing.T) {
	s, db := testEnv(t)
	const title = "Headline Only Post"
	entryID := createTestEntryWithTitle(t, db, "index-title-only", title,
		"<script>var x = 1;</script><style>p { color: red; }</style>")

	fe := &fakeEmbedder{}
	idx := New(s, fe)
	if err := idx.IndexEntry(context.Background(), entryID); err != nil {
		t.Fatalf("IndexEntry failed: %v", err)
	}

	passages := passagesFor(t, db, entryID)
	if len(passages) != 1 {
		t.Fatalf("expected exactly 1 passage (the title), got %d: %+v", len(passages), passages)
	}
	if passages[0].Source != "title" || passages[0].Text != title {
		t.Fatalf("expected the sole passage to be the title, got %+v", passages[0])
	}
	if fe.calls.Load() == 0 {
		t.Fatal("expected the embedder to be called for the title passage")
	}

	var status string
	if err := db.QueryRow(`SELECT status FROM search.entry_index_state WHERE entry_id=$1`, entryID).Scan(&status); err != nil {
		t.Fatalf("unable to read index state: %v", err)
	}
	if status != "ok" {
		t.Fatalf("expected status 'ok' for a title-only entry, got %q", status)
	}
}

// 1e. Re-indexing after both the title and the content change
// replaces both kinds of passage atomically -- no orphaned title or body
// passage from the previous version survives.
func TestIndexEntryReindexReplacesTitleAndContentPassagesAtomically(t *testing.T) {
	s, db := testEnv(t)
	entryID := createTestEntryWithTitle(t, db, "index-title-reindex", "Original Title",
		"<p>Original body text.</p>")

	fe := &fakeEmbedder{}
	idx := New(s, fe)
	if err := idx.IndexEntry(context.Background(), entryID); err != nil {
		t.Fatalf("first IndexEntry failed: %v", err)
	}

	updateEntryTitleAndContent(t, db, entryID, "New Title", "<p>New body text entirely.</p>")

	if err := idx.IndexEntry(context.Background(), entryID); err != nil {
		t.Fatalf("second IndexEntry failed: %v", err)
	}

	passages := passagesFor(t, db, entryID)
	if len(passages) < 2 {
		t.Fatalf("expected a title passage and at least one content passage after re-indexing, got %d", len(passages))
	}
	for _, p := range passages {
		if strings.Contains(p.Text, "Original") {
			t.Fatalf("found an orphaned passage from before the edit: %+v", p)
		}
	}
	if passages[0].Source != "title" || passages[0].Text != "New Title" {
		t.Fatalf("expected the title passage to be updated to 'New Title', got %+v", passages[0])
	}
}

// 2. An entry with no usable text ANYWHERE -- no title and no usable body
// -- is skipped: no passages written, status='skipped', reason non-empty,
// and a second IndexEntry call does not re-embed it; the entry also stays
// out of the pending set (not retried). (An entry with a usable title but
// no usable body no longer takes this path -- see
// TestIndexEntryTitleOnlyEntryWithNoUsableBody... below -- so this
// fixture must have an empty title too, to still genuinely exercise
// "nothing to index at all".)
func TestIndexEntryWithNoUsableTextIsSkipped(t *testing.T) {
	s, db := testEnv(t)
	entryID := createTestEntryWithTitle(t, db, "index-empty", "",
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

// 3. Re-indexing is atomic: index an entry, change its content (title held
// fixed), re-index, and the passage set matches the new content exactly —
// no orphans from the first pass. The fixture's title ("Test entry", from
// createTestEntry) is unchanged across the edit, so it is expected to
// still appear as the title passage; only the source='content' passages
// are checked against the old/new body text.
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

	passages := passagesFor(t, db, entryID)
	if len(passages) == 0 {
		t.Fatal("expected passages after re-indexing")
	}

	var bodyTexts []string
	for _, p := range passages {
		if p.Source == "title" {
			continue
		}
		bodyTexts = append(bodyTexts, p.Text)
	}
	if len(bodyTexts) == 0 {
		t.Fatal("expected body passages after re-indexing")
	}
	for _, text := range bodyTexts {
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

	entry, err := s.EntryForIndexing(context.Background(), entryID)
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

// TestNewWiresEmbedderIdentityIntoContentHash proves the production wiring
// that connects a configured Embedder to contentHash (spec §13.1), rather
// than assuming it: New's call to store.SetModelIdentity(e.Identity()) is
// the ONLY place in production code that ever does this, and every other
// test in this package deliberately shares one testModelIdentity constant
// across its fakes (see distinctIdentityEmbedder's doc comment) — so
// nothing else here would notice if that call were deleted, reordered to
// run too late, or a second Indexer were built without updating the
// identity. This test builds two Indexers, via New, over two embedders
// with genuinely different identities, and checks the result through the
// exported store.PendingEntryIDs/PendingEntryCount — never by poking the
// unexported modelIdentity var directly, which is exactly what would keep
// passing if the wiring were broken.
func TestNewWiresEmbedderIdentityIntoContentHash(t *testing.T) {
	s, db := testEnv(t)

	entryID := createTestEntry(t, db, "identity-wiring",
		"<p>Content for the New-wires-identity test, held fixed throughout.</p>")
	afterID := entryID - 1

	idxA := New(s, &distinctIdentityEmbedder{identity: "model-a@rev1#768"})
	if err := idxA.IndexEntry(context.Background(), entryID); err != nil {
		t.Fatalf("IndexEntry under model A failed: %v", err)
	}
	if entryStatus(t, db, entryID) != "ok" {
		t.Fatalf("expected entry #%d to be 'ok' after indexing under model A, got %q", entryID, entryStatus(t, db, entryID))
	}

	idsBefore, err := s.PendingEntryIDs(afterID, 100)
	if err != nil {
		t.Fatalf("PendingEntryIDs failed: %v", err)
	}
	for _, id := range idsBefore {
		if id == entryID {
			t.Fatalf("entry #%d should not be pending right after being indexed under model A", entryID)
		}
	}

	// Constructing a second Indexer, via New, with a genuinely different
	// embedder identity -- and doing nothing else -- must be what marks
	// the entry pending again. Nothing here calls IndexEntry a second
	// time, and nothing here touches store.modelIdentity directly: if
	// this passes, New's wiring did the work.
	New(s, &distinctIdentityEmbedder{identity: "model-b@rev1#768"})

	idsAfter, err := s.PendingEntryIDs(afterID, 100)
	if err != nil {
		t.Fatalf("PendingEntryIDs failed: %v", err)
	}
	found := false
	for _, id := range idsAfter {
		if id == entryID {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected entry #%d to be pending after constructing a new Indexer with a different embedder identity via New, got %v", entryID, idsAfter)
	}

	count, err := s.PendingEntryCount(afterID)
	if err != nil {
		t.Fatalf("PendingEntryCount failed: %v", err)
	}
	if count < 1 {
		t.Fatalf("expected PendingEntryCount to count the entry after the embedder identity changed via New, got %d", count)
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

// 5a. (spec §13.1.) An embedder reporting itself UNAVAILABLE —
// wrapping embed.ErrUnavailable, exactly as internal/embed/remote's own
// error sites do — must NOT be treated like the ordinary embedding
// failure above: IndexEntry must classify it (isEmbedderUnavailable) and
// must leave entry_index_state completely untouched, not call
// MarkEntryFailed. This is the load-bearing distinction this design
// draws: a network outage must never mark the entry it happened
// to be embedding when it hit "failed", because that is what forces
// thousands of entries into the retry-backoff machinery during a
// five-minute blip.
func TestIndexEntryEmbedderUnavailableLeavesIndexStateUntouched(t *testing.T) {
	s, db := testEnv(t)
	entryID := createTestEntry(t, db, "index-embedder-unavailable",
		"<p>Some content that cannot be embedded because the remote embedder is down.</p>")

	ue := &unavailableEmbedder{}
	idx := New(s, ue)

	err := idx.IndexEntry(context.Background(), entryID)
	if err == nil {
		t.Fatal("expected IndexEntry to return an error")
	}
	if !isEmbedderUnavailable(err) {
		t.Fatalf("expected the error to classify as embedder-unavailable, got %v", err)
	}
	if !errors.Is(err, embed.ErrUnavailable) {
		t.Fatalf("expected the error to still wrap embed.ErrUnavailable through IndexEntry's own wrapping, got %v", err)
	}

	var count int
	if err := db.QueryRow(
		`SELECT count(*) FROM search.entry_index_state WHERE entry_id=$1`, entryID,
	).Scan(&count); err != nil {
		t.Fatalf("unable to count index state rows: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected NO entry_index_state row at all (untouched, still genuinely pending) after an "+
			"embedder-unavailable error, got %d row(s) -- this must never be marked 'failed'", count)
	}

	var passageCount int
	if err := db.QueryRow(`SELECT count(*) FROM search.passages WHERE entry_id=$1`, entryID).Scan(&passageCount); err != nil {
		t.Fatalf("unable to count passages: %v", err)
	}
	if passageCount != 0 {
		t.Fatalf("expected no passages written, got %d", passageCount)
	}

	// Untouched means still pending, exactly like an entry that was never
	// attempted at all -- not merely "not marked ok".
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
		t.Fatalf("expected entry #%d to remain in the pending set", entryID)
	}
}

// 5b. failureCause must never be asked to classify an embedder-unavailable
// error via the normal per-entry path in production (both lanes intercept
// it first with isEmbedderUnavailable), but it must still return a bounded
// label if it ever is, rather than falling through to genericCause's
// per-error text -- see failureCause's own defensive case.
func TestFailureCauseClassifiesEmbedderUnavailableDefensively(t *testing.T) {
	err := fmt.Errorf("%w #7: %w", errEmbedderUnavailable, errors.New("simulated: remote unreachable"))
	if got, want := failureCause(err), "embedder unavailable"; got != want {
		t.Fatalf("failureCause = %q, want %q", got, want)
	}
}

// (spec §13.1.) requiresEmbedderRestart must
// distinguish a plain outage (self-heals) from a mid-run identity change
// (never self-heals) purely via errors.Is against embed.ErrRequiresRestart
// -- both wrap embed.ErrUnavailable, so isEmbedderUnavailable alone cannot
// tell them apart; this is the second, independent classification the
// admin page's requires-restart signal depends on.
func TestRequiresEmbedderRestartDistinguishesIdentityChangeFromPlainOutage(t *testing.T) {
	plainOutage := fmt.Errorf("%w: simulated: connection refused", embed.ErrUnavailable)
	if requiresEmbedderRestart(plainOutage) {
		t.Fatalf("expected requiresEmbedderRestart=false for a plain outage, got true (err=%v)", plainOutage)
	}
	if !isEmbedderUnavailable(fmt.Errorf("%w #1: %w", errEmbedderUnavailable, plainOutage)) {
		t.Fatal("test setup invalid: plainOutage must still classify as embedder-unavailable")
	}

	identityChanged := fmt.Errorf("%w: %w: simulated: remote identity changed mid-run", embed.ErrUnavailable, embed.ErrRequiresRestart)
	if !requiresEmbedderRestart(identityChanged) {
		t.Fatalf("expected requiresEmbedderRestart=true for a mid-run identity change, got false (err=%v)", identityChanged)
	}
	if !isEmbedderUnavailable(fmt.Errorf("%w #1: %w", errEmbedderUnavailable, identityChanged)) {
		t.Fatal("test setup invalid: identityChanged must still classify as embedder-unavailable too")
	}
}

// 6. SetBatchSize is the runtime-editable knob
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

	// A distinct in-range value, not MinBatchSize (which the preceding
	// clamp assertion already left it at) -- setting it to the SAME
	// value it already holds would prove nothing about SetBatchSize
	// actually taking effect versus just staying where it was.
	const chosenBatchSize = 20
	if chosenBatchSize == MinBatchSize || chosenBatchSize == MaxBatchSize {
		t.Fatal("test setup invalid: chosenBatchSize must differ from both clamp bounds to prove SetBatchSize takes a real value, not just a bound")
	}
	idx.SetBatchSize(chosenBatchSize)
	if got := idx.BatchSize(); got != chosenBatchSize {
		t.Fatalf("expected SetBatchSize(%d) to take effect exactly, got %d", chosenBatchSize, got)
	}

	if err := idx.IndexEntry(context.Background(), entryID); err != nil {
		t.Fatalf("IndexEntry failed: %v", err)
	}

	sizes := be.sizes()
	if len(sizes) < 2 {
		t.Fatalf("test setup invalid: expected multiple embed batches with this much content, got %d", len(sizes))
	}
	sawChosenSize := false
	for _, n := range sizes {
		if n == chosenBatchSize {
			sawChosenSize = true
		}
		if n > chosenBatchSize {
			t.Fatalf("expected every Embed call to receive at most %d texts (the batch size in effect), got %d in %v",
				chosenBatchSize, n, sizes)
		}
	}
	if !sawChosenSize {
		t.Fatalf("expected at least one full batch of exactly %d texts (the chosen batch size), got sizes %v", chosenBatchSize, sizes)
	}
}

// Every error IndexEntry can return must classify to a short, stable
// label, so that the backfill lane's by-cause aggregation is a breakdown
// by cause and not one row per entry.
func TestFailureCauseIsStableAcrossEntries(t *testing.T) {
	embedErr := errors.New("onnxruntime: input tensor shape mismatch at offset 4096")

	byEntry := map[string]bool{}
	for _, entryID := range []int64{1, 12345, 999999} {
		err := fmt.Errorf("%w #%d: %w", errEmbed, entryID, embedErr)
		byEntry[failureCause(err)] = true
	}
	if len(byEntry) != 1 {
		t.Fatalf("expected the same embedding failure on three entries to classify to ONE cause, got %d: %v", len(byEntry), byEntry)
	}
	for cause := range byEntry {
		if strings.ContainsAny(cause, "0123456789#") {
			t.Fatalf("a cause used as an aggregation key must not carry an entry id or any other number, got %q", cause)
		}
		if cause != "embedding failed" {
			t.Fatalf("unexpected cause label %q", cause)
		}
	}

	cases := []struct {
		name string
		err  error
		want string
	}{
		{"load entry", fmt.Errorf("%w #7: %w", errLoadEntry, errors.New("boom")), "could not read the entry"},
		{"load state", fmt.Errorf("%w #7: %w", errLoadIndexState, errors.New("boom")), "could not read the entry's index state"},
		{"mark skipped", fmt.Errorf("%w #7: %w", errMarkSkipped, errors.New("boom")), "could not record the entry's index state"},
		{"mark failed", fmt.Errorf("%w #7: %w", errMarkFailed, errors.New("boom")), "could not record the entry's index state"},
		{"write passages", fmt.Errorf("%w #7: %w", errWritePassages, errors.New("boom")), "could not write the entry's passages"},
		{"interrupted", fmt.Errorf("%w #7: %w", errInterrupted, context.Canceled), "interrupted"},
	}
	for _, tc := range cases {
		if got := failureCause(tc.err); got != tc.want {
			t.Fatalf("%s: failureCause = %q, want %q", tc.name, got, tc.want)
		}
	}

	// An error wrapping no known kind still has to yield a bounded key.
	stray := failureCause(errors.New("pq: deadlock detected on relation 41234 for pid 9981"))
	if strings.ContainsAny(stray, "0123456789") {
		t.Fatalf("expected the generic fallback to strip digit runs, got %q", stray)
	}
	long := failureCause(errors.New(strings.Repeat("x", 4096)))
	if len(long) > maxCauseLength+3 {
		t.Fatalf("expected the generic fallback to truncate to at most %d characters, got %d", maxCauseLength+3, len(long))
	}
}

// An IndexEntry error produced by the real code path, not a
// hand-assembled one, must classify the same way — the wrapping has to
// actually be in place.
func TestIndexEntryEmbeddingFailureClassifiesByCause(t *testing.T) {
	s, db := testEnv(t)

	idA := createTestEntry(t, db, "cause-classify-a", "<p>First entry with plenty of usable text in it.</p>")
	idB := createTestEntry(t, db, "cause-classify-b", "<p>Second entry with plenty of usable text in it.</p>")

	idx := New(s, &fakeEmbedder{err: errors.New("model session closed")})

	errA := idx.IndexEntry(context.Background(), idA)
	errB := idx.IndexEntry(context.Background(), idB)
	if errA == nil || errB == nil {
		t.Fatalf("expected both entries to fail to index, got %v and %v", errA, errB)
	}
	if !errors.Is(errA, errEmbed) {
		t.Fatalf("expected the error to wrap errEmbed, got %v", errA)
	}
	if failureCause(errA) != failureCause(errB) {
		t.Fatalf("two entries failing the same way must share one cause, got %q and %q", failureCause(errA), failureCause(errB))
	}
	// The full detail still has to reach the log line's error value.
	if !strings.Contains(errA.Error(), "model session closed") {
		t.Fatalf("expected the full error to keep the embedder's own message, got %q", errA.Error())
	}
	if !strings.Contains(errA.Error(), fmt.Sprintf("#%d", idA)) {
		t.Fatalf("expected the full error to keep the entry id, got %q", errA.Error())
	}
}

// causeCounts must stay bounded no matter how many distinct causes it is
// handed: the lane runs unattended for up to 41 hours.
func TestCauseCountsAreBounded(t *testing.T) {
	c := newCauseCounts()
	for i := 0; i < maxCauseKeys*10; i++ {
		c.add(fmt.Sprintf("cause-%d", i))
	}

	snap := c.snapshot()
	if len(snap) > maxCauseKeys+1 {
		t.Fatalf("expected at most %d keys (maxCauseKeys plus the overflow bucket), got %d", maxCauseKeys+1, len(snap))
	}
	if snap[overflowCause] == 0 {
		t.Fatalf("expected causes beyond the cap to be counted under %q, got %v", overflowCause, snap)
	}

	var total int64
	for _, v := range snap {
		total += v
	}
	if total != int64(maxCauseKeys*10) {
		t.Fatalf("expected every add to be counted somewhere (%d), got %d", maxCauseKeys*10, total)
	}
}
