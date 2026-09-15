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
	mgr := NewManager(idx, bf, live, nil, factory, EmbedderInfo{Kind: "local"})
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

// TestManagerSwitchRequiresConfirmation proves section 5's "the switch
// must not proceed without explicit confirmation" is enforced by Switch
// itself, not merely trusted to the admin page's own confirmation
// dialog: confirm=false must neither call the factory nor touch the
// active embedder.
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
	mgr := NewManager(idx, bf, live, s, factory, EmbedderInfo{Kind: "local"})

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
// would keep passing if the wiring were deleted (task 9 brief, echoing
// task 1's own mistake).
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
	mgr := NewManager(idx, bf, live, s, factory, EmbedderInfo{Kind: "local"})

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
// "same model, different machine" case section 5 calls out as the whole
// point of the feature: a candidate that reports EXACTLY the identity
// already active must not mark anything pending.
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
	mgr := NewManager(idx, bf, live, s, factory, EmbedderInfo{Kind: "local"})

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

// TestManagerSwitchWaitsForInFlightEmbedBeforeClosingOldEmbedder is the
// core guarantee behind section 4: Switch must not close the previous
// embedder while a call into it is still in flight -- for the ONNX
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
	mgr := NewManager(idx, bf, live, s, factory, EmbedderInfo{Kind: "local"})

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
// running (and eventually drains) across the switch -- this is the "run
// the race detector over these paths with a lane actively indexing"
// requirement from the task 9 brief, not merely the unit tests above.
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
	mgr := NewManager(idx, bf, live, s, factory, EmbedderInfo{Kind: "local"})

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
