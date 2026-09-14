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
// different entry in the same pass, and (with the marker never cleared)
// that a persistently failing entry does not block progress on newer
// entries either.
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

// flippableEmbedder fails Embed for any batch containing the marker text
// until healthy is set true, and succeeds otherwise (marked or not). Used
// to prove a failed entry is genuinely retried later — not merely that it
// eventually reports "ok" for some unrelated reason.
type flippableEmbedder struct {
	marker         string
	healthy        atomic.Bool
	callsForMarker atomic.Int64
}

func (f *flippableEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	marked := false
	for _, text := range texts {
		if strings.Contains(text, f.marker) {
			marked = true
		}
	}
	if marked {
		f.callsForMarker.Add(1)
		if !f.healthy.Load() {
			return nil, errors.New("simulated: not yet healthy")
		}
	}
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = make([]float32, 384)
	}
	return out, nil
}

func (f *flippableEmbedder) Dimensions() int { return 384 }
func (f *flippableEmbedder) Close() error    { return nil }

// blockingEmbedder deliberately ignores ctx — it just sleeps for delay,
// unconditionally, before returning success. This is what a slow but
// cooperative-cancellation-unaware embedder would do. It is deliberately
// NOT select{}-ing on ctx.Done(): if it were, TestRunLiveCancelsMidPass-
// BetweenEntries could pass purely because of that internal race rather
// than because of RunLive's own per-entry ctx check — the exact false
// confidence a fix-round review caught. With this embedder, the ONLY thing
// that can produce an early return from a multi-entry pass once ctx is
// cancelled is RunLive checking ctx.Done() before starting the next entry.
type blockingEmbedder struct {
	delay      time.Duration
	callsStart atomic.Int64
	callsDone  atomic.Int64
}

func (b *blockingEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	b.callsStart.Add(1)
	time.Sleep(b.delay)
	b.callsDone.Add(1)
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = make([]float32, 384)
	}
	return out, nil
}

func (b *blockingEmbedder) Dimensions() int { return 384 }
func (b *blockingEmbedder) Close() error    { return nil }

// boundedUpTo returns an *atomic.Int64 initialized to id, for runLive's
// upTo parameter. Every test below except TestRunLiveExitsOnContext-
// Cancellation passes one, scoping the lane it starts to never touch any
// id beyond what the test itself knows about — necessary, not just
// belt-and-braces: see runLive's doc comment for the proof that startAfter
// alone lets an unbounded lane pick up and corrupt a concurrently running
// unrelated package's own fixtures.
func boundedUpTo(id int64) *atomic.Int64 {
	var b atomic.Int64
	b.Store(id)
	return &b
}

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
	// startAfter=entryID-1 and upTo=entryID scope this run exactly to the
	// one id this test created (see runLive's doc comment and boundedUpTo).
	go func() { done <- runLive(ctx, idx, pollInterval, entryID-1, boundedUpTo(entryID)) }()

	waitFor(t, 2*time.Second, "entry indexed", func() bool {
		return entryStatus(t, db, entryID) == "ok"
	})

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runLive returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runLive did not return after cancellation")
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
	go func() { done <- runLive(ctx, idx, pollInterval, failingID-1, boundedUpTo(okID)) }()

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
			t.Fatalf("runLive returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runLive did not return after cancellation")
	}

	if fe.failCalls.Load() == 0 {
		t.Fatal("expected at least one failing embed call")
	}
	if fe.okCalls.Load() == 0 {
		t.Fatal("expected at least one successful embed call")
	}
}

// 3. RunLive exits cleanly on context cancellation, and promptly — it must
// not wait out a full poll interval, let alone longer. This test exercises
// the exported RunLive directly (unbounded upTo; startAfter is whatever
// RunLive's own store.MaxEntryID snapshot resolves to at the moment it
// starts, not 0 — see RunLive's doc comment, fix round 1 finding 3), the
// only test in this file that does, to prove the public entry point is
// wired correctly and not just runLive. It creates no entries of its own
// and keeps its window deliberately short (one tick) to minimise — it
// cannot eliminate, since it deliberately exercises the unbounded path —
// the chance of touching a fixture row from a concurrently running
// package's test (see runLive's doc comment).
func TestRunLiveExitsOnContextCancellation(t *testing.T) {
	s, _ := testEnv(t)
	idx := New(s, &fakeEmbedder{})

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- RunLive(ctx, idx, pollInterval) }()

	// Let it run at least one tick before cancelling.
	time.Sleep(pollInterval)
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
// merely between polls: with several entries pending in one page and an
// embedder that ignores ctx entirely (blockingEmbedder — see its doc
// comment for why that matters), cancelling shortly after the pass starts
// must return long before every entry in that pass would otherwise have
// finished.
func TestRunLiveCancelsMidPassBetweenEntries(t *testing.T) {
	s, db := testEnv(t)

	const entryCount = 6
	const perEntryDelay = 150 * time.Millisecond

	var firstID, lastID int64
	for i := 0; i < entryCount; i++ {
		id := createTestEntry(t, db, "live-midpass-"+string(rune('a'+i)),
			"<p>Entry content for the mid-pass cancellation test, number "+string(rune('a'+i))+".</p>")
		if i == 0 {
			firstID = id
		}
		lastID = id
	}

	be := &blockingEmbedder{delay: perEntryDelay}
	idx := New(s, be)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	start := time.Now()
	// A short poll interval means everything above is fetched into a
	// single pass, which is the scenario this test needs: several entries
	// fetched together, so a mid-pass cancellation genuinely has entries
	// left to skip past.
	go func() { done <- runLive(ctx, idx, 10*time.Millisecond, firstID-1, boundedUpTo(lastID)) }()

	// Wait until at least one entry has started embedding, proving the
	// pass is under way, then cancel while several entries remain.
	waitFor(t, 2*time.Second, "first embed call started", func() bool {
		return be.callsStart.Load() >= 1
	})
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runLive returned error: %v", err)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("runLive did not return promptly after mid-pass cancellation")
	}
	elapsed := time.Since(start)

	// If cancellation were only checked between polls (or not at all
	// between entries), all entryCount entries would each take
	// perEntryDelay to run to completion (blockingEmbedder does not race
	// ctx itself, so nothing but RunLive's own per-entry check can cut
	// this short), totalling entryCount*perEntryDelay. Checking it between
	// every entry must return well before that.
	maxAcceptable := time.Duration(entryCount) * perEntryDelay / 2
	if elapsed > maxAcceptable {
		t.Fatalf("runLive took %v to return after mid-pass cancellation; expected well under %v "+
			"(entryCount*perEntryDelay=%v), suggesting ctx.Done() is not checked between entries",
			elapsed, maxAcceptable, time.Duration(entryCount)*perEntryDelay)
	}

	// It must not have run every entry to completion.
	if be.callsDone.Load() >= entryCount {
		t.Fatalf("expected fewer than %d completed embed calls after mid-pass cancellation, got %d",
			entryCount, be.callsDone.Load())
	}
}

// 5. A failed entry is genuinely retried, with backoff: it is not retried
// on every subsequent tick (which would be "a tight loop", spec §10's own
// words), but once retried after becoming healthy it does recover to
// status='ok' — proving the recovery is a real retry of the SAME entry,
// not a coincidence.
func TestRunLiveRetriesFailedEntryWithBackoff(t *testing.T) {
	s, db := testEnv(t)

	marker := "RETRY-MARKER-live-backoff"
	entryID := createTestEntry(t, db, "live-retry",
		"<p>Entry containing "+marker+" for the retry backoff test.</p>")

	fe := &flippableEmbedder{marker: marker}
	idx := New(s, fe)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- runLive(ctx, idx, pollInterval, entryID-1, boundedUpTo(entryID)) }()

	waitFor(t, 2*time.Second, "entry marked failed", func() bool {
		return entryStatus(t, db, entryID) == "failed"
	})

	// While still unhealthy, let several poll intervals pass. With
	// initialBackoff = pollInterval*retryBackoffMultiple (4), at most one
	// backoff-gated retry can plausibly land inside this window — nowhere
	// near "every tick". This is the evidence that failures are not
	// retried in a tight loop.
	time.Sleep(pollInterval * 8)
	callsWhileUnhealthy := fe.callsForMarker.Load()
	if callsWhileUnhealthy > 4 {
		t.Fatalf("expected the failing entry to be retried with backoff, not roughly every tick; "+
			"got %d embed calls across 8 poll intervals while still unhealthy", callsWhileUnhealthy)
	}

	fe.healthy.Store(true)

	waitFor(t, 2*time.Second, "entry recovered to ok after becoming healthy", func() bool {
		return entryStatus(t, db, entryID) == "ok"
	})

	if fe.callsForMarker.Load() <= callsWhileUnhealthy {
		t.Fatal("expected at least one more embed call for this entry after becoming healthy, " +
			"proving the recovery came from a genuine retry rather than some other path")
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runLive returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runLive did not return after cancellation")
	}
}

// 6. A persistently failing entry does not block newer entries: once it
// has failed once (and is sitting in the backoff-gated retry set, not the
// lastSeen cursor), a later-arriving entry with a higher id must still be
// indexed promptly.
func TestRunLiveDoesNotBlockNewerEntriesOnPersistentFailure(t *testing.T) {
	s, db := testEnv(t)

	marker := "PERSIST-FAIL-MARKER-live"
	failingID := createTestEntry(t, db, "live-block-fail",
		"<p>Entry with "+marker+" that always fails to embed.</p>")

	fe := &failMarkerEmbedder{failMarker: marker}
	idx := New(s, fe)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	upTo := boundedUpTo(failingID)

	done := make(chan error, 1)
	go func() { done <- runLive(ctx, idx, pollInterval, failingID-1, upTo) }()

	waitFor(t, 2*time.Second, "failing entry marked failed", func() bool {
		return entryStatus(t, db, failingID) == "failed"
	})

	// Only now create the newer entry, once the failing one is already
	// parked in the retry set and lastSeen has moved past it. Extending
	// upTo (an *atomic.Int64 the running lane re-reads every tick — see
	// runLive's doc comment) admits exactly this one additional id rather
	// than lifting the bound entirely.
	newerID := createTestEntry(t, db, "live-block-newer",
		"<p>A perfectly fine, later-arriving entry that must not be starved.</p>")
	upTo.Store(newerID)

	waitFor(t, 2*time.Second, "newer entry indexed despite the earlier persistent failure", func() bool {
		return entryStatus(t, db, newerID) == "ok"
	})

	// The failing entry must still be failed, not silently dropped.
	if entryStatus(t, db, failingID) != "failed" {
		t.Fatalf("expected the persistently failing entry #%d to remain status='failed', got %q",
			failingID, entryStatus(t, db, failingID))
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runLive returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runLive did not return after cancellation")
	}
}

// 7. (Fix round 1, finding 3; test added fix round 2.) RunLive actually
// skips pre-existing entries at startup — not merely that store.MaxEntryID
// itself works in isolation (already covered in the store package), but
// that the exported RunLive genuinely uses it as a cursor floor and never
// touches anything at or below it, no matter how long it keeps running.
// This is deterministic, not a race: an entry created before RunLive's
// startup snapshot has an id at or below that snapshot by construction, so
// "id > lastSeen" can never match it on any subsequent tick.
// This deliberately does NOT call the exported, unbounded RunLive for
// several seconds while creating fixtures: that shape is exactly what
// runLive's own doc comment (above) documents at length as having
// indexed, and corrupted, a concurrently running package's rows under
// Go's default cross-package test parallelism -- it's why the
// pre-existing TestRunLiveExitsOnContextCancellation, the one test in
// this file that does exercise the real RunLive entry point directly,
// creates no entries of its own and keeps to a single tick. An earlier
// version of this test made exactly that mistake in the other direction
// (fix round 3 review).
//
// Instead, this calls store.MaxEntryID() itself -- the exact call
// RunLive makes internally -- and feeds the result into the
// already-bounded runLive helper, which proves the same underlying
// behaviour (a cursor starting at the pre-existing max id skips
// everything at or below it, and still catches genuinely new arrivals)
// without ever running an unbounded lane.
func TestRunLiveSkipsPreexistingEntries(t *testing.T) {
	s, db := testEnv(t)

	// Created BEFORE the cursor snapshot: must never be touched.
	oldID := createTestEntry(t, db, "live-preexisting-old",
		"<p>Pre-existing entry that must be left to the backfill lane.</p>")

	startAfter, err := s.MaxEntryID()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if startAfter < oldID {
		t.Fatalf("test setup invalid: expected MaxEntryID (%d) to be at least oldID (%d)", startAfter, oldID)
	}

	idx := New(s, &fakeEmbedder{})

	// Created AFTER the snapshot: must still be picked up promptly,
	// proving the lane is genuinely running, not merely refusing to
	// touch anything.
	newID := createTestEntry(t, db, "live-preexisting-new",
		"<p>Newly arrived entry that must still be indexed promptly.</p>")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runLive(ctx, idx, pollInterval, startAfter, boundedUpTo(newID)) }()

	waitFor(t, 2*time.Second, "the newly arrived entry indexed", func() bool {
		return entryStatus(t, db, newID) == "ok"
	})

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runLive returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runLive did not return after cancellation")
	}

	if entryStatus(t, db, oldID) != "" {
		t.Fatalf("expected the pre-existing entry #%d to be left untouched, got status %q", oldID, entryStatus(t, db, oldID))
	}
}
