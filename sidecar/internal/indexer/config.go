// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package indexer // import "miniflux.app/v2/sidecar/internal/indexer"

import (
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Spec §9.2 requires three knobs, "all live-editable without a restart":
// the schedule window, the concurrency ceiling, and the embedding batch
// size. The setters for those existed (Controller.SetConfig,
// Controller.SetWindow, Indexer.SetBatchSize, Backfill.SetPageSize,
// Backfill.SetPollInterval) but had no non-test caller whatsoever — no
// flag, no environment variable, no endpoint — so the window was
// permanently "always", concurrency permanently [1,2] and batch size
// permanently 16. For a 41-hour CPU-saturating job on a personal machine
// an unbounded window is exactly what §9.2's window exists to prevent.
//
// This file is the one place that decides what a valid value is, so the
// startup path (cmd/sidecar's environment variables) and the runtime path
// (internal/web's POST /api/backfill/config) cannot disagree about it.

// Bounds on the live-editable knobs. Numeric values outside these are
// CLAMPED, not rejected: every one of them has a meaningful nearest legal
// value, and a backfill that refuses to start because a worker count was
// one too high helps nobody. A malformed schedule window is different —
// there is no sensible nearest window to a typo — and is rejected.
const (
	MinPageSize = 1
	MaxPageSize = 500

	MinPollInterval = 1 * time.Second
	MaxPollInterval = 10 * time.Minute

	MinIdleResweepInterval = 1 * time.Second
	MaxIdleResweepInterval = 24 * time.Hour

	MinLoadThreshold = 0.1
	MaxLoadThreshold = 8.0
)

// MaxAllowedWorkers is the hard ceiling on MaxWorkers, whatever an
// operator asks for. Embedding is CPU-bound against one shared,
// unconstrained ORT session (spec §6.7), so more workers than the machine
// has cores cannot buy throughput and only costs contention — and §6.7
// measured the optimum at 2 on an 8-core machine, so this is a guard
// rail, not a recommendation.
func MaxAllowedWorkers() int {
	if n := runtime.NumCPU(); n > 1 {
		return n
	}
	return 1
}

// RuntimeConfig is the full set of live-editable settings, as they
// currently stand. It is what a configuration change reports back and what
// the admin page renders.
type RuntimeConfig struct {
	WindowStart int `json:"window_start"` // hour of day, inclusive
	WindowEnd   int `json:"window_end"`   // hour of day, exclusive; equal to WindowStart means "always"

	MinWorkers    int     `json:"min_workers"`
	MaxWorkers    int     `json:"max_workers"`
	LoadThreshold float64 `json:"load_threshold"` // per core; see ControllerConfig.LoadThreshold

	BatchSize int `json:"batch_size"` // passages per forward pass
	PageSize  int `json:"page_size"`  // pending entry ids fetched per pagination page

	PollIntervalSeconds     float64 `json:"poll_interval_seconds"`
	IdleResweepIntervalSecs float64 `json:"idle_resweep_interval_seconds"`
	MaxAllowedWorkersOnHost int     `json:"max_allowed_workers"` // this host's ceiling, so the admin page can say why a request was clamped
	MinBatchSizeAllowed     int     `json:"min_batch_size"`
	MaxBatchSizeAllowed     int     `json:"max_batch_size"`
}

// WindowDescription renders the schedule window the way the admin page and
// the startup log line should say it.
func (c RuntimeConfig) WindowDescription() string {
	if c.WindowStart == c.WindowEnd {
		return "always"
	}
	return fmt.Sprintf("%02d:00-%02d:00", c.WindowStart, c.WindowEnd)
}

// ConfigPatch is a partial configuration change: every field is optional,
// and a nil field leaves that setting exactly as it is. Both the startup
// path and the runtime endpoint build one of these.
type ConfigPatch struct {
	Window              *Window
	MinWorkers          *int
	MaxWorkers          *int
	LoadThreshold       *float64
	BatchSize           *int
	PageSize            *int
	PollInterval        *time.Duration
	IdleResweepInterval *time.Duration
}

// IsEmpty reports whether the patch would change nothing.
func (p ConfigPatch) IsEmpty() bool {
	return p.Window == nil && p.MinWorkers == nil && p.MaxWorkers == nil &&
		p.LoadThreshold == nil && p.BatchSize == nil && p.PageSize == nil &&
		p.PollInterval == nil && p.IdleResweepInterval == nil
}

// ParseWindow parses a schedule window as an operator writes it: "always"
// (or the empty string) for no restriction, or an hour range as
// "02:00-07:00" or "2-7". Minutes are accepted only as ":00" — Window has
// hour granularity (spec §9.2 says "wall-clock hours"), and silently
// rounding 02:30 to 02:00 would be worse than saying so.
//
// A range whose start equals its end means "always", matching Window's own
// zero-value rule; a range whose end is before its start wraps past
// midnight, e.g. "22-06".
func ParseWindow(s string) (Window, error) {
	trimmed := strings.ToLower(strings.TrimSpace(s))
	if trimmed == "" || trimmed == "always" {
		return Window{}, nil
	}

	start, end, found := strings.Cut(trimmed, "-")
	if !found {
		return Window{}, fmt.Errorf("indexer: schedule window %q must be \"always\" or an hour range such as \"02:00-07:00\"", s)
	}

	startHour, err := parseWindowHour(start)
	if err != nil {
		return Window{}, err
	}
	endHour, err := parseWindowHour(end)
	if err != nil {
		return Window{}, err
	}

	return Window{Start: startHour, End: endHour}, nil
}

func parseWindowHour(s string) (int, error) {
	s = strings.TrimSpace(s)
	hourPart, minutePart, hasMinutes := strings.Cut(s, ":")
	if hasMinutes && strings.TrimSpace(minutePart) != "00" {
		return 0, fmt.Errorf("indexer: schedule window bound %q must be on the hour; minutes other than \":00\" are not supported", s)
	}

	hour, err := strconv.Atoi(strings.TrimSpace(hourPart))
	if err != nil {
		return 0, fmt.Errorf("indexer: schedule window bound %q is not a number", s)
	}
	if hour < 0 || hour > 23 {
		return 0, fmt.Errorf("indexer: schedule window bound %q is not an hour of day (0-23)", s)
	}
	return hour, nil
}

// RuntimeConfig returns the lane's current live-editable settings, read
// from the places that actually hold them — the Controller, the Indexer
// and this Backfill's own config — rather than from a copy that could
// drift out of step with them.
func (b *Backfill) RuntimeConfig() RuntimeConfig {
	ctrl := b.controller.Config()

	b.mu.Lock()
	cfg := b.cfg
	b.mu.Unlock()

	return RuntimeConfig{
		WindowStart:             ctrl.Window.Start,
		WindowEnd:               ctrl.Window.End,
		MinWorkers:              ctrl.MinWorkers,
		MaxWorkers:              ctrl.MaxWorkers,
		LoadThreshold:           ctrl.LoadThreshold,
		BatchSize:               b.idx.BatchSize(),
		PageSize:                cfg.PageSize,
		PollIntervalSeconds:     cfg.PollInterval.Seconds(),
		IdleResweepIntervalSecs: cfg.IdleResweepInterval.Seconds(),
		MaxAllowedWorkersOnHost: MaxAllowedWorkers(),
		MinBatchSizeAllowed:     MinBatchSize,
		MaxBatchSizeAllowed:     MaxBatchSize,
	}
}

// ApplyConfig validates, clamps and applies a partial configuration
// change, returning the settings as they stand afterwards. It is the only
// caller of the live-edit setters, so that "what is a legal value" is
// decided once rather than once per entry point.
//
// It returns an error only for input with no sensible nearest legal value
// (an unparseable or out-of-range schedule window); everything else is
// clamped into range, and the returned RuntimeConfig is what actually took
// effect — a caller that cares whether it got what it asked for should
// compare, and the admin page shows the result for exactly that reason.
//
// Nothing here stops or restarts the lane. Every setting it touches is
// read between batches, never mid-batch, by machinery that was already
// lock-safe.
func (b *Backfill) ApplyConfig(patch ConfigPatch) (RuntimeConfig, error) {
	if patch.Window != nil {
		w := *patch.Window
		if w.Start < 0 || w.Start > 23 || w.End < 0 || w.End > 23 {
			return b.RuntimeConfig(), fmt.Errorf("indexer: schedule window hours must be 0-23, got %d-%d", w.Start, w.End)
		}
	}

	// The controller's settings move together, through one SetConfig, so a
	// change to both bounds can never be observed half-applied. A
	// window-only change goes through SetWindow instead, which leaves
	// every other controller field alone.
	ctrl := b.controller.Config()
	touchedController := false

	if patch.MinWorkers != nil {
		ctrl.MinWorkers = clampInt(*patch.MinWorkers, 1, MaxAllowedWorkers())
		touchedController = true
	}
	if patch.MaxWorkers != nil {
		ctrl.MaxWorkers = clampInt(*patch.MaxWorkers, 1, MaxAllowedWorkers())
		touchedController = true
	}
	if patch.LoadThreshold != nil {
		ctrl.LoadThreshold = clampFloat(*patch.LoadThreshold, MinLoadThreshold, MaxLoadThreshold)
		touchedController = true
	}
	if ctrl.MaxWorkers < ctrl.MinWorkers {
		// withDefaults would raise MaxWorkers to match, which silently
		// grants more concurrency than was asked for. Lower MinWorkers to
		// the stated ceiling instead: the ceiling is the safety property.
		ctrl.MinWorkers = ctrl.MaxWorkers
	}

	switch {
	case touchedController:
		if patch.Window != nil {
			ctrl.Window = *patch.Window
		}
		b.controller.SetConfig(ctrl)
	case patch.Window != nil:
		b.controller.SetWindow(*patch.Window)
	}

	if patch.BatchSize != nil {
		// SetBatchSize applies spec §6.7's own [8, 32] clamp.
		b.idx.SetBatchSize(*patch.BatchSize)
	}
	if patch.PageSize != nil {
		b.SetPageSize(clampInt(*patch.PageSize, MinPageSize, MaxPageSize))
	}
	if patch.PollInterval != nil {
		b.SetPollInterval(clampDuration(*patch.PollInterval, MinPollInterval, MaxPollInterval))
	}
	if patch.IdleResweepInterval != nil {
		b.SetIdleResweepInterval(clampDuration(*patch.IdleResweepInterval, MinIdleResweepInterval, MaxIdleResweepInterval))
	}

	return b.RuntimeConfig(), nil
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func clampFloat(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func clampDuration(v, lo, hi time.Duration) time.Duration {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
