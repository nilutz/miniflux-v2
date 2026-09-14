// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package search // import "miniflux.app/v2/sidecar/internal/search"

import "testing"

// This file is hermetic on purpose (task brief): no database, no embedder,
// no network. Fuse and aggregate are pure functions over []PassageHit, so
// every property claimed for them (spec §6.3, §6.4) must be checkable
// without either retrieval half actually running.

func TestRRFFusesOnRankNotScore(t *testing.T) {
	// Two passages from the SAME channel (lexical): one ranked 1st, one
	// ranked 3rd. Their Score fields are deliberately the reverse of what
	// their rank would suggest -- a huge raw score on the worse-ranked
	// passage, a tiny one on the better-ranked passage. If Fuse ever read
	// Score instead of Rank, the worse-ranked passage would win here. It
	// must not: RRF's contribution from a list is 1/(k+rank), a function
	// of Rank alone, never of Score -- that is the whole point of fusing
	// on rank position (spec §6.3).
	rankedFirst := PassageHit{PassageID: 1, EntryID: 100, Rank: 1, Score: 0.001}
	rankedThird := PassageHit{PassageID: 2, EntryID: 200, Rank: 3, Score: 999.0}

	fused := Fuse([]PassageHit{rankedFirst, rankedThird}, nil)

	if len(fused) != 2 {
		t.Fatalf("got %d fused hits, want 2", len(fused))
	}
	if fused[0].PassageID != 1 {
		t.Fatalf("fused[0].PassageID = %d, want 1: rank 1 must beat rank 3 despite its far smaller raw Score", fused[0].PassageID)
	}
	if fused[1].PassageID != 2 {
		t.Fatalf("fused[1].PassageID = %d, want 2", fused[1].PassageID)
	}
}

func TestRRFRewardsAppearingInBothLists(t *testing.T) {
	// Present in both lists at rank 2 must beat present in one list at
	// rank 1: 2*(1/62) > 1/61. This is what makes RRF a genuine fusion of
	// two signals rather than just "take whichever ranking is better".
	onlyInLexical := PassageHit{PassageID: 1, EntryID: 100, Rank: 1}
	inBothLists := PassageHit{PassageID: 2, EntryID: 200, Rank: 2}

	fused := Fuse(
		[]PassageHit{onlyInLexical, inBothLists},
		[]PassageHit{{PassageID: 2, EntryID: 200, Rank: 2}},
	)

	if len(fused) != 2 {
		t.Fatalf("got %d fused hits, want 2", len(fused))
	}
	if fused[0].PassageID != 2 {
		t.Fatalf("fused[0].PassageID = %d, want 2: present in both lists at rank 2 must outrank rank 1 in one list", fused[0].PassageID)
	}

	const wantBoth = 2.0 / 62.0
	if diff := fused[0].Score - wantBoth; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("fused[0].Score = %v, want %v (2/62)", fused[0].Score, wantBoth)
	}
	const wantOne = 1.0 / 61.0
	if diff := fused[1].Score - wantOne; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("fused[1].Score = %v, want %v (1/61)", fused[1].Score, wantOne)
	}
}

func TestFuseIsStableForEqualScores(t *testing.T) {
	// Two passages that end up with an identical fused score (same rank,
	// each present in exactly one list) must still come out in a
	// deterministic order across repeated runs -- otherwise identical
	// queries would reshuffle their results between requests, which would
	// be a visible, confusing bug in any UI built on top of this.
	higherID := PassageHit{PassageID: 5, EntryID: 500, Rank: 1}
	lowerID := PassageHit{PassageID: 3, EntryID: 300, Rank: 1}

	var firstOrder [2]int64
	for i := 0; i < 20; i++ {
		fused := Fuse([]PassageHit{higherID}, []PassageHit{lowerID})
		if len(fused) != 2 {
			t.Fatalf("run %d: got %d fused hits, want 2", i, len(fused))
		}
		order := [2]int64{fused[0].PassageID, fused[1].PassageID}
		if i == 0 {
			firstOrder = order
			continue
		}
		if order != firstOrder {
			t.Fatalf("run %d: order = %v, want %v (unstable ordering for equal scores)", i, order, firstOrder)
		}
	}

	// The documented tie-break is PassageID ascending.
	if firstOrder != [2]int64{3, 5} {
		t.Fatalf("tie-break order = %v, want [3 5] (PassageID ascending)", firstOrder)
	}
}

func TestAggregateRanksEntriesByBestPassage(t *testing.T) {
	// Spec §6.4: "entries ranked by their best-scoring passage". Entry 10
	// has two matching passages, one much better-scoring than the other;
	// entry 20 has one. Entries must come out ordered by each one's own
	// BEST passage, not by, say, the sum of its passages' scores (which
	// would put entry 10's 0.9+0.5=1.4 ahead of entry 20's 0.8 for a
	// different, wrong reason that happens to agree here -- the next
	// assertion on Best pins the actual mechanism).
	hits := []PassageHit{
		{PassageID: 1, EntryID: 10, Score: 0.9},
		{PassageID: 2, EntryID: 20, Score: 0.8},
		{PassageID: 3, EntryID: 10, Score: 0.5},
	}

	entries := aggregate(hits)

	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}
	if entries[0].EntryID != 10 {
		t.Fatalf("entries[0].EntryID = %d, want 10", entries[0].EntryID)
	}
	if entries[0].Score != 0.9 || entries[0].Best.PassageID != 1 {
		t.Fatalf("entries[0] Score/Best = %v/%d, want 0.9/passage 1", entries[0].Score, entries[0].Best.PassageID)
	}
	if entries[1].EntryID != 20 || entries[1].Score != 0.8 {
		t.Fatalf("entries[1] = %+v, want entry 20 at score 0.8", entries[1])
	}
}

func TestAggregateKeepsAllPassagesForPassageMode(t *testing.T) {
	// aggregate must not discard an entry's non-Best passages: passage
	// mode (spec §6.4) needs the full per-entry set, not just the one
	// chosen as the entry's snippet.
	hits := []PassageHit{
		{PassageID: 1, EntryID: 10, Score: 0.9},
		{PassageID: 3, EntryID: 10, Score: 0.5},
		{PassageID: 2, EntryID: 20, Score: 0.8},
	}

	entries := aggregate(hits)

	var entry10 *EntryHit
	for i := range entries {
		if entries[i].EntryID == 10 {
			entry10 = &entries[i]
		}
	}
	if entry10 == nil {
		t.Fatal("entry 10 missing from aggregate output")
	}
	if len(entry10.Passages) != 2 {
		t.Fatalf("entry10.Passages has %d passages, want 2 (both, not just Best)", len(entry10.Passages))
	}
	if entry10.Passages[0].PassageID != 1 || entry10.Passages[1].PassageID != 3 {
		t.Fatalf("entry10.Passages = %+v, want [passage 1, passage 3] in best-first order", entry10.Passages)
	}
}
