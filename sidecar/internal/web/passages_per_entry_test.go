// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package web // import "miniflux.app/v2/sidecar/internal/web"

import (
	"testing"

	"miniflux.app/v2/sidecar/internal/indexer"
	"miniflux.app/v2/sidecar/internal/store"
)

// TestBuildViewPassagesPerEntryDividesByIndexedNotByEveryEntry is defect
// 7's fix, as a test: a real deployment's admin page showed "Passages
// per entry 0.07" with 606 passages and 8,845 total entries (606/8845 =
// 0.0685, the wrong-denominator reading) while only ~46 entries were
// actually indexed (606/13 measured passages-per-entry). Divided
// correctly (606/46) it reads ~13.2, the number spec §13.2 exists to
// surface and the entire reason disk growth is predictable from it.
//
// This reproduces the exact reported figures and fails against a
// buildView that divides by dm.EntryCount (the prior behaviour) rather
// than st.Indexed.
func TestBuildViewPassagesPerEntryDividesByIndexedNotByEveryEntry(t *testing.T) {
	st := indexer.Stats{Indexed: 46, Remaining: 8797}
	dm := store.DatabaseMetrics{
		PassageCount: 606,
		EntryCount:   8845,
	}

	v := buildView(st, indexer.LiveStats{}, indexer.RuntimeConfig{}, dm, true)

	if v.PassagesPerEntryUnknown {
		t.Fatalf("expected a known ratio with Indexed=46 and PassageCount=606, got PassagesPerEntryUnknown=true")
	}

	const wrongDenominatorReading = float64(606) / float64(8845) // ~0.0685, the reported bug
	if v.PassagesPerEntry == wrongDenominatorReading {
		t.Fatalf("PassagesPerEntry = %v matches the wrong-denominator (606/8845) reading the bug report showed -- it must divide by Indexed (46), not EntryCount (8845)", v.PassagesPerEntry)
	}

	want := float64(606) / float64(46)
	if v.PassagesPerEntry != want {
		t.Fatalf("PassagesPerEntry = %v, want %v (606/46, the number of entries actually indexed)", v.PassagesPerEntry, want)
	}
}

// TestBuildViewPassagesPerEntryUnknownWhenNothingIndexedYet covers the
// boundary defect 7's fix has to get right in the other direction: with
// Indexed == 0 (nothing indexed yet -- the start of a fresh backfill,
// exactly when an operator is most likely watching), the ratio must
// render as unknown, not as a real zero -- dividing by zero indexed
// entries is not a small ratio, it is no ratio.
func TestBuildViewPassagesPerEntryUnknownWhenNothingIndexedYet(t *testing.T) {
	st := indexer.Stats{Indexed: 0, Remaining: 8845}
	dm := store.DatabaseMetrics{PassageCount: 0, EntryCount: 8845}

	v := buildView(st, indexer.LiveStats{}, indexer.RuntimeConfig{}, dm, true)

	if !v.PassagesPerEntryUnknown {
		t.Fatalf("expected PassagesPerEntryUnknown=true when Indexed is 0, got false (PassagesPerEntry=%v)", v.PassagesPerEntry)
	}
}
