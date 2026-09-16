// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main // import "miniflux.app/v2/sidecar/cmd/sidecar"

import (
	"testing"
	"time"

	"miniflux.app/v2/sidecar/internal/indexer"
	"miniflux.app/v2/sidecar/internal/store"
)

// TestResolveBackfillPatchPrefersPersistedRowOverEnvironmentVariable
// mirrors TestResolveEmbedderChoicePrefersPersistedRowOverEnvironmentVariable:
// cfg and persisted are set to DIFFERENT, genuinely conflicting values, so
// this only passes if resolveBackfillPatch actually prefers the persisted
// row, not merely "whichever argument happens to be non-zero".
func TestResolveBackfillPatchPrefersPersistedRowOverEnvironmentVariable(t *testing.T) {
	envMinWorkers := 2
	cfg := config{
		backfill: indexer.ConfigPatch{MinWorkers: &envMinWorkers},
	}
	persisted := &store.BackfillSettings{
		WindowStart: 2, WindowEnd: 7,
		MinWorkers: 1, MaxWorkers: 1,
		LoadThreshold:           4.0,
		BatchSize:               16,
		PageSize:                20,
		PollIntervalSeconds:     5,
		IdleResweepIntervalSecs: 900,
	}

	got := resolveBackfillPatch(cfg, persisted)

	if got.MinWorkers == nil || *got.MinWorkers != 1 {
		t.Fatalf("MinWorkers = %v, want 1 (the persisted row's value) -- the environment variable's value (%d) must not have won", got.MinWorkers, envMinWorkers)
	}
	if got.MaxWorkers == nil || *got.MaxWorkers != 1 {
		t.Fatalf("MaxWorkers = %v, want 1 (the persisted row's value)", got.MaxWorkers)
	}
	if got.LoadThreshold == nil || *got.LoadThreshold != 4.0 {
		t.Fatalf("LoadThreshold = %v, want 4.0 (the persisted row's value)", got.LoadThreshold)
	}
	if got.Window == nil || got.Window.Start != 2 || got.Window.End != 7 {
		t.Fatalf("Window = %v, want {2 7} (the persisted row's value)", got.Window)
	}
	if got.PollInterval == nil || *got.PollInterval != 5*time.Second {
		t.Fatalf("PollInterval = %v, want 5s (the persisted row's value)", got.PollInterval)
	}
	if got.IdleResweepInterval == nil || *got.IdleResweepInterval != 900*time.Second {
		t.Fatalf("IdleResweepInterval = %v, want 900s (the persisted row's value)", got.IdleResweepInterval)
	}
}

// TestResolveBackfillPatchFallsBackToEnvironmentWhenNoRowExists is spec
// §9.2's other half, mirroring
// TestResolveEmbedderChoiceFallsBackToEnvironmentWhenNoRowExists:
// persisted is nil (store.GetBackfillSettings' own contract for "no
// operator has ever changed the config from the admin page"), and the
// environment-derived patch must be exactly what is used, unmodified.
func TestResolveBackfillPatchFallsBackToEnvironmentWhenNoRowExists(t *testing.T) {
	envMinWorkers := 3
	envPatch := indexer.ConfigPatch{MinWorkers: &envMinWorkers}
	cfg := config{backfill: envPatch}

	got := resolveBackfillPatch(cfg, nil)

	if got.MinWorkers != envPatch.MinWorkers {
		t.Fatalf("expected resolveBackfillPatch to return cfg.backfill unmodified when no row exists, got a different MinWorkers pointer/value")
	}
	if got.MinWorkers == nil || *got.MinWorkers != 3 {
		t.Fatalf("MinWorkers = %v, want 3 (cfg's own value, no persisted row)", got.MinWorkers)
	}
}
