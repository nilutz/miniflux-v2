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
// window, a PendingEntryIDs query itself errored, or a just-ended sweep
// still has outstanding retries cooling down (see retryTracker). It is
// unrelated to how fast entries are actually processed once work is
// allowed, and it is also the base unit each failing entry's own backoff
// is measured in (see retryBackoffMultiple/retryBackoffCapMultiple in
// live.go, reused here).
const DefaultBackfillPollInterval = 5 * time.Second

// DefaultRemainingCountTTL bounds how often Stats() actually queries
// store.PendingEntryCount for its Remaining figure, rather than serving a
// memoised value. That query has no LIMIT to stop early at and must
// content-hash every candidate row (see PendingEntryCount's own doc
// comment) — on a large, mostly-unindexed table it can take seconds, and
// Remaining is exactly the value an admin page is likely to poll
// most often for its ETA. 45s is ample staleness for an ETA display (spec
// §9.4) while keeping Stats() itself a cheap, effectively in-memory read
// the rest of the time.
const DefaultRemainingCountTTL = 45 * time.Second

// throughputEWMAAlpha weights each newly observed batch's rate against
// the running average: Stats().ThroughputPerSec is an exponentially
// weighted moving average over batches, not a lifetime average over all
// active time, so it reflects how fast the lane is running RIGHT NOW —
// what an ETA needs — rather than being dragged down for the rest
// of a 41-hour run by one early, unrepresentative batch. 0.3 gives noticeable weight to the most recent batch while
// still smoothing out one-off jitter.
const throughputEWMAAlpha = 0.3

// DefaultIdleResweepInterval is how long a drained lane waits before
// sweeping the table again.
//
// A drained lane must keep sweeping, because nothing else re-examines
// entries below the live lane's cursor. RunLive only ever looks at
// id > lastSeen, seeded from the highest entry id that existed at startup
// — so once Backfill.Start returned on a clean sweep, spec §10's "entry
// content changed -> content_hash mismatch triggers re-index" held only
// for entries above that cursor, or until the next process restart. That
// is not hypothetical: P0's scrape-backfill CLI rewrites entries.content
// for precisely the old entries this excluded, so the sidecar would index
// them from their RSS excerpts, report Done, and never notice when the
// real article text landed underneath.
//
// 15 minutes is chosen against the cost of the sweep itself, not against
// any latency requirement: a drained sweep is a single PendingEntryIDs
// query returning nothing, so this is four cheap queries an hour, while
// still being far longer than PollInterval so a genuine drain cannot spin
// hot.
const DefaultIdleResweepInterval = 15 * time.Minute

// BackfillConfig configures a Backfill lane.
type BackfillConfig struct {
	PageSize     int           // pending entry ids fetched per page/batch
	PollInterval time.Duration // wait between checks while idle (schedule window closed, a fetch error, or outstanding retries cooling down)

	// IdleResweepInterval is how long to wait, after a sweep that found
	// nothing left to do, before sweeping again. Ignored entirely when
	// StopWhenDrained is set.
	IdleResweepInterval time.Duration

	// StopWhenDrained makes Start return once a sweep completes with
	// nothing pending and nothing outstanding, instead of idling and
	// sweeping again. Production leaves this false — see
	// DefaultIdleResweepInterval for why a lane that stops is a
	// correctness problem, not merely an idle one. Tests set it so a
	// drained lane is observable as a returning call.
	StopWhenDrained bool
}

func (cfg BackfillConfig) withDefaults() BackfillConfig {
	if cfg.PageSize <= 0 {
		cfg.PageSize = DefaultBackfillPageSize
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = DefaultBackfillPollInterval
	}
	if cfg.IdleResweepInterval <= 0 {
		cfg.IdleResweepInterval = DefaultIdleResweepInterval
	}
	return cfg
}

// maxCauseKeys bounds how many distinct causes a causeCounts will track
// before folding everything further into overflowCause. The lane is meant
// to run unattended for up to 41 hours (spec §9.3) over a corpus of up to
// a million entries, and both of its cause sources are ultimately strings
// produced elsewhere — failureCause's generic fallback, and skip reasons
// read back from the database — so neither is structurally guaranteed to
// stay small. A cap makes "the map cannot grow without bound" a property
// of this type rather than a property of every caller getting its
// classification right, and 64 rows is already more than spec §9.4's
// by-cause table can usefully render.
const maxCauseKeys = 64

// overflowCause is the bucket every cause beyond maxCauseKeys is counted
// under. It is deliberately conspicuous: seeing it on the admin page means
// the classification upstream is leaking detail into its keys.
const overflowCause = "(other causes)"

// causeCounts aggregates counts keyed by a short cause string (a
// classified failure cause, or a recorded skip reason), safe for
// concurrent use by the backfill lane's worker goroutines, and bounded at
// maxCauseKeys distinct keys.
type causeCounts struct {
	mu sync.Mutex
	m  map[string]int64
}

func newCauseCounts() *causeCounts { return &causeCounts{m: make(map[string]int64)} }

func (c *causeCounts) add(cause string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, known := c.m[cause]; !known && len(c.m) >= maxCauseKeys {
		c.m[overflowCause]++
		return
	}
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

// backfillRetryState tracks one persistently-failing entry's backoff and
// last-seen failure cause across sweeps — the same discipline live.go's
// retryState applies to the live lane, adapted to the sweep model.
type backfillRetryState struct {
	nextAttempt time.Time
	backoff     time.Duration
	lastCause   string
}

// retryTracker is Backfill's per-entry backoff state.
//
// Before this existed, every sweep that had seen a failure reset its
// cursor and reattempted every still-failing entry unconditionally: with
// a fixed pollInterval between sweeps, a single durably broken entry was
// re-embedded roughly 720 times an hour, forever, against a CPU-bound
// model, while Stats().Failed climbed by one per sweep for that same
// entry until the number meant nothing.
//
// shouldAttempt gates whether an id is even attempted this round,
// skipping it — at zero embedding cost, not merely zero counted-as-failed
// cost — until its own escalating backoff elapses. recordFailure reports
// whether a given failure is new information (a never-before-seen id, or
// one whose cause changed) worth counting in Stats(); a persistently
// broken entry failing with the identical cause every time it's actually
// attempted is counted once, not once per attempt.
type retryTracker struct {
	mu    sync.Mutex
	state map[int64]*backfillRetryState
}

func newRetryTracker() *retryTracker {
	return &retryTracker{state: make(map[int64]*backfillRetryState)}
}

func (r *retryTracker) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.state = make(map[int64]*backfillRetryState)
}

// shouldAttempt reports whether id may be attempted right now: true if it
// has no recorded failure, or its backoff has elapsed.
func (r *retryTracker) shouldAttempt(id int64, now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	st, ok := r.state[id]
	if !ok {
		return true
	}
	return !now.Before(st.nextAttempt)
}

// recordFailure records id failing with cause at now: it escalates the
// entry's backoff (doubling, capped at maxBackoff) if already tracked, or
// starts it at initialBackoff for a first-seen failure. It reports
// whether this failure is new information (countIt) — true the first
// time an id is seen, or whenever its cause differs from the last one
// recorded for it — which the caller uses to decide whether to increment
// Stats().Failed / FailedByReason.
func (r *retryTracker) recordFailure(id int64, cause string, now time.Time, initialBackoff, maxBackoff time.Duration) (countIt bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	st, ok := r.state[id]
	if !ok {
		r.state[id] = &backfillRetryState{nextAttempt: now.Add(initialBackoff), backoff: initialBackoff, lastCause: cause}
		return true
	}

	st.backoff = min(st.backoff*2, maxBackoff)
	st.nextAttempt = now.Add(st.backoff)
	if st.lastCause == cause {
		return false
	}
	st.lastCause = cause
	return true
}

// recordSuccess forgets id's retry state: it recovered.
func (r *retryTracker) recordSuccess(id int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.state, id)
}

// hasOutstanding reports whether any entry is still tracked as failing —
// including one merely cooling down, not due for another attempt yet.
// Backfill.start uses this, not "did anything fail just now", to decide
// whether a sweep that reached its end may declare Done: an entry skipped
// this round purely because its backoff has not elapsed is still
// unresolved, and Done must not be reported while it is.
func (r *retryTracker) hasOutstanding() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.state) > 0
}

// isTracked reports whether id currently has recorded retry state.
func (r *retryTracker) isTracked(id int64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.state[id]
	return ok
}

// pruneNotIn removes every tracked entry whose id is not a key of seen —
// entries that were NOT observed anywhere in the sweep that just
// completed.
//
// This exists to fix a real regression: without
// it, an entry's retry state is cleared only by recordSuccess (it was
// re-attempted and worked) or a fresh Start (a whole new run). If a
// tracked entry stops being returned by PendingEntryIDs at all — its row
// was deleted (a feed removed, retention cleanup — both real, ordinary
// events during a backfill run that can last 4-41 hours), not fixed —
// process is never called for it again, so neither recordSuccess nor
// another recordFailure ever runs, and hasOutstanding stays true forever.
// Start would then never return, and Stats().Done would never become
// true, indistinguishable from a lane that is still genuinely working.
// Pruning after every completed sweep against what that sweep actually
// saw closes this: an id no longer returned by the database at all is
// removed from retry tracking exactly like a fixed one, letting Done
// become true once nothing genuinely outstanding remains.
func (r *retryTracker) pruneNotIn(seen map[int64]bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id := range r.state {
		if !seen[id] {
			delete(r.state, id)
		}
	}
}

// Stats is a snapshot of a Backfill lane's progress, for the admin page
// (spec §9.4): "progress and ETA, current throughput, live
// concurrency with the controller's reason for it, error and skip counts
// by cause, and pause/resume."
type Stats struct {
	Indexed int64 // entries successfully indexed (status='ok')
	Skipped int64 // entries with no usable text (status='skipped')
	Failed  int64 // distinct failures: a persistently failing entry counts once per distinct cause, not once per retry attempt

	// Remaining is how many entries currently still need (re-)indexing,
	// approximately — the denominator the admin page needs to render "N indexed
	// of M" and derive an ETA from ThroughputPerSec. It comes from
	// store.PendingEntryCountApprox (which does not detoast or hash
	// anything, and slightly under-counts entries edited since they were
	// indexed), memoised behind DefaultRemainingCountTTL, with at most one
	// refresh in flight at a time. It is -1 if no count could be read
	// (logged, not fatal to the rest of the snapshot).
	Remaining int64

	SkippedByReason map[string]int64 // skip reason -> count
	FailedByReason  map[string]int64 // failure cause -> count of distinct (id, cause) events, same de-duplication as Failed

	Workers          int     // the controller's current worker count right now
	ControllerReason string  // the controller's reason for that count
	ThroughputPerSec float64 // exponentially-weighted recent rate (entries actually attempted per second) -- reflects current speed, not a lifetime average

	Paused bool // true between Pause() and the matching Resume() -- an OPERATOR pause; see EmbedderPaused for the lane's own automatic one

	// EmbedderPaused is true while the lane has automatically paused
	// itself because the embedder reported it cannot currently serve
	// requests (isEmbedderUnavailable; spec §13.1) — a network outage, or
	// a remote that changed identity mid-run. It is deliberately a
	// SEPARATE flag from Paused, not folded into it: an operator whose
	// backfill stopped needs to be able to tell "I paused this" from
	// "the embedder is unreachable" at a glance, since the two point at
	// entirely different places to look (their own admin action, versus
	// the network or the remote host). It clears itself automatically
	// the next time an attempt succeeds — no operator action required.
	EmbedderPaused bool

	// EmbedderPauseReason is the classified error's own message (e.g.
	// "embed/remote: request failed: ...") while EmbedderPaused is true,
	// and empty otherwise — the admin page's answer to "why".
	EmbedderPauseReason string

	// EmbedderPauseRequiresRestart is true when the current embedder
	// pause can never clear itself no matter how long the lane keeps
	// retrying — the remote reported a different model mid-run
	// (embed.ErrRequiresRestart; spec §13.1) — as opposed to an ordinary
	// transient outage, which resolves the moment the network/remote
	// recovers with no operator action at all. Kept as a distinct signal
	// from EmbedderPauseReason's prose so the admin page can render "wait"
	// vs. "go restart the sidecar" as a glance-able fact, not something an
	// operator has to read a sentence to determine.
	EmbedderPauseRequiresRestart bool

	// Done reports that the backlog is currently drained: the most recent
	// full sweep found nothing pending and left nothing outstanding. It
	// is not a terminal state — the lane keeps sweeping every
	// IdleResweepInterval, and Done goes back to false as soon as a sweep
	// finds work again (an edited entry, a newly scraped body, a fresh
	// import). Only a lane configured with StopWhenDrained stops here.
	Done bool
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
//
// This is also the invariant RunLive's own coordination depends on
// (see live.go's doc comment) — Backfill
// must always rescan from its starting cursor on every process start, and
// must never skip a run because an earlier one reported Done, or an entry
// created during a shutdown window can become invisible to both lanes.
type Backfill struct {
	idx        *Indexer
	controller *Controller

	mu       sync.Mutex
	cfg      BackfillConfig // PageSize/PollInterval are live-editable (spec §9.2); always read through pageSize()/pollInterval()
	paused   bool
	resumeCh chan struct{}

	// embedderPaused and embedderPauseReason are the lane's OWN pause,
	// entered automatically when the embedder reports itself unavailable
	// (isEmbedderUnavailable; spec §13.1), and cleared automatically the
	// next time an attempt succeeds. Deliberately a separate pair of
	// fields from paused/resumeCh above, not a reuse of them: an operator
	// pause and an embedder-outage pause must be distinguishable on the
	// admin page ("paused by operator" vs "paused: embedder
	// unreachable"), and only the operator's own Resume() may clear
	// paused, while only a successful attempt may clear embedderPaused.
	embedderPaused               bool
	embedderPauseReason          string
	embedderPauseRequiresRestart bool
	done                         bool
	running                      bool
	startedAt                    time.Time
	startAfterCursor             int64 // the id this run's pagination began after; scopes Remaining's PendingEntryCount query

	indexed         atomic.Int64
	skipped         atomic.Int64
	failed          atomic.Int64
	batchAttempted  atomic.Int64 // ids actually attempted (not backoff-skipped) in the batch currently/most recently running -- read-and-reset by start() between batches
	skippedByReason *causeCounts
	failedByReason  *causeCounts
	retries         *retryTracker

	throughputMu   sync.Mutex
	throughputEWMA float64

	remainingMu         sync.Mutex
	remainingCached     int64
	remainingCachedAt   time.Time
	remainingCountTTL   time.Duration // defaults to DefaultRemainingCountTTL; same-package tests may set it directly for a short TTL
	remainingRefreshing bool          // guards against concurrent refreshes; see cachedRemaining

	// remainingCountFn is the query cachedRemaining refreshes from. A
	// field rather than a direct call so that this package's own tests can
	// substitute a counting, blocking stand-in and observe how many
	// queries N concurrent Stats() calls actually issue; production always
	// leaves it as NewBackfill sets it.
	remainingCountFn func(afterID int64) (int64, error)
}

// NewBackfill builds a Backfill lane over idx, throttled by controller.
func NewBackfill(idx *Indexer, controller *Controller, cfg BackfillConfig) *Backfill {
	b := &Backfill{
		idx:               idx,
		controller:        controller,
		cfg:               cfg.withDefaults(),
		resumeCh:          make(chan struct{}),
		skippedByReason:   newCauseCounts(),
		failedByReason:    newCauseCounts(),
		retries:           newRetryTracker(),
		remainingCountTTL: DefaultRemainingCountTTL,
	}
	// The approximate count, not the exact one: this is refreshed on a
	// timer behind an admin page that polls, and the exact predicate has
	// to detoast and MD5 every candidate row. See store.PendingEntryCountApprox for exactly how it differs.
	b.remainingCountFn = idx.store.PendingEntryCountApprox
	return b
}

// SetPageSize live-edits the number of pending entry ids fetched per
// pagination page, taking effect on the next page fetch. This is a
// distinct quantity from the embedding batch size spec §9.2 names as its
// third live-editable knob ("batch size — passages per forward pass; the
// main lever on CPU efficiency") — that one is Indexer.SetBatchSize.
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

// SetIdleResweepInterval live-edits how long a drained lane waits before
// sweeping again. Values <= 0 are ignored.
func (b *Backfill) SetIdleResweepInterval(d time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if d > 0 {
		b.cfg.IdleResweepInterval = d
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

func (b *Backfill) idleResweepInterval() time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.cfg.IdleResweepInterval
}

func (b *Backfill) stopWhenDrained() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.cfg.StopWhenDrained
}

func (b *Backfill) boundStartAfter() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.startAfterCursor
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

// pauseForEmbedder marks the lane paused because the embedder reported
// itself unavailable (isEmbedderUnavailable; spec §13.1) — automatic, not
// an operator action, and distinguishable from Pause() on the admin page
// via Stats().EmbedderPaused/EmbedderPauseReason. requiresRestart records
// whether this specific pause can ever clear on its own (see
// requiresEmbedderRestart) — surfaced separately so the admin page can
// tell "wait" from "go restart the sidecar" without parsing reason. It is
// idempotent: every worker that hits the same outage concurrently just
// refreshes the recorded reason, rather than stacking state.
func (b *Backfill) pauseForEmbedder(reason string, requiresRestart bool) {
	b.mu.Lock()
	already := b.embedderPaused
	b.embedderPaused = true
	b.embedderPauseReason = reason
	b.embedderPauseRequiresRestart = requiresRestart
	b.mu.Unlock()
	if !already {
		slog.Warn("backfill lane: pausing -- embedder unavailable",
			slog.String("reason", reason), slog.Bool("requires_restart", requiresRestart))
	}
}

// clearEmbedderPause resumes a lane that had auto-paused for the embedder,
// called whenever an attempt succeeds — spec §13.1's "indexing resumes
// automatically", with no operator action required. A no-op when the lane
// was not embedder-paused.
func (b *Backfill) clearEmbedderPause() {
	b.mu.Lock()
	was := b.embedderPaused
	b.embedderPaused = false
	b.embedderPauseReason = ""
	b.embedderPauseRequiresRestart = false
	b.mu.Unlock()
	if was {
		slog.Info("backfill lane: resuming -- embedder available again")
	}
}

func (b *Backfill) isEmbedderPaused() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.embedderPaused
}

// waitWhilePaused blocks while the lane is paused — by the operator
// (Pause), by an embedder outage (pauseForEmbedder), or both — and returns
// false if ctx is cancelled while waiting, true otherwise (including
// immediately, if not paused at all).
//
// The two pauses are woken differently, matching spec §13.1's requirement
// that an outage clears itself. An operator pause only ever ends when
// Resume is called, so this blocks on resumeCh with no timeout. A pure
// embedder pause instead waits out one pollInterval and then returns true
// unconditionally, letting the caller make a real attempt again: if the
// outage continues, that attempt's failure re-arms pauseForEmbedder (with
// a fresh reason) and the NEXT call here waits another interval; if the
// embedder has recovered, the attempt succeeds, clearEmbedderPause runs,
// and every subsequent call returns immediately. This deliberately reuses
// the entries already about to be processed as the recovery probe rather
// than adding a separate health check.
func (b *Backfill) waitWhilePaused(ctx context.Context) bool {
	for {
		b.mu.Lock()
		paused := b.paused
		embedderPaused := b.embedderPaused
		ch := b.resumeCh
		b.mu.Unlock()

		if !paused && !embedderPaused {
			return true
		}

		if paused {
			select {
			case <-ch:
				continue
			case <-ctx.Done():
				return false
			}
		}

		timer := time.NewTimer(b.pollInterval())
		select {
		case <-timer.C:
			return true
		case <-ctx.Done():
			timer.Stop()
			return false
		}
	}
}

// Start runs the backfill lane: it pages through pending entry ids and
// indexes them with a worker pool sized by the Controller, until ctx is
// cancelled. A sweep that finds no pending work left at all — including
// no entries left to retry (see start's doc comment for what "a full
// sweep" means) — sets Stats().Done and then idles for
// IdleResweepInterval before sweeping again; it does not return. Only a
// lane configured with StopWhenDrained returns there.
//
// That it keeps sweeping is load-bearing, not tidiness: the live lane
// only ever looks at ids above the highest one that existed when the
// process started, so a drained lane that returned left everything below
// that cursor with nothing watching it — and P0's scrape-backfill CLI
// rewrites entries.content for exactly those old entries (see
// DefaultIdleResweepInterval). Start returns nil in every case, mirroring
// RunLive: cancellation is the expected way to stop it, not a failure.
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
// last page comes back empty is therefore wrong whenever that sweep left
// any entry outstanding (still failing, or merely cooling down under its
// own backoff): the backlog is not actually drained, just not visible from
// where the cursor happens to be sitting. So: if
// a sweep that reaches its end still has outstanding entries (per
// retryTracker.hasOutstanding), the cursor resets to startAfter and
// another sweep begins after a pollInterval pause; only a sweep that
// reaches its end with nothing outstanding at all is declared Done.
//
// Before checking hasOutstanding, every completed sweep prunes retry
// state for any tracked id it never once saw returned by
// PendingEntryIDs. Without this, an entry whose row is deleted
// entirely — a feed removed, retention cleanup, both ordinary events
// over this lane's 4-41 hour runtime — is never returned by
// PendingEntryIDs again, so process is never called for it again, so
// nothing ever clears its retry state: hasOutstanding stays true and
// Start never returns, indistinguishable from a lane still genuinely
// working. Pruning against what the sweep actually saw fixes this
// without weakening the guarantee above: an entry that is STILL
// being returned (still genuinely failing, or merely cooling down under
// its own backoff) is never pruned, only one the database has stopped
// offering at all.
//
// A sweep that ends with nothing outstanding is the drained case: Done is
// set, the lane waits IdleResweepInterval rather than pollInterval, and
// then sweeps again from startAfter. Done is therefore a live flag rather
// than a latch — the next sweep to find work clears it again.
//
// Resetting the cursor every pollInterval does not mean re-embedding
// every outstanding entry every pollInterval, though: retryTracker gates
// each entry's own next attempt behind its individual, escalating backoff.
// A durably broken entry with nothing else
// pending therefore still causes repeated, cheap PendingEntryIDs queries
// every pollInterval, but its embedder is called on a rapidly widening
// schedule, and Stats().Failed stops climbing once its cause stops
// changing. For a corpus containing an entry that fails forever,
// Stats().Done never becomes
// true — the accurate state of the world, not a bug: the backlog
// genuinely never reaches zero while something in it is stuck.
func (b *Backfill) start(ctx context.Context, startAfter int64, upTo *atomic.Int64) error {
	b.mu.Lock()
	if b.running {
		b.mu.Unlock()
		return fmt.Errorf("backfill: already running")
	}
	b.running = true
	b.startedAt = time.Now()
	b.done = false
	b.startAfterCursor = startAfter
	b.mu.Unlock()

	// A fresh run's Stats() should reflect only this run's progress, not
	// accumulate across an earlier Start/cancel/Start cycle on the same
	// Backfill.
	b.indexed.Store(0)
	b.skipped.Store(0)
	b.failed.Store(0)
	b.batchAttempted.Store(0)
	b.skippedByReason.reset()
	b.failedByReason.reset()
	b.retries.reset()
	b.throughputMu.Lock()
	b.throughputEWMA = 0
	b.throughputMu.Unlock()
	b.remainingMu.Lock()
	b.remainingCachedAt = time.Time{}
	b.remainingMu.Unlock()

	defer func() {
		b.mu.Lock()
		b.running = false
		b.mu.Unlock()
	}()

	cursor := startAfter
	// seenTrackedIDs accumulates, across every page fetched during the
	// CURRENT sweep, which currently-tracked (retries.isTracked) ids the
	// database actually returned. Deliberately not "every id fetched" —
	// that could be the whole table's worth over a long sweep — only
	// tracked ids are ever relevant to pruning,
	// and there should be few of those at once. Reset at the start of
	// every new sweep, below.
	seenTrackedIDs := make(map[int64]bool)

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
				// A sweep that finds work is no longer drained. Done is
				// now a live "is the backlog currently empty" flag, not
				// a one-way latch, because the lane keeps sweeping past
				// its first clean pass.
				b.setDone(false)
				b.batchAttempted.Store(0)

				batchStart := time.Now()
				b.runBatch(ctx, ids, workers)
				elapsed := time.Since(batchStart)

				attempted := b.batchAttempted.Load()
				if attempted > 0 && elapsed > 0 {
					// Per-entry latency, not the whole page's: a short,
					// upTo-thinned, or mostly-backed-off-and-skipped page
					// must not read as an artificially fast or slow
					// batch. The worker count
					// actually used is reported alongside it — runBatch
					// clamps workers down to len(ids), so the controller's
					// own figure is not always what ran — because the
					// controller needs it to turn an inverse-throughput
					// reading back into a per-worker service time
					// comparable across worker counts (see Observe).
					b.controller.Observe(elapsed/time.Duration(attempted), min(workers, len(ids)))
					b.updateThroughputEWMA(float64(attempted) / elapsed.Seconds())
				}
			}

			// Checked AFTER processing, not before: an entry that just
			// failed for the very first time this batch only becomes
			// tracked as a side effect of runBatch above, and one that
			// just recovered stops being tracked the same way. Checking
			// isTracked before processing would miss the former
			// entirely, pruning a genuinely brand-new failure in the
			// very sweep it was discovered in.
			for _, id := range rawIDs {
				if b.retries.isTracked(id) {
					seenTrackedIDs[id] = true
				}
			}

			if upTo != nil && cursor >= upTo.Load() {
				sweepEnded = true
			}
		}

		if sweepEnded {
			// Prune before checking: any tracked id this sweep never
			// once returned from the database is no longer genuinely
			// pending (deleted, or otherwise resolved outside this
			// lane's own retry path) and must stop pinning Done open.
			b.retries.pruneNotIn(seenTrackedIDs)

			wait := b.pollInterval()
			// isEmbedderPaused() is checked here too, not only via
			// hasOutstanding: entries skipped because the lane itself
			// was embedder-paused were never handed to IndexEntry at
			// all, so they were never added to retryTracker either —
			// hasOutstanding alone would see nothing outstanding and
			// wrongly declare the sweep drained during a genuine outage
			// (spec §13.1: entries stay pending, which is not the same
			// as the backlog being empty).
			if !b.retries.hasOutstanding() && !b.isEmbedderPaused() {
				// Genuinely drained. Report it, and then — unless the
				// caller explicitly asked for a one-shot — keep
				// sweeping, because nothing else ever re-examines
				// entries below the live lane's cursor (see
				// DefaultIdleResweepInterval).
				b.setDone(true)
				if b.stopWhenDrained() {
					return nil
				}
				wait = b.idleResweepInterval()
			}
			if !sleepCtx(ctx, wait) {
				return nil
			}
			cursor = startAfter
			seenTrackedIDs = make(map[int64]bool)
			continue
		}

		select {
		case <-ctx.Done():
			return nil
		default:
		}
	}
}

func (b *Backfill) setDone(done bool) {
	b.mu.Lock()
	b.done = done
	b.mu.Unlock()
}

func (b *Backfill) updateThroughputEWMA(rate float64) {
	b.throughputMu.Lock()
	defer b.throughputMu.Unlock()
	if b.throughputEWMA == 0 {
		b.throughputEWMA = rate
		return
	}
	b.throughputEWMA = throughputEWMAAlpha*rate + (1-throughputEWMAAlpha)*b.throughputEWMA
}

func (b *Backfill) currentThroughput() float64 {
	b.throughputMu.Lock()
	defer b.throughputMu.Unlock()
	return b.throughputEWMA
}

// runBatch indexes ids using up to workers goroutines pulling from a
// shared channel — the concurrency lever spec §6.7 measured: several
// goroutines sharing one unconstrained embedder session, never ORT thread
// pinning. It returns once every id has been attempted (or skipped under
// its own backoff) or ctx is cancelled, whichever comes first.
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
// outcome by cause — the admin page (spec §9.4) renders exactly
// this breakdown. IndexEntry itself does not distinguish "indexed" from
// "skipped" in its return value (both return nil), so process reads back
// the just-written index state to tell them apart and to recover the skip
// reason for SkippedByReason.
//
// Before calling IndexEntry at all, process checks retryTracker: an id
// still cooling down under its own backoff is skipped entirely, at zero
// embedding cost.
//
// An error caused by ctx being cancelled (a shutdown or interruption, not
// a real embedding failure — see IndexEntry's own doc comment) is not
// counted as a failure, nor recorded in retryTracker, either: a cancelled
// attempt was never actually finished, so treating it as this entry's
// "latest cause" would be misleading, and counting it would inflate
// Stats().Failed on every graceful shutdown.
func (b *Backfill) process(ctx context.Context, id int64) {
	now := time.Now()
	if !b.retries.shouldAttempt(id, now) {
		return
	}
	b.batchAttempted.Add(1)

	err := b.idx.IndexEntry(ctx, id)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		if isEmbedderUnavailable(err) {
			// LANE-level (spec §13.1), not this entry's fault: pause the
			// lane rather than counting a per-entry failure. Deliberately
			// none of failed/failedByReason/retries are touched — the
			// entry's index state was left untouched by IndexEntry too,
			// so it stays genuinely pending and is retried in full once
			// the lane resumes, with no backoff to unwind.
			restart := requiresEmbedderRestart(err)
			b.pauseForEmbedder(err.Error(), restart)
			slog.Warn("backfill lane: entry not attempted -- embedder unavailable",
				slog.Int64("entry_id", id), slog.Any("error", err), slog.Bool("requires_restart", restart))
			return
		}
		// The CLASSIFIED cause, never err.Error(): this string is a map
		// key in failedByReason, a row in spec §9.4's by-cause table, and
		// the value recordFailure de-duplicates repeat failures against.
		// The full error, entry id and all, goes to the log line below
		// where it belongs.
		cause := failureCause(err)
		interval := b.pollInterval()
		initialBackoff := interval * retryBackoffMultiple
		maxBackoff := interval * retryBackoffCapMultiple
		if countIt := b.retries.recordFailure(id, cause, now, initialBackoff, maxBackoff); countIt {
			b.failed.Add(1)
			b.failedByReason.add(cause)
		}
		slog.Error("backfill lane: unable to index entry", slog.Int64("entry_id", id), slog.Any("error", err))
		return
	}
	b.clearEmbedderPause()
	b.retries.recordSuccess(id)

	state, err := b.idx.store.EntryIndexState(ctx, id)
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

// cachedRemaining returns Stats().Remaining, refreshing it at most once
// per remainingCountTTL and serving the memoised value otherwise. The query is scoped to this run's own starting
// cursor (boundStartAfter), which lets Postgres skip everything at or
// below it via the primary key index.
//
// At most ONE refresh is ever in flight. Without that guard, three
// separately-minor things composed into a way to wedge the database
// Miniflux itself is serving from: the TTL expiring starts a query that
// can run for a long time on a large corpus, the admin page reloads every
// ten seconds, and each of those reloads would start another one on top of
// the last. A caller arriving while a
// refresh is running gets the previous value — stale by definition, which
// an ETA can carry — rather than queueing behind it or starting a second
// scan. With no previous value at all it gets -1, which the admin page
// already renders as an unknown ETA.
func (b *Backfill) cachedRemaining() int64 {
	b.remainingMu.Lock()
	ttl := b.remainingCountTTL
	if ttl <= 0 {
		ttl = DefaultRemainingCountTTL
	}
	fresh := !b.remainingCachedAt.IsZero() && time.Since(b.remainingCachedAt) < ttl
	cached := b.remainingCached
	haveCached := !b.remainingCachedAt.IsZero()
	if fresh {
		b.remainingMu.Unlock()
		return cached
	}
	if b.remainingRefreshing {
		b.remainingMu.Unlock()
		if haveCached {
			return cached
		}
		return -1
	}
	b.remainingRefreshing = true
	b.remainingMu.Unlock()

	count, err := b.remainingCountFn(b.boundStartAfter())

	b.remainingMu.Lock()
	b.remainingRefreshing = false
	if err == nil {
		b.remainingCached = count
		b.remainingCachedAt = time.Now()
	}
	b.remainingMu.Unlock()

	if err != nil {
		slog.Error("backfill lane: unable to count remaining pending entries for Stats()", slog.Any("error", err))
		return -1
	}
	return count
}

// Stats returns a snapshot of the lane's current progress.
func (b *Backfill) Stats() Stats {
	b.mu.Lock()
	paused := b.paused
	embedderPaused := b.embedderPaused
	embedderPauseReason := b.embedderPauseReason
	embedderPauseRequiresRestart := b.embedderPauseRequiresRestart
	done := b.done
	b.mu.Unlock()

	return Stats{
		Indexed:                      b.indexed.Load(),
		Skipped:                      b.skipped.Load(),
		Failed:                       b.failed.Load(),
		Remaining:                    b.cachedRemaining(),
		SkippedByReason:              b.skippedByReason.snapshot(),
		FailedByReason:               b.failedByReason.snapshot(),
		Workers:                      b.controller.Workers(),
		ControllerReason:             b.controller.Reason(),
		ThroughputPerSec:             b.currentThroughput(),
		Paused:                       paused,
		EmbedderPaused:               embedderPaused,
		EmbedderPauseReason:          embedderPauseReason,
		EmbedderPauseRequiresRestart: embedderPauseRequiresRestart,
		Done:                         done,
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
