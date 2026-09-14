// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package indexer // import "miniflux.app/v2/sidecar/internal/indexer"

import (
	"context"
	"fmt"
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
// window, a PendingEntryIDs query itself errored, or a just-finished sweep
// needs to pause briefly before retrying entries that failed during it
// (see start's hadFailureThisSweep). It is unrelated to how fast entries
// are actually processed once work is allowed.
const DefaultBackfillPollInterval = 5 * time.Second

// BackfillConfig configures a Backfill lane.
type BackfillConfig struct {
	PageSize     int           // pending entry ids fetched per page/batch
	PollInterval time.Duration // wait between checks while idle (schedule window closed, a fetch error, or between retry sweeps)
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

func (c *causeCounts) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m = make(map[string]int64)
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

	// Remaining is how many entries currently still need (re-)indexing,
	// by the same criteria PendingEntryIDs uses — the denominator Task 7
	// needs to render "N indexed of M" and derive an ETA from
	// ThroughputPerSec. It is -1 if the count could not be read (logged,
	// not fatal to the snapshot).
	Remaining int64

	SkippedByReason map[string]int64 // skip reason -> count
	FailedByReason  map[string]int64 // failure cause (the returned error's message) -> count

	Workers          int     // the controller's current worker count right now
	ControllerReason string  // the controller's reason for that count
	ThroughputPerSec float64 // (Indexed+Skipped+Failed) / time actually spent running batches -- excludes paused and outside-window time

	Paused bool // true between Pause() and the matching Resume()
	Done   bool // true once a full sweep processed nothing at all, including no retries
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

	mu        sync.Mutex
	cfg       BackfillConfig // PageSize/PollInterval are live-editable (spec §9.2); always read through pageSize()/pollInterval()
	paused    bool
	resumeCh  chan struct{}
	done      bool
	running   bool
	startedAt time.Time

	indexed         atomic.Int64
	skipped         atomic.Int64
	failed          atomic.Int64
	activeNanos     atomic.Int64 // cumulative wall-clock time spent inside runBatch, across every batch -- the ThroughputPerSec denominator
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

// SetPageSize live-edits the page size, taking effect on the next page
// fetch (spec §9.2: "batch size ... live-editable without a restart").
// Values <= 0 are ignored.
func (b *Backfill) SetPageSize(n int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if n > 0 {
		b.cfg.PageSize = n
	}
}

// SetPollInterval live-edits the idle poll interval. Values <= 0 are
// ignored.
func (b *Backfill) SetPollInterval(d time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if d > 0 {
		b.cfg.PollInterval = d
	}
}

func (b *Backfill) pageSize() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.cfg.PageSize
}

func (b *Backfill) pollInterval() time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.cfg.PollInterval
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
// ctx is cancelled or a full sweep finds no pending work left at all —
// including no entries left to retry (see start's doc comment for what
// "a full sweep" means). Draining the backlog to completion is this
// lane's job — unlike the always-on live lane, "nothing left pending" is
// an expected, successful way for it to stop, not an error to retry past.
// It never idles forever waiting for new entries to arrive; that is
// RunLive's job. Start returns nil in every case, mirroring RunLive:
// cancellation is the expected way to stop it early, not a failure.
//
// Start is not re-entrant: calling it while a previous call on the same
// Backfill is still running returns an error immediately rather than
// running two overlapping worker pools against the same counters.
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
// A "sweep" is one linear pass of pagination from startAfter up to the
// current end of the table (or, when upTo is set, up to the bound).
// Because the cursor only ever moves forward within a sweep, an entry that
// fails partway through is never revisited again within that same sweep —
// it sits below the cursor, still status='failed' and therefore still
// matched by PendingEntryIDs, but never fetched again until something
// resets the cursor. Declaring the whole lane "done" the instant a sweep's
// last page comes back empty is therefore wrong whenever that sweep saw
// any failure: the backlog is not actually drained, just not visible from
// where the cursor happens to be sitting (fix round 1, finding 2). So: if
// a sweep that reaches its end had at least one failure during it, the
// cursor resets to startAfter and another sweep begins (after a
// pollInterval pause, so a durably broken entry is retried once per sweep
// rather than in a tight loop across sweeps — spec §10's "never retried in
// a tight loop" is lane-agnostic). Only a sweep that reaches its end
// having had zero failures is declared Done. For a corpus containing an
// entry that fails forever, this means Start legitimately never returns
// on its own (Stats().Done stays false, Stats().Failed keeps climbing by
// one per sweep) until the caller cancels ctx or the entry starts
// succeeding — which is the accurate state of the world, not a bug: the
// backlog genuinely never reaches zero while something in it is stuck.
func (b *Backfill) start(ctx context.Context, startAfter int64, upTo *atomic.Int64) error {
	b.mu.Lock()
	if b.running {
		b.mu.Unlock()
		return fmt.Errorf("backfill: already running")
	}
	b.running = true
	b.startedAt = time.Now()
	b.done = false
	b.mu.Unlock()

	// A fresh run's Stats() should reflect only this run's progress, not
	// accumulate across an earlier Start/cancel/Start cycle on the same
	// Backfill (fix round 1, finding 10).
	b.indexed.Store(0)
	b.skipped.Store(0)
	b.failed.Store(0)
	b.activeNanos.Store(0)
	b.skippedByReason.reset()
	b.failedByReason.reset()

	defer func() {
		b.mu.Lock()
		b.running = false
		b.mu.Unlock()
	}()

	cursor := startAfter
	hadFailureThisSweep := false

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
			if !sleepCtx(ctx, b.pollInterval()) {
				return nil
			}
			continue
		}

		rawIDs, err := b.idx.store.PendingEntryIDs(cursor, b.pageSize())
		if err != nil {
			slog.Error("backfill lane: unable to fetch pending entries", slog.Any("error", err))
			if !sleepCtx(ctx, b.pollInterval()) {
				return nil
			}
			continue
		}

		sweepEnded := len(rawIDs) == 0

		if !sweepEnded {
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
				failedBefore := b.failed.Load()

				batchStart := time.Now()
				b.runBatch(ctx, ids, workers)
				elapsed := time.Since(batchStart)
				b.activeNanos.Add(int64(elapsed))
				// Per-entry latency, not the whole page's: a short or
				// upTo-thinned page must not read as an artificially fast
				// batch, nor a full page after short ones as a degraded
				// one (fix round 1, finding 6).
				b.controller.Observe(elapsed / time.Duration(len(ids)))

				if b.failed.Load() > failedBefore {
					hadFailureThisSweep = true
				}
			}

			if upTo != nil && cursor >= upTo.Load() {
				sweepEnded = true
			}
		}

		if sweepEnded {
			if !hadFailureThisSweep {
				b.markDone()
				return nil
			}
			if !sleepCtx(ctx, b.pollInterval()) {
				return nil
			}
			cursor = startAfter
			hadFailureThisSweep = false
			continue
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
//
// An error caused by ctx being cancelled (a shutdown or interruption, not
// a real embedding failure — see IndexEntry's own doc comment) is not
// counted as a failure here either, mirroring IndexEntry's choice not to
// write entry_index_state for it: a cancelled attempt was never actually
// finished, so counting it would inflate Stats().Failed on every graceful
// shutdown (fix round 1, finding 8).
func (b *Backfill) process(ctx context.Context, id int64) {
	err := b.idx.IndexEntry(ctx, id)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
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
	b.mu.Unlock()

	indexed := b.indexed.Load()
	skipped := b.skipped.Load()
	failed := b.failed.Load()

	var throughput float64
	if active := time.Duration(b.activeNanos.Load()); active > 0 {
		throughput = float64(indexed+skipped+failed) / active.Seconds()
	}

	remaining, err := b.idx.store.PendingEntryCount()
	if err != nil {
		slog.Error("backfill lane: unable to count remaining pending entries for Stats()", slog.Any("error", err))
		remaining = -1
	}

	return Stats{
		Indexed:          indexed,
		Skipped:          skipped,
		Failed:           failed,
		Remaining:        remaining,
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
