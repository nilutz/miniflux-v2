// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package storage // import "miniflux.app/v2/internal/storage"

import (
	"testing"

	"miniflux.app/v2/internal/config"
	"miniflux.app/v2/internal/model"
)

// configureHiddenTestOptions installs a parsed configuration so that
// GetNavMetadata (which reads config.Opts.PollingParsingErrorLimit) does not
// panic on a nil config in tests that never otherwise touch config.Opts.
func configureHiddenTestOptions(t *testing.T) {
	t.Helper()

	parsedOptions, err := config.NewConfigParser().ParseEnvironmentVariables()
	if err != nil {
		t.Fatalf("unable to configure test options: %v", err)
	}

	previousOptions := config.Opts
	config.Opts = parsedOptions
	t.Cleanup(func() { config.Opts = previousOptions })
}

// TestHiddenEntryExcludedOnlyWhenExplicitlyFilteredOutOfTheUnreadList covers
// the core of spec §13.3: hiding drops an entry from the unread list, but
// only when a caller explicitly asks for that (WithHidden(false)). Nothing
// else changes: without that filter, the same query still returns the entry.
func TestHiddenEntryExcludedOnlyWhenExplicitlyFilteredOutOfTheUnreadList(t *testing.T) {
	store := testStorage(t)
	entryID, _ := createTestEntry(t, store, "hidden-unread-list")
	userID, _, err := store.EntryOwner(entryID)
	if err != nil {
		t.Fatalf("unable to get entry owner: %v", err)
	}

	if err := store.SetEntriesHiddenState(userID, []int64{entryID}, true); err != nil {
		t.Fatalf("unable to hide entry: %v", err)
	}

	entries, err := store.NewEntryQueryBuilder(userID).
		WithEntryIDs(entryID).
		WithStatuses(model.EntryStatusUnread).
		WithHidden(false).
		GetEntries()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected the hidden entry to be excluded from the unread list, got %d entries", len(entries))
	}

	// Without the explicit filter, the same status query still returns it:
	// hiding is opt-in per query, exactly like starred.
	entries, err = store.NewEntryQueryBuilder(userID).
		WithEntryIDs(entryID).
		WithStatuses(model.EntryStatusUnread).
		GetEntries()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected the hidden entry to remain reachable without WithHidden, got %d entries", len(entries))
	}
	if !entries[0].Hidden {
		t.Fatal("expected entry.Hidden to be true")
	}
}

// TestHiddenEntryStillAppearsInFeedAndCategoryViews pins the other half of
// the decision: hiding is not a third status. The entry stays reachable
// through ordinary feed and category browsing.
func TestHiddenEntryStillAppearsInFeedAndCategoryViews(t *testing.T) {
	store := testStorage(t)
	entryID, feedID := createTestEntry(t, store, "hidden-feed-category")
	userID, _, err := store.EntryOwner(entryID)
	if err != nil {
		t.Fatalf("unable to get entry owner: %v", err)
	}

	feed, err := store.FeedByID(userID, feedID)
	if err != nil || feed == nil {
		t.Fatalf("unable to load feed: %v", err)
	}

	if err := store.SetEntriesHiddenState(userID, []int64{entryID}, true); err != nil {
		t.Fatalf("unable to hide entry: %v", err)
	}

	feedEntries, err := store.NewEntryQueryBuilder(userID).
		WithFeedID(feedID).
		GetEntries()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(feedEntries) != 1 {
		t.Fatalf("expected the hidden entry to still appear in the feed view, got %d entries", len(feedEntries))
	}

	categoryEntries, err := store.NewEntryQueryBuilder(userID).
		WithCategoryID(feed.Category.ID).
		GetEntries()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(categoryEntries) != 1 {
		t.Fatalf("expected the hidden entry to still appear in the category view, got %d entries", len(categoryEntries))
	}
}

// TestHiddenEntryStillReturnedBySearch covers the other explicit
// requirement: hiding must not remove the entry from full-text search.
func TestHiddenEntryStillReturnedBySearch(t *testing.T) {
	store := testStorage(t)
	entryID, _ := createTestEntry(t, store, "hidden-search")
	userID, _, err := store.EntryOwner(entryID)
	if err != nil {
		t.Fatalf("unable to get entry owner: %v", err)
	}

	// createTestEntry does not populate document_vectors (it inserts
	// directly with SQL, bypassing the normal entry-creation path), so give
	// this entry a searchable, distinctive marker the same way a real
	// refresh would: through the same code path that maintains the tsvector.
	if err := store.UpdateEntryTitleAndContent(&model.Entry{
		ID:          entryID,
		UserID:      userID,
		Title:       "Distinctive-Marker-Zyzzyva",
		Content:     "<p>content</p>",
		ReadingTime: 1,
	}); err != nil {
		t.Fatalf("unable to set searchable content: %v", err)
	}

	if err := store.SetEntriesHiddenState(userID, []int64{entryID}, true); err != nil {
		t.Fatalf("unable to hide entry: %v", err)
	}

	entries, err := store.NewEntryQueryBuilder(userID).
		WithSearchQuery("Distinctive-Marker-Zyzzyva").
		GetEntries()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected the hidden entry to still be found by search, got %d entries", len(entries))
	}
}

// TestUnhidingRestoresEntryToTheUnreadList covers the reversibility
// requirement: hiding says "not now", not "never".
func TestUnhidingRestoresEntryToTheUnreadList(t *testing.T) {
	store := testStorage(t)
	entryID, _ := createTestEntry(t, store, "unhide-restore")
	userID, _, err := store.EntryOwner(entryID)
	if err != nil {
		t.Fatalf("unable to get entry owner: %v", err)
	}

	if err := store.SetEntriesHiddenState(userID, []int64{entryID}, true); err != nil {
		t.Fatalf("unable to hide entry: %v", err)
	}

	unreadListQuery := func() int {
		entries, err := store.NewEntryQueryBuilder(userID).
			WithEntryIDs(entryID).
			WithStatuses(model.EntryStatusUnread).
			WithHidden(false).
			GetEntries()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		return len(entries)
	}

	if got := unreadListQuery(); got != 0 {
		t.Fatalf("expected the hidden entry to be excluded, got %d entries", got)
	}

	if err := store.SetEntriesHiddenState(userID, []int64{entryID}, false); err != nil {
		t.Fatalf("unable to unhide entry: %v", err)
	}

	if got := unreadListQuery(); got != 1 {
		t.Fatalf("expected unhiding to restore the entry to the unread list, got %d entries", got)
	}
}

// TestToggleHiddenTogglesTheFlag mirrors TestToggleStarred's shape for the
// hidden flag's own toggle path.
func TestToggleHiddenTogglesTheFlag(t *testing.T) {
	store := testStorage(t)
	entryID, _ := createTestEntry(t, store, "toggle-hidden")
	userID, _, err := store.EntryOwner(entryID)
	if err != nil {
		t.Fatalf("unable to get entry owner: %v", err)
	}

	getHidden := func() bool {
		entry, err := store.NewEntryQueryBuilder(userID).WithEntryIDs(entryID).GetEntry()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		return entry.Hidden
	}

	if got := getHidden(); got {
		t.Fatal("expected a freshly created entry to not be hidden")
	}

	if err := store.ToggleHidden(userID, entryID); err != nil {
		t.Fatalf("unable to toggle hidden: %v", err)
	}
	if got := getHidden(); !got {
		t.Fatal("expected the entry to be hidden after toggling")
	}

	if err := store.ToggleHidden(userID, entryID); err != nil {
		t.Fatalf("unable to toggle hidden: %v", err)
	}
	if got := getHidden(); got {
		t.Fatal("expected the entry to no longer be hidden after toggling again")
	}
}

// TestHiddenEntryDoesNotCountTowardNavUnreadBadge covers the global unread
// badge shown in the navigation.
func TestHiddenEntryDoesNotCountTowardNavUnreadBadge(t *testing.T) {
	configureHiddenTestOptions(t)
	store := testStorage(t)
	entryID, _ := createTestEntry(t, store, "nav-badge-hidden")
	userID, _, err := store.EntryOwner(entryID)
	if err != nil {
		t.Fatalf("unable to get entry owner: %v", err)
	}

	navBefore, err := store.GetNavMetadata(userID)
	if err != nil {
		t.Fatalf("unable to get nav metadata: %v", err)
	}
	if navBefore.CountUnread != 1 {
		t.Fatalf("expected 1 unread entry before hiding, got %d", navBefore.CountUnread)
	}

	if err := store.SetEntriesHiddenState(userID, []int64{entryID}, true); err != nil {
		t.Fatalf("unable to hide entry: %v", err)
	}

	navAfter, err := store.GetNavMetadata(userID)
	if err != nil {
		t.Fatalf("unable to get nav metadata: %v", err)
	}
	if navAfter.CountUnread != 0 {
		t.Fatalf("expected the hidden entry to not count toward the unread badge, got %d", navAfter.CountUnread)
	}
}

// TestHiddenEntryDoesNotCountTowardCategoryUnreadBadge covers the per-category
// unread badge.
func TestHiddenEntryDoesNotCountTowardCategoryUnreadBadge(t *testing.T) {
	store := testStorage(t)
	entryID, feedID := createTestEntry(t, store, "category-badge-hidden")
	userID, _, err := store.EntryOwner(entryID)
	if err != nil {
		t.Fatalf("unable to get entry owner: %v", err)
	}

	feed, err := store.FeedByID(userID, feedID)
	if err != nil || feed == nil {
		t.Fatalf("unable to load feed: %v", err)
	}

	totalUnreadFor := func(categoryID int64) int {
		categories, err := store.CategoriesWithFeedCount(userID, "")
		if err != nil {
			t.Fatalf("unable to fetch categories: %v", err)
		}
		for _, category := range categories {
			if category.ID == categoryID {
				if category.TotalUnread == nil {
					return 0
				}
				return *category.TotalUnread
			}
		}
		t.Fatalf("category %d not found", categoryID)
		return 0
	}

	if got := totalUnreadFor(feed.Category.ID); got != 1 {
		t.Fatalf("expected 1 unread entry before hiding, got %d", got)
	}

	if err := store.SetEntriesHiddenState(userID, []int64{entryID}, true); err != nil {
		t.Fatalf("unable to hide entry: %v", err)
	}

	if got := totalUnreadFor(feed.Category.ID); got != 0 {
		t.Fatalf("expected the hidden entry to not count toward the category unread badge, got %d", got)
	}
}

// TestHiddenEntryDoesNotCountTowardFeedUnreadBadge covers the per-feed
// unread badge, while leaving the read counter (an orthogonal concern)
// untouched by the change.
func TestHiddenEntryDoesNotCountTowardFeedUnreadBadge(t *testing.T) {
	store := testStorage(t)
	entryID, feedID := createTestEntry(t, store, "feed-badge-hidden")
	userID, _, err := store.EntryOwner(entryID)
	if err != nil {
		t.Fatalf("unable to get entry owner: %v", err)
	}

	countersBefore, err := store.FetchCounters(userID)
	if err != nil {
		t.Fatalf("unable to fetch counters: %v", err)
	}
	if countersBefore.UnreadCounters[feedID] != 1 {
		t.Fatalf("expected 1 unread entry before hiding, got %d", countersBefore.UnreadCounters[feedID])
	}

	if err := store.SetEntriesHiddenState(userID, []int64{entryID}, true); err != nil {
		t.Fatalf("unable to hide entry: %v", err)
	}

	countersAfter, err := store.FetchCounters(userID)
	if err != nil {
		t.Fatalf("unable to fetch counters: %v", err)
	}
	if countersAfter.UnreadCounters[feedID] != 0 {
		t.Fatalf("expected the hidden entry to not count toward the feed unread badge, got %d", countersAfter.UnreadCounters[feedID])
	}
	if countersAfter.ReadCounters[feedID] != 0 {
		t.Fatalf("hiding an unread entry must not make it count as read, got %d", countersAfter.ReadCounters[feedID])
	}
}
