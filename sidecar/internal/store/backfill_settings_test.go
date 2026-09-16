// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package store // import "miniflux.app/v2/sidecar/internal/store"

import (
	"context"
	"testing"
)

// clearBackfillSettings deletes the singleton settings row, leaving the
// table in the "no operator has ever changed the config from this page"
// state -- testStore's database is shared and persists across test runs
// (it is not recreated per test), so a row left behind by an earlier test
// or an earlier run of this suite must not leak into a test asserting the
// no-row state. Mirrors clearEmbedderSettings exactly.
func clearBackfillSettings(t *testing.T, s *Store) {
	t.Helper()
	if _, err := s.db.Exec(`DELETE FROM search.backfill_settings`); err != nil {
		t.Fatalf("unable to clear backfill_settings: %v", err)
	}
}

func sampleBackfillSettings() BackfillSettings {
	return BackfillSettings{
		WindowStart:             2,
		WindowEnd:               7,
		MinWorkers:              1,
		MaxWorkers:              2,
		LoadThreshold:           1.5,
		BatchSize:               16,
		PageSize:                20,
		PollIntervalSeconds:     5,
		IdleResweepIntervalSecs: 900,
	}
}

// TestGetBackfillSettingsReturnsNilWhenNoRowExists is the state
// cmd/sidecar must be able to tell apart from any real row's content: "no
// operator has ever changed the backfill config from the admin page",
// which is what lets the SIDECAR_BACKFILL_* startup environment variables
// keep acting as the default (spec §9.2, mirroring spec §13.1's
// embedder-settings precedent).
func TestGetBackfillSettingsReturnsNilWhenNoRowExists(t *testing.T) {
	s := testStore(t)
	if err := s.Migrate(); err != nil {
		t.Fatalf("migrate failed: %v", err)
	}
	clearBackfillSettings(t, s)

	got, err := s.GetBackfillSettings(context.Background())
	if err != nil {
		t.Fatalf("GetBackfillSettings failed: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil settings on a fresh database, got %+v", got)
	}
}

// TestSetBackfillSettingsPersistsAndGetReadsItBack proves the round trip
// through the real database, not just the SQL compiling: a config change
// with every field set to a distinctive, non-default value must be
// exactly what a later read reports.
func TestSetBackfillSettingsPersistsAndGetReadsItBack(t *testing.T) {
	s := testStore(t)
	if err := s.Migrate(); err != nil {
		t.Fatalf("migrate failed: %v", err)
	}
	t.Cleanup(func() { clearBackfillSettings(t, s) })

	want := sampleBackfillSettings()
	if err := s.SetBackfillSettings(context.Background(), want); err != nil {
		t.Fatalf("SetBackfillSettings failed: %v", err)
	}

	got, err := s.GetBackfillSettings(context.Background())
	if err != nil {
		t.Fatalf("GetBackfillSettings failed: %v", err)
	}
	if got == nil {
		t.Fatalf("expected a persisted row after SetBackfillSettings, got nil")
	}
	if got.WindowStart != want.WindowStart || got.WindowEnd != want.WindowEnd ||
		got.MinWorkers != want.MinWorkers || got.MaxWorkers != want.MaxWorkers ||
		got.LoadThreshold != want.LoadThreshold || got.BatchSize != want.BatchSize ||
		got.PageSize != want.PageSize || got.PollIntervalSeconds != want.PollIntervalSeconds ||
		got.IdleResweepIntervalSecs != want.IdleResweepIntervalSecs {
		t.Fatalf("got %+v, want the same fields as %+v", got, want)
	}
	if got.UpdatedAt.IsZero() {
		t.Fatalf("expected UpdatedAt to be set, got the zero value")
	}
}

// TestSetBackfillSettingsOverwritesPreviousChoice proves the singleton
// upsert actually replaces the row rather than erroring on the second
// call (the CHECK(id = 1) constraint would reject a second row outright
// if this used a plain INSERT) -- mirrors
// TestSetEmbedderSettingsOverwritesPreviousChoice.
func TestSetBackfillSettingsOverwritesPreviousChoice(t *testing.T) {
	s := testStore(t)
	if err := s.Migrate(); err != nil {
		t.Fatalf("migrate failed: %v", err)
	}
	t.Cleanup(func() { clearBackfillSettings(t, s) })

	first := sampleBackfillSettings()
	if err := s.SetBackfillSettings(context.Background(), first); err != nil {
		t.Fatalf("first SetBackfillSettings failed: %v", err)
	}

	second := first
	second.MinWorkers = 1
	second.MaxWorkers = 1
	second.LoadThreshold = 4.0
	if err := s.SetBackfillSettings(context.Background(), second); err != nil {
		t.Fatalf("second SetBackfillSettings failed: %v", err)
	}

	got, err := s.GetBackfillSettings(context.Background())
	if err != nil {
		t.Fatalf("GetBackfillSettings failed: %v", err)
	}
	if got == nil || got.MaxWorkers != 1 || got.LoadThreshold != 4.0 {
		t.Fatalf("expected the second change to overwrite the first, got %+v", got)
	}

	var rowCount int
	if err := s.db.QueryRow(`SELECT count(*) FROM search.backfill_settings`).Scan(&rowCount); err != nil {
		t.Fatalf("unable to count backfill_settings rows: %v", err)
	}
	if rowCount != 1 {
		t.Fatalf("expected exactly one row after two changes, got %d", rowCount)
	}
}
