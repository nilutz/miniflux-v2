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

// DefaultBackfillPageSize bounds how many pending entry ids Backfill fetches
// per page. Each page is one "batch" for the Controller's purposes: worker
// count is rechecked between pages, never mid-page (spec §9.2), so a
// smaller page lets the controller react sooner, at the cost of slightly
// more frequent PendingEntryIDs queries — a good trade at backfill's
// hours-long timescale (spec §9.3).
const DefaultBackfillPageSize = 20

// DefaultBackfillPollInterval is how long Backfill waits before checking
// again when it currently cannot do any work — outside the schedule
// window. It is unrelated to how fast entries are actually processed once
// work is allowed.
const DefaultBackfillPollInterval = 5 * time.Second

// BackfillConfig configures a Backfill lane.
type BackfillConfig struct {
	PageSize     int           // pending entry ids fetched per page/batch
	PollInterval time.Duration // wait between checks while the schedule window is closed
}

func (cfg BackfillConfig) withDefaults() BackfillConfig {
	if cfg.PageSize <= 0 {
		cfg.PageSize = DefaultBackfillPageSize
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = DefaultBackfillPollInterval
	}
	return cfg
}

// causeCounts aggregates counts keyed by a short cause string (an error
// message, or a recorded skip reason), safe for concurrent use by the
// backfill lane's worker goroutines.
type causeCounts struct {
	mu sync.Mutex
	m  map[string]int64
}

func newCauseCounts() *causeCounts { return &causeCounts{m: make(map[string]int64)} }

func (c *causeCounts) add(cause string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[cause]++
}

func (c *causeCounts) snapshot() map[string]int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]int64, len(c.m))
	for k, v := range c.m {
		out[k] = v
	}
	return out
}

// Stats is a snapshot of a Backfill lane's progress, for the admin page
// (Task 7, spec §9.4): "progress and ETA, current throughput, live
// concurrency with the controller's reason for it, error and skip counts
// by cause, and pause/resume."
type Stats struct {
	Indexed int64 // entries successfully indexed (status='ok')
	Skipped int64 // entries with no usable text (status='skipped')
	Failed  int64 // entries that failed to index (status='failed', retryable)

	SkippedByReason map[string]int64 // skip reason -> count
	FailedByReason  map[string]int64 // failure cause (the returned error's message) -> count

	Workers          int     // the controller's current worker count right now
	ControllerReason string  // the controller's reason for that count
	ThroughputPerSec float64 // (Indexed+Skipped+Failed) / elapsed seconds since Start

	Paused bool // true between Pause() and the matching Resume()
	Done   bool // true once a full pass found nothing left to do
}

// Backfill is the throttled backfill lane (spec §9.1, §9.2): a worker pool
// sized by a Controller that pages through store.PendingEntryIDs and calls
// Indexer.IndexEntry for each id it fetches.
//
// Checkpointing is implicit. search.entry_index_state already records what
// is done and at which content hash, and store.PendingEntryIDs already
// excludes anything indexed at its current hash — so Backfill keeps no
// checkpoint of its own. A fresh Backfill started after a crash, or after
// its schedule window closed and reopened, simply rescans and finds that
// everything already done is no longer pending (spec §10: "Backfill crash
// or window close -> Resumes from the entry_index_state checkpoint").
type Backfill struct {
	idx        *Indexer
	controller *Controller
	cfg        BackfillConfig

	mu        sync.Mutex
	paused    bool
	resumeCh  chan struct{}
	done      bool
	startedAt time.Time

	indexed         atomic.Int64
	skipped         atomic.Int64
	failed          atomic.Int64
	skippedByReason *causeCounts
	failedByReason  *causeCounts
}

// NewBackfill builds a Backfill lane over idx, throttled by controller.
func NewBackfill(idx *Indexer, controller *Controller, cfg BackfillConfig) *Backfill {
	return &Backfill{
		idx:             idx,
		controller:      controller,
		cfg:             cfg.withDefaults(),
		resumeCh:        make(chan struct{}),
		skippedByReason: newCauseCounts(),
		failedByReason:  newCauseCounts(),
	}
}

// Pause requests that the lane stop starting new batches. A batch already
// in flight is allowed to finish — recheck happens between batches, never
// mid-batch, same as the controller's own worker-count adjustments (spec
// §9.2) — after which Start blocks until Resume. Safe to call from any
// goroutine, before or after Start.
func (b *Backfill) Pause() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.paused = true
}

// Resume releases a paused lane, waking a blocked Start immediately.
// Calling it when not paused is a harmless no-op.
func (b *Backfill) Resume() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.paused {
		b.paused = false
		close(b.resumeCh)
		b.resumeCh = make(chan struct{})
	}
}

// waitWhilePaused blocks while the lane is paused, waking as soon as
// Resume is called. It returns false if ctx is cancelled while waiting,
// true otherwise (including immediately, if not paused at all).
func (b *Backfill) waitWhilePaused(ctx context.Context) bool {
	for {
		b.mu.Lock()
		paused := b.paused
		ch := b.resumeCh
		b.mu.Unlock()
		if !paused {
			return true
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return false
		}
	}
}

// Start runs the backfill lane: it pages through pending entry ids and
// indexes them with a worker pool sized by the Controller, until either
// ctx is cancelled or a full pass finds no pending work left at all.
// Draining the backlog to completion is this lane's job — unlike the
// always-on live lane, "nothing left pending" is an expected, successful
// way for it to stop, not an error to retry past. It never idles forever
// waiting for new entries to arrive; that is RunLive's job. Start returns
// nil in every case, mirroring RunLive: cancellation is the expected way
// to stop it early, not a failure.
func (b *Backfill) Start(ctx context.Context) error {
	return b.start(ctx, 0, nil)
}

// start is Start's implementation, parameterised by a starting cursor and
// an optional upper bound — the same convention runLive uses (see
// live.go), and for the same reason: it lets tests scope a run to exactly
// the fixtures they created, so it never touches another concurrently
// running package's own rows (see live.go's doc comment for the proof
// that an unbounded lane can and did do exactly that under Go's default
// cross-package test parallelism).
//
// Pagination itself is never skipped for ids above upTo — they are simply
// excluded from processing — so a page straddling the bound still advances
// the cursor past every id it fetched. Once the cursor reaches or passes
// upTo, everything at or below it has necessarily already been seen (ids
// are fetched in ascending order with no gaps), so the scoped run is
// declared done immediately rather than continuing to fetch pages that can
// only ever be filtered to empty — which would otherwise busy-loop forever
// if some other, unrelated backlog exists above the bound.
func (b *Backfill) start(ctx context.Context, startAfter int64, upTo *atomic.Int64) error {
	b.mu.Lock()
	b.startedAt = time.Now()
	b.mu.Unlock()

	cursor := startAfter

	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		if !b.waitWhilePaused(ctx) {
			return nil
		}

		workers := b.controller.Workers()
		if workers <= 0 {
			// Outside the schedule window: wait and re-check rather than
			// treating "no workers allowed right now" as "done" — the
			// backlog can be paused for a week without being considered
			// finished (spec §9.1).
			if !sleepCtx(ctx, b.cfg.PollInterval) {
				return nil
			}
			continue
		}

		rawIDs, err := b.idx.store.PendingEntryIDs(cursor, b.cfg.PageSize)
		if err != nil {
			slog.Error("backfill lane: unable to fetch pending entries", slog.Any("error", err))
			if !sleepCtx(ctx, b.cfg.PollInterval) {
				return nil
			}
			continue
		}

		if len(rawIDs) == 0 {
			b.markDone()
			return nil
		}
		cursor = rawIDs[len(rawIDs)-1]

		ids := rawIDs
		if upTo != nil {
			bound := upTo.Load()
			kept := rawIDs[:0:0]
			for _, id := range rawIDs {
				if id <= bound {
					kept = append(kept, id)
				}
			}
			ids = kept
		}

		if len(ids) > 0 {
			start := time.Now()
			b.runBatch(ctx, ids, workers)
			b.controller.Observe(time.Since(start))
		}

		if upTo != nil && cursor >= upTo.Load() {
			b.markDone()
			return nil
		}

		select {
		case <-ctx.Done():
			return nil
		default:
		}
	}
}

func (b *Backfill) markDone() {
	b.mu.Lock()
	b.done = true
	b.mu.Unlock()
}

// runBatch indexes ids using up to workers goroutines pulling from a
// shared channel — the concurrency lever spec §6.7 measured: several
// goroutines sharing one unconstrained embedder session, never ORT thread
// pinning. It returns once every id has been attempted or ctx is
// cancelled, whichever comes first.
func (b *Backfill) runBatch(ctx context.Context, ids []int64, workers int) {
	if workers > len(ids) {
		workers = len(ids)
	}
	if workers < 1 {
		workers = 1
	}

	idCh := make(chan int64)
	go func() {
		defer close(idCh)
		for _, id := range ids {
			select {
			case idCh <- id:
			case <-ctx.Done():
				return
			}
		}
	}()

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				default:
				}
				id, ok := <-idCh
				if !ok {
					return
				}
				b.process(ctx, id)
			}
		}()
	}
	wg.Wait()
}

// process indexes one entry and updates Stats' counters, classifying the
// outcome by cause — the admin page (Task 7, spec §9.4) renders exactly
// this breakdown. IndexEntry itself does not distinguish "indexed" from
// "skipped" in its return value (both return nil), so process reads back
// the just-written index state to tell them apart and to recover the skip
// reason for SkippedByReason.
func (b *Backfill) process(ctx context.Context, id int64) {
	err := b.idx.IndexEntry(ctx, id)
	if err != nil {
		b.failed.Add(1)
		b.failedByReason.add(err.Error())
		slog.Error("backfill lane: unable to index entry", slog.Int64("entry_id", id), slog.Any("error", err))
		return
	}

	state, err := b.idx.store.EntryIndexState(id)
	if err != nil {
		// Indexing itself succeeded; a failure to read back the state we
		// just wrote is logged and does not fail the batch, but the entry
		// cannot be classified as indexed vs. skipped, so it is counted
		// conservatively as indexed rather than lost from Stats entirely.
		slog.Error("backfill lane: indexed entry but could not read back its state",
			slog.Int64("entry_id", id), slog.Any("error", err))
		b.indexed.Add(1)
		return
	}

	if state != nil && state.Status == "skipped" {
		b.skipped.Add(1)
		reason := state.Reason
		if reason == "" {
			reason = "skipped"
		}
		b.skippedByReason.add(reason)
		return
	}

	b.indexed.Add(1)
}

// Stats returns a snapshot of the lane's current progress.
func (b *Backfill) Stats() Stats {
	b.mu.Lock()
	paused := b.paused
	done := b.done
	startedAt := b.startedAt
	b.mu.Unlock()

	indexed := b.indexed.Load()
	skipped := b.skipped.Load()
	failed := b.failed.Load()

	var throughput float64
	if !startedAt.IsZero() {
		if elapsed := time.Since(startedAt).Seconds(); elapsed > 0 {
			throughput = float64(indexed+skipped+failed) / elapsed
		}
	}

	return Stats{
		Indexed:          indexed,
		Skipped:          skipped,
		Failed:           failed,
		SkippedByReason:  b.skippedByReason.snapshot(),
		FailedByReason:   b.failedByReason.snapshot(),
		Workers:          b.controller.Workers(),
		ControllerReason: b.controller.Reason(),
		ThroughputPerSec: throughput,
		Paused:           paused,
		Done:             done,
	}
}

// sleepCtx waits out d, or ctx cancellation, whichever comes first. It
// returns true if the full duration elapsed, false if ctx was cancelled
// first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}
