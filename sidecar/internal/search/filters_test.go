// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package search // import "miniflux.app/v2/sidecar/internal/search"

import (
	"strings"
	"testing"
	"time"
)

// TestFiltersWithOnlyAUserIDIsNotEmpty guards the subtle half of the
// user-scoping fix (whole-branch review, finding 3). empty() is what
// decides whether a retrieval joins public.entries at all; if UserID did
// not count as a constraint, setting it would produce a query with no
// join, no WHERE clause and no scoping — a filter that silently does
// nothing, which is exactly the failure mode the fix exists to remove.
func TestFiltersWithOnlyAUserIDIsNotEmpty(t *testing.T) {
	if (Filters{UserID: 7}).empty() {
		t.Fatal("Filters{UserID: 7}.empty() = true; a user scope is a constraint, and returning true here would skip the join that applies it")
	}
	if !(Filters{}).empty() {
		t.Fatal("Filters{}.empty() = false; the unfiltered case must still skip the join")
	}
}

// TestWhereClauseScopesByUser proves the predicate is emitted, is
// parameterised (never interpolated), and numbers its placeholder from
// the caller's argStart.
func TestWhereClauseScopesByUser(t *testing.T) {
	where, args := Filters{UserID: 42}.whereClause(3)

	if where != "e.user_id = $3" {
		t.Fatalf("whereClause = %q, want %q", where, "e.user_id = $3")
	}
	if len(args) != 1 {
		t.Fatalf("args = %v, want exactly one", args)
	}
	if got, ok := args[0].(int64); !ok || got != 42 {
		t.Fatalf("args[0] = %#v, want int64(42) passed as a bind parameter", args[0])
	}
	if strings.Contains(where, "42") {
		t.Fatalf("whereClause interpolated the user id into SQL: %q", where)
	}
}

// TestWhereClauseNumbersUserAlongsideOtherFilters proves the user
// predicate takes the FIRST placeholder and the remaining filters shift
// up behind it — an off-by-one here would bind the wrong value to the
// wrong column, which is the kind of bug that silently returns another
// user's rows.
func TestWhereClauseNumbersUserAlongsideOtherFilters(t *testing.T) {
	since := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	f := Filters{
		UserID:      7,
		FeedIDs:     []int64{1, 2},
		CategoryIDs: []int64{3},
		Since:       since,
		UnreadOnly:  true,
		StarredOnly: true,
	}

	where, args := f.whereClause(1)

	want := "e.user_id = $1 AND " +
		"e.feed_id = ANY($2) AND " +
		"e.feed_id IN (SELECT id FROM feeds WHERE category_id = ANY($3)) AND " +
		"e.published_at >= $4 AND " +
		"e.status = 'unread' AND " +
		"e.starred = true"
	if where != want {
		t.Fatalf("whereClause =\n  %q\nwant\n  %q", where, want)
	}
	if len(args) != 4 {
		t.Fatalf("args = %v, want 4 (user, feeds, categories, since)", args)
	}
	if got, ok := args[0].(int64); !ok || got != 7 {
		t.Fatalf("args[0] = %#v, want the user id first", args[0])
	}
	if !args[3].(time.Time).Equal(since) {
		t.Fatalf("args[3] = %#v, want the Since bound", args[3])
	}
}
