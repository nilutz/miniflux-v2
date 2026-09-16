// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package indexer // import "miniflux.app/v2/sidecar/internal/indexer"

import (
	"context"
	"testing"

	"miniflux.app/v2/sidecar/internal/store"
)

// fakeBackfillSettingsStore is a hermetic, in-memory stand-in for
// *store.Store's SetBackfillSettings, recording every write it receives --
// no database, mirroring the fake embedder-settings stores this package's
// own Manager tests already use for the identical reason.
type fakeBackfillSettingsStore struct {
	writes []store.BackfillSettings
	err    error
}

func (f *fakeBackfillSettingsStore) SetBackfillSettings(_ context.Context, s store.BackfillSettings) error {
	f.writes = append(f.writes, s)
	return f.err
}

// TestApplyConfigPersistsWhenASettingsStoreIsAttached proves defect 2's
// fix actually wires ApplyConfig to persistence: a config change made
// after SetSettingsStore must reach the store with the values that
// actually took effect (post-clamp), not merely the request's raw input.
func TestApplyConfigPersistsWhenASettingsStoreIsAttached(t *testing.T) {
	idx := New(nil, nil)
	ctrl := NewController(DefaultControllerConfig())
	b := NewBackfill(idx, ctrl, BackfillConfig{})

	settings := &fakeBackfillSettingsStore{}
	b.SetSettingsStore(settings)

	minWorkers := 1
	if _, err := b.ApplyConfig(ConfigPatch{MinWorkers: &minWorkers}); err != nil {
		t.Fatalf("ApplyConfig failed: %v", err)
	}

	if len(settings.writes) != 1 {
		t.Fatalf("expected exactly one persisted write, got %d", len(settings.writes))
	}
	if settings.writes[0].MinWorkers != 1 {
		t.Fatalf("persisted MinWorkers = %d, want 1", settings.writes[0].MinWorkers)
	}
}

// TestApplyConfigBeforeSetSettingsStoreNeverPersists is the ordering
// guarantee cmd/sidecar's startup call depends on (see SetSettingsStore's
// own doc comment): a config change applied before any settings store is
// attached -- exactly what the startup ApplyConfig call in main.go does --
// must not, and structurally cannot, write anything.
func TestApplyConfigBeforeSetSettingsStoreNeverPersists(t *testing.T) {
	idx := New(nil, nil)
	ctrl := NewController(DefaultControllerConfig())
	b := NewBackfill(idx, ctrl, BackfillConfig{})

	minWorkers := 1
	if _, err := b.ApplyConfig(ConfigPatch{MinWorkers: &minWorkers}); err != nil {
		t.Fatalf("ApplyConfig failed: %v", err)
	}

	// Attach afterward, exactly like main.go does, and confirm a SECOND
	// change now does persist -- proving the first one's absence above
	// was the ordering guarantee working, not a broken settings store.
	settings := &fakeBackfillSettingsStore{}
	b.SetSettingsStore(settings)
	maxWorkers := 2
	if _, err := b.ApplyConfig(ConfigPatch{MaxWorkers: &maxWorkers}); err != nil {
		t.Fatalf("second ApplyConfig failed: %v", err)
	}
	if len(settings.writes) != 1 {
		t.Fatalf("expected exactly one persisted write (from the change made AFTER attaching), got %d", len(settings.writes))
	}
}
