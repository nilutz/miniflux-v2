// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package indexer // import "miniflux.app/v2/sidecar/internal/indexer"

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// controllerBaselineSamples is how many of the first observed batches are
// used to establish the latency baseline against which LatencyMargin is
// measured. A handful of samples smooths out one noisy first batch without
// delaying the controller's first real decision for long.
const controllerBaselineSamples = 3

// Window is a wall-clock hours-of-day range during which the backfill lane
// is allowed to run at all, e.g. Window{Start: 2, End: 7} for 02:00-07:00
// (spec §9.2). It wraps past midnight when End <= Start, so Window{Start:
// 22, End: 6} means 22:00 through 05:59 the next day.
//
// The zero value (Start: 0, End: 0) means "always", never "never": a
// zero-width bound is not a meaningful restriction, and a ControllerConfig
// built without explicitly setting Window must not silently make the
// backfill lane refuse to run at all.
type Window struct {
	Start int // hour of day, 0-23, inclusive
	End   int // hour of day, 0-23, exclusive
}

// Contains reports whether t's hour of day falls inside the window.
func (w Window) Contains(t time.Time) bool {
	if w.Start == w.End {
		// Covers the zero value (always) and any other degenerate
		// zero-width bound the same way: a window that starts and ends at
		// the same hour restricts nothing.
		return true
	}
	h := t.Hour()
	if w.Start < w.End {
		return h >= w.Start && h < w.End
	}
	return h >= w.Start || h < w.End // wraps past midnight
}

// ControllerConfig configures a Controller.
type ControllerConfig struct {
	MinWorkers int // never fewer; at least 1 so progress never stops
	MaxWorkers int // hard ceiling; the controller never exceeds it

	Window Window // wall-clock hours the lane may run; zero value means "always"

	LatencyMargin float64 // degradation factor (per-worker service time / baseline) that triggers backing off

	// LoadThreshold is the PER-CORE 1-minute load average above which to
	// back off: the raw load average divided by runtime.NumCPU() (see
	// sampleLoadAverage, which does the division). 0.8 therefore means
	// "the machine is 80% busy", not "0.8 runnable processes".
	//
	// Absolute would be unusable as a default. This lane deliberately runs
	// two CPU-saturating embedding workers against an unconstrained ORT
	// session (spec §6.7); on any machine that pushes the raw 1-minute
	// average past 0.8 within a minute and holds it there, so an absolute
	// 0.8 made every Observe after the baseline take the load branch and
	// step down, with the condition never clearing again — a one-way
	// ratchet to MinWorkers for the whole run (whole-branch review,
	// finding 2).
	LoadThreshold float64
}

// DefaultControllerConfig returns MinWorkers: 1, MaxWorkers: 2,
// LatencyMargin: 1.5, LoadThreshold: 0.8 (per core — see
// ControllerConfig.LoadThreshold). MaxWorkers 2 matches the measured
// optimum (spec §6.7: 2 workers, batch 8, unconstrained ORT threading,
// 33.9 passages/sec) -- higher is permitted but was not faster on the
// spike machine.
func DefaultControllerConfig() ControllerConfig {
	return ControllerConfig{MinWorkers: 1, MaxWorkers: 2, LatencyMargin: 1.5, LoadThreshold: 0.8}
}

// withDefaults fills in a floor for MinWorkers and MaxWorkers so a
// zero-value (or otherwise under-specified) ControllerConfig can never
// make the lane stall forever. Without this, ControllerConfig{} sets
// MinWorkers=MaxWorkers=0: workersLocked clamps any value into [0, 0] and
// returns 0, which Backfill.start reads exactly like "outside the
// schedule window" and polls forever -- silently, since Reason() would
// still say "starting at the configured minimum worker count". MinWorkers
// is floored at 1 (the same floor the brief requires: "at least 1 so
// progress never stops"), and MaxWorkers is raised to match if it was left
// below MinWorkers (whether zero or simply misconfigured).
func (cfg ControllerConfig) withDefaults() ControllerConfig {
	if cfg.MinWorkers < 1 {
		cfg.MinWorkers = 1
	}
	if cfg.MaxWorkers < cfg.MinWorkers {
		cfg.MaxWorkers = cfg.MinWorkers
	}
	return cfg
}

// LoadAverage reports the current 1-minute system load average NORMALISED
// PER CORE: 1.0 means "as many runnable tasks as this machine has cores".
// Production controllers sample the real host (see sampleLoadAverage,
// which divides by runtime.NumCPU()); tests inject a fake so a controller
// test never depends on the real machine's load -- see throttle_test.go's
// fixedLoad.
type LoadAverage func() (float64, error)

// Controller decides how many worker goroutines the backfill lane should
// run right now (spec §9.2). Its only lever is worker count against one
// shared, unconstrained embedder session -- it must never be used to pin
// ORT thread counts (spec §6.7: doing so measured 11.7 passages/sec against
// 33.9, three times slower).
//
// It samples system load and per-batch latency, establishes a baseline
// from the first controllerBaselineSamples healthy batches, and then moves
// the worker count at most one step per Observe call within [MinWorkers,
// MaxWorkers] -- one step at a time so it does not oscillate. It records a
// short human-readable reason for the current count (Reason), because the
// admin page (Task 7, spec §9.4) displays it and "why is it at 1?" is the
// first question an operator asks.
type Controller struct {
	cfg  ControllerConfig
	now  func() time.Time
	load LoadAverage

	mu              sync.Mutex
	workers         int
	reason          string
	baselineSamples int
	baseline        time.Duration
}

// NewController builds a Controller that samples the real system clock and
// the real host's load average.
func NewController(cfg ControllerConfig) *Controller {
	return newController(cfg, time.Now, sampleLoadAverage)
}

// newController is NewController's implementation, parameterised by the
// clock and the load sampler so tests can inject both (see throttle_test.go)
// rather than depending on real wall-clock time or the real host's load.
func newController(cfg ControllerConfig, now func() time.Time, load LoadAverage) *Controller {
	cfg = cfg.withDefaults()
	return &Controller{
		cfg:     cfg,
		now:     now,
		load:    load,
		workers: cfg.MinWorkers,
		reason:  "starting at the configured minimum worker count",
	}
}

// SetConfig atomically replaces the controller's configuration, taking
// effect on the very next Workers()/Reason()/Observe() call — spec §9.2:
// "window, concurrency and batch size are all live-editable without a
// restart." The current worker count is immediately re-clamped into the
// new [MinWorkers, MaxWorkers] bounds rather than waiting for the next
// Observe to notice, so a lowered ceiling takes visible effect at once.
func (c *Controller) SetConfig(cfg ControllerConfig) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cfg = cfg.withDefaults()
	if c.workers < c.cfg.MinWorkers {
		c.workers = c.cfg.MinWorkers
	}
	if c.workers > c.cfg.MaxWorkers {
		c.workers = c.cfg.MaxWorkers
	}
}

// SetWindow live-edits just the schedule window, leaving every other
// setting untouched — the common case of an operator changing the
// backfill's allowed hours without wanting to also respecify concurrency
// and latency settings.
func (c *Controller) SetWindow(w Window) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cfg.Window = w
}

// Config returns the controller's current configuration.
func (c *Controller) Config() ControllerConfig {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cfg
}

// Workers returns the number of workers that should be in flight right
// now: zero whenever now is outside the configured schedule Window,
// otherwise the controller's current count -- always within [MinWorkers,
// MaxWorkers]; the hard ceiling and floor always win over anything Observe
// computed.
func (c *Controller) Workers() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.workersLocked()
}

func (c *Controller) workersLocked() int {
	if !c.cfg.Window.Contains(c.now()) {
		return 0
	}
	w := c.workers
	if w < c.cfg.MinWorkers {
		w = c.cfg.MinWorkers
	}
	if w > c.cfg.MaxWorkers {
		w = c.cfg.MaxWorkers
	}
	return w
}

// Reason returns a short, human-readable explanation of the current worker
// count, for the admin page (Task 7).
func (c *Controller) Reason() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.cfg.Window.Contains(c.now()) {
		return "outside the scheduled backfill window"
	}
	return c.reason
}

// Observe records one batch's per-entry latency measured at the given
// worker count, samples system load, and adjusts the worker count by at
// most one step, all within [MinWorkers, MaxWorkers]. The backfill lane
// calls this once per batch, between batches, never mid-batch (spec §9.2)
// -- Observe itself does no I/O other than the load sample, so it returns
// quickly.
//
// workers is why this takes two arguments. perEntryLatency is elapsed
// wall-clock divided by entries attempted, so it is an inverse THROUGHPUT
// and falls as workers are added even when nothing improved per worker.
// Comparing it directly against a baseline captured at MinWorkers, as this
// did before, made every multi-worker sample read as roughly workers-times
// healthier than the baseline no matter what the machine was doing: the
// latency arm could then only ever vote "step up" (whole-branch review,
// finding 2, folding in the deferred baseline finding). Multiplying it
// back out by the worker count in effect recovers the per-worker service
// time -- how long one worker takes for one entry -- which is flat under
// perfect scaling, rises under contention, and is therefore comparable
// across worker counts. That is what the baseline records and what
// LatencyMargin is measured against.
//
// While the controller is outside its schedule window, Observe still
// records the reason but leaves the worker count and baseline untouched,
// so that when the window reopens the controller resumes from where it
// left off rather than restarting its baseline.
func (c *Controller) Observe(perEntryLatency time.Duration, workers int) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.cfg.Window.Contains(c.now()) {
		c.reason = "outside the scheduled backfill window"
		return
	}

	if workers < 1 {
		workers = 1
	}
	serviceTime := perEntryLatency * time.Duration(workers)

	load, loadErr := 0.0, error(nil)
	if c.load != nil {
		load, loadErr = c.load()
	}

	if c.baselineSamples < controllerBaselineSamples {
		c.baselineSamples++
		// Running average, so one noisy first sample does not fully
		// determine the baseline.
		c.baseline += (serviceTime - c.baseline) / time.Duration(c.baselineSamples)
		c.workers = c.cfg.MinWorkers
		c.reason = fmt.Sprintf("establishing latency baseline (%d/%d batches)", c.baselineSamples, controllerBaselineSamples)
		return
	}

	switch {
	case loadErr == nil && load > c.cfg.LoadThreshold:
		c.stepDown(fmt.Sprintf("per-core load average %.2f exceeds threshold %.2f", load, c.cfg.LoadThreshold))
	case c.baseline > 0 && float64(serviceTime) > float64(c.baseline)*c.cfg.LatencyMargin:
		c.stepDown(fmt.Sprintf("per-worker service time %s exceeds %.1fx baseline %s", serviceTime, c.cfg.LatencyMargin, c.baseline))
	default:
		c.stepUp("per-worker service time and load average within bounds")
	}
}

// stepDown decreases the worker count by exactly one step, never below
// MinWorkers, and records reason.
func (c *Controller) stepDown(reason string) {
	if c.workers > c.cfg.MinWorkers {
		c.workers--
	}
	if c.workers < c.cfg.MinWorkers {
		c.workers = c.cfg.MinWorkers
	}
	c.reason = reason
}

// stepUp increases the worker count by exactly one step, never above
// MaxWorkers, and records reason.
func (c *Controller) stepUp(reason string) {
	if c.workers < c.cfg.MaxWorkers {
		c.workers++
	}
	if c.workers > c.cfg.MaxWorkers {
		c.workers = c.cfg.MaxWorkers
	}
	c.reason = reason
}

// sampleLoadAverage is the production LoadAverage: it reads the raw
// 1-minute load average from the host and divides it by the core count,
// so what the Controller compares against LoadThreshold is a per-core
// figure (see ControllerConfig.LoadThreshold for why an absolute one was
// unusable). It is never exercised by the controller unit tests
// (throttle_test.go), which inject a fixed LoadAverage instead -- per the
// task-6 brief, "a controller test that samples the host's real load
// average is not a test".
func sampleLoadAverage() (float64, error) {
	raw, err := rawLoadAverage()
	if err != nil {
		return 0, err
	}
	return normalizeLoad(raw), nil
}

// normalizeLoad divides a raw 1-minute load average by the number of
// usable cores, so 1.0 means "fully busy" on any machine. runtime.NumCPU
// cannot return less than 1, but the floor costs nothing and makes a
// division by zero impossible.
func normalizeLoad(raw float64) float64 {
	cores := runtime.NumCPU()
	if cores < 1 {
		cores = 1
	}
	return raw / float64(cores)
}

// rawLoadAverage reads the host's unnormalised 1-minute load average from
// /proc/loadavg on Linux, falling back to `sysctl -n vm.loadavg` on
// platforms (macOS/BSD) that have no /proc.
func rawLoadAverage() (float64, error) {
	if data, err := os.ReadFile("/proc/loadavg"); err == nil {
		fields := strings.Fields(string(data))
		if len(fields) > 0 {
			if v, err := strconv.ParseFloat(fields[0], 64); err == nil {
				return v, nil
			}
		}
		return 0, fmt.Errorf("throttle: unexpected /proc/loadavg contents: %q", data)
	}

	out, err := exec.Command("sysctl", "-n", "vm.loadavg").Output()
	if err != nil {
		return 0, fmt.Errorf("throttle: unable to read load average: %w", err)
	}
	fields := strings.Fields(strings.Trim(strings.TrimSpace(string(out)), "{}"))
	if len(fields) == 0 {
		return 0, fmt.Errorf("throttle: unexpected sysctl vm.loadavg output: %q", out)
	}
	v, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, fmt.Errorf("throttle: unable to parse load average %q: %w", fields[0], err)
	}
	return v, nil
}
