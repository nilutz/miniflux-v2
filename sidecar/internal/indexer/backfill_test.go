// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package indexer // import "miniflux.app/v2/sidecar/internal/indexer"

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// backfillTestConfig is a Controller/Backfill configuration pinned to
// exactly one worker and one entry per page, used by every test in this
// file that needs deterministic, strictly-sequential-by-id processing
// order. Tests that don't care about ordering still use it for simplicity;
// none of these tests are about proving concurrency itself (the controller
// tests own that), just correct sequencing, pausing and resumption.
func backfillTestConfig() (ControllerConfig, BackfillConfig) {
	return ControllerConfig{MinWorkers: 1, MaxWorkers: 1, LatencyMargin: 1.5, LoadThreshold: 0.8},
		BackfillConfig{PageSize: 1, PollInterval: 20 * time.Millisecond}
}

// markerCallCounts counts, per marker substring, how many Embed calls saw
// text containing it. Shared across two Indexer/Embedder pairs standing in
// for two separate processes (a crash and a restart), it lets a test
// assert a specific entry's marker was embedded exactly N times in total,
// proving neither reprocessing nor a missed entry regardless of which of
// the two "processes" did the work.
type markerCallCounts struct {
	mu     sync.Mutex
	counts map[string]int64
}

func newMarkerCallCounts() *markerCallCounts {
	return &markerCallCounts{counts: make(map[string]int64)}
}

func (m *markerCallCounts) record(texts []string, markers []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, text := range texts {
		for _, marker := range markers {
			if strings.Contains(text, marker) {
				m.counts[marker]++
			}
		}
	}
}

func (m *markerCallCounts) get(marker string) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.counts[marker]
}

// countingEmbedder always succeeds and records every call into a shared
// markerCallCounts.
type countingEmbedder struct {
	counts  *markerCallCounts
	markers []string
}

func (e *countingEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	e.counts.record(texts, e.markers)
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = make([]float32, 384)
	}
	return out, nil
}
func (e *countingEmbedder) Dimensions() int { return 384 }
func (e *countingEmbedder) Close() error    { return nil }

// blockOnMarkerEmbedder behaves like countingEmbedder, except that while
// block is true, a call whose text contains marker blocks on ctx.Done()
// instead of returning — closing blocked (once, via blockedOnce) right
// before it starts waiting, so a test can synchronise on "this specific
// entry's embedding is now in flight and stuck" without polling or
// sleeping. Cancelling the context is therefore both how the test learns
// the pipeline reached exactly this point AND how it deterministically
// interrupts it, with no timing race.
type blockOnMarkerEmbedder struct {
	marker  string
	block   atomic.Bool
	counts  *markerCallCounts
	markers []string

	blocked     chan struct{}
	blockedOnce sync.Once
}

func (e *blockOnMarkerEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	e.counts.record(texts, e.markers)

	marked := false
	for _, text := range texts {
		if strings.Contains(text, e.marker) {
			marked = true
		}
	}
	if marked && e.block.Load() {
		e.blockedOnce.Do(func() { close(e.blocked) })
		<-ctx.Done()
		return nil, ctx.Err()
	}

	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = make([]float32, 384)
	}
	return out, nil
}
func (e *blockOnMarkerEmbedder) Dimensions() int { return 384 }
func (e *blockOnMarkerEmbedder) Close() error    { return nil }

// runStartAsync runs a Backfill's start in a goroutine and returns a
// channel that receives its error when it returns.
func runStartAsync(b *Backfill, ctx context.Context, startAfter int64, upTo *atomic.Int64) chan error {
	done := make(chan error, 1)
	go func() { done <- b.start(ctx, startAfter, upTo) }()
	return done
}

func waitDone(t *testing.T, done chan error, timeout time.Duration, what string) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("%s returned error: %v", what, err)
		}
	case <-time.After(timeout):
		t.Fatalf("%s did not return within %v", what, timeout)
	}
}

// 1. Processes every pending entry and terminates.
func TestBackfillProcessesEveryPendingEntryAndTerminates(t *testing.T) {
	s, db := testEnv(t)

	var firstID, lastID int64
	const n = 4
	for i := 0; i < n; i++ {
		id := createTestEntry(t, db, "backfill-drain-"+string(rune('a'+i)),
			"<p>Backfill drain test entry number "+string(rune('a'+i))+" with some plain content.</p>")
		if i == 0 {
			firstID = id
		}
		lastID = id
	}

	idx := New(s, &fakeEmbedder{})
	ctrlCfg, bfCfg := backfillTestConfig()
	controller := newController(ctrlCfg, time.Now, fixedLoad(0.1))
	b := NewBackfill(idx, controller, bfCfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := runStartAsync(b, ctx, firstID-1, boundedUpTo(lastID))
	waitDone(t, done, 5*time.Second, "Backfill.start")

	for i := 0; i < n; i++ {
		id := firstID + int64(i)
		if entryStatus(t, db, id) != "ok" {
			t.Fatalf("expected entry #%d to be indexed (status='ok'), got %q", id, entryStatus(t, db, id))
		}
	}

	stats := b.Stats()
	if !stats.Done {
		t.Fatal("expected Stats().Done to be true after draining every pending entry")
	}
	if stats.Indexed != n {
		t.Fatalf("expected Stats().Indexed=%d, got %d", n, stats.Indexed)
	}
}

// 2. Resumes from its checkpoint: interrupted after some entries, a fresh
// Backfill restarted from scratch neither reprocesses the entries already
// done nor skips any of the rest. Checkpointing is implicit —
// search.entry_index_state, not an in-memory cursor, is what a restart
// relies on — so this test genuinely restarts the pagination cursor at
// zero (startAfter is the same on both calls) rather than remembering
// where the first run stopped.
func TestBackfillResumesFromCheckpointAfterInterruption(t *testing.T) {
	s, db := testEnv(t)

	const n = 5
	ids := make([]int64, n)
	markers := make([]string, n)
	for i := 0; i < n; i++ {
		markers[i] = "RESUME-MARKER-" + string(rune('a'+i))
		ids[i] = createTestEntry(t, db, "backfill-resume-"+string(rune('a'+i)),
			"<p>Entry containing "+markers[i]+" for the resumability test.</p>")
	}
	firstID, lastID := ids[0], ids[n-1]

	counts := newMarkerCallCounts()
	// entries[3] (index 3, 0-based) is where phase 1 gets stuck.
	blockMarker := markers[3]
	embedder1 := &blockOnMarkerEmbedder{marker: blockMarker, counts: counts, markers: markers, blocked: make(chan struct{})}
	embedder1.block.Store(true)

	idx1 := New(s, embedder1)
	ctrlCfg, bfCfg := backfillTestConfig()
	controller1 := newController(ctrlCfg, time.Now, fixedLoad(0.1))
	b1 := NewBackfill(idx1, controller1, bfCfg)

	ctx1, cancel1 := context.WithCancel(context.Background())
	done1 := runStartAsync(b1, ctx1, firstID-1, boundedUpTo(lastID))

	select {
	case <-embedder1.blocked:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for phase 1 to reach and block on the marked entry")
	}

	// At this point entries 0,1,2 must already be durably 'ok': with
	// PageSize=1 and MaxWorkers=1, IndexEntry for each of them completed
	// (embed, then write) strictly before the next page — including
	// entry 3's — was even fetched.
	for i := 0; i < 3; i++ {
		if entryStatus(t, db, ids[i]) != "ok" {
			t.Fatalf("expected entry #%d (index %d) to already be 'ok' once phase 1 reached the blocked entry, got %q",
				ids[i], i, entryStatus(t, db, ids[i]))
		}
	}
	// Entry 4 must not have been touched at all yet — pageSize=1 means it
	// cannot have been fetched before entry 3.
	if entryStatus(t, db, ids[4]) != "" {
		t.Fatalf("expected entry #%d (index 4) to be untouched while phase 1 is stuck on entry 3, got %q",
			ids[4], entryStatus(t, db, ids[4]))
	}

	cancel1() // interrupts the blocked embed call and, with it, the whole lane
	waitDone(t, done1, 5*time.Second, "phase 1 Backfill.start")

	if entryStatus(t, db, ids[3]) != "failed" {
		t.Fatalf("expected the interrupted entry #%d to be recorded as 'failed' (retryable), got %q",
			ids[3], entryStatus(t, db, ids[3]))
	}
	for i := 0; i < 3; i++ {
		if got := counts.get(markers[i]); got != 1 {
			t.Fatalf("expected entry index %d to have been embedded exactly once after phase 1, got %d", i, got)
		}
	}

	// Phase 2: a brand new Backfill/Indexer/Controller — standing in for a
	// fresh process after a restart — rescanning from the very same
	// starting cursor. Nothing here remembers phase 1's progress in
	// memory; only search.entry_index_state does.
	embedder1.block.Store(false) // disarm, in case anything still references marker 3
	embedder2 := &countingEmbedder{counts: counts, markers: markers}
	idx2 := New(s, embedder2)
	controller2 := newController(ctrlCfg, time.Now, fixedLoad(0.1))
	b2 := NewBackfill(idx2, controller2, bfCfg)

	done2 := runStartAsync(b2, context.Background(), firstID-1, boundedUpTo(lastID))
	waitDone(t, done2, 5*time.Second, "phase 2 Backfill.start")

	for i, id := range ids {
		if entryStatus(t, db, id) != "ok" {
			t.Fatalf("expected entry #%d (index %d) to be 'ok' after phase 2, got %q", id, i, entryStatus(t, db, id))
		}
	}

	// The proof of resumability: entries 0-2 were embedded exactly once
	// EVER, across both phases — phase 2 must not have re-embedded them.
	for i := 0; i < 3; i++ {
		if got := counts.get(markers[i]); got != 1 {
			t.Fatalf("entry index %d was reprocessed: expected exactly 1 embed call total across both phases, got %d", i, got)
		}
	}
	// Entry 3 legitimately gets a second, successful attempt in phase 2
	// (its first attempt was genuinely interrupted, not skipped).
	if got := counts.get(markers[3]); got != 2 {
		t.Fatalf("expected entry index 3 to have exactly 2 embed calls total (1 interrupted + 1 successful retry), got %d", got)
	}
	// Entry 4 is embedded exactly once, in phase 2 only.
	if got := counts.get(markers[4]); got != 1 {
		t.Fatalf("expected entry index 4 to have exactly 1 embed call (phase 2 only), got %d", got)
	}
}

// 3. Pause stops work; resume continues.
func TestBackfillPauseStopsWorkResumeContinues(t *testing.T) {
	s, db := testEnv(t)

	firstID := createTestEntry(t, db, "backfill-pause-a", "<p>First entry for the pause/resume test.</p>")
	lastID := createTestEntry(t, db, "backfill-pause-b", "<p>Second entry for the pause/resume test.</p>")

	idx := New(s, &fakeEmbedder{})
	ctrlCfg, bfCfg := backfillTestConfig()
	controller := newController(ctrlCfg, time.Now, fixedLoad(0.1))
	b := NewBackfill(idx, controller, bfCfg)
	b.Pause() // paused before Start is ever called: nothing must be processed yet

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runStartAsync(b, ctx, firstID-1, boundedUpTo(lastID))

	if !b.Stats().Paused {
		t.Fatal("expected Stats().Paused to be true immediately after Pause()")
	}

	// Prove the lane genuinely did not start work: no event will ever
	// prove a negative instantaneously, so this waits out a generous,
	// bounded window (matching this package's existing precedent in
	// live_test.go's backoff test) rather than polling for something that
	// is not supposed to happen.
	time.Sleep(200 * time.Millisecond)
	if entryStatus(t, db, firstID) != "" {
		t.Fatalf("expected no work to happen while paused, but entry #%d has status %q", firstID, entryStatus(t, db, firstID))
	}
	if entryStatus(t, db, lastID) != "" {
		t.Fatalf("expected no work to happen while paused, but entry #%d has status %q", lastID, entryStatus(t, db, lastID))
	}

	b.Resume()
	if b.Stats().Paused {
		t.Fatal("expected Stats().Paused to be false immediately after Resume()")
	}

	waitFor(t, 2*time.Second, "both entries indexed after resume", func() bool {
		return entryStatus(t, db, firstID) == "ok" && entryStatus(t, db, lastID) == "ok"
	})

	cancel()
	waitDone(t, done, 2*time.Second, "Backfill.start")
}

// 4. Respects the schedule window: outside it, no work happens.
func TestBackfillRespectsScheduleWindow(t *testing.T) {
	s, db := testEnv(t)

	entryID := createTestEntry(t, db, "backfill-window", "<p>Entry that must not be indexed outside the window.</p>")

	// A one-hour window guaranteed to exclude the real current hour, so a
	// real (unfaked) Controller genuinely reports zero workers right now.
	now := time.Now()
	closedStart := (now.Hour() + 2) % 24
	closedEnd := (closedStart + 1) % 24

	idx := New(s, &fakeEmbedder{})
	ctrlCfg := ControllerConfig{
		MinWorkers: 1, MaxWorkers: 1, LatencyMargin: 1.5, LoadThreshold: 0.8,
		Window: Window{Start: closedStart, End: closedEnd},
	}
	controller := NewController(ctrlCfg)
	if got := controller.Workers(); got != 0 {
		t.Fatalf("test setup invalid: expected the chosen window to be closed right now (Workers()=0), got %d", got)
	}

	_, bfCfg := backfillTestConfig()
	bfCfg.PollInterval = 20 * time.Millisecond
	b := NewBackfill(idx, controller, bfCfg)

	ctx, cancel := context.WithCancel(context.Background())
	done := runStartAsync(b, ctx, entryID-1, boundedUpTo(entryID))

	time.Sleep(200 * time.Millisecond)
	if entryStatus(t, db, entryID) != "" {
		t.Fatalf("expected no work outside the schedule window, but entry #%d has status %q", entryID, entryStatus(t, db, entryID))
	}
	if got := b.Stats().Workers; got != 0 {
		t.Fatalf("expected Stats().Workers=0 outside the schedule window, got %d", got)
	}

	cancel()
	waitDone(t, done, 2*time.Second, "Backfill.start")
}

// 5. Stats() reports progress, throughput, current worker count, and
// error/skip counts by cause.
func TestBackfillStatsReportsProgressAndCauses(t *testing.T) {
	s, db := testEnv(t)

	okID := createTestEntry(t, db, "backfill-stats-ok",
		"<p>A perfectly normal entry that indexes successfully.</p>")
	skipID := createTestEntry(t, db, "backfill-stats-skip",
		"<script>var x = 1;</script><style>p { color: red; }</style>")
	failMarker := "FAIL-MARKER-backfill-stats"
	failID := createTestEntry(t, db, "backfill-stats-fail",
		"<p>Entry containing "+failMarker+" that always fails to embed.</p>")

	fe := &failMarkerEmbedder{failMarker: failMarker}
	idx := New(s, fe)
	ctrlCfg, bfCfg := backfillTestConfig()
	bfCfg.PageSize = 10
	controller := newController(ctrlCfg, time.Now, fixedLoad(0.1))
	b := NewBackfill(idx, controller, bfCfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runStartAsync(b, ctx, okID-1, boundedUpTo(failID))
	waitDone(t, done, 5*time.Second, "Backfill.start")

	stats := b.Stats()
	if stats.Indexed != 1 {
		t.Fatalf("expected Stats().Indexed=1, got %d", stats.Indexed)
	}
	if stats.Skipped != 1 {
		t.Fatalf("expected Stats().Skipped=1, got %d", stats.Skipped)
	}
	if stats.Failed != 1 {
		t.Fatalf("expected Stats().Failed=1, got %d", stats.Failed)
	}
	if len(stats.SkippedByReason) == 0 {
		t.Fatal("expected at least one skip cause recorded in Stats().SkippedByReason")
	}
	if len(stats.FailedByReason) == 0 {
		t.Fatal("expected at least one failure cause recorded in Stats().FailedByReason")
	}
	var skipTotal, failTotal int64
	for _, c := range stats.SkippedByReason {
		skipTotal += c
	}
	for _, c := range stats.FailedByReason {
		failTotal += c
	}
	if skipTotal != stats.Skipped {
		t.Fatalf("expected SkippedByReason to sum to Stats().Skipped=%d, got %d", stats.Skipped, skipTotal)
	}
	if failTotal != stats.Failed {
		t.Fatalf("expected FailedByReason to sum to Stats().Failed=%d, got %d", stats.Failed, failTotal)
	}
	if stats.Workers < ctrlCfg.MinWorkers || stats.Workers > ctrlCfg.MaxWorkers {
		t.Fatalf("expected Stats().Workers within [%d,%d], got %d", ctrlCfg.MinWorkers, ctrlCfg.MaxWorkers, stats.Workers)
	}
	if stats.ControllerReason == "" {
		t.Fatal("expected a non-empty Stats().ControllerReason")
	}
	if stats.ThroughputPerSec < 0 {
		t.Fatalf("expected a non-negative Stats().ThroughputPerSec, got %f", stats.ThroughputPerSec)
	}
	if !stats.Done {
		t.Fatal("expected Stats().Done to be true after draining")
	}

	if entryStatus(t, db, okID) != "ok" {
		t.Fatal("expected the normal entry to be indexed ok")
	}
	if entryStatus(t, db, skipID) != "skipped" {
		t.Fatal("expected the empty-content entry to be skipped")
	}
	if entryStatus(t, db, failID) != "failed" {
		t.Fatal("expected the always-failing entry to be marked failed")
	}
}

// 6. An entry whose content changed is re-indexed.
func TestBackfillReindexesEntryWithChangedContent(t *testing.T) {
	s, db := testEnv(t)

	entryID := createTestEntry(t, db, "backfill-changed",
		"<p>Original content for the change-detection test, version one.</p>")

	fe := &fakeEmbedder{}
	idx := New(s, fe)
	ctrlCfg, bfCfg := backfillTestConfig()
	controller := newController(ctrlCfg, time.Now, fixedLoad(0.1))
	b := NewBackfill(idx, controller, bfCfg)

	ctx := context.Background()
	if err := b.start(ctx, entryID-1, boundedUpTo(entryID)); err != nil {
		t.Fatalf("first pass failed: %v", err)
	}
	if entryStatus(t, db, entryID) != "ok" {
		t.Fatal("expected the entry to be indexed after the first pass")
	}
	callsAfterFirst := fe.calls
	if callsAfterFirst == 0 {
		t.Fatal("expected at least one embed call on the first pass")
	}

	updateEntryContent(t, db, entryID,
		"<p>Completely different content after the edit, version two, unrelated sentence.</p>")

	b2 := NewBackfill(idx, controller, bfCfg)
	if err := b2.start(ctx, entryID-1, boundedUpTo(entryID)); err != nil {
		t.Fatalf("second pass failed: %v", err)
	}

	if fe.calls <= callsAfterFirst {
		t.Fatalf("expected additional embed calls after the content changed, got %d -> %d", callsAfterFirst, fe.calls)
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
			t.Fatalf("found an orphaned passage from before the content change: %q", text)
		}
	}

	entry, err := s.EntryForIndexing(entryID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	state, err := s.EntryIndexState(entryID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if state == nil || state.ContentHash != entry.ContentHash {
		t.Fatalf("expected the recorded content_hash to match the new content's hash")
	}
}
