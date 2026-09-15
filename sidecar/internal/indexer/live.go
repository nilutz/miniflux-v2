// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package indexer // import "miniflux.app/v2/sidecar/internal/indexer"

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// livePageSize bounds how many pending entry ids RunLive fetches per tick.
// At a few hundred entries a day (spec §9.1) this is generous headroom for
// a single poll; sustained backlog clearing across the whole corpus is the
// backfill lane's job, not this one's.
const livePageSize = 50

// Retry backoff for entries that fail to index, expressed as multiples of
// the poll interval rather than a fixed wall-clock duration. Spec §10 is
// unconditional and lane-agnostic: "Embedding model error -> status =
// 'failed' with a reason; retried on the next pass with backoff." The
// backfill lane's schedule window (spec §9.2) may be nightly, so the live
// lane cannot leave retrying failures solely to it — a transient failure
// must not go unretried for hours or days just because it happened to be
// this lane, not backfill, that first saw the entry.
//
// Backoff doubles on each consecutive failure of the same entry, capped at
// retryBackoffCapMultiple*interval, so a persistently broken entry is
// retried ever less often rather than hammered every tick (spec §10's
// closing line: "never retried in a tight loop"), while a transient one
// recovers quickly. Tying both to interval rather than a fixed duration
// keeps retry cadence proportional to how fast the lane already polls, so
// tests using a short interval can observe backoff and its growth without
// slowing the suite down.
const (
	retryBackoffMultiple    = 4
	retryBackoffCapMultiple = 32
)

// LiveStats is the live lane's status snapshot for the admin page (spec
// §13.1's requirement 3: the page must say which lane is paused and why).
// The live lane has no backlog/throughput concept like Backfill.Stats()
// -- it only ever auto-pauses for the one reason Backfill can also
// auto-pause for, plus the same operator pause Backfill has always had,
// so this carries just those.
type LiveStats struct {
	// Paused is true between Pause() and the matching Resume() -- an
	// OPERATOR pause, mirroring Backfill.Stats().Paused exactly;
	// see EmbedderPaused below for the lane's own automatic one.
	Paused bool

	// EmbedderPaused is true while the live lane is not attempting new
	// ids because the embedder reported itself unavailable
	// (isEmbedderUnavailable; spec §13.1) -- kept as the live lane's OWN
	// state, entirely separate from Backfill.Stats().EmbedderPaused, so
	// the two lanes' pause states never collapse into one indistinct
	// "something is paused" on the admin page.
	EmbedderPaused bool

	// EmbedderPauseReason is the classified error's own message while
	// EmbedderPaused is true, and empty otherwise.
	EmbedderPauseReason string

	// EmbedderPauseRequiresRestart is true when the current pause cannot
	// clear itself no matter how long the lane keeps retrying -- the
	// remote reported a different model mid-run (embed.ErrRequiresRestart;
	// spec §13.1) -- as opposed to an ordinary transient outage, which
	// resolves on its own the moment the network/remote recovers. Without
	// this distinction, an operator watching a boolean has no way to tell
	// "wait, this clears itself" from "go restart the sidecar", and would
	// have to read the full reason text every time to find out.
	EmbedderPauseRequiresRestart bool
}

// LiveMonitor holds the live lane's current embedder-pause state, read by
// internal/web to render it on the admin page. It is deliberately its own
// type, not a field folded into *Indexer (which both lanes share) or into
// *Backfill (which is backfill's own): the live and backfill lanes must be
// able to show DIFFERENT pause states at the same time, so each needs its
// own place to keep one.
//
// This additionally gave it an OPERATOR pause (spec §13.1) -- paused/
// resumeCh below -- mirroring Backfill's own Pause/Resume/resumeCh
// exactly, rather than inventing a second pause mechanism: the live lane
// previously had no way for anything outside itself to stop it starting
// new work, which Manager.Switch needs so it can quiesce both lanes
// before swapping the embedder they share.
//
// A nil *LiveMonitor is valid and every method on it is a no-op (Pause/
// Resume) or returns the zero value (Stats) -- runLive/RunLive never
// require one, so tests that don't care about pause visibility don't need
// to construct one.
type LiveMonitor struct {
	mu                  sync.Mutex
	embedderPaused      bool
	embedderPauseReason string
	requiresRestart     bool

	// paused/resumeCh are the operator pause, guarded by the same
	// mu as the fields above -- structurally identical to Backfill's own
	// paused/resumeCh (see backfill.go's Pause/Resume/waitWhilePaused):
	// Pause sets paused and lets a batch/tick already in flight finish;
	// Resume closes resumeCh to wake every waiter and replaces it with a
	// fresh one, exactly the "close to broadcast, then swap" pattern
	// Backfill already uses.
	paused   bool
	resumeCh chan struct{}
}

// NewLiveMonitor builds a LiveMonitor with no pause recorded yet.
func NewLiveMonitor() *LiveMonitor { return &LiveMonitor{resumeCh: make(chan struct{})} }

// Pause requests that the live lane stop starting new attempts -- the
// live-lane counterpart to Backfill.Pause, added for the embedder
// switch (Manager.Switch quiesces both lanes the same way before
// swapping). A tick already in progress is allowed to finish attempting
// the ids it already fetched (checked between ticks, never mid-tick, the
// same discipline Backfill uses between batches); after that, runLive
// blocks until Resume. Safe to call from any goroutine, nil-tolerant like
// every other LiveMonitor method.
func (m *LiveMonitor) Pause() {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.paused = true
}

// Resume releases a paused live lane, waking a blocked runLive
// immediately. Calling it when not paused is a harmless no-op, mirroring
// Backfill.Resume.
func (m *LiveMonitor) Resume() {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.paused {
		m.paused = false
		close(m.resumeCh)
		m.resumeCh = make(chan struct{})
	}
}

// waitWhilePaused blocks while the live lane is paused by the operator,
// returning false if ctx is cancelled while waiting and true otherwise
// (including immediately, when not paused or when m is nil) -- the
// live-lane counterpart to Backfill.waitWhilePaused's operator-pause
// branch.
func (m *LiveMonitor) waitWhilePaused(ctx context.Context) bool {
	if m == nil {
		return true
	}
	for {
		m.mu.Lock()
		paused := m.paused
		ch := m.resumeCh
		m.mu.Unlock()

		if !paused {
			return true
		}

		select {
		case <-ch:
			continue
		case <-ctx.Done():
			return false
		}
	}
}

// pauseForEmbedder records the live lane pausing for the embedder,
// mirroring Backfill.pauseForEmbedder -- idempotent, safe to call from the
// single goroutine runLive runs on every tick it detects the outage.
func (m *LiveMonitor) pauseForEmbedder(reason string, requiresRestart bool) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.embedderPaused = true
	m.embedderPauseReason = reason
	m.requiresRestart = requiresRestart
}

// clearEmbedderPause resumes the live lane's own pause state, mirroring
// Backfill.clearEmbedderPause -- called whenever an attempt succeeds.
func (m *LiveMonitor) clearEmbedderPause() {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.embedderPaused = false
	m.embedderPauseReason = ""
	m.requiresRestart = false
}

// Stats returns a snapshot of the live lane's current pause state.
func (m *LiveMonitor) Stats() LiveStats {
	if m == nil {
		return LiveStats{}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return LiveStats{
		Paused:                       m.paused,
		EmbedderPaused:               m.embedderPaused,
		EmbedderPauseReason:          m.embedderPauseReason,
		EmbedderPauseRequiresRestart: m.requiresRestart,
	}
}

// retryState tracks one failed entry's next eligible retry time and
// current backoff, purely in memory for the lifetime of one RunLive call —
// like lastSeen, it is not a durability mechanism. A process restart loses
// it, but search.entry_index_state.status='failed' is unaffected, so a
// fresh run's very first pass (lastSeen back at its starting cursor) will
// naturally rediscover any entry still failed via PendingEntryIDs' own
// status='failed' clause.
type retryState struct {
	nextAttempt time.Time
	backoff     time.Duration
}

// RunLive runs the live indexing lane until ctx is cancelled, then returns
// nil (never a context error — cancellation is the expected, clean way to
// stop this lane). It is a simple ticker loop: on every tick it fetches up
// to livePageSize pending entry ids greater than the highest id it has
// seen so far, and indexes each in turn — plain "WHERE id > lastSeen"
// pagination (spec §4). At a few hundred entries a day this does not
// justify LISTEN/NOTIFY (spec §9.1); a short poll interval is simpler and
// cheap enough.
//
// If the embedder itself reports it is unavailable (isEmbedderUnavailable
// — a network outage, or a remote that changed identity mid-run; spec
// §13.1), this pauses rather than treating it as an ordinary per-entry
// failure: the tick stops attempting further ids, lastSeen is left
// exactly where it is (so every id it did not get to, including the one
// that just failed, is offered again next tick), and nothing is marked
// failed or added to the retry/backoff map — the entry stays genuinely
// pending. There is no separate health check; the next tick's normal
// attempt IS the recovery probe, so indexing resumes automatically the
// moment the embedder starts succeeding again, with no operator action.
// This mirrors the backfill lane's own pause/resume behaviour for the
// same condition (see backfill.go's pauseForEmbedder).
//
// Its starting cursor is the highest entry id that already exists at
// startup (store.MaxEntryID), not 0. This is deliberate coordination with
// the Backfill lane: both lanes' default
// starting point was 0, so on a freshly deployed instance with an existing
// backlog, RunLive's very first ticks would fetch and attempt to index the
// OLDEST pending entries — exactly the rows Backfill is, separately and
// concurrently, also working through from the beginning. Both calling
// IndexEntry on the same id is not corrupting (ReplacePassages is a single
// transaction; the loser's insert aborts on the ordinal unique constraint
// and its error is logged and retried, self-healing per spec §10), but it
// wastes duplicate embedding work and fills Stats() with unique-violation
// noise on every cold start over any nontrivial backlog.
//
// Starting at the current max id instead makes the two lanes' domains
// disjoint by construction: RunLive only ever attempts entries created
// AFTER it started (id > startup snapshot) — genuinely new arrivals, which
// is exactly its job per spec §9.1 ("the live lane handles new entries") —
// while every pre-existing entry, at any id, is left entirely to Backfill.
// A rare residual overlap is still possible (an entry created in the
// narrow window around the snapshot query, or one Backfill's slow-moving
// cursor reaches before the live lane got to it) but is no longer the
// routine, guaranteed collision over the entire historical backlog; it
// falls back to the same safe, self-healing behaviour described above. If
// the snapshot query itself fails, RunLive logs the error and falls back
// to starting at 0 rather than refusing to start the live lane at all.
//
// This coordination is safe only because of an invariant Backfill upholds
// on its side: Backfill always rescans from its own
// starting cursor on every process start — it persists no cursor of its
// own across restarts, and never skips a run just because an earlier one
// reported Done. If a future change ever violates that (persisting a
// backfill cursor across restarts, say, or treating a prior Done as
// "nothing to do this time") an entry created during a window where
// neither lane happens to be running becomes invisible to BOTH of them
// forever: RunLive's snapshot has already moved past it by the time the
// next live-lane instance starts, and a backfill that trusts a stale
// Done, or a persisted cursor already past that id, will never look at it
// either. Nothing enforces this today beyond the two lanes' current
// implementations agreeing on it; whoever changes either side needs to
// keep it true.
func RunLive(ctx context.Context, ix *Indexer, interval time.Duration, monitor *LiveMonitor) error {
	startAfter, err := ix.store.MaxEntryID()
	if err != nil {
		slog.Error("live lane: unable to determine starting cursor, starting from the beginning of the table instead",
			slog.Any("error", err))
		startAfter = 0
	}
	return runLive(ctx, ix, interval, startAfter, nil, monitor)
}

// runLive is RunLive's implementation, parameterised by the starting
// cursor and an optional upper bound.
//
// startAfter mirrors internal/store's own tests' convention (afterID =
// entryID-1): tests pass their first fixture id minus one so a fresh
// cursor does not see whatever pending backlog already existed.
//
// upTo, when non-nil, additionally EXCLUDES any fetched id greater than
// its current value — something startAfter alone cannot do. A lower bound
// only narrows what a monotonically-advancing lastSeen has already passed;
// a poller that keeps running for a test's duration still sees anything
// ELSE created system-wide above that bound while it runs. Under Go's
// default cross-package test parallelism something else reliably is: this
// was proven directly (see live_test.go)
// — internal/store's own fixtures commonly carry a hash that never matches
// their entry's real content_hash (e.g. "hash-good"), which makes those
// rows look permanently pending to PendingEntryIDs, and an unbounded live
// lane left running for as little as a waitFor timeout allows can and did
// pick one up and genuinely re-index it, corrupting a concurrently running
// unrelated test. upTo is an *atomic.Int64, not a plain int64, so a test
// can extend it after the lane has already started (see
// TestRunLiveDoesNotBlockNewerEntriesOnPersistentFailure, which only
// learns the id it needs to admit after the lane is already running).
// nil disables the bound entirely, which is what production RunLive
// wants — it has no fixed set of ids to scope itself to.
//
// monitor, if non-nil, is kept in step with this tick loop's own
// embedder-pause detection (see LiveMonitor) so internal/web can render
// the live lane's pause state on the admin page distinctly from the
// backfill lane's own. nil is accepted and simply does nothing, which
// every test in this file that doesn't care about pause visibility
// passes.
func runLive(ctx context.Context, ix *Indexer, interval time.Duration, startAfter int64, upTo *atomic.Int64, monitor *LiveMonitor) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	lastSeen := startAfter
	retries := make(map[int64]retryState)

	initialBackoff := interval * retryBackoffMultiple
	maxBackoff := interval * retryBackoffCapMultiple

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}

		// Checked once per tick, never mid-tick -- the same discipline
		// Backfill applies between batches (spec §9.2) -- so an operator
		// pause never interrupts ids already being attempted,
		// only stops the NEXT tick from starting. Blocks here until
		// Resume(); a nil monitor never blocks.
		if !monitor.waitWhilePaused(ctx) {
			return nil
		}

		now := time.Now()

		ids, err := ix.store.PendingEntryIDs(lastSeen, livePageSize)
		if err != nil {
			slog.Error("live lane: unable to fetch pending entries", slog.Any("error", err))
			continue
		}

		if upTo != nil {
			bound := upTo.Load()
			filtered := ids[:0]
			for _, id := range ids {
				if id <= bound {
					filtered = append(filtered, id)
				}
			}
			ids = filtered
		}

		// Entries whose backoff has elapsed, gathered before this tick's
		// fresh ids are processed (and so before any of them could be
		// added to retries), so the two sets below are always disjoint.
		var retryIDs []int64
		for id, st := range retries {
			if !now.Before(st.nextAttempt) {
				retryIDs = append(retryIDs, id)
			}
		}

		indexed, failed := 0, 0

		// attempt indexes one entry, checking ctx.Done() first so
		// cancellation is honoured between every single entry, not merely
		// between ticks. It reports whether the pass was cancelled, and
		// separately whether the failure means the EMBEDDER itself is
		// currently unavailable (isEmbedderUnavailable; spec §13.1) —
		// lane-level, not this entry's fault. In that case entry_index_state
		// was already left untouched by IndexEntry, and attempt does the
		// same: no failed++, no retries[id] entry, so nothing here ever
		// looks like a per-entry failure that needs backoff to unwind.
		attempt := func(id int64) (cancelled, embedderDown bool) {
			select {
			case <-ctx.Done():
				return true, false
			default:
			}

			err := ix.IndexEntry(ctx, id)
			if err == nil {
				indexed++
				delete(retries, id)
				monitor.clearEmbedderPause()
				return false, false
			}

			if isEmbedderUnavailable(err) {
				restart := requiresEmbedderRestart(err)
				monitor.pauseForEmbedder(err.Error(), restart)
				slog.Warn("live lane: entry not attempted -- embedder unavailable, pausing",
					slog.Int64("entry_id", id), slog.Any("error", err),
					slog.Bool("requires_restart", restart),
				)
				return false, true
			}

			failed++
			backoff := initialBackoff
			if st, retrying := retries[id]; retrying {
				backoff = min(st.backoff*2, maxBackoff)
			}
			retries[id] = retryState{nextAttempt: now.Add(backoff), backoff: backoff}
			slog.Error("live lane: unable to index entry",
				slog.Int64("entry_id", id),
				slog.Any("error", err),
				slog.Duration("retry_backoff", backoff),
			)
			return false, false
		}

		embedderDown := false
		for _, id := range ids {
			if embedderDown {
				// The lane is paused for the rest of this tick: leave
				// lastSeen exactly where it is, so every remaining id in
				// ids -- including the one that just failed -- is offered
				// again on the very next tick (spec §13.1: entries stay
				// pending during an outage, and indexing resumes
				// automatically once the embedder recovers, with no
				// operator action and no separate health check needed).
				break
			}
			cancelled, down := attempt(id)
			if cancelled {
				return nil
			}
			if down {
				embedderDown = true
				continue
			}
			// Advance the cursor regardless of outcome. A failed entry
			// stays retryable — via the retries map above, not via this
			// cursor — so advancing past it here does not lose it, and
			// does not stall newer ids behind it either.
			if id > lastSeen {
				lastSeen = id
			}
		}

		if !embedderDown {
			for _, id := range retryIDs {
				cancelled, down := attempt(id)
				if cancelled {
					return nil
				}
				if down {
					embedderDown = true
					break
				}
			}
		}

		if len(ids) > 0 || len(retryIDs) > 0 {
			slog.Info("live lane: pass complete",
				slog.Int("pending", len(ids)),
				slog.Int("retried", len(retryIDs)),
				slog.Int("indexed", indexed),
				slog.Int("failed", failed),
				slog.Bool("embedder_paused", embedderDown),
			)
		}
	}
}
