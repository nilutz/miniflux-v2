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

// variableDelayEmbedder always succeeds, sleeping delay (settable live,
// via an atomic so a test can change it between phases without racing the
// worker goroutine) before returning. Used to prove Stats().ThroughputPerSec
// tracks recent batches rather than a lifetime average: a test can run a
// slow phase, then a fast one, and check the reported rate follows.
type variableDelayEmbedder struct {
	delay atomic.Int64 // nanoseconds
}

func (d *variableDelayEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	if ns := d.delay.Load(); ns > 0 {
		time.Sleep(time.Duration(ns))
	}
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = make([]float32, 384)
	}
	return out, nil
}
func (d *variableDelayEmbedder) Dimensions() int { return 384 }
func (d *variableDelayEmbedder) Close() error    { return nil }

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

	// Track the actual ids created, rather than assuming they are
	// consecutive (firstID, firstID+1, ...): under Go's default
	// cross-package test parallelism, another concurrently running
	// package can consume an id from the same shared entries sequence in
	// between these calls, leaving a gap. Checking firstID+i against that
	// gap silently checks a foreign row (or no row at all) instead of one
	// of ours, which is exactly what caused this test to flake — see this
	// fix round's report.
	const n = 4
	ids := make([]int64, n)
	for i := 0; i < n; i++ {
		ids[i] = createTestEntry(t, db, "backfill-drain-"+string(rune('a'+i)),
			"<p>Backfill drain test entry number "+string(rune('a'+i))+" with some plain content.</p>")
	}
	firstID, lastID := ids[0], ids[n-1]

	idx := New(s, &fakeEmbedder{})
	ctrlCfg, bfCfg := backfillTestConfig()
	controller := newController(ctrlCfg, time.Now, fixedLoad(0.1))
	b := NewBackfill(idx, controller, bfCfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := runStartAsync(b, ctx, firstID-1, boundedUpTo(lastID))
	waitDone(t, done, 5*time.Second, "Backfill.start")

	for _, id := range ids {
		if entryStatus(t, db, id) != "ok" {
			t.Fatalf("expected entry #%d to be indexed (status='ok'), got %q", id, entryStatus(t, db, id))
		}
	}

	stats := b.Stats()
	if !stats.Done {
		t.Fatal("expected Stats().Done to be true after draining every pending entry")
	}
	// At least n: a lower bound, not exact equality, in case a
	// concurrently running package's own fixture landed at an id inside
	// [firstID, lastID] and got swept up too (see the ids-tracking fix
	// above; the same underlying, precedent-accepted residual risk).
	if stats.Indexed < n {
		t.Fatalf("expected Stats().Indexed>=%d, got %d", n, stats.Indexed)
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

	// Interrupted by cancellation, not a genuine embedding failure: fix
	// round 1, finding 8 changed IndexEntry to leave entry_index_state
	// untouched in this case rather than writing status='failed', so a
	// graceful shutdown never manufactures a spurious failed row. The
	// entry is exactly as untouched as one that was never attempted.
	if entryStatus(t, db, ids[3]) != "" {
		t.Fatalf("expected the interrupted entry #%d to be left untouched (no status), got %q",
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
//
// The failing entry here is durably, permanently broken (failMarkerEmbedder
// never recovers), so — per fix round 1, finding 2 — Backfill.start will
// legitimately never declare itself Done on its own: every sweep that
// reaches its end having retried that entry resets and tries again. This
// test therefore does not wait for natural termination; it polls Stats()
// until the three outcomes it wants to observe have all happened at least
// once, then cancels.
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
	done := runStartAsync(b, ctx, okID-1, boundedUpTo(failID))

	waitFor(t, 2*time.Second, "at least one indexed, one skipped and one failure observed", func() bool {
		s := b.Stats()
		return s.Indexed >= 1 && s.Skipped >= 1 && s.Failed >= 1
	})

	cancel()
	waitDone(t, done, 2*time.Second, "Backfill.start")

	stats := b.Stats()
	// At least 1, not exactly 1: a lower bound, in case a concurrently
	// running package's own fixture landed at an id inside [okID,
	// failID] and got swept up and processed too (the same
	// precedent-accepted residual risk noted throughout this file).
	if stats.Indexed < 1 {
		t.Fatalf("expected Stats().Indexed>=1, got %d", stats.Indexed)
	}
	if stats.Skipped < 1 {
		t.Fatalf("expected Stats().Skipped>=1, got %d", stats.Skipped)
	}
	if stats.Failed < 1 {
		t.Fatalf("expected Stats().Failed>=1, got %d", stats.Failed)
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
	if stats.Remaining < 0 {
		t.Fatalf("expected a non-negative Stats().Remaining, got %d (a negative value means the count itself failed)", stats.Remaining)
	}
	// The permanently-broken entry means the backlog never actually
	// drains: Done must stay false, not falsely report success while a
	// failure is still pending (fix round 1, finding 2 — this is the
	// exact assertion that used to encode the bug).
	if stats.Done {
		t.Fatal("expected Stats().Done to remain false while a permanently-failing entry is still pending")
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
	callsAfterFirst := fe.calls.Load()
	if callsAfterFirst == 0 {
		t.Fatal("expected at least one embed call on the first pass")
	}

	updateEntryContent(t, db, entryID,
		"<p>Completely different content after the edit, version two, unrelated sentence.</p>")

	b2 := NewBackfill(idx, controller, bfCfg)
	if err := b2.start(ctx, entryID-1, boundedUpTo(entryID)); err != nil {
		t.Fatalf("second pass failed: %v", err)
	}

	if fe.calls.Load() <= callsAfterFirst {
		t.Fatalf("expected additional embed calls after the content changed, got %d -> %d", callsAfterFirst, fe.calls.Load())
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

// 7. (Fix round 1, finding 2.) Stats().Done must not go true while a
// failed entry is still pending, and must become true once that entry is
// genuinely retried and recovers. Before this fix, the cursor only ever
// moved forward within one sweep: an entry that failed early in a pass
// sat below the cursor, permanently un-retried within that run, while the
// pass still reported Done=true the moment its last page came up empty —
// even though the failure was never looked at again. This test uses a
// TRANSIENT failure (flippableEmbedder, from live_test.go, same package)
// specifically so it can observe both halves: Done stays false across
// several sweep-and-retry cycles while unhealthy, then becomes true once
// the entry starts succeeding — proving the retry is genuine, not that
// Done is simply never computed correctly at all.
func TestBackfillDoneWaitsForFailedEntryRetry(t *testing.T) {
	s, db := testEnv(t)

	okID := createTestEntry(t, db, "backfill-done-ok",
		"<p>A perfectly normal entry, unaffected by the other one's trouble.</p>")
	marker := "TRANSIENT-FAIL-MARKER-backfill-done"
	failID := createTestEntry(t, db, "backfill-done-fail",
		"<p>Entry containing "+marker+" that fails until nudged healthy.</p>")

	fe := &flippableEmbedder{marker: marker}
	idx := New(s, fe)
	ctrlCfg, bfCfg := backfillTestConfig()
	controller := newController(ctrlCfg, time.Now, fixedLoad(0.1))
	b := NewBackfill(idx, controller, bfCfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runStartAsync(b, ctx, okID-1, boundedUpTo(failID))

	waitFor(t, 2*time.Second, "the failing entry recorded as failed at least once", func() bool {
		return entryStatus(t, db, failID) == "failed"
	})
	if entryStatus(t, db, okID) != "ok" {
		t.Fatal("expected the healthy entry to be indexed regardless of the other one's failure")
	}

	// Let several sweep-and-retry cycles happen while still unhealthy.
	// hadFailureThisSweep's pollInterval pause between sweeps (see
	// start's doc comment) means this is not a tight loop; the assertion
	// itself is on the final state after waiting a generous, bounded
	// window, the same "prove a negative" pattern already established in
	// live_test.go's own backoff test.
	time.Sleep(bfCfg.PollInterval * 8)
	if b.Stats().Done {
		t.Fatal("expected Stats().Done to remain false while the failed entry has not yet been retried successfully")
	}
	callsWhileUnhealthy := fe.callsForMarker.Load()
	if callsWhileUnhealthy == 0 {
		t.Fatal("expected at least one retry attempt while unhealthy")
	}

	fe.healthy.Store(true)

	waitDone(t, done, 3*time.Second, "Backfill.start")

	if entryStatus(t, db, failID) != "ok" {
		t.Fatal("expected the entry to recover to 'ok' once the embedder became healthy")
	}
	if fe.callsForMarker.Load() <= callsWhileUnhealthy {
		t.Fatal("expected at least one additional embed call after becoming healthy, proving a genuine retry")
	}
	if !b.Stats().Done {
		t.Fatal("expected Stats().Done to become true once the retried entry succeeded and nothing remains pending")
	}
}

// 8. (Fix round 1, finding 10.) Start is not re-entrant: calling it again
// while a previous call on the same Backfill is still running returns an
// error immediately, rather than running two overlapping worker pools
// against the same counters.
func TestBackfillStartIsNotReentrant(t *testing.T) {
	s, db := testEnv(t)

	marker := "REENTRANT-MARKER"
	entryID := createTestEntry(t, db, "backfill-reentrant",
		"<p>Entry containing "+marker+" for the re-entrancy test.</p>")

	// A blocking embedder keeps the first start() call busy long enough
	// for the second call to observe it as still running -- no polling or
	// timing race, since the very first Embed call blocks on ctx.Done()
	// and signals blocked (closed) right before it starts waiting.
	be := &blockOnMarkerEmbedder{marker: marker, counts: newMarkerCallCounts(), markers: []string{marker}, blocked: make(chan struct{})}
	be.block.Store(true)
	idx := New(s, be)

	ctrlCfg, bfCfg := backfillTestConfig()
	controller := newController(ctrlCfg, time.Now, fixedLoad(0.1))
	b := NewBackfill(idx, controller, bfCfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := runStartAsync(b, ctx, entryID-1, boundedUpTo(entryID))

	select {
	case <-be.blocked:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the first start() call to begin processing")
	}

	if err := b.start(context.Background(), entryID-1, boundedUpTo(entryID)); err == nil {
		t.Fatal("expected a second, concurrent start() call to return an error")
	}

	cancel() // unblocks the first call's blocked embed call too
	waitDone(t, done, 2*time.Second, "first Backfill.start")
}

// 9. (Fix round 2, finding 1 -- regression fix.) The rescan loop backs off
// per entry instead of hammering a durably failing one every sweep, and
// Stats().Failed does not grow without bound while the SAME cause keeps
// repeating: it counts distinct problems, not attempts.
//
// Before this fix, fix round 1's Done-semantics change (finding 2) reset
// the cursor and reattempted every still-failing entry on every sweep,
// gated only by a flat pollInterval pause between sweeps -- at a 20ms
// test pollInterval that is dozens of embed calls a second, forever, for
// one permanently broken entry. This test lets real wall-clock time pass
// and checks that only a handful of attempts happened, and that the
// failure count never climbed past 1.
func TestBackfillRescanBacksOffAndDoesNotInflateFailedCount(t *testing.T) {
	s, db := testEnv(t)

	marker := "PERMANENT-FAIL-MARKER-backoff"
	entryID := createTestEntry(t, db, "backfill-backoff",
		"<p>Entry containing "+marker+" that always fails to embed, forever.</p>")

	fe := &failMarkerEmbedder{failMarker: marker}
	idx := New(s, fe)
	ctrlCfg, bfCfg := backfillTestConfig() // PollInterval: 20ms
	controller := newController(ctrlCfg, time.Now, fixedLoad(0.1))
	b := NewBackfill(idx, controller, bfCfg)

	ctx, cancel := context.WithCancel(context.Background())
	done := runStartAsync(b, ctx, entryID-1, boundedUpTo(entryID))

	waitFor(t, 2*time.Second, "the entry recorded failed at least once", func() bool {
		return entryStatus(t, db, entryID) == "failed"
	})

	// Let real time pass: at the pre-fix, unthrottled retry cadence (one
	// attempt roughly every pollInterval=20ms), this window is long
	// enough for on the order of 100 embed calls. With per-entry backoff
	// (base = pollInterval*retryBackoffMultiple, capped at
	// pollInterval*retryBackoffCapMultiple -- see start's doc comment,
	// reusing live.go's own backoff constants), it should be retried only
	// a handful of times.
	time.Sleep(2 * time.Second)

	cancel()
	waitDone(t, done, 2*time.Second, "Backfill.start")

	calls := fe.failCalls.Load()
	if calls < 1 {
		t.Fatal("expected at least one embed call")
	}
	if calls > 10 {
		t.Fatalf("expected the permanently-failing entry to be retried only a handful of times over 2s under backoff, got %d embed calls", calls)
	}

	stats := b.Stats()
	if stats.Failed != 1 {
		t.Fatalf("expected Stats().Failed to stay at exactly 1 for a persistently-failing entry with an unchanged cause, got %d", stats.Failed)
	}
	if len(stats.FailedByReason) != 1 {
		t.Fatalf("expected exactly one distinct failure cause recorded, got %d: %v", len(stats.FailedByReason), stats.FailedByReason)
	}
	for cause, count := range stats.FailedByReason {
		if count != 1 {
			t.Fatalf("expected cause %q to be counted exactly once despite repeated attempts, got %d", cause, count)
		}
	}
	if stats.Done {
		t.Fatal("expected Stats().Done to remain false while the entry is still failing")
	}
}

// 10. (Fix round 2, finding 3.) Stats() no longer performs a blocking,
// unbounded full-corpus scan on every call: its Remaining figure is
// memoised behind a TTL, so a second call within that window returns the
// cached value rather than re-querying, even though the real pending
// count changed in between.
func TestBackfillStatsMemoisesRemainingCount(t *testing.T) {
	s, db := testEnv(t)

	entryID := createTestEntry(t, db, "backfill-remaining-cache",
		"<p>Entry whose pending status changes between two Stats() calls, to prove Remaining is cached rather than re-queried.</p>")

	idx := New(s, &fakeEmbedder{})
	ctrlCfg, bfCfg := backfillTestConfig()
	controller := newController(ctrlCfg, time.Now, fixedLoad(0.1))
	b := NewBackfill(idx, controller, bfCfg)
	b.remainingCountTTL = 10 * time.Second // long enough that this test's own two calls land inside the same window

	firstStart := time.Now()
	first := b.Stats().Remaining
	firstElapsed := time.Since(firstStart)
	if first < 1 {
		t.Fatalf("expected at least 1 pending entry (our own fixture), got %d", first)
	}

	// Index the entry directly, bypassing Backfill, so the real pending
	// count changes between the two Stats() calls below.
	if err := idx.IndexEntry(context.Background(), entryID); err != nil {
		t.Fatalf("IndexEntry failed: %v", err)
	}

	secondStart := time.Now()
	second := b.Stats().Remaining
	secondElapsed := time.Since(secondStart)
	t.Logf("Stats() timing: first call (query) = %v, second call (cached) = %v", firstElapsed, secondElapsed)
	if second != first {
		t.Fatalf("expected Stats().Remaining to still return the cached value %d within the TTL window, got %d -- "+
			"it appears to have re-queried instead of serving the memoised count", first, second)
	}

	// And it does eventually refresh once the TTL elapses.
	b.remainingCountTTL = 1 * time.Millisecond
	time.Sleep(5 * time.Millisecond)
	third := b.Stats().Remaining
	if third < 0 {
		t.Fatalf("expected a non-negative refreshed Remaining, got %d", third)
	}
}

// 11. (Fix round 2, finding 4.) ThroughputPerSec reflects recent batches,
// not a lifetime average: once a slow phase is followed by a fast one,
// the reported rate rises to track the fast phase rather than staying
// pinned near the slow phase's rate the way dividing lifetime work by
// lifetime active time would.
func TestBackfillThroughputReflectsRecentBatchesNotLifetimeAverage(t *testing.T) {
	s, db := testEnv(t)

	const nSlow = 3
	const nFast = 5
	var ids []int64
	for i := 0; i < nSlow+nFast; i++ {
		id := createTestEntry(t, db, "throughput-"+string(rune('a'+i)),
			"<p>Entry "+string(rune('a'+i))+" for the throughput EWMA test.</p>")
		ids = append(ids, id)
	}
	firstID, lastID := ids[0], ids[len(ids)-1]

	de := &variableDelayEmbedder{}
	de.delay.Store(int64(150 * time.Millisecond)) // slow phase
	idx := New(s, de)
	ctrlCfg, bfCfg := backfillTestConfig() // PageSize=1, MaxWorkers=1: strictly sequential by id
	controller := newController(ctrlCfg, time.Now, fixedLoad(0.1))
	b := NewBackfill(idx, controller, bfCfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runStartAsync(b, ctx, firstID-1, boundedUpTo(lastID))

	waitFor(t, 5*time.Second, "slow phase entries indexed", func() bool {
		return entryStatus(t, db, ids[nSlow-1]) == "ok"
	})
	slowThroughput := b.Stats().ThroughputPerSec
	if slowThroughput <= 0 {
		t.Fatalf("expected a positive throughput after the slow phase, got %f", slowThroughput)
	}
	if slowThroughput > 10 {
		t.Fatalf("expected a slow throughput reading during the 150ms/entry phase, got %f/s", slowThroughput)
	}

	de.delay.Store(0) // fast phase: no artificial delay from here on

	waitFor(t, 5*time.Second, "all entries indexed", func() bool {
		return entryStatus(t, db, lastID) == "ok"
	})
	fastThroughput := b.Stats().ThroughputPerSec

	cancel()
	waitDone(t, done, 2*time.Second, "Backfill.start")

	if fastThroughput <= slowThroughput*2 {
		t.Fatalf("expected throughput to rise substantially once the slow phase ended (recent-weighted, not a lifetime average): slow=%.2f/s fast=%.2f/s",
			slowThroughput, fastThroughput)
	}
}

// 12. (Fix round 3, finding 1 -- regression against 21cd5600.) A tracked
// failing entry that is later removed from the pending set ENTIRELY --
// its row deleted, as by a feed removal or retention cleanup during this
// lane's 4-41 hour runtime, not merely fixed -- must not pin Done open
// forever. Before this fix, retryTracker state was cleared only by a
// successful re-attempt or a fresh Start; an id that stops being
// returned by PendingEntryIDs at all is never attempted again, so
// nothing ever clears it, and hasOutstanding() stays true permanently.
func TestBackfillPrunesRetryStateForEntryRemovedFromPendingSet(t *testing.T) {
	s, db := testEnv(t)

	okID := createTestEntry(t, db, "backfill-prune-ok", "<p>A perfectly normal entry, unaffected.</p>")
	marker := "PRUNE-FAIL-MARKER-round3"
	failID := createTestEntry(t, db, "backfill-prune-fail",
		"<p>Entry containing "+marker+" that always fails to embed.</p>")

	fe := &failMarkerEmbedder{failMarker: marker}
	idx := New(s, fe)
	ctrlCfg, bfCfg := backfillTestConfig()
	controller := newController(ctrlCfg, time.Now, fixedLoad(0.1))
	b := NewBackfill(idx, controller, bfCfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runStartAsync(b, ctx, okID-1, boundedUpTo(failID))

	waitFor(t, 2*time.Second, "the failing entry recorded failed at least once", func() bool {
		return entryStatus(t, db, failID) == "failed"
	})
	// Give it at least one more sweep to confirm it is genuinely tracked
	// as outstanding (not a one-off race), matching the reviewer's own
	// reproduction shape.
	waitFor(t, 2*time.Second, "the lane is still running with retries outstanding", func() bool {
		return !b.Stats().Done
	})

	// Simulate the entry disappearing from the pending set entirely --
	// e.g. its feed was removed -- rather than being fixed. Deleting the
	// row (and its search-schema rows, mirroring createTestEntry's own
	// cleanup) is the simplest way to make PendingEntryIDs stop returning
	// it without needing to fake an actual retention-cleanup code path.
	if _, err := db.Exec(`DELETE FROM search.entry_index_state WHERE entry_id=$1`, failID); err != nil {
		t.Fatalf("unable to delete index state: %v", err)
	}
	if _, err := db.Exec(`DELETE FROM search.passages WHERE entry_id=$1`, failID); err != nil {
		t.Fatalf("unable to delete passages: %v", err)
	}
	if _, err := db.Exec(`DELETE FROM entries WHERE id=$1`, failID); err != nil {
		t.Fatalf("unable to delete entry: %v", err)
	}

	// The lane must notice within a handful of sweeps and reach Done --
	// not hang forever the way the un-pruned tracker did.
	waitDone(t, done, 5*time.Second, "Backfill.start")

	if !b.Stats().Done {
		t.Fatal("expected Stats().Done to become true once the failing entry was removed from the pending set entirely")
	}
	if entryStatus(t, db, okID) != "ok" {
		t.Fatal("expected the unrelated healthy entry to still have been indexed")
	}
}
