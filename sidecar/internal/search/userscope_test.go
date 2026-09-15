// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package search // import "miniflux.app/v2/sidecar/internal/search"

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"miniflux.app/v2/sidecar/internal/store"
)

// ownerOf returns the user id that owns an entry, so a test can build the
// Filters the fork would send. createFixtureEntry creates a user per
// fixture but only reports the entry/feed/category ids.
func ownerOf(t *testing.T, db *sql.DB, entryID int64) int64 {
	t.Helper()
	var userID int64
	if err := db.QueryRow(`SELECT user_id FROM entries WHERE id=$1`, entryID).Scan(&userID); err != nil {
		t.Fatalf("unable to read entry #%d's owner: %v", entryID, err)
	}
	return userID
}

// hogPassages builds n identical, minimal passages whose only token is
// term. Identical text means identical BM25 score and identical document
// length, so these rows tie with each other and are ordered purely by
// passage id — and a SHORTER document scores higher under BM25's length
// normalisation than the deliberately padded victim passage below, so
// every one of these outranks it. That is what makes the crowd-out below
// deterministic rather than a coin flip on scoring details.
func hogPassages(term string, n int) []store.PassageRow {
	rows := make([]store.PassageRow, n)
	for i := range rows {
		rows[i] = store.PassageRow{
			Ordinal:   i,
			Text:      term,
			CharStart: 0,
			CharEnd:   len(term),
			Source:    "content",
			Embedding: zeroEmbedding(),
		}
	}
	return rows
}

// TestLexicalUserFilterKeepsAnotherUsersContentOutOfTheCandidateSet
// demonstrates the user-scoping bug Filters.UserID guards against.
//
// search.passages is global. Before this fix, Filters carried no user, so
// a retrieval's candidate set was drawn from every user's content and
// only the fork's own hydration step (NewEntryQueryBuilder(user.ID))
// removed other users' entries — after the ranking had already been
// decided. The security property held (nothing another user owns was ever
// displayed), but the reader's result page did not: another user with a
// lot of matching content consumed the slots, and the reader got a short
// page, or an empty one, for a query their own articles match.
//
// The fixture makes that concrete. One user ("hog") has 60 passages that
// all match the query term perfectly; another ("victim") has a single,
// longer passage that also matches. candidateMultiplier floors the
// candidate set at minCandidates (50), so the hog's rows fill it entirely
// and the victim's passage never even reaches the filter stage.
func TestLexicalUserFilterKeepsAnotherUsersContentOutOfTheCandidateSet(t *testing.T) {
	s := testStore(t)
	db := testDB(t)
	searcher := NewSearcher(s)
	ctx := context.Background()

	const term = "zzyzxuserscopeterm"

	// The hog is written first so its passages take the LOWER passage
	// ids: BM25 ties break on id ascending, so this is what makes the
	// crowd-out reproducible rather than incidental.
	hog := createFixtureEntry(t, db, "userscope-hog", "Zzyzx User Scope Hog", "<p>irrelevant</p>")
	writePassages(t, s, hog.EntryID, hogPassages(term, 60))

	victimText := term + " " + strings.Repeat("padding ", 60)
	victim := createFixtureEntry(t, db, "userscope-victim", "Zzyzx User Scope Victim", "<p>irrelevant</p>")
	writePassages(t, s, victim.EntryID, []store.PassageRow{
		{Ordinal: 0, Text: victimText, CharStart: 0, CharEnd: len(victimText), Source: "content", Embedding: zeroEmbedding()},
	})

	victimUserID := ownerOf(t, db, victim.EntryID)
	hogUserID := ownerOf(t, db, hog.EntryID)
	if victimUserID == hogUserID {
		t.Fatalf("fixture error: both entries belong to user %d", victimUserID)
	}

	// Unfiltered: this is the old behaviour, and it is the bug. The
	// victim's own matching passage is nowhere in a full page of results.
	unscoped, err := searcher.Lexical(ctx, term, 10, Filters{})
	if err != nil {
		t.Fatalf("Lexical (unscoped): %v", err)
	}
	if len(unscoped) != 10 {
		t.Fatalf("expected a full page of 10 unscoped hits, got %d", len(unscoped))
	}
	if hasEntryID(unscoped, victim.EntryID) {
		t.Skip("the hog fixture did not crowd out the victim; the corpus or the candidate floor changed, so this test can no longer demonstrate the bug it guards")
	}

	// Scoped: the victim gets their own top-N. Every slot is theirs, and
	// their one matching passage is in it.
	scoped, err := searcher.Lexical(ctx, term, 10, Filters{UserID: victimUserID})
	if err != nil {
		t.Fatalf("Lexical (scoped): %v", err)
	}
	if !hasEntryID(scoped, victim.EntryID) {
		t.Fatalf("the user's own matching entry #%d is missing from their own scoped search: %+v", victim.EntryID, scoped)
	}
	for _, h := range scoped {
		if h.EntryID != victim.EntryID {
			t.Fatalf("a user-scoped search returned entry #%d, which user %d does not own", h.EntryID, victimUserID)
		}
	}

	// And the scope is symmetric: the hog's own search is unaffected by
	// the victim's existence.
	hogScoped, err := searcher.Lexical(ctx, term, 10, Filters{UserID: hogUserID})
	if err != nil {
		t.Fatalf("Lexical (hog scoped): %v", err)
	}
	if len(hogScoped) != 10 {
		t.Fatalf("expected the hog's own scoped search to still fill 10 slots, got %d", len(hogScoped))
	}
	for _, h := range hogScoped {
		if h.EntryID != hog.EntryID {
			t.Fatalf("the hog's scoped search returned entry #%d, which it does not own", h.EntryID)
		}
	}
}

// TestSearchUserFilterAppliesThroughTheFullRequestPath proves the scope
// survives the Request -> Lexical -> aggregate path the HTTP handler
// actually uses, not just a direct Lexical call. ModeKeyword because this
// Searcher has no embedder.
func TestSearchUserFilterAppliesThroughTheFullRequestPath(t *testing.T) {
	s := testStore(t)
	db := testDB(t)
	searcher := NewSearcher(s)
	ctx := context.Background()

	const term = "zzyzxuserscopepathterm"

	a := createFixtureEntry(t, db, "userscope-path-a", "Zzyzx User Scope Path A", "<p>irrelevant</p>")
	writePassages(t, s, a.EntryID, []store.PassageRow{
		{Ordinal: 0, Text: term + " owned by user a", CharStart: 0, CharEnd: 40, Source: "content", Embedding: zeroEmbedding()},
	})
	b := createFixtureEntry(t, db, "userscope-path-b", "Zzyzx User Scope Path B", "<p>irrelevant</p>")
	writePassages(t, s, b.EntryID, []store.PassageRow{
		{Ordinal: 0, Text: term + " owned by user b", CharStart: 0, CharEnd: 40, Source: "content", Embedding: zeroEmbedding()},
	})

	userA := ownerOf(t, db, a.EntryID)

	resp, err := searcher.Search(ctx, Request{
		Query:   term,
		Mode:    ModeKeyword,
		Limit:   10,
		Filters: Filters{UserID: userA},
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	var sawA, sawB bool
	for _, hit := range resp.Entries {
		switch hit.EntryID {
		case a.EntryID:
			sawA = true
		case b.EntryID:
			sawB = true
		}
	}
	if !sawA {
		t.Fatalf("user A's own entry #%d is missing from their scoped search: %+v", a.EntryID, resp.Entries)
	}
	if sawB {
		t.Fatalf("user B's entry #%d leaked into user A's scoped search", b.EntryID)
	}
}
