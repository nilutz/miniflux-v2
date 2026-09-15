// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package indexer // import "miniflux.app/v2/sidecar/internal/indexer"

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"miniflux.app/v2/sidecar/internal/embed"
)

// switchBlockingEmbedder is a deterministic stand-in for an Embedder whose
// single EmbedDocuments call blocks until the test unblocks it, closing
// blocked right before it starts waiting so the test can synchronise on
// "this call is now in flight" with no timing race -- the same technique
// backfill_test.go's blockOnMarkerEmbedder uses, trimmed to one call.
// closed records whether Close was ever invoked, and when, relative to
// unblock -- the whole point of
// TestManagerSwitchWaitsForInFlightEmbedBeforeClosingOldEmbedder.
type switchBlockingEmbedder struct {
	identity string
	blocked  chan struct{}
	unblock  chan struct{}
	closed   atomic.Bool
}

func (e *switchBlockingEmbedder) EmbedDocuments(ctx context.Context, texts []string) ([][]float32, error) {
	close(e.blocked)
	select {
	case <-e.unblock:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = make([]float32, 768)
	}
	return out, nil
}

func (e *switchBlockingEmbedder) EmbedQuery(context.Context, string) ([]float32, error) {
	panic("switchBlockingEmbedder: EmbedQuery should never be called by internal/indexer")
}

func (e *switchBlockingEmbedder) Dimensions() int  { return 768 }
func (e *switchBlockingEmbedder) Identity() string { return e.identity }
func (e *switchBlockingEmbedder) Close() error     { e.closed.Store(true); return nil }

// closeTrackingEmbedder never blocks -- it just records whether/when
// Close was called, for tests that need to assert a candidate was (or,
// critically, was NOT) closed at a specific point without also needing
// it to block an in-flight call.
type closeTrackingEmbedder struct {
	identity string
	closed   atomic.Bool
}

func (e *closeTrackingEmbedder) EmbedDocuments(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = make([]float32, 384)
	}
	return out, nil
}

func (e *closeTrackingEmbedder) EmbedQuery(context.Context, string) ([]float32, error) {
	panic("closeTrackingEmbedder: EmbedQuery should never be called by internal/indexer")
}

func (e *closeTrackingEmbedder) Dimensions() int  { return 384 }
func (e *closeTrackingEmbedder) Identity() string { return e.identity }
func (e *closeTrackingEmbedder) Close() error     { e.closed.Store(true); return nil }

// fakeFactory returns an EmbedderFactory that always returns e/backend/err
// regardless of what kind/url/timeout it is called with -- these tests are
// about Manager's own orchestration, not about which kind was requested.
func fakeFactory(e embed.Embedder, backend string, err error) EmbedderFactory {
	return func(context.Context, string, string, time.Duration) (embed.Embedder, string, error) {
		return e, backend, err
	}
}

// newHermeticManager builds a Manager over an in-memory Indexer/Backfill/
// LiveMonitor -- no store, no database -- for tests that only care about
// Manager's own orchestration (confirmation, probing, pause/resume), not
// about persistence or the real content_hash mechanism.
func newHermeticManager(t *testing.T, current embed.Embedder, factory EmbedderFactory) (*Manager, *Indexer, *Backfill, *LiveMonitor) {
	t.Helper()
	idx := New(nil, current)
	ctrl := NewController(DefaultControllerConfig())
	bf := NewBackfill(idx, ctrl, BackfillConfig{})
	live := NewLiveMonitor()
	mgr := NewManager(idx, bf, live, nil, factory, EmbedderInfo{Kind: "local"}, nil)
	return mgr, idx, bf, live
}

// TestManagerPreviewRejectsDimensionMismatchAndSwapsNothing proves the
// probe/preview path refuses a candidate whose dimensions do not match
// the fixed schema width -- the exact error internal/embed/remote.New
// itself produces for this case -- and, critically, that nothing was
// swapped: the active embedder's identity is unchanged.
func TestManagerPreviewRejectsDimensionMismatchAndSwapsNothing(t *testing.T) {
	factory := fakeFactory(nil, "", fmt.Errorf(
		"embed/remote: remote http://bad-dims:9000/embed reports 768 dimensions, but search.passages requires 384 — a dimension change needs an explicit, operator-initiated re-index, not a config swap (spec §13.1)",
	))
	mgr, idx, _, _ := newHermeticManager(t, &distinctIdentityEmbedder{identity: testModelIdentity}, factory)

	preview := mgr.Preview(context.Background(), "remote", "http://bad-dims:9000")
	if preview.Error == "" {
		t.Fatalf("expected Preview to report an error for a dimension mismatch, got a clean result: %+v", preview)
	}
	if !strings.Contains(preview.Error, "384") || !strings.Contains(preview.Error, "768") {
		t.Fatalf("expected the error to mention both dimensions, got %q", preview.Error)
	}
	if preview.Reachable {
		t.Fatalf("expected Reachable=false for a refused candidate")
	}

	if got := idx.Embedder().Identity(); got != testModelIdentity {
		t.Fatalf("expected the active embedder to be unchanged after a rejected preview, got identity %q, want %q", got, testModelIdentity)
	}
}

// TestManagerPreviewAgainstUnreachableURLReportsUnreachableAndSwapsNothing
// proves the same refuse-and-swap-nothing contract for an unreachable
// remote (a connection failure, not a dimension mismatch).
func TestManagerPreviewAgainstUnreachableURLReportsUnreachableAndSwapsNothing(t *testing.T) {
	factory := fakeFactory(nil, "", fmt.Errorf("embed/remote: probe http://unreachable:9000/embed: request failed: dial tcp: connection refused"))
	mgr, idx, _, _ := newHermeticManager(t, &distinctIdentityEmbedder{identity: testModelIdentity}, factory)

	preview := mgr.Preview(context.Background(), "remote", "http://unreachable:9000")
	if preview.Error == "" {
		t.Fatalf("expected Preview to report an error for an unreachable remote, got a clean result: %+v", preview)
	}
	if preview.Reachable {
		t.Fatalf("expected Reachable=false for an unreachable remote")
	}

	if got := idx.Embedder().Identity(); got != testModelIdentity {
		t.Fatalf("expected the active embedder to be unchanged after a failed probe, got identity %q, want %q", got, testModelIdentity)
	}
}

// TestManagerSwitchRequiresConfirmation proves that "the switch must not
// proceed without explicit confirmation" is enforced by Switch itself,
// not merely trusted to the admin page's own confirmation dialog:
// confirm=false must neither call the factory nor touch the active
// embedder.
func TestManagerSwitchRequiresConfirmation(t *testing.T) {
	called := false
	factory := func(context.Context, string, string, time.Duration) (embed.Embedder, string, error) {
		called = true
		return &distinctIdentityEmbedder{identity: "model-b@rev1#384"}, "", nil
	}
	mgr, idx, _, _ := newHermeticManager(t, &distinctIdentityEmbedder{identity: testModelIdentity}, factory)

	_, err := mgr.Switch(context.Background(), "remote", "http://host:9000", false)
	if !errors.Is(err, ErrSwitchNotConfirmed) {
		t.Fatalf("expected ErrSwitchNotConfirmed, got %v", err)
	}
	if called {
		t.Fatalf("expected the factory not to be called at all without confirmation")
	}
	if got := idx.Embedder().Identity(); got != testModelIdentity {
		t.Fatalf("expected the active embedder unchanged without confirmation, got %q", got)
	}
}

// TestManagerSwitchResumesBothLanesAfterward proves both lanes are
// resumed once a switch completes -- Stats().Paused must be false on
// each, not merely "the operator never called Resume manually". It uses
// a real store (rather than newHermeticManager's nil one): Backfill.Stats
// reads store.PendingEntryCountApprox to fill in Remaining.
func TestManagerSwitchResumesBothLanesAfterward(t *testing.T) {
	s, _ := testEnv(t)

	idx := New(s, &distinctIdentityEmbedder{identity: testModelIdentity})
	ctrl := NewController(DefaultControllerConfig())
	bf := NewBackfill(idx, ctrl, BackfillConfig{})
	live := NewLiveMonitor()
	factory := fakeFactory(&distinctIdentityEmbedder{identity: "model-b@rev1#384"}, "", nil)
	mgr := NewManager(idx, bf, live, s, factory, EmbedderInfo{Kind: "local"}, nil)

	if _, err := mgr.Switch(context.Background(), "remote", "http://host:9000", true); err != nil {
		t.Fatalf("Switch failed: %v", err)
	}
	if bf.Stats().Paused {
		t.Fatalf("expected the backfill lane to be resumed after a switch")
	}
	if live.Stats().Paused {
		t.Fatalf("expected the live lane to be resumed after a switch")
	}
}

// TestManagerSwitchRecordsNewModelIdentityAndMarksCorpusPending is the
// DB-backed proof that a successful switch actually calls
// store.SetModelIdentity with the NEW identity -- asserted through the
// exported store.PendingEntryIDs/PendingEntryCount, never by reading the
// unexported modelIdentity var directly, which is exactly the guard that
// would keep passing if the wiring were deleted.
func TestManagerSwitchRecordsNewModelIdentityAndMarksCorpusPending(t *testing.T) {
	s, db := testEnv(t)

	entryID := createTestEntry(t, db, "mgr-switch-identity", "<p>Content for the switch-identity test.</p>")
	afterID := entryID - 1

	idx := New(s, &distinctIdentityEmbedder{identity: "model-a@rev1#384"})
	if err := idx.IndexEntry(context.Background(), entryID); err != nil {
		t.Fatalf("IndexEntry under model A failed: %v", err)
	}
	if entryStatus(t, db, entryID) != "ok" {
		t.Fatalf("expected entry #%d to be 'ok' after indexing under model A", entryID)
	}

	idsBefore, err := s.PendingEntryIDs(afterID, 100)
	if err != nil {
		t.Fatalf("PendingEntryIDs failed: %v", err)
	}
	for _, id := range idsBefore {
		if id == entryID {
			t.Fatalf("entry #%d should not be pending right after indexing", entryID)
		}
	}

	ctrl := NewController(DefaultControllerConfig())
	bf := NewBackfill(idx, ctrl, BackfillConfig{})
	live := NewLiveMonitor()
	factory := fakeFactory(&distinctIdentityEmbedder{identity: "model-b@rev1#384"}, "", nil)
	mgr := NewManager(idx, bf, live, s, factory, EmbedderInfo{Kind: "local"}, nil)

	result, err := mgr.Switch(context.Background(), "remote", "http://host:9000", true)
	if err != nil {
		t.Fatalf("Switch failed: %v", err)
	}
	if result.NewIdentity != "model-b@rev1#384" {
		t.Fatalf("expected NewIdentity %q, got %q", "model-b@rev1#384", result.NewIdentity)
	}
	if result.SameIdentity {
		t.Fatalf("expected SameIdentity=false for a genuinely different model")
	}

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
		t.Fatalf("expected entry #%d to be pending after switching to a genuinely different model, got %v", entryID, idsAfter)
	}

	count, err := s.PendingEntryCount(afterID)
	if err != nil {
		t.Fatalf("PendingEntryCount failed: %v", err)
	}
	if count < 1 {
		t.Fatalf("expected PendingEntryCount >= 1 after switching models, got %d", count)
	}
}

// TestManagerSwitchToIdenticalIdentityLeavesEntriesNotPending is the
// "same model, different machine" case -- the whole point of the
// feature: a candidate that reports EXACTLY the identity already active
// must not mark anything pending.
func TestManagerSwitchToIdenticalIdentityLeavesEntriesNotPending(t *testing.T) {
	s, db := testEnv(t)

	const sharedIdentity = "model-a@rev1#384"
	entryID := createTestEntry(t, db, "mgr-switch-same-identity", "<p>Content for the same-identity test.</p>")
	afterID := entryID - 1

	idx := New(s, &distinctIdentityEmbedder{identity: sharedIdentity})
	if err := idx.IndexEntry(context.Background(), entryID); err != nil {
		t.Fatalf("IndexEntry failed: %v", err)
	}

	ctrl := NewController(DefaultControllerConfig())
	bf := NewBackfill(idx, ctrl, BackfillConfig{})
	live := NewLiveMonitor()
	// A DIFFERENT embedder object/instance, but reporting the identical
	// identity string -- exactly the "same model on another machine"
	// case: a different Go value, a different (simulated) host, the same
	// model.
	factory := fakeFactory(&distinctIdentityEmbedder{identity: sharedIdentity}, "", nil)
	mgr := NewManager(idx, bf, live, s, factory, EmbedderInfo{Kind: "local"}, nil)

	result, err := mgr.Switch(context.Background(), "remote", "http://host:9000", true)
	if err != nil {
		t.Fatalf("Switch failed: %v", err)
	}
	if !result.SameIdentity {
		t.Fatalf("expected SameIdentity=true when the candidate reports the identical identity")
	}

	ids, err := s.PendingEntryIDs(afterID, 100)
	if err != nil {
		t.Fatalf("PendingEntryIDs failed: %v", err)
	}
	for _, id := range ids {
		if id == entryID {
			t.Fatalf("entry #%d should NOT be pending after switching to an embedder with an identical identity", entryID)
		}
	}
}

// TestManagerSwitchWaitsForInFlightEmbedBeforeClosingOldEmbedder is a
// core guarantee: Switch must not close the previous embedder while a
// call into it is still in flight -- for the ONNX
// backend that is a segfault, not an error. This drives a real,
// deterministically-blocked IndexEntry call and proves, in order: Switch
// does not return while the call is still blocked, the old embedder is
// not closed while blocked, and both complete correctly once the call is
// allowed to finish.
func TestManagerSwitchWaitsForInFlightEmbedBeforeClosingOldEmbedder(t *testing.T) {
	s, db := testEnv(t)
	entryID := createTestEntry(t, db, "mgr-switch-blocks", "<p>Content that will embed while a switch is requested.</p>")

	oldEmbedder := &switchBlockingEmbedder{
		identity: "model-a@rev1#384",
		blocked:  make(chan struct{}),
		unblock:  make(chan struct{}),
	}
	idx := New(s, oldEmbedder)

	ctrl := NewController(DefaultControllerConfig())
	bf := NewBackfill(idx, ctrl, BackfillConfig{})
	live := NewLiveMonitor()

	indexDone := make(chan error, 1)
	go func() { indexDone <- idx.IndexEntry(context.Background(), entryID) }()

	select {
	case <-oldEmbedder.blocked:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for the in-flight embed call to start")
	}

	newEmbedder := &distinctIdentityEmbedder{identity: "model-b@rev1#384"}
	factory := fakeFactory(newEmbedder, "", nil)
	mgr := NewManager(idx, bf, live, s, factory, EmbedderInfo{Kind: "local"}, nil)

	switchDone := make(chan error, 1)
	go func() {
		_, err := mgr.Switch(context.Background(), "remote", "http://host:9000", true)
		switchDone <- err
	}()

	select {
	case err := <-switchDone:
		t.Fatalf("Switch returned (err=%v) before the in-flight embed call finished -- it must wait for it", err)
	case <-time.After(200 * time.Millisecond):
	}
	if oldEmbedder.closed.Load() {
		t.Fatalf("the old embedder was closed while a call into it was still in flight -- a segfault for the ONNX backend, not an error")
	}

	close(oldEmbedder.unblock)

	select {
	case err := <-switchDone:
		if err != nil {
			t.Fatalf("Switch failed: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("Switch did not complete after the in-flight embed call was unblocked")
	}
	if !oldEmbedder.closed.Load() {
		t.Fatalf("expected the old embedder to be closed once Switch completed")
	}
	if got := idx.Embedder().Identity(); got != "model-b@rev1#384" {
		t.Fatalf("expected the active embedder to be the new one after Switch, got %q", got)
	}

	select {
	case err := <-indexDone:
		if err != nil {
			t.Fatalf("IndexEntry failed: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("IndexEntry did not complete")
	}
}

// TestManagerSwitchIsRaceFreeWithBackfillLaneActivelyIndexing runs a real
// Backfill lane, with a real worker pool concurrently calling
// IndexEntry (and therefore embedPassages, which reads idx.embedder
// under embedderMu), while Manager.Switch concurrently replaces it. It
// asserts no data race under `go test -race` and that the lane keeps
// running (and eventually drains) across the switch -- this is "run
// the race detector over these paths with a lane actively indexing",
// not merely the unit tests above.
func TestManagerSwitchIsRaceFreeWithBackfillLaneActivelyIndexing(t *testing.T) {
	s, db := testEnv(t)

	const entryCount = 40
	for i := 0; i < entryCount; i++ {
		createTestEntry(t, db, fmt.Sprintf("mgr-switch-race-%d", i), "<p>Content for the race test.</p>")
	}

	idx := New(s, &countingEmbedder{counts: newMarkerCallCounts()})
	ctrl := NewController(ControllerConfig{MinWorkers: 2, MaxWorkers: 2, LatencyMargin: 1.5, LoadThreshold: 8.0})
	bf := NewBackfill(idx, ctrl, BackfillConfig{PageSize: 5, PollInterval: 10 * time.Millisecond})
	live := NewLiveMonitor()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	backfillDone := make(chan error, 1)
	go func() { backfillDone <- bf.Start(ctx) }()

	// Let the lane get genuinely into flight before switching -- not load
	// bearing for correctness (SetEmbedder is safe whenever it is called),
	// only for making the test actually exercise concurrent embedding
	// rather than switching before the lane has started anything.
	time.Sleep(30 * time.Millisecond)

	newEmbedder := &countingEmbedder{counts: newMarkerCallCounts()}
	factory := fakeFactory(newEmbedder, "", nil)
	mgr := NewManager(idx, bf, live, s, factory, EmbedderInfo{Kind: "local"}, nil)

	if _, err := mgr.Switch(context.Background(), "remote", "http://host:9000", true); err != nil {
		t.Fatalf("Switch failed: %v", err)
	}
	if got := idx.Embedder().Identity(); got != testModelIdentity {
		// countingEmbedder's Identity() is testModelIdentity regardless of
		// which instance is active -- both old and new share it, by this
		// package's own convention (see testModelIdentity's doc comment)
		// -- so this just proves SetEmbedder actually ran without error.
		t.Fatalf("expected the active embedder's identity to still be %q after switching between two countingEmbedders, got %q", testModelIdentity, got)
	}

	cancel()
	select {
	case err := <-backfillDone:
		if err != nil {
			t.Fatalf("Backfill.Start returned an error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("Backfill.Start did not stop after cancellation")
	}
}

// TestManagerSwitchAbandonedByCancelledContextNeverInstallsAClosedEmbedder
// is the deterministic reproduction of a bug where a prior version of
// Switch closed its candidate embedder itself on the
// ctx-cancelled/timeout escape hatch, racing the still-running background
// goroutine's own, entirely separate attempt to install that exact
// value -- so the goroutine could go on to publish an ALREADY-CLOSED
// embedder as idx.embedder once the in-flight call it was waiting on
// released the lock. Every subsequent embedder call would then run
// against a closed session -- a segfault for the ONNX backend, not an
// error.
//
// This blocks an in-flight embed to hold embedderMu's read lock, calls
// Switch with an ALREADY-cancelled context (so it gives up waiting
// immediately, well before the block is released), releases the block,
// and asserts: the candidate is not closed while it could still be
// installed; once the background goroutine has had time to observe the
// abandonment and dispose of the candidate, the ACTIVE embedder is still
// the original, open one -- never the abandoned, now-closed candidate.
func TestManagerSwitchAbandonedByCancelledContextNeverInstallsAClosedEmbedder(t *testing.T) {
	s, db := testEnv(t)
	entryID := createTestEntry(t, db, "mgr-switch-abandoned",
		"<p>Content that will embed while a switch is abandoned mid-flight.</p>")

	oldEmbedder := &switchBlockingEmbedder{
		identity: "model-a@rev1#384",
		blocked:  make(chan struct{}),
		unblock:  make(chan struct{}),
	}
	idx := New(s, oldEmbedder)

	ctrl := NewController(DefaultControllerConfig())
	bf := NewBackfill(idx, ctrl, BackfillConfig{})
	live := NewLiveMonitor()

	indexDone := make(chan error, 1)
	go func() { indexDone <- idx.IndexEntry(context.Background(), entryID) }()

	select {
	case <-oldEmbedder.blocked:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for the in-flight embed call to start")
	}

	newEmbedder := &closeTrackingEmbedder{identity: "model-b@rev1#384"}
	factory := fakeFactory(newEmbedder, "", nil)
	mgr := NewManager(idx, bf, live, s, factory, EmbedderInfo{Kind: "local"}, nil)

	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled before Switch is even called

	switchDone := make(chan error, 1)
	go func() {
		_, err := mgr.Switch(cancelledCtx, "remote", "http://host:9000", true)
		switchDone <- err
	}()

	select {
	case err := <-switchDone:
		if err == nil {
			t.Fatalf("expected Switch to report an error for an already-cancelled context")
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("Switch did not return promptly for an already-cancelled context")
	}

	// The in-flight embed is still blocked, so the background goroutine
	// cannot yet have decided anything -- the candidate must not have
	// been closed while it could still, in principle, be installed.
	//
	// Deliberately NOT calling idx.Embedder() here to also check the
	// active identity: embedderMu.RLock (Embedder) versus Lock
	// (SetEmbedderIfNotAbandoned, pending in the background goroutine
	// right now) follow sync.RWMutex's own writer-preference rule -- once
	// a Lock() is pending, a NEW RLock() blocks until it is granted -- so
	// calling Embedder() at this exact point would itself block until
	// oldEmbedder.unblock is closed below, deadlocking this test against
	// itself. That is the mutex working correctly, not a bug; the
	// post-unblock assertion after waitFor below covers the same ground
	// safely.
	if newEmbedder.closed.Load() {
		t.Fatalf("candidate embedder was closed before the abandonment decision could be made safely")
	}

	close(oldEmbedder.unblock)

	waitFor(t, 2*time.Second, "candidate embedder closed after abandonment", func() bool {
		return newEmbedder.closed.Load()
	})

	// The critical assertion: the active embedder is still the ORIGINAL,
	// open one -- never the abandoned, now-closed candidate.
	if got := idx.Embedder().Identity(); got != "model-a@rev1#384" {
		t.Fatalf("expected the active embedder to remain the original one after an abandoned switch, got %q -- the closed candidate must never be installed", got)
	}

	select {
	case err := <-indexDone:
		if err != nil {
			t.Fatalf("IndexEntry failed: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("IndexEntry did not complete")
	}
}

// TestManagerSwitchRejectsConcurrentSwitchWhileOneIsInFlight verifies: a
// second Switch call while one is already running must be rejected with
// ErrSwitchInProgress rather than interleave with
// it, and the guard must stay held until the FIRST switch's background
// work has genuinely finished -- not merely until its own Switch call
// returned -- so a third attempt, made only after the first truly
// completes, must succeed.
func TestManagerSwitchRejectsConcurrentSwitchWhileOneIsInFlight(t *testing.T) {
	s, db := testEnv(t)
	entryID := createTestEntry(t, db, "mgr-switch-reentrant",
		"<p>Content that will embed while a second switch is attempted.</p>")

	oldEmbedder := &switchBlockingEmbedder{
		identity: "model-a@rev1#384",
		blocked:  make(chan struct{}),
		unblock:  make(chan struct{}),
	}
	idx := New(s, oldEmbedder)

	ctrl := NewController(DefaultControllerConfig())
	bf := NewBackfill(idx, ctrl, BackfillConfig{})
	live := NewLiveMonitor()

	indexDone := make(chan error, 1)
	go func() { indexDone <- idx.IndexEntry(context.Background(), entryID) }()

	select {
	case <-oldEmbedder.blocked:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for the in-flight embed call to start")
	}

	var counter atomic.Int64
	factory := func(context.Context, string, string, time.Duration) (embed.Embedder, string, error) {
		n := counter.Add(1)
		return &distinctIdentityEmbedder{identity: fmt.Sprintf("model-%d@rev1#384", n)}, "", nil
	}
	mgr := NewManager(idx, bf, live, s, factory, EmbedderInfo{Kind: "local"}, nil)

	firstDone := make(chan error, 1)
	go func() {
		_, err := mgr.Switch(context.Background(), "remote", "http://first:9000", true)
		firstDone <- err
	}()

	// Give the first Switch time to actually TryLock switchMu and reach
	// the point of waiting on the in-flight (still blocked) embed.
	time.Sleep(150 * time.Millisecond)
	select {
	case err := <-firstDone:
		t.Fatalf("the first Switch returned early (err=%v) -- it should still be blocked on the in-flight embed", err)
	default:
	}

	if _, err := mgr.Switch(context.Background(), "remote", "http://second:9000", true); !errors.Is(err, ErrSwitchInProgress) {
		t.Fatalf("expected ErrSwitchInProgress for a concurrent switch, got %v", err)
	}

	close(oldEmbedder.unblock)

	select {
	case err := <-firstDone:
		if err != nil {
			t.Fatalf("first Switch failed: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("first Switch did not complete")
	}

	// The guard must be released once the first switch is FULLY done --
	// a third switch, attempted only now, must succeed.
	if _, err := mgr.Switch(context.Background(), "remote", "http://third:9000", true); err != nil {
		t.Fatalf("expected a switch after the first completed to succeed, got %v", err)
	}

	select {
	case err := <-indexDone:
		if err != nil {
			t.Fatalf("IndexEntry failed: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("IndexEntry did not complete")
	}
}

// fakeQueryCacheInvalidator is a hermetic stand-in for *search.Searcher's
// ClearQueryCache, recording whether (and how many times) it was called.
type fakeQueryCacheInvalidator struct {
	calls atomic.Int64
}

func (f *fakeQueryCacheInvalidator) ClearQueryCache() { f.calls.Add(1) }

// TestManagerSwitchClearsQueryCacheOnlyOnSuccessfulInstall verifies: a
// successful switch must invalidate the query cache exactly
// once; a REFUSED switch (no confirmation) and an ABANDONED one (context
// cancelled before installation) must not touch it at all -- a spurious
// clear on a switch that never actually happened would just be a cache
// hit for a search request, not a correctness bug, but asserting it stays
// untouched keeps the test honest about what the guard actually covers.
func TestManagerSwitchClearsQueryCacheOnlyOnSuccessfulInstall(t *testing.T) {
	s, _ := testEnv(t)

	idx := New(s, &distinctIdentityEmbedder{identity: testModelIdentity})
	ctrl := NewController(DefaultControllerConfig())
	bf := NewBackfill(idx, ctrl, BackfillConfig{})
	live := NewLiveMonitor()

	invalidator := &fakeQueryCacheInvalidator{}
	factory := fakeFactory(&distinctIdentityEmbedder{identity: "model-b@rev1#384"}, "", nil)
	mgr := NewManager(idx, bf, live, s, factory, EmbedderInfo{Kind: "local"}, invalidator)

	// Refused: no confirmation.
	if _, err := mgr.Switch(context.Background(), "remote", "http://host:9000", false); !errors.Is(err, ErrSwitchNotConfirmed) {
		t.Fatalf("expected ErrSwitchNotConfirmed, got %v", err)
	}
	if invalidator.calls.Load() != 0 {
		t.Fatalf("expected the query cache to be untouched by a refused switch, got %d calls", invalidator.calls.Load())
	}

	// Successful.
	if _, err := mgr.Switch(context.Background(), "remote", "http://host:9000", true); err != nil {
		t.Fatalf("Switch failed: %v", err)
	}
	if got := invalidator.calls.Load(); got != 1 {
		t.Fatalf("expected exactly one ClearQueryCache call after a successful switch, got %d", got)
	}
}

// TestManagerSwitchGuardStaysHeldThroughAnAbandonedSwitchsBackgroundWork
// is the specific gap TestManagerSwitchRejectsConcurrentSwitchWhileOneIsInFlight
// does not cover: that test's first Switch call never returns early (its
// context never cancels), so it never exercises whether the reentrancy
// guard is released too early on the ABANDON path specifically. This
// does: the first Switch is called with an ALREADY-cancelled context
// while an embed is in flight, so it returns almost immediately with an
// error -- but its background installEmbedder goroutine is still
// pending on embedderMu's write lock. A second Switch attempted right
// after that first call returns must STILL be rejected with
// ErrSwitchInProgress, because the first switch's background work has
// not actually finished yet; only once the in-flight embed is released
// and installEmbedder finishes must a retried Switch succeed.
func TestManagerSwitchGuardStaysHeldThroughAnAbandonedSwitchsBackgroundWork(t *testing.T) {
	s, db := testEnv(t)
	entryID := createTestEntry(t, db, "mgr-switch-guard-abandoned",
		"<p>Content that will embed while an abandoned switch's guard is checked.</p>")

	oldEmbedder := &switchBlockingEmbedder{
		identity: "model-a@rev1#384",
		blocked:  make(chan struct{}),
		unblock:  make(chan struct{}),
	}
	idx := New(s, oldEmbedder)

	ctrl := NewController(DefaultControllerConfig())
	bf := NewBackfill(idx, ctrl, BackfillConfig{})
	live := NewLiveMonitor()

	indexDone := make(chan error, 1)
	go func() { indexDone <- idx.IndexEntry(context.Background(), entryID) }()

	select {
	case <-oldEmbedder.blocked:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for the in-flight embed call to start")
	}

	var counter atomic.Int64
	factory := func(context.Context, string, string, time.Duration) (embed.Embedder, string, error) {
		n := counter.Add(1)
		return &distinctIdentityEmbedder{identity: fmt.Sprintf("model-%d@rev1#384", n)}, "", nil
	}
	mgr := NewManager(idx, bf, live, s, factory, EmbedderInfo{Kind: "local"}, nil)

	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := mgr.Switch(cancelledCtx, "remote", "http://first:9000", true); err == nil {
		t.Fatalf("expected the first (already-cancelled) Switch to report an error")
	}

	// The first switch's own call returned, but its background
	// installEmbedder is still blocked on the in-flight embed -- a
	// second switch attempted right now must still be rejected.
	if _, err := mgr.Switch(context.Background(), "remote", "http://second:9000", true); !errors.Is(err, ErrSwitchInProgress) {
		t.Fatalf("expected ErrSwitchInProgress while the first switch's background work is still pending, got %v", err)
	}

	close(oldEmbedder.unblock)

	// Once the in-flight embed releases and the first switch's
	// background work genuinely finishes, a retried switch must succeed.
	waitFor(t, 2*time.Second, "a switch to succeed once the guard is released", func() bool {
		_, err := mgr.Switch(context.Background(), "remote", "http://third:9000", true)
		return err == nil
	})

	select {
	case err := <-indexDone:
		if err != nil {
			t.Fatalf("IndexEntry failed: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("IndexEntry did not complete")
	}
}

// TestManagerInfoCachesReachabilityWithinTTL verifies: GET / and GET
// /api/embedder must not each trigger a fresh
// network probe against the configured remote on every call -- a
// repeated Info() call within reachabilityCacheTTL must reuse the
// memoised result rather than calling the factory again.
func TestManagerInfoCachesReachabilityWithinTTL(t *testing.T) {
	orig := reachabilityCacheTTL
	reachabilityCacheTTL = 200 * time.Millisecond
	t.Cleanup(func() { reachabilityCacheTTL = orig })

	var probeCalls atomic.Int64
	factory := func(context.Context, string, string, time.Duration) (embed.Embedder, string, error) {
		probeCalls.Add(1)
		return &closeTrackingEmbedder{identity: "remote-model@rev#384"}, "", nil
	}
	mgr, _, _, _ := newHermeticManager(t, &distinctIdentityEmbedder{identity: "remote-model@rev#384"}, factory)
	// newHermeticManager's Manager.info defaults to Kind "local" -- set it
	// to "remote" with a URL directly, mirroring what a successful Switch
	// would have recorded, since Info only probes for Kind=="remote".
	mgr.mu.Lock()
	mgr.info = EmbedderInfo{Kind: "remote", RemoteURL: "http://gpu-host:9000"}
	mgr.mu.Unlock()

	ctx := context.Background()
	first := mgr.Info(ctx)
	if !first.Reachable {
		t.Fatalf("expected the first Info() call to report reachable, got %+v", first)
	}
	if got := probeCalls.Load(); got != 1 {
		t.Fatalf("expected exactly one probe call after the first Info(), got %d", got)
	}

	second := mgr.Info(ctx)
	if !second.Reachable {
		t.Fatalf("expected the second Info() call to still report reachable, got %+v", second)
	}
	if got := probeCalls.Load(); got != 1 {
		t.Fatalf("expected the second Info() within the TTL to reuse the cached probe, got %d calls", got)
	}

	time.Sleep(3 * reachabilityCacheTTL)

	third := mgr.Info(ctx)
	if !third.Reachable {
		t.Fatalf("expected the third Info() call to still report reachable, got %+v", third)
	}
	if got := probeCalls.Load(); got != 2 {
		t.Fatalf("expected a fresh probe once the TTL elapsed, got %d calls", got)
	}
}
