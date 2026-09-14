// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package indexer // import "miniflux.app/v2/sidecar/internal/indexer"

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"
)

// livePageSize bounds how many pending entry ids RunLive fetches per tick.
// At a few hundred entries a day (spec §9.1) this is generous headroom for
// a single poll; sustained backlog clearing across the whole corpus is the
// backfill lane's job (Task 6), not this one's.
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
// A single entry's indexing failure is logged and the loop moves on to the
// next id, never aborting the pass; the failed entry is added to an
// in-memory, backoff-gated retry set (see retryState) so it is retried on
// a later pass without blocking forward progress on newer entries — the
// lastSeen cursor advances past a failed id exactly as it does past a
// succeeded one. ctx.Done() is checked before every entry — both freshly
// fetched and retried ones — not merely between polls, so shutdown during
// a long pass is prompt.
func RunLive(ctx context.Context, ix *Indexer, interval time.Duration) error {
	return runLive(ctx, ix, interval, 0, nil)
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
// was proven directly (see live_test.go and Task 5's fix-round-1 report)
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
func runLive(ctx context.Context, ix *Indexer, interval time.Duration, startAfter int64, upTo *atomic.Int64) error {
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
		// between ticks. It reports whether the pass was cancelled.
		attempt := func(id int64) (cancelled bool) {
			select {
			case <-ctx.Done():
				return true
			default:
			}

			if err := ix.IndexEntry(ctx, id); err != nil {
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
			} else {
				indexed++
				delete(retries, id)
			}
			return false
		}

		for _, id := range ids {
			if attempt(id) {
				return nil
			}
			// Advance the cursor regardless of outcome. A failed entry
			// stays retryable — via the retries map above, not via this
			// cursor — so advancing past it here does not lose it, and
			// does not stall newer ids behind it either.
			if id > lastSeen {
				lastSeen = id
			}
		}

		for _, id := range retryIDs {
			if attempt(id) {
				return nil
			}
		}

		if len(ids) > 0 || len(retryIDs) > 0 {
			slog.Info("live lane: pass complete",
				slog.Int("pending", len(ids)),
				slog.Int("retried", len(retryIDs)),
				slog.Int("indexed", indexed),
				slog.Int("failed", failed),
			)
		}
	}
}
