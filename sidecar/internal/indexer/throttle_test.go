// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package indexer // import "miniflux.app/v2/sidecar/internal/indexer"

import (
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// fixedClock returns a func() time.Time that always reports t, for
// injecting a controllable clock into newController — a controller test
// that depends on wall-clock time passing for real would be flaky, and one
// that samples the host's real load average would not be a test at all.
func fixedClock(t time.Time) func() time.Time {
	return func() time.Time { return t }
}

// fixedLoad returns a LoadAverage that always reports v with no error.
func fixedLoad(v float64) LoadAverage {
	return func() (float64, error) { return v, nil }
}

// healthyBaselineLatency is a per-batch latency comfortably reported as
// "healthy" once a baseline has been established from repeated observations
// at this same value.
const healthyBaselineLatency = 100 * time.Millisecond

// establishBaseline feeds the controller enough consistent, low-load
// observations to move it past its baseline-establishment phase, so tests
// of the subsequent up/down behaviour are not entangled with that startup
// transient.
func establishBaseline(c *Controller) {
	for i := 0; i < controllerBaselineSamples; i++ {
		observeHealthy(c)
	}
}

// observeHealthy feeds one observation describing a machine that is
// scaling perfectly at the controller's CURRENT worker count: per-worker
// service time exactly at healthyBaselineLatency, which means the
// per-entry latency the lane would actually measure is that divided by the
// worker count. Observe multiplies it back out, so this reads as "no
// degradation" at every worker count -- which is the point: feeding a
// constant per-entry latency as workers climb would describe a machine
// whose per-worker service time is DOUBLING each step, and the controller
// is now supposed to notice that.
func observeHealthy(c *Controller) {
	w := c.Workers()
	if w < 1 {
		w = 1
	}
	c.Observe(healthyBaselineLatency/time.Duration(w), w)
}

// observeAtCurrentWorkers feeds one observation of an explicit per-entry
// latency at whatever worker count the controller is currently reporting.
func observeAtCurrentWorkers(c *Controller, perEntryLatency time.Duration) {
	w := c.Workers()
	if w < 1 {
		w = 1
	}
	c.Observe(perEntryLatency, w)
}

// 1. Starts at the configured minimum worker count.
func TestControllerStartsAtMinWorkers(t *testing.T) {
	cfg := ControllerConfig{MinWorkers: 1, MaxWorkers: 4, LatencyMargin: 1.5, LoadThreshold: 0.8}
	c := newController(cfg, fixedClock(time.Now()), fixedLoad(0.1))

	if got := c.Workers(); got != cfg.MinWorkers {
		t.Fatalf("expected a freshly constructed controller to report MinWorkers=%d, got %d", cfg.MinWorkers, got)
	}
}

// 2. Increases workers, up to the ceiling, while observed latency stays
// near baseline.
func TestControllerIncreasesTowardCeilingWhenHealthy(t *testing.T) {
	cfg := ControllerConfig{MinWorkers: 1, MaxWorkers: 4, LatencyMargin: 1.5, LoadThreshold: 0.8}
	c := newController(cfg, fixedClock(time.Now()), fixedLoad(0.1))

	establishBaseline(c)
	if got := c.Workers(); got != cfg.MinWorkers {
		t.Fatalf("expected workers to still be at MinWorkers=%d right after establishing baseline, got %d", cfg.MinWorkers, got)
	}

	seen := map[int]bool{c.Workers(): true}
	for i := 0; i < 10; i++ {
		observeHealthy(c)
		seen[c.Workers()] = true
	}

	if got := c.Workers(); got != cfg.MaxWorkers {
		t.Fatalf("expected sustained healthy observations to climb to MaxWorkers=%d, got %d", cfg.MaxWorkers, got)
	}
	// One step at a time: every worker count between Min and Max must have
	// been visited on the way up, none skipped.
	for w := cfg.MinWorkers; w <= cfg.MaxWorkers; w++ {
		if !seen[w] {
			t.Fatalf("expected the controller to pass through worker count %d on its way to the ceiling, but it never reported it; seen=%v", w, seen)
		}
	}
}

// 3. Decreases workers when observed latency degrades past the threshold.
func TestControllerDecreasesWhenLatencyDegrades(t *testing.T) {
	cfg := ControllerConfig{MinWorkers: 1, MaxWorkers: 4, LatencyMargin: 1.5, LoadThreshold: 0.8}
	c := newController(cfg, fixedClock(time.Now()), fixedLoad(0.1))

	establishBaseline(c)
	for i := 0; i < 10; i++ {
		observeHealthy(c)
	}
	if got := c.Workers(); got != cfg.MaxWorkers {
		t.Fatalf("setup failed: expected to reach MaxWorkers=%d before testing degradation, got %d", cfg.MaxWorkers, got)
	}

	// Per-worker service time well past LatencyMargin x baseline, expressed
	// as the per-entry latency the lane would measure at this worker count.
	degraded := time.Duration(float64(healthyBaselineLatency) * (cfg.LatencyMargin + 0.5) / float64(c.Workers()))
	observeAtCurrentWorkers(c, degraded)

	if got := c.Workers(); got != cfg.MaxWorkers-1 {
		t.Fatalf("expected one degraded batch to step workers down by exactly one, from %d to %d, got %d",
			cfg.MaxWorkers, cfg.MaxWorkers-1, got)
	}
}

// 4. Never exceeds MaxWorkers regardless of observations.
func TestControllerNeverExceedsMaxWorkers(t *testing.T) {
	cfg := ControllerConfig{MinWorkers: 1, MaxWorkers: 2, LatencyMargin: 1.5, LoadThreshold: 0.8}
	c := newController(cfg, fixedClock(time.Now()), fixedLoad(0.0))

	establishBaseline(c)
	for i := 0; i < 500; i++ {
		observeAtCurrentWorkers(c, 1*time.Nanosecond) // absurdly fast batches, never degraded
		if got := c.Workers(); got > cfg.MaxWorkers {
			t.Fatalf("controller reported %d workers, exceeding the hard ceiling MaxWorkers=%d (observation #%d)", got, cfg.MaxWorkers, i)
		}
	}
	if got := c.Workers(); got != cfg.MaxWorkers {
		t.Fatalf("expected sustained excellent observations to sit at the ceiling MaxWorkers=%d, got %d", cfg.MaxWorkers, got)
	}
}

// 5. Never drops below MinWorkers.
func TestControllerNeverDropsBelowMinWorkers(t *testing.T) {
	cfg := ControllerConfig{MinWorkers: 1, MaxWorkers: 4, LatencyMargin: 1.5, LoadThreshold: 0.8}
	c := newController(cfg, fixedClock(time.Now()), fixedLoad(99.0)) // absurd load, every batch

	establishBaseline(c)
	for i := 0; i < 500; i++ {
		observeAtCurrentWorkers(c, 10*time.Second) // absurdly slow batches too
		if got := c.Workers(); got < cfg.MinWorkers {
			t.Fatalf("controller reported %d workers, below the floor MinWorkers=%d (observation #%d)", got, cfg.MinWorkers, i)
		}
	}
	if got := c.Workers(); got != cfg.MinWorkers {
		t.Fatalf("expected sustained terrible observations to sit at the floor MinWorkers=%d, got %d", cfg.MinWorkers, got)
	}
}

// 6. Outside its window, reports zero workers.
func TestControllerOutsideWindowReportsZeroWorkers(t *testing.T) {
	cfg := ControllerConfig{
		MinWorkers: 1, MaxWorkers: 4,
		LatencyMargin: 1.5, LoadThreshold: 0.8,
		Window: Window{Start: 2, End: 7}, // 02:00-07:00
	}

	outsideWindow := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC) // noon: outside 02:00-07:00
	c := newController(cfg, fixedClock(outsideWindow), fixedLoad(0.1))

	if got := c.Workers(); got != 0 {
		t.Fatalf("expected a controller outside its scheduled window to report zero workers, got %d", got)
	}

	// Even after healthy observations, still zero: the window always wins.
	establishBaseline(c)
	for i := 0; i < 5; i++ {
		observeHealthy(c)
	}
	if got := c.Workers(); got != 0 {
		t.Fatalf("expected the controller to remain at zero workers throughout while outside its window, got %d", got)
	}
}

// A zero-value ControllerConfig must not stall the lane forever. Before
// withDefaults existed, ControllerConfig{} set MinWorkers=MaxWorkers=0,
// which workersLocked clamped right back to 0 -- Backfill.start reads any
// workers<=0 exactly like "outside the schedule window" and polls
// forever, silently, since Reason() would still claim "starting at the
// configured minimum worker count".
func TestControllerZeroValueConfigDoesNotStall(t *testing.T) {
	c := newController(ControllerConfig{}, fixedClock(time.Now()), fixedLoad(0.1))

	if got := c.Workers(); got < 1 {
		t.Fatalf("expected a zero-value ControllerConfig to still report at least 1 worker, got %d", got)
	}

	// And it must keep working, not just report a healthy-looking number
	// once and then stall.
	establishBaseline(c)
	for i := 0; i < 5; i++ {
		observeHealthy(c)
		if got := c.Workers(); got < 1 {
			t.Fatalf("expected at least 1 worker after Observe, got %d", got)
		}
	}
}

// A misconfigured MaxWorkers below MinWorkers must not leave the
// controller permanently wedged below its own stated floor either.
func TestControllerMaxBelowMinIsClampedUpToMin(t *testing.T) {
	c := newController(ControllerConfig{MinWorkers: 3, MaxWorkers: 1}, fixedClock(time.Now()), fixedLoad(0.1))

	if got := c.Workers(); got < 3 {
		t.Fatalf("expected MaxWorkers below MinWorkers=3 to be raised to match, got %d workers", got)
	}
}

// SetConfig takes effect immediately -- both lowering the ceiling (which
// must re-clamp the current count right away) and changing the window.
func TestControllerSetConfigTakesEffectLive(t *testing.T) {
	cfg := ControllerConfig{MinWorkers: 1, MaxWorkers: 4, LatencyMargin: 1.5, LoadThreshold: 0.8}
	now := time.Now()
	c := newController(cfg, fixedClock(now), fixedLoad(0.1))

	establishBaseline(c)
	for i := 0; i < 10; i++ {
		observeHealthy(c)
	}
	if got := c.Workers(); got != 4 {
		t.Fatalf("setup failed: expected to reach MaxWorkers=4, got %d", got)
	}

	c.SetConfig(ControllerConfig{MinWorkers: 1, MaxWorkers: 2, LatencyMargin: 1.5, LoadThreshold: 0.8})
	if got := c.Workers(); got != 2 {
		t.Fatalf("expected SetConfig to immediately re-clamp the worker count to the new MaxWorkers=2, got %d", got)
	}

	c.SetWindow(Window{Start: 2, End: 7})
	outsideWindow := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	c2 := newController(cfg, fixedClock(outsideWindow), fixedLoad(0.1))
	c2.SetWindow(Window{Start: 2, End: 7})
	if got := c2.Workers(); got != 0 {
		t.Fatalf("expected SetWindow to take effect immediately, got %d workers outside the new window", got)
	}
}

// The zero value of Window must mean "always", never "never" -- a
// freshly-constructed ControllerConfig{} (as any caller might build
// incrementally) must not silently make Workers() report zero forever.
func TestWindowZeroValueMeansAlways(t *testing.T) {
	var w Window
	for hour := 0; hour < 24; hour++ {
		got := time.Date(2026, 1, 1, hour, 0, 0, 0, time.UTC)
		if !w.Contains(got) {
			t.Fatalf("expected the zero-value Window to contain every hour of day, but it excluded hour %d", hour)
		}
	}
}

// A window that wraps past midnight (e.g. 22:00-06:00) is contained
// correctly on both sides of midnight, and excludes the hours in between.
func TestWindowWrapsPastMidnight(t *testing.T) {
	w := Window{Start: 22, End: 6}
	cases := []struct {
		hour int
		want bool
	}{
		{23, true},
		{0, true},
		{5, true},
		{6, false},
		{12, false},
		{21, false},
		{22, true},
	}
	for _, c := range cases {
		got := time.Date(2026, 1, 1, c.hour, 0, 0, 0, time.UTC)
		if w.Contains(got) != c.want {
			t.Fatalf("Window{22,6}.Contains(hour=%d) = %v, want %v", c.hour, !c.want, c.want)
		}
	}
}

// mutableLoad returns a LoadAverage whose reported value a test can change
// between observations, plus the setter.
func mutableLoad(initial float64) (LoadAverage, func(float64)) {
	var mu sync.Mutex
	v := initial
	return func() (float64, error) {
			mu.Lock()
			defer mu.Unlock()
			return v, nil
		}, func(n float64) {
			mu.Lock()
			v = n
			mu.Unlock()
		}
}

// normalizeLoad must turn a raw load average into a per-core figure, so
// that LoadThreshold means "this fraction of the machine is busy" on an
// 8-core laptop and a 64-core server alike.
func TestNormalizeLoadIsPerCore(t *testing.T) {
	cores := float64(runtime.NumCPU())

	if got := normalizeLoad(cores); got != 1.0 {
		t.Fatalf("a raw load average equal to the core count (%v) must normalise to 1.0, got %v", cores, got)
	}
	if got := normalizeLoad(cores / 2); got != 0.5 {
		t.Fatalf("a half-busy machine must normalise to 0.5, got %v", got)
	}
	if got := normalizeLoad(0); got != 0 {
		t.Fatalf("an idle machine must normalise to 0, got %v", got)
	}
	if got := normalizeLoad(2 * cores); got != 2.0 {
		t.Fatalf("a doubly-oversubscribed machine must normalise to 2.0, got %v", got)
	}
}

// The step-UP arm has to work against a load reading that is large in
// ABSOLUTE terms but healthy per core. This is the regression test for the
// one-way ratchet: with the raw load average fed in unnormalised, two
// CPU-bound embedding workers pushed it past LoadThreshold=0.8 within a
// minute on any machine and held it there, so every Observe after the
// baseline took the load branch and stepped down, and the condition never
// cleared again -- one worker for the entire backfill.
func TestControllerStepsUpUnderHealthyPerCoreLoad(t *testing.T) {
	cfg := ControllerConfig{MinWorkers: 1, MaxWorkers: 4, LatencyMargin: 1.5, LoadThreshold: 0.8}

	// Exactly the shape of reading that used to wedge this: half the
	// machine's cores busy, which on any multi-core host is an absolute
	// load average well above 0.8.
	raw := float64(runtime.NumCPU()) * 0.5
	if raw <= cfg.LoadThreshold {
		t.Skipf("this host reports only %d core(s); a half-busy raw load average (%v) is not above the threshold, so there is nothing to distinguish", runtime.NumCPU(), raw)
	}
	c := newController(cfg, fixedClock(time.Now()), fixedLoad(normalizeLoad(raw)))

	establishBaseline(c)
	for i := 0; i < 10; i++ {
		observeHealthy(c)
	}

	if got := c.Workers(); got != cfg.MaxWorkers {
		t.Fatalf("expected a half-busy machine (raw 1-minute load %.2f on %d cores, %.2f per core) to be healthy enough to climb to MaxWorkers=%d, got %d workers with reason %q",
			raw, runtime.NumCPU(), normalizeLoad(raw), cfg.MaxWorkers, got, c.Reason())
	}
}

// Both arms, in sequence, on one controller: it steps down while load is
// genuinely over the per-core threshold, and steps back UP once the load
// clears. The recovery half is the one that was dead in production.
func TestControllerStepsBackUpAfterLoadClears(t *testing.T) {
	cfg := ControllerConfig{MinWorkers: 1, MaxWorkers: 4, LatencyMargin: 1.5, LoadThreshold: 0.8}
	load, setLoad := mutableLoad(0.2)
	c := newController(cfg, fixedClock(time.Now()), load)

	establishBaseline(c)
	for i := 0; i < 10; i++ {
		observeHealthy(c)
	}
	if got := c.Workers(); got != cfg.MaxWorkers {
		t.Fatalf("setup failed: expected to climb to MaxWorkers=%d under a quiet machine, got %d", cfg.MaxWorkers, got)
	}

	// The machine gets genuinely oversubscribed: 1.5 runnable tasks per
	// core, well past the 0.8 per-core threshold.
	setLoad(1.5)
	for i := 0; i < 10; i++ {
		observeHealthy(c)
	}
	if got := c.Workers(); got != cfg.MinWorkers {
		t.Fatalf("expected sustained per-core load 1.5 to ratchet down to MinWorkers=%d, got %d (reason %q)", cfg.MinWorkers, got, c.Reason())
	}

	// ...and then settles.
	setLoad(0.2)
	observeHealthy(c)
	if got := c.Workers(); got != cfg.MinWorkers+1 {
		t.Fatalf("expected exactly one step back up to %d once load cleared, got %d (reason %q)", cfg.MinWorkers+1, got, c.Reason())
	}
	for i := 0; i < 10; i++ {
		observeHealthy(c)
	}
	if got := c.Workers(); got != cfg.MaxWorkers {
		t.Fatalf("expected the controller to recover all the way to MaxWorkers=%d once load cleared, got %d (reason %q)", cfg.MaxWorkers, got, c.Reason())
	}
}

// The latency baseline is established at MinWorkers, so a multi-worker
// sample is only comparable to it once the worker count is divided back
// out. Before that, a batch whose per-entry latency was UNCHANGED at two
// workers -- i.e. adding a worker bought nothing, per-worker service time
// doubled -- read as twice as healthy as the baseline, and the latency arm
// could only ever vote to step up.
func TestControllerLatencyBaselineIsComparedPerWorker(t *testing.T) {
	cfg := ControllerConfig{MinWorkers: 1, MaxWorkers: 4, LatencyMargin: 1.5, LoadThreshold: 0.8}

	t.Run("perfect scaling stays healthy", func(t *testing.T) {
		c := newController(cfg, fixedClock(time.Now()), fixedLoad(0.1))
		establishBaseline(c)
		observeHealthy(c)
		if got := c.Workers(); got != 2 {
			t.Fatalf("setup failed: expected one healthy step to 2 workers, got %d", got)
		}
		// Two workers, per-entry latency halved: service time flat.
		c.Observe(healthyBaselineLatency/2, 2)
		if got := c.Workers(); got != 3 {
			t.Fatalf("expected a perfectly-scaling 2-worker batch to still read healthy and step up to 3, got %d (reason %q)", got, c.Reason())
		}
	})

	t.Run("no scaling reads as degraded", func(t *testing.T) {
		c := newController(cfg, fixedClock(time.Now()), fixedLoad(0.1))
		establishBaseline(c)
		observeHealthy(c)
		if got := c.Workers(); got != 2 {
			t.Fatalf("setup failed: expected one healthy step to 2 workers, got %d", got)
		}
		// Two workers, per-entry latency unchanged: the second worker
		// bought nothing, so per-worker service time doubled -- past
		// LatencyMargin=1.5 -- and the controller must step back down.
		c.Observe(healthyBaselineLatency, 2)
		if got := c.Workers(); got != 1 {
			t.Fatalf("expected a 2-worker batch with unchanged per-entry latency to step back down to 1, got %d (reason %q)", got, c.Reason())
		}
		if !strings.Contains(c.Reason(), "service time") {
			t.Fatalf("expected the recorded reason to name per-worker service time, got %q", c.Reason())
		}
	})
}
