// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package store // import "miniflux.app/v2/sidecar/internal/store"

import (
	"context"
	"testing"
)

// clearEmbedderSettings deletes the singleton settings row, leaving the
// table in the "no operator has ever switched embedders" state --
// testStore's database is shared and persists across test runs (it is
// not recreated per test), so a row left behind by an earlier test or an
// earlier run of this suite must not leak into a test asserting the
// no-row state.
func clearEmbedderSettings(t *testing.T, s *Store) {
	t.Helper()
	if _, err := s.db.Exec(`DELETE FROM search.embedder_settings`); err != nil {
		t.Fatalf("unable to clear embedder_settings: %v", err)
	}
}

// TestGetEmbedderSettingsReturnsNilWhenNoRowExists is the state cmd/sidecar
// must be able to tell apart from any real row's content: "an operator has
// never switched embedders", which is what lets the startup environment
// variables keep acting as the default (spec §13.1).
func TestGetEmbedderSettingsReturnsNilWhenNoRowExists(t *testing.T) {
	s := testStore(t)
	if err := s.Migrate(); err != nil {
		t.Fatalf("migrate failed: %v", err)
	}
	clearEmbedderSettings(t, s)

	got, err := s.GetEmbedderSettings(context.Background())
	if err != nil {
		t.Fatalf("GetEmbedderSettings failed: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil settings on a fresh database, got %+v", got)
	}
}

// TestSetEmbedderSettingsPersistsAndGetReadsItBack proves the round trip
// through the real database, not just the SQL compiling: a switch to
// "remote" with a specific URL must be exactly what a later read reports.
func TestSetEmbedderSettingsPersistsAndGetReadsItBack(t *testing.T) {
	s := testStore(t)
	if err := s.Migrate(); err != nil {
		t.Fatalf("migrate failed: %v", err)
	}
	t.Cleanup(func() { clearEmbedderSettings(t, s) })

	if err := s.SetEmbedderSettings(context.Background(), "remote", "http://gpu-host:9000"); err != nil {
		t.Fatalf("SetEmbedderSettings failed: %v", err)
	}

	got, err := s.GetEmbedderSettings(context.Background())
	if err != nil {
		t.Fatalf("GetEmbedderSettings failed: %v", err)
	}
	if got == nil {
		t.Fatalf("expected a persisted row after SetEmbedderSettings, got nil")
	}
	if got.Kind != "remote" || got.RemoteURL != "http://gpu-host:9000" {
		t.Fatalf("got Kind=%q RemoteURL=%q, want Kind=%q RemoteURL=%q", got.Kind, got.RemoteURL, "remote", "http://gpu-host:9000")
	}
	if got.UpdatedAt.IsZero() {
		t.Fatalf("expected UpdatedAt to be set, got the zero value")
	}
}

// TestSetEmbedderSettingsOverwritesPreviousChoice proves the singleton
// upsert actually replaces the row rather than erroring on the second
// call (the CHECK(id = 1) constraint would reject a second row outright
// if this used a plain INSERT).
func TestSetEmbedderSettingsOverwritesPreviousChoice(t *testing.T) {
	s := testStore(t)
	if err := s.Migrate(); err != nil {
		t.Fatalf("migrate failed: %v", err)
	}
	t.Cleanup(func() { clearEmbedderSettings(t, s) })

	if err := s.SetEmbedderSettings(context.Background(), "remote", "http://first-host:9000"); err != nil {
		t.Fatalf("first SetEmbedderSettings failed: %v", err)
	}
	if err := s.SetEmbedderSettings(context.Background(), "local", ""); err != nil {
		t.Fatalf("second SetEmbedderSettings failed: %v", err)
	}

	got, err := s.GetEmbedderSettings(context.Background())
	if err != nil {
		t.Fatalf("GetEmbedderSettings failed: %v", err)
	}
	if got == nil || got.Kind != "local" || got.RemoteURL != "" {
		t.Fatalf("expected the second switch to overwrite the first, got %+v", got)
	}

	var rowCount int
	if err := s.db.QueryRow(`SELECT count(*) FROM search.embedder_settings`).Scan(&rowCount); err != nil {
		t.Fatalf("unable to count embedder_settings rows: %v", err)
	}
	if rowCount != 1 {
		t.Fatalf("expected exactly one row after two switches, got %d", rowCount)
	}
}
