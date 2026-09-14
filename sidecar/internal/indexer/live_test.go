// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package indexer // import "miniflux.app/v2/sidecar/internal/indexer"

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// pollInterval is short enough that the live-lane tests complete quickly
// without sleeping a fixed duration and hoping — every timing assertion
// below polls for the condition it wants (see waitFor), so the tests are
// fast when things work and still fail honestly, rather than flakily, when
// they don't.
const pollInterval = 50 * time.Millisecond

// waitFor polls cond every 5ms until it returns true or timeout elapses,
// failing the test if the condition is never met.
func waitFor(t *testing.T, timeout time.Duration, msg string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !cond() {
		t.Fatalf("timed out waiting for: %s", msg)
	}
}

// failMarkerEmbedder fails Embed for any batch containing a text with the
// configured marker substring, and otherwise succeeds. Used to prove that
// one entry's embedding failure does not stop the lane from indexing a
// different entry in the same pass.
type failMarkerEmbedder struct {
	failMarker string
	failCalls  atomic.Int64
	okCalls    atomic.Int64
}

func (f *failMarkerEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	for _, text := range texts {
		if strings.Contains(text, f.failMarker) {
			f.failCalls.Add(1)
			return nil, errors.New("simulated embedding failure")
		}
	}
	f.okCalls.Add(1)
	out := make([][]float32, len(texts))
	for i := range texts {
		v := make([]float32, 384)
		v[0] = float32(len(texts[i]))
		out[i] = v
	}
	return out, nil
}

func (f *failMarkerEmbedder) Dimensions() int { return 384 }
func (f *failMarkerEmbedder) Close() error    { return nil }

// slowEmbedder takes delay to embed each batch, and counts how many
// batches it started. Used to prove RunLive checks ctx.Done() between
// entries within a single pass, not merely between polls.
type slowEmbedder struct {
	delay      time.Duration
	callsStart atomic.Int64
	callsDone  atomic.Int64
}

func (s *slowEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	s.callsStart.Add(1)
	select {
	case <-time.After(s.delay):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	s.callsDone.Add(1)
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = make([]float32, 384)
	}
	return out, nil
}

func (s *slowEmbedder) Dimensions() int { return 384 }
func (s *slowEmbedder) Close() error    { return nil }

// entryStatus reads back search.entry_index_state.status for an entry, or
// "" if no row exists yet.
func entryStatus(t *testing.T, db *sql.DB, entryID int64) string {
	t.Helper()
	var status string
	err := db.QueryRow(`SELECT status FROM search.entry_index_state WHERE entry_id=$1`, entryID).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return ""
	}
	if err != nil {
		t.Fatalf("unable to read index state: %v", err)
	}
	return status
}

// 1. A newly inserted entry becomes indexed within a couple of poll
// intervals.
func TestRunLiveIndexesNewEntry(t *testing.T) {
	s, db := testEnv(t)
	entryID := createTestEntry(t, db, "live-new",
		"<p>The quick brown fox jumps over the lazy dog for the live lane test.</p>")

	idx := New(s, &fakeEmbedder{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- RunLive(ctx, idx, pollInterval) }()

	waitFor(t, 2*time.Second, "entry indexed", func() bool {
		return entryStatus(t, db, entryID) == "ok"
	})

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunLive returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RunLive did not return after cancellation")
	}
}

// 2. The lane survives a single entry failing: it logs, marks the entry
// failed, and carries on to index the next entry rather than aborting the
// loop.
func TestRunLiveSurvivesOneEntryFailing(t *testing.T) {
	s, db := testEnv(t)

	failMarker := "FAIL-MARKER-live-survives"
	failingID := createTestEntry(t, db, "live-fail",
		"<p>This entry contains "+failMarker+" and will fail to embed.</p>")
	okID := createTestEntry(t, db, "live-ok",
		"<p>This entry is perfectly fine and should still get indexed.</p>")

	fe := &failMarkerEmbedder{failMarker: failMarker}
	idx := New(s, fe)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- RunLive(ctx, idx, pollInterval) }()

	waitFor(t, 2*time.Second, "ok entry indexed", func() bool {
		return entryStatus(t, db, okID) == "ok"
	})
	waitFor(t, 2*time.Second, "failing entry marked failed", func() bool {
		return entryStatus(t, db, failingID) == "failed"
	})

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunLive returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RunLive did not return after cancellation")
	}

	if fe.failCalls.Load() == 0 {
		t.Fatal("expected at least one failing embed call")
	}
	if fe.okCalls.Load() == 0 {
		t.Fatal("expected at least one successful embed call")
	}
}

// 3. RunLive exits cleanly on context cancellation, and promptly — it must
// not wait out a full poll interval, let alone longer.
func TestRunLiveExitsOnContextCancellation(t *testing.T) {
	s, _ := testEnv(t)
	idx := New(s, &fakeEmbedder{})

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- RunLive(ctx, idx, pollInterval) }()

	// Let it run at least one tick before cancelling.
	time.Sleep(pollInterval * 2)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunLive returned error: %v", err)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("RunLive did not return promptly after cancellation")
	}
}

// 4. Cancellation is respected BETWEEN entries within a single pass, not
// merely between polls: with several entries pending in one page and a
// slow embedder, cancelling shortly after the pass starts must return long
// before every entry in that pass would have finished.
func TestRunLiveCancelsMidPassBetweenEntries(t *testing.T) {
	s, db := testEnv(t)

	const entryCount = 6
	const perEntryDelay = 150 * time.Millisecond

	for i := 0; i < entryCount; i++ {
		createTestEntry(t, db, "live-midpass-"+string(rune('a'+i)),
			"<p>Entry content for the mid-pass cancellation test, number "+string(rune('a'+i))+".</p>")
	}

	se := &slowEmbedder{delay: perEntryDelay}
	idx := New(s, se)

	// A long poll interval means everything above is fetched into a single
	// pass, which is the scenario this test needs: several entries fetched
	// together, so a mid-pass cancellation genuinely has entries left to
	// skip past.
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- RunLive(ctx, idx, 10*time.Millisecond) }()

	// Wait until at least one entry has started embedding, proving the
	// pass is under way, then cancel while several entries remain.
	waitFor(t, 2*time.Second, "first embed call started", func() bool {
		return se.callsStart.Load() >= 1
	})
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunLive returned error: %v", err)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("RunLive did not return promptly after mid-pass cancellation")
	}
	elapsed := time.Since(start)

	// If cancellation were only checked between polls (or not at all
	// between entries), all entryCount entries would each take
	// perEntryDelay, totalling entryCount*perEntryDelay. Checking it
	// between every entry must return well before that.
	maxAcceptable := time.Duration(entryCount) * perEntryDelay / 2
	if elapsed > maxAcceptable {
		t.Fatalf("RunLive took %v to return after mid-pass cancellation; expected well under %v "+
			"(entryCount*perEntryDelay=%v), suggesting ctx.Done() is not checked between entries",
			elapsed, maxAcceptable, time.Duration(entryCount)*perEntryDelay)
	}

	// It must not have run every entry to completion.
	if se.callsDone.Load() >= entryCount {
		t.Fatalf("expected fewer than %d completed embed calls after mid-pass cancellation, got %d",
			entryCount, se.callsDone.Load())
	}
}
