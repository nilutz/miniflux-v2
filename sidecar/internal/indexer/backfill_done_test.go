// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package indexer // import "miniflux.app/v2/sidecar/internal/indexer"

import (
	"errors"
	"testing"
)

// TestBackfillStatsNeverReportsDoneWhilePendingCountIsNonZero is the
// deterministic reproduction of defect 4: a real deployment's admin page
// showed "Done: yes" while the database held 8,815 genuinely pending
// entries.
//
// Root cause: b.done (see Stats' own doc comment) is a LATCH the sweep
// loop in start() sets once, the instant a sweep's last page comes back
// empty with nothing outstanding -- correct at that exact moment, but
// never re-checked again until the next sweep, up to IdleResweepInterval
// (15 minutes in production) later. A sweep that legitimately drains a
// small corpus and then sees it grow -- a newly subscribed feed, an OPML
// import, a bulk backfill CLI inserting rows below the live lane's own
// cursor, all ordinary events this package's own comments already call
// out elsewhere -- leaves that latch stuck true for the whole idle
// window, even though Remaining is refreshed far more often (its own 45s
// TTL) and already knows better. This is not a store bug:
// PendingEntryIDs and PendingEntryCount/PendingEntryCountApprox share one
// predicate. It is also not defect 1's crash leaving a stale done from a
// previous process: b.done is unconditionally reset to false at the top
// of every start() call, which runs exactly once per process, and this
// test never calls start() at all -- it reproduces the latch going stale
// entirely through Stats() itself, the only moving part the fix touches.
//
// This test constructs the exact snapshot the Pi showed -- a lane whose
// sweep loop has latched done=true, with a store that (via the injectable
// remainingCountFn, the same seam TestBackfillStatsRefreshesRemainingOnceUnderConcurrency
// uses) reports a large, confidently nonzero pending count -- and asserts
// the invariant the brief names: Stats().Done must never be true while
// Stats().Remaining is nonzero. It fails against the prior Stats()
// (Done was a bare pass-through of b.done, never cross-checked against
// Remaining) and passes once Stats() refuses to report Done true in the
// same snapshot as a confidently nonzero Remaining.
func TestBackfillStatsNeverReportsDoneWhilePendingCountIsNonZero(t *testing.T) {
	idx := New(nil, &fakeEmbedder{})
	ctrl := NewController(DefaultControllerConfig())
	b := NewBackfill(idx, ctrl, BackfillConfig{})

	// Simulate the sweep loop's own latch, stuck true from an earlier,
	// genuinely-drained sweep -- exactly what start() leaves behind once
	// it sets Done and goes on to idle for IdleResweepInterval.
	b.done = true

	// Simulate the corpus having grown since that sweep: 8,815 entries
	// now genuinely pending, the exact figure from the Pi's report,
	// reported through the same seam Stats() itself refreshes from.
	b.remainingCountTTL = 0 // force cachedRemaining to call remainingCountFn, not serve a stale zero-value cache
	b.remainingCountFn = func(afterID int64) (int64, error) { return 8815, nil }

	st := b.Stats()
	if st.Remaining != 8815 {
		t.Fatalf("expected Stats().Remaining == 8815, got %d", st.Remaining)
	}
	if st.Done {
		t.Fatalf("Stats().Done was true while Stats().Remaining was %d -- Done must never be true while entries are genuinely pending", st.Remaining)
	}
}

// TestBackfillStatsLeavesDoneAloneWhenRemainingIsUnknown covers the other
// side of the fix's own condition: a failed PendingEntryCountApprox call
// (Remaining == -1, "unknown", not "confidently zero") must not be treated
// as license to force Done false -- there is no fresher signal to prefer
// over the sweep's own latch in that case, and forcing it either way would
// be inventing certainty the code does not have.
func TestBackfillStatsLeavesDoneAloneWhenRemainingIsUnknown(t *testing.T) {
	idx := New(nil, &fakeEmbedder{})
	ctrl := NewController(DefaultControllerConfig())
	b := NewBackfill(idx, ctrl, BackfillConfig{})

	b.done = true
	b.remainingCountTTL = 0
	b.remainingCountFn = func(afterID int64) (int64, error) { return 0, errors.New("simulated PendingEntryCountApprox failure") }

	st := b.Stats()
	if st.Remaining != -1 {
		t.Fatalf("expected Stats().Remaining == -1 for a failed count, got %d", st.Remaining)
	}
	if !st.Done {
		t.Fatalf("expected Stats().Done to remain true when Remaining is unknown, got false")
	}
}
