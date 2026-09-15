// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package storage // import "miniflux.app/v2/internal/storage"

import (
	"database/sql"
	"testing"
	"time"

	"miniflux.app/v2/internal/model"
)

// bulkHiddenFixtureUser creates a user and a category and returns their ids.
// Everything is removed, cascading to categories/feeds/entries, when the
// test finishes.
func bulkHiddenFixtureUser(t *testing.T, s *Storage, username string) (userID, categoryID int64) {
	t.Helper()

	if err := s.db.QueryRow(
		`INSERT INTO users (username, password) VALUES ($1, 'x') RETURNING id`,
		username,
	).Scan(&userID); err != nil {
		t.Fatalf("unable to create user: %v", err)
	}
	t.Cleanup(func() {
		s.db.Exec(`DELETE FROM users WHERE id=$1`, userID)
	})

	if err := s.db.QueryRow(
		`INSERT INTO categories (user_id, title) VALUES ($1, 'Test') RETURNING id`,
		userID,
	).Scan(&categoryID); err != nil {
		t.Fatalf("unable to create category: %v", err)
	}

	return userID, categoryID
}

// bulkHiddenFixtureCategory creates an additional category for userID.
func bulkHiddenFixtureCategory(t *testing.T, s *Storage, userID int64, title string, hideGlobally bool) int64 {
	t.Helper()

	var categoryID int64
	if err := s.db.QueryRow(
		`INSERT INTO categories (user_id, title, hide_globally) VALUES ($1, $2, $3) RETURNING id`,
		userID, title, hideGlobally,
	).Scan(&categoryID); err != nil {
		t.Fatalf("unable to create category: %v", err)
	}
	return categoryID
}

// bulkHiddenFixtureFeed creates a feed for userID/categoryID.
func bulkHiddenFixtureFeed(t *testing.T, s *Storage, userID, categoryID int64, slug string, hideGlobally bool) int64 {
	t.Helper()

	var feedID int64
	if err := s.db.QueryRow(
		`INSERT INTO feeds (feed_url, site_url, title, category_id, user_id, hide_globally)
		 VALUES ($1, $1, 'Test feed', $2, $3, $4) RETURNING id`,
		"https://example.org/"+slug+".xml", categoryID, userID, hideGlobally,
	).Scan(&feedID); err != nil {
		t.Fatalf("unable to create feed: %v", err)
	}
	return feedID
}

type bulkHiddenEntryOptions struct {
	status       string
	hidden       bool
	hiddenReason sql.NullString
	publishedAt  time.Time
}

// bulkHiddenFixtureEntry inserts an entry with explicit status/hidden state,
// so scoping tests can set up exactly the before/after picture they need to
// prove a bulk action stayed inside its declared scope.
func bulkHiddenFixtureEntry(t *testing.T, s *Storage, userID, feedID int64, slug string, opts bulkHiddenEntryOptions) int64 {
	t.Helper()

	var entryID int64
	if err := s.db.QueryRow(
		`INSERT INTO entries (title, hash, url, published_at, changed_at, user_id, feed_id, content, author, status, hidden, hidden_reason)
		 VALUES ('Test entry', $1, 'https://example.org/'||$1, $2, now(), $3, $4, '<p>excerpt</p>', '', $5, $6, $7)
		 RETURNING id`,
		slug, opts.publishedAt, userID, feedID, opts.status, opts.hidden, opts.hiddenReason,
	).Scan(&entryID); err != nil {
		t.Fatalf("unable to create entry: %v", err)
	}
	return entryID
}

func readHiddenState(t *testing.T, s *Storage, entryID int64) (hidden bool, hiddenReason sql.NullString) {
	t.Helper()
	if err := s.db.QueryRow(`SELECT hidden, hidden_reason FROM entries WHERE id=$1`, entryID).Scan(&hidden, &hiddenReason); err != nil {
		t.Fatalf("unable to read hidden state of entry #%d: %v", entryID, err)
	}
	return hidden, hiddenReason
}

// TestMarkFeedAsHiddenStaysInsideItsScope covers MarkFeedAsHidden, the
// storage behind the "mark all as hidden" route scoped to one feed. It must
// hide only that feed's unread, not-yet-hidden entries published before the
// cutoff — and, critically, must never overwrite an entry a person already
// hid by hand (hidden_reason NULL) with 'bulk', which would destroy a real
// preference signal for goal 3.
func TestMarkFeedAsHiddenStaysInsideItsScope(t *testing.T) {
	store := testStorage(t)
	userID, categoryID := bulkHiddenFixtureUser(t, store, "mark-feed-hidden")
	targetFeedID := bulkHiddenFixtureFeed(t, store, userID, categoryID, "mark-feed-hidden-target", false)
	otherFeedID := bulkHiddenFixtureFeed(t, store, userID, categoryID, "mark-feed-hidden-other", false)

	before := time.Now().Add(time.Hour)
	past := time.Now().Add(-time.Hour)
	future := time.Now().Add(2 * time.Hour)

	targetUnread := bulkHiddenFixtureEntry(t, store, userID, targetFeedID, "target-unread", bulkHiddenEntryOptions{
		status: model.EntryStatusUnread, publishedAt: past,
	})
	targetHandHidden := bulkHiddenFixtureEntry(t, store, userID, targetFeedID, "target-hand-hidden", bulkHiddenEntryOptions{
		status: model.EntryStatusUnread, hidden: true, hiddenReason: sql.NullString{}, publishedAt: past,
	})
	targetRead := bulkHiddenFixtureEntry(t, store, userID, targetFeedID, "target-read", bulkHiddenEntryOptions{
		status: model.EntryStatusRead, publishedAt: past,
	})
	targetFuture := bulkHiddenFixtureEntry(t, store, userID, targetFeedID, "target-future", bulkHiddenEntryOptions{
		status: model.EntryStatusUnread, publishedAt: future,
	})
	otherFeedUnread := bulkHiddenFixtureEntry(t, store, userID, otherFeedID, "other-unread", bulkHiddenEntryOptions{
		status: model.EntryStatusUnread, publishedAt: past,
	})

	if err := store.MarkFeedAsHidden(userID, targetFeedID, before); err != nil {
		t.Fatalf("unable to mark feed as hidden: %v", err)
	}

	if hidden, reason := readHiddenState(t, store, targetUnread); !hidden || !reason.Valid || reason.String != model.EntryHiddenReasonBulk {
		t.Fatalf("expected the target feed's unread entry to be hidden with reason 'bulk', got hidden=%v reason=%+v", hidden, reason)
	}

	if hidden, reason := readHiddenState(t, store, targetHandHidden); !hidden || reason.Valid {
		t.Fatalf("expected the hand-hidden entry's reason to stay NULL (not overwritten to 'bulk'), got hidden=%v reason=%+v", hidden, reason)
	}

	if hidden, _ := readHiddenState(t, store, targetRead); hidden {
		t.Fatal("expected the already-read entry to be left alone (scope is unread entries only)")
	}

	if hidden, _ := readHiddenState(t, store, targetFuture); hidden {
		t.Fatal("expected an entry published after the cutoff to be left alone, matching MarkFeedAsRead's before semantics")
	}

	if hidden, _ := readHiddenState(t, store, otherFeedUnread); hidden {
		t.Fatal("expected another feed's unread entry to be left alone")
	}
}

// TestMarkCategoryAsHiddenStaysInsideItsScope covers MarkCategoryAsHidden:
// it must hide only entries belonging to feeds in the target category, never
// another category's, and must not overwrite a hand-hidden reason.
func TestMarkCategoryAsHiddenStaysInsideItsScope(t *testing.T) {
	store := testStorage(t)
	userID, targetCategoryID := bulkHiddenFixtureUser(t, store, "mark-category-hidden")
	otherCategoryID := bulkHiddenFixtureCategory(t, store, userID, "Other", false)

	targetFeedID := bulkHiddenFixtureFeed(t, store, userID, targetCategoryID, "mark-category-hidden-target", false)
	otherFeedID := bulkHiddenFixtureFeed(t, store, userID, otherCategoryID, "mark-category-hidden-other", false)

	before := time.Now().Add(time.Hour)
	past := time.Now().Add(-time.Hour)

	targetUnread := bulkHiddenFixtureEntry(t, store, userID, targetFeedID, "cat-target-unread", bulkHiddenEntryOptions{
		status: model.EntryStatusUnread, publishedAt: past,
	})
	targetHandHidden := bulkHiddenFixtureEntry(t, store, userID, targetFeedID, "cat-target-hand-hidden", bulkHiddenEntryOptions{
		status: model.EntryStatusUnread, hidden: true, publishedAt: past,
	})
	otherCategoryUnread := bulkHiddenFixtureEntry(t, store, userID, otherFeedID, "cat-other-unread", bulkHiddenEntryOptions{
		status: model.EntryStatusUnread, publishedAt: past,
	})

	if err := store.MarkCategoryAsHidden(userID, targetCategoryID, before); err != nil {
		t.Fatalf("unable to mark category as hidden: %v", err)
	}

	if hidden, reason := readHiddenState(t, store, targetUnread); !hidden || !reason.Valid || reason.String != model.EntryHiddenReasonBulk {
		t.Fatalf("expected the target category's unread entry to be hidden with reason 'bulk', got hidden=%v reason=%+v", hidden, reason)
	}

	if hidden, reason := readHiddenState(t, store, targetHandHidden); !hidden || reason.Valid {
		t.Fatalf("expected the hand-hidden entry's reason to stay NULL, got hidden=%v reason=%+v", hidden, reason)
	}

	if hidden, _ := readHiddenState(t, store, otherCategoryUnread); hidden {
		t.Fatal("expected another category's unread entry to be left alone")
	}
}

// TestMarkGloballyVisibleFeedsAsHiddenStaysInsideItsScope covers the storage
// behind the top-level "mark all as hidden" route. It must respect
// feed/category hide_globally exactly like MarkGloballyVisibleFeedsAsRead,
// must never touch another user's entries, and must not overwrite a
// hand-hidden reason.
func TestMarkGloballyVisibleFeedsAsHiddenStaysInsideItsScope(t *testing.T) {
	store := testStorage(t)
	userID, visibleCategoryID := bulkHiddenFixtureUser(t, store, "mark-global-hidden")
	hiddenCategoryID := bulkHiddenFixtureCategory(t, store, userID, "Hidden category", true)

	visibleFeedID := bulkHiddenFixtureFeed(t, store, userID, visibleCategoryID, "mark-global-hidden-visible", false)
	hideGloballyFeedID := bulkHiddenFixtureFeed(t, store, userID, visibleCategoryID, "mark-global-hidden-feed-hidden", true)
	hiddenCategoryFeedID := bulkHiddenFixtureFeed(t, store, userID, hiddenCategoryID, "mark-global-hidden-cat-hidden", false)

	otherUserID, otherCategoryID := bulkHiddenFixtureUser(t, store, "mark-global-hidden-other-user")
	otherUserFeedID := bulkHiddenFixtureFeed(t, store, otherUserID, otherCategoryID, "mark-global-hidden-other-user-feed", false)

	past := time.Now().Add(-time.Hour)

	visibleUnread := bulkHiddenFixtureEntry(t, store, userID, visibleFeedID, "global-visible-unread", bulkHiddenEntryOptions{
		status: model.EntryStatusUnread, publishedAt: past,
	})
	visibleHandHidden := bulkHiddenFixtureEntry(t, store, userID, visibleFeedID, "global-visible-hand-hidden", bulkHiddenEntryOptions{
		status: model.EntryStatusUnread, hidden: true, publishedAt: past,
	})
	feedHiddenEntry := bulkHiddenFixtureEntry(t, store, userID, hideGloballyFeedID, "global-feed-hide-globally", bulkHiddenEntryOptions{
		status: model.EntryStatusUnread, publishedAt: past,
	})
	categoryHiddenEntry := bulkHiddenFixtureEntry(t, store, userID, hiddenCategoryFeedID, "global-category-hide-globally", bulkHiddenEntryOptions{
		status: model.EntryStatusUnread, publishedAt: past,
	})
	otherUserEntry := bulkHiddenFixtureEntry(t, store, otherUserID, otherUserFeedID, "global-other-user", bulkHiddenEntryOptions{
		status: model.EntryStatusUnread, publishedAt: past,
	})

	if err := store.MarkGloballyVisibleFeedsAsHidden(userID); err != nil {
		t.Fatalf("unable to mark globally visible feeds as hidden: %v", err)
	}

	if hidden, reason := readHiddenState(t, store, visibleUnread); !hidden || !reason.Valid || reason.String != model.EntryHiddenReasonBulk {
		t.Fatalf("expected the globally visible unread entry to be hidden with reason 'bulk', got hidden=%v reason=%+v", hidden, reason)
	}

	if hidden, reason := readHiddenState(t, store, visibleHandHidden); !hidden || reason.Valid {
		t.Fatalf("expected the hand-hidden entry's reason to stay NULL, got hidden=%v reason=%+v", hidden, reason)
	}

	if hidden, _ := readHiddenState(t, store, feedHiddenEntry); hidden {
		t.Fatal("expected an entry of a hide_globally feed to be left alone")
	}

	if hidden, _ := readHiddenState(t, store, categoryHiddenEntry); hidden {
		t.Fatal("expected an entry of a hide_globally category to be left alone")
	}

	if hidden, _ := readHiddenState(t, store, otherUserEntry); hidden {
		t.Fatal("expected another user's entry to be left alone")
	}
}

// TestHideFeedBacklogStaysInsideItsScope covers HideFeedBacklog, the helper
// behind the "hide existing articles" subscribe checkbox: it must hide every
// entry of the target feed, regardless of status, but never another feed's.
func TestHideFeedBacklogStaysInsideItsScope(t *testing.T) {
	store := testStorage(t)
	userID, categoryID := bulkHiddenFixtureUser(t, store, "hide-feed-backlog")
	targetFeedID := bulkHiddenFixtureFeed(t, store, userID, categoryID, "hide-feed-backlog-target", false)
	otherFeedID := bulkHiddenFixtureFeed(t, store, userID, categoryID, "hide-feed-backlog-other", false)

	now := time.Now()
	targetEntryOne := bulkHiddenFixtureEntry(t, store, userID, targetFeedID, "backlog-one", bulkHiddenEntryOptions{
		status: model.EntryStatusUnread, publishedAt: now,
	})
	targetEntryTwo := bulkHiddenFixtureEntry(t, store, userID, targetFeedID, "backlog-two", bulkHiddenEntryOptions{
		status: model.EntryStatusUnread, publishedAt: now,
	})
	otherFeedEntry := bulkHiddenFixtureEntry(t, store, userID, otherFeedID, "backlog-other-feed", bulkHiddenEntryOptions{
		status: model.EntryStatusUnread, publishedAt: now,
	})

	if err := store.HideFeedBacklog(userID, targetFeedID); err != nil {
		t.Fatalf("unable to hide feed backlog: %v", err)
	}

	for _, entryID := range []int64{targetEntryOne, targetEntryTwo} {
		if hidden, reason := readHiddenState(t, store, entryID); !hidden || !reason.Valid || reason.String != model.EntryHiddenReasonBulk {
			t.Fatalf("expected entry #%d to be hidden with reason 'bulk', got hidden=%v reason=%+v", entryID, hidden, reason)
		}
	}

	if hidden, _ := readHiddenState(t, store, otherFeedEntry); hidden {
		t.Fatal("expected another feed's entry to be left alone")
	}
}

// TestSetEntriesHiddenStateReasonSemantics pins the contract new bulk callers
// rely on: "" records no reason (NULL, hand-driven), a non-empty reason is
// recorded verbatim when hiding, and unhiding always clears the reason back
// to NULL regardless of what reason is passed — proving the unhide path.
func TestSetEntriesHiddenStateReasonSemantics(t *testing.T) {
	store := testStorage(t)
	entryID, _ := createTestEntry(t, store, "reason-semantics")
	userID, _, err := store.EntryOwner(entryID)
	if err != nil {
		t.Fatalf("unable to get entry owner: %v", err)
	}

	if err := store.SetEntriesHiddenState(userID, []int64{entryID}, true, ""); err != nil {
		t.Fatalf("unable to hide entry: %v", err)
	}
	if hidden, reason := readHiddenState(t, store, entryID); !hidden || reason.Valid {
		t.Fatalf("expected a hand hide (reason \"\") to record NULL, got hidden=%v reason=%+v", hidden, reason)
	}

	if err := store.SetEntriesHiddenState(userID, []int64{entryID}, true, model.EntryHiddenReasonBulk); err != nil {
		t.Fatalf("unable to hide entry: %v", err)
	}
	if hidden, reason := readHiddenState(t, store, entryID); !hidden || !reason.Valid || reason.String != model.EntryHiddenReasonBulk {
		t.Fatalf("expected reason %q to be recorded verbatim, got hidden=%v reason=%+v", model.EntryHiddenReasonBulk, hidden, reason)
	}

	// Unhiding must clear the reason, even though a non-empty reason is
	// passed alongside hidden=false: a stale reason on a visible entry must
	// never survive (spec §13.3).
	if err := store.SetEntriesHiddenState(userID, []int64{entryID}, false, model.EntryHiddenReasonBulk); err != nil {
		t.Fatalf("unable to unhide entry: %v", err)
	}
	if hidden, reason := readHiddenState(t, store, entryID); hidden || reason.Valid {
		t.Fatalf("expected unhiding to clear both hidden and hidden_reason, got hidden=%v reason=%+v", hidden, reason)
	}
}

// TestToggleHiddenAlwaysClearsReason proves ToggleHidden's half of the
// unhide contract: whichever direction the flag flips, hidden_reason ends up
// NULL, because the single-entry control is always a hand action.
func TestToggleHiddenAlwaysClearsReason(t *testing.T) {
	store := testStorage(t)
	entryID, _ := createTestEntry(t, store, "toggle-clears-reason")
	userID, _, err := store.EntryOwner(entryID)
	if err != nil {
		t.Fatalf("unable to get entry owner: %v", err)
	}

	if err := store.SetEntriesHiddenState(userID, []int64{entryID}, true, model.EntryHiddenReasonBulk); err != nil {
		t.Fatalf("unable to bulk-hide entry: %v", err)
	}
	if hidden, reason := readHiddenState(t, store, entryID); !hidden || !reason.Valid {
		t.Fatalf("setup: expected the entry to be bulk-hidden, got hidden=%v reason=%+v", hidden, reason)
	}

	// Unhide via the single-entry control: the stale 'bulk' reason must go.
	if err := store.ToggleHidden(userID, entryID); err != nil {
		t.Fatalf("unable to toggle hidden: %v", err)
	}
	if hidden, reason := readHiddenState(t, store, entryID); hidden || reason.Valid {
		t.Fatalf("expected toggling off to clear hidden_reason, got hidden=%v reason=%+v", hidden, reason)
	}

	// Hide again via the single-entry control: this is a hand action, so it
	// must record NULL, never 'bulk'.
	if err := store.ToggleHidden(userID, entryID); err != nil {
		t.Fatalf("unable to toggle hidden: %v", err)
	}
	if hidden, reason := readHiddenState(t, store, entryID); !hidden || reason.Valid {
		t.Fatalf("expected toggling on to record no reason (hand action), got hidden=%v reason=%+v", hidden, reason)
	}
}
