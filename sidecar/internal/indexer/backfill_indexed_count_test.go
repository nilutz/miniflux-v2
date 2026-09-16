// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package indexer // import "miniflux.app/v2/sidecar/internal/indexer"

import (
	"errors"
	"testing"
)

// TestBackfillStatsReportsDatabaseIndexedCountAfterARestart is the
// deterministic reproduction of defect 6: a real deployment's admin page
// showed "Indexed 0 of 8,797" while the database's Database section, on
// the SAME page, reported 606 passages -- genuine prior work (~46
// entries at the measured ~13 passages/entry) that this run's own
// counters had no way to know about.
//
// Root cause: b.indexed is purely an in-memory atomic counter,
// incremented only when THIS run's own process() actually attempts an
// entry. It is never seeded from the database, so on a freshly
// constructed Backfill -- every process start, including one forced by
// defect 1's crash -- it starts at 0 regardless of how much of the
// corpus a PREVIOUS run already finished. Those already-finished entries
// are never re-offered by PendingEntryIDs (correctly -- they are not
// pending), so process() is never called for them again in this run's
// entire lifetime, and b.indexed never catches up. This is the same
// class of bug as defect 4 (Done): an in-memory counter treated as an
// authoritative total when only a database read can actually be one.
//
// This test constructs exactly that snapshot -- a freshly built Backfill
// (b.indexed == 0, as it is immediately after NewBackfill, before
// Start() has attempted anything) paired with a store that (via the
// injectable indexedCountFn) reports entries already genuinely indexed
// from a prior run -- and asserts Stats().Indexed reflects that database
// reality rather than the untouched in-memory zero. It fails against the
// prior Stats() (Indexed was b.indexed.Load() alone) and passes once
// Stats() reconciles it against the database.
func TestBackfillStatsReportsDatabaseIndexedCountAfterARestart(t *testing.T) {
	idx := New(nil, &fakeEmbedder{})
	ctrl := NewController(DefaultControllerConfig())
	b := NewBackfill(idx, ctrl, BackfillConfig{})

	// b.indexed is untouched -- 0, exactly as a freshly restarted process
	// finds it, having attempted nothing yet in its own lifetime.
	b.indexedCountTTL = 0 // force cachedIndexedFromDB to call indexedCountFn, not serve a stale zero-value cache
	b.indexedCountFn = func(afterID int64) (int64, error) { return 46, nil }
	// idx.store is nil in this hermetic test (New(nil, ...)); stub the
	// remaining-count query too so Stats()'s own cachedRemaining call
	// (unrelated to what this test is about) never dereferences it.
	b.remainingCountFn = func(afterID int64) (int64, error) { return 0, nil }

	st := b.Stats()
	if st.Indexed != 46 {
		t.Fatalf("Stats().Indexed = %d, want 46 -- the database's own count of already-indexed entries must not be hidden behind a fresh process's untouched in-memory counter", st.Indexed)
	}
}

// TestBackfillStatsIndexedNeverGoesBackwardsAsThisRunMakesProgress proves
// the fix does not regress the ordinary, single-continuous-run case
// every other backfill test in this package depends on: once b.indexed
// (this run's own live counter) exceeds a stale or lagging database
// figure, Stats().Indexed must track the LIVE counter, not fall back to
// the smaller cached one.
func TestBackfillStatsIndexedNeverGoesBackwardsAsThisRunMakesProgress(t *testing.T) {
	idx := New(nil, &fakeEmbedder{})
	ctrl := NewController(DefaultControllerConfig())
	b := NewBackfill(idx, ctrl, BackfillConfig{})

	b.indexedCountTTL = 0
	b.indexedCountFn = func(afterID int64) (int64, error) { return 5, nil }
	b.remainingCountFn = func(afterID int64) (int64, error) { return 0, nil }

	// This run's own live counter has already outpaced the (possibly
	// stale) database figure -- simulating process() having actually
	// indexed 10 entries since this run started.
	b.indexed.Store(10)

	st := b.Stats()
	if st.Indexed != 10 {
		t.Fatalf("Stats().Indexed = %d, want 10 -- the live, currently-running count must win once it exceeds the (possibly lagging) database figure", st.Indexed)
	}
}

// TestBackfillStatsIndexedFallsBackToLiveCounterWhenTheCountQueryFails
// covers cachedIndexedFromDB's own -1 ("no reading available") case:
// max(liveCounter, -1) must always resolve to the live counter, with no
// special-casing needed at the Stats() call site.
func TestBackfillStatsIndexedFallsBackToLiveCounterWhenTheCountQueryFails(t *testing.T) {
	idx := New(nil, &fakeEmbedder{})
	ctrl := NewController(DefaultControllerConfig())
	b := NewBackfill(idx, ctrl, BackfillConfig{})

	b.indexedCountTTL = 0
	b.indexedCountFn = func(afterID int64) (int64, error) { return 0, errors.New("simulated IndexedEntryCount failure") }
	b.remainingCountFn = func(afterID int64) (int64, error) { return 0, nil }
	b.indexed.Store(3)

	st := b.Stats()
	if st.Indexed != 3 {
		t.Fatalf("Stats().Indexed = %d, want 3 (the live counter) when the database count fails outright", st.Indexed)
	}
}
