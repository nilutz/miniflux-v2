// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package indexer // import "miniflux.app/v2/sidecar/internal/indexer"

import (
	"runtime"
	"testing"
	"time"
)

// newConfigTestBackfill builds a Backfill whose live-editable settings can
// be exercised without a store or an embedder: ApplyConfig and
// RuntimeConfig touch the Controller, the Indexer's batch size and the
// lane's own config, none of which do any I/O.
func newConfigTestBackfill(t *testing.T) *Backfill {
	t.Helper()
	idx := New(nil, nil)
	controller := newController(DefaultControllerConfig(), fixedClock(time.Now()), fixedLoad(0.1))
	return NewBackfill(idx, controller, BackfillConfig{})
}

func ptr[T any](v T) *T { return &v }

func TestParseWindow(t *testing.T) {
	cases := []struct {
		in         string
		wantStart  int
		wantEnd    int
		wantErr    bool
		wantAlways bool
	}{
		{in: "", wantAlways: true},
		{in: "always", wantAlways: true},
		{in: "  ALWAYS  ", wantAlways: true},
		{in: "02:00-07:00", wantStart: 2, wantEnd: 7},
		{in: "2-7", wantStart: 2, wantEnd: 7},
		{in: "22-06", wantStart: 22, wantEnd: 6},
		{in: "22:00 - 06:00", wantStart: 22, wantEnd: 6},
		{in: "02:30-07:00", wantErr: true},
		{in: "24-07", wantErr: true},
		{in: "-1-7", wantErr: true},
		{in: "morning", wantErr: true},
		{in: "07:00", wantErr: true},
	}

	for _, tc := range cases {
		got, err := ParseWindow(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Fatalf("ParseWindow(%q) should have failed, got %+v", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Fatalf("ParseWindow(%q) failed: %v", tc.in, err)
		}
		if tc.wantAlways {
			if !got.Contains(time.Date(2026, 1, 1, 13, 0, 0, 0, time.UTC)) {
				t.Fatalf("ParseWindow(%q) should mean always, got %+v", tc.in, got)
			}
			continue
		}
		if got.Start != tc.wantStart || got.End != tc.wantEnd {
			t.Fatalf("ParseWindow(%q) = %d-%d, want %d-%d", tc.in, got.Start, got.End, tc.wantStart, tc.wantEnd)
		}
	}
}

// The whole point of finding 1: a configuration change actually reaches
// the Controller, the Indexer and the lane, and is visible afterwards.
func TestApplyConfigReachesEveryKnob(t *testing.T) {
	b := newConfigTestBackfill(t)

	before := b.RuntimeConfig()
	if before.WindowStart != before.WindowEnd {
		t.Fatalf("setup: expected the default window to be \"always\", got %s", before.WindowDescription())
	}

	applied, err := b.ApplyConfig(ConfigPatch{
		Window:              ptr(Window{Start: 2, End: 7}),
		MinWorkers:          ptr(1),
		MaxWorkers:          ptr(2),
		BatchSize:           ptr(24),
		PageSize:            ptr(50),
		PollInterval:        ptr(30 * time.Second),
		IdleResweepInterval: ptr(5 * time.Minute),
	})
	if err != nil {
		t.Fatalf("ApplyConfig failed: %v", err)
	}

	if applied.WindowDescription() != "02:00-07:00" {
		t.Fatalf("expected the window to be 02:00-07:00, got %s", applied.WindowDescription())
	}
	if applied.MaxWorkers != 2 || applied.MinWorkers != 1 {
		t.Fatalf("expected a [1,2] worker band, got [%d,%d]", applied.MinWorkers, applied.MaxWorkers)
	}
	if applied.BatchSize != 24 {
		t.Fatalf("expected batch size 24, got %d", applied.BatchSize)
	}
	if applied.PageSize != 50 {
		t.Fatalf("expected page size 50, got %d", applied.PageSize)
	}
	if applied.PollIntervalSeconds != 30 {
		t.Fatalf("expected a 30s poll interval, got %v", applied.PollIntervalSeconds)
	}
	if applied.IdleResweepIntervalSecs != 300 {
		t.Fatalf("expected a 300s idle re-sweep interval, got %v", applied.IdleResweepIntervalSecs)
	}

	// And it reached the real machinery, not just the report: the
	// controller must refuse to run outside the new window, and the
	// indexer must report the new batch size.
	outside := newController(ControllerConfig{MinWorkers: 1, MaxWorkers: 2}, fixedClock(time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)), fixedLoad(0.1))
	outside.SetWindow(Window{Start: 2, End: 7})
	if got := outside.Workers(); got != 0 {
		t.Fatalf("sanity: a controller outside 02:00-07:00 must report 0 workers, got %d", got)
	}
	if got := b.idx.BatchSize(); got != 24 {
		t.Fatalf("expected the Indexer's own batch size to be 24, got %d", got)
	}
	if got := b.pageSize(); got != 50 {
		t.Fatalf("expected the lane's own page size to be 50, got %d", got)
	}
	if got := b.pollInterval(); got != 30*time.Second {
		t.Fatalf("expected the lane's own poll interval to be 30s, got %v", got)
	}
	if got := b.idleResweepInterval(); got != 5*time.Minute {
		t.Fatalf("expected the lane's own idle re-sweep interval to be 5m, got %v", got)
	}
}

// A patch leaves unmentioned settings exactly as they were, so applying
// one knob cannot clobber another.
func TestApplyConfigLeavesAbsentFieldsAlone(t *testing.T) {
	b := newConfigTestBackfill(t)

	if _, err := b.ApplyConfig(ConfigPatch{
		Window:     ptr(Window{Start: 1, End: 5}),
		MaxWorkers: ptr(2),
		BatchSize:  ptr(32),
	}); err != nil {
		t.Fatalf("ApplyConfig failed: %v", err)
	}

	applied, err := b.ApplyConfig(ConfigPatch{BatchSize: ptr(8)})
	if err != nil {
		t.Fatalf("ApplyConfig failed: %v", err)
	}
	if applied.BatchSize != 8 {
		t.Fatalf("expected batch size 8, got %d", applied.BatchSize)
	}
	if applied.WindowDescription() != "01:00-05:00" {
		t.Fatalf("expected the window to survive a batch-size-only change, got %s", applied.WindowDescription())
	}
	if applied.MaxWorkers != 2 {
		t.Fatalf("expected MaxWorkers to survive a batch-size-only change, got %d", applied.MaxWorkers)
	}
}

// Everything numeric is clamped rather than accepted or refused, and the
// report says what actually took effect.
func TestApplyConfigClampsOutOfRangeValues(t *testing.T) {
	b := newConfigTestBackfill(t)

	applied, err := b.ApplyConfig(ConfigPatch{
		MinWorkers:          ptr(-5),
		MaxWorkers:          ptr(10000),
		LoadThreshold:       ptr(999.0),
		BatchSize:           ptr(64), // spec §6.7: batch 64 is SLOWER than batch 8
		PageSize:            ptr(100000),
		PollInterval:        ptr(time.Millisecond),
		IdleResweepInterval: ptr(400 * time.Hour),
	})
	if err != nil {
		t.Fatalf("ApplyConfig should clamp, not fail: %v", err)
	}

	if applied.MinWorkers < 1 {
		t.Fatalf("expected MinWorkers to be floored at 1, got %d", applied.MinWorkers)
	}
	if applied.MaxWorkers != MaxAllowedWorkers() {
		t.Fatalf("expected MaxWorkers to be capped at this host's ceiling %d, got %d", MaxAllowedWorkers(), applied.MaxWorkers)
	}
	if applied.MaxWorkers > runtime.NumCPU() && runtime.NumCPU() > 1 {
		t.Fatalf("expected MaxWorkers never to exceed the core count %d, got %d", runtime.NumCPU(), applied.MaxWorkers)
	}
	if applied.LoadThreshold != MaxLoadThreshold {
		t.Fatalf("expected the load threshold to be capped at %v, got %v", MaxLoadThreshold, applied.LoadThreshold)
	}
	if applied.BatchSize != MaxBatchSize {
		t.Fatalf("expected batch size 64 to be clamped to %d, got %d", MaxBatchSize, applied.BatchSize)
	}
	if applied.PageSize != MaxPageSize {
		t.Fatalf("expected page size to be capped at %d, got %d", MaxPageSize, applied.PageSize)
	}
	if applied.PollIntervalSeconds != MinPollInterval.Seconds() {
		t.Fatalf("expected the poll interval to be floored at %v, got %vs", MinPollInterval, applied.PollIntervalSeconds)
	}
	if applied.IdleResweepIntervalSecs != MaxIdleResweepInterval.Seconds() {
		t.Fatalf("expected the idle re-sweep interval to be capped at %v, got %vs", MaxIdleResweepInterval, applied.IdleResweepIntervalSecs)
	}

	// Batch size below the floor too.
	applied, err = b.ApplyConfig(ConfigPatch{BatchSize: ptr(1)})
	if err != nil {
		t.Fatalf("ApplyConfig failed: %v", err)
	}
	if applied.BatchSize != MinBatchSize {
		t.Fatalf("expected batch size 1 to be clamped up to %d, got %d", MinBatchSize, applied.BatchSize)
	}
}

// MaxWorkers below MinWorkers must lower the floor, never quietly raise
// the ceiling: the ceiling is the safety property an operator set.
func TestApplyConfigNeverRaisesTheCeilingToMeetTheFloor(t *testing.T) {
	b := newConfigTestBackfill(t)

	applied, err := b.ApplyConfig(ConfigPatch{MinWorkers: ptr(4), MaxWorkers: ptr(1)})
	if err != nil {
		t.Fatalf("ApplyConfig failed: %v", err)
	}
	if applied.MaxWorkers != 1 {
		t.Fatalf("expected the requested ceiling of 1 to be honoured, got %d", applied.MaxWorkers)
	}
	if applied.MinWorkers != 1 {
		t.Fatalf("expected the floor to be lowered to the ceiling, got %d", applied.MinWorkers)
	}
}

// An out-of-range schedule window has no sensible nearest legal value, so
// it is rejected and nothing is applied.
func TestApplyConfigRejectsAnInvalidWindow(t *testing.T) {
	b := newConfigTestBackfill(t)
	before := b.RuntimeConfig()

	if _, err := b.ApplyConfig(ConfigPatch{Window: ptr(Window{Start: 2, End: 47}), BatchSize: ptr(32)}); err == nil {
		t.Fatal("expected an out-of-range window to be rejected")
	}
	if got := b.RuntimeConfig(); got.BatchSize != before.BatchSize {
		t.Fatalf("a rejected patch must not apply any of its other fields either; batch size moved from %d to %d", before.BatchSize, got.BatchSize)
	}
}
