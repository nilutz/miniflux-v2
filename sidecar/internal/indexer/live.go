// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package indexer // import "miniflux.app/v2/sidecar/internal/indexer"

import (
	"context"
	"log/slog"
	"time"
)

// livePageSize bounds how many pending entry ids RunLive fetches per tick.
// At a few hundred entries a day (spec §9.1) this is generous headroom for
// a single poll; sustained backlog clearing across the whole corpus is the
// backfill lane's job (Task 6), not this one's.
const livePageSize = 50

// RunLive runs the live indexing lane until ctx is cancelled, then returns
// nil (never a context error — cancellation is the expected, clean way to
// stop this lane). It is a simple ticker loop: on every tick it fetches up
// to livePageSize pending entry ids greater than the highest id it has
// seen so far, and indexes each in turn — plain "WHERE id > lastSeen"
// pagination (spec §4). At a few hundred entries a day this does not
// justify LISTEN/NOTIFY (spec §9.1); a short poll interval is simpler and
// cheap enough.
//
// lastSeen lives only in memory for the lifetime of this call. It is a
// pagination optimisation, not a durability mechanism: the real resume
// state is search.entry_index_state itself (IndexEntry's content-hash
// short-circuit), so a process restart losing lastSeen just means the next
// run re-scans from the bottom of the id space and finds nothing to do for
// everything already indexed.
//
// A single entry's indexing failure is logged and the loop moves on to the
// next id — IndexEntry has already recorded the failure against that entry
// (status='failed', retryable later), so one bad entry never aborts the
// pass, still less the lane. ctx.Done() is checked before every entry, not
// merely between polls, so shutdown during a long pass is prompt.
func RunLive(ctx context.Context, ix *Indexer, interval time.Duration) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	var lastSeen int64

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}

		ids, err := ix.store.PendingEntryIDs(lastSeen, livePageSize)
		if err != nil {
			slog.Error("live lane: unable to fetch pending entries", slog.Any("error", err))
			continue
		}

		indexed, failed := 0, 0
		for _, id := range ids {
			select {
			case <-ctx.Done():
				return nil
			default:
			}

			if err := ix.IndexEntry(ctx, id); err != nil {
				failed++
				slog.Error("live lane: unable to index entry",
					slog.Int64("entry_id", id),
					slog.Any("error", err),
				)
			} else {
				indexed++
			}

			// Advance the cursor regardless of outcome: a failed entry is
			// still retryable (see IndexEntry / PendingEntryIDs), just not
			// tightly, within this same pass, by this lane. It remains
			// eligible for the backfill lane's own sweep, and for this
			// lane's next pass once its content changes.
			if id > lastSeen {
				lastSeen = id
			}
		}

		if len(ids) > 0 {
			slog.Info("live lane: pass complete",
				slog.Int("pending", len(ids)),
				slog.Int("indexed", indexed),
				slog.Int("failed", failed),
			)
		}
	}
}
