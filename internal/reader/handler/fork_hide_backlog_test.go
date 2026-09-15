// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package handler // import "miniflux.app/v2/internal/reader/handler"

import (
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	_ "github.com/lib/pq"

	"miniflux.app/v2/internal/model"
	"miniflux.app/v2/internal/storage"
)

// TestCreateFeedHidesBacklogWhenRequested is the positive case for the "hide
// existing articles" checkbox: with it set, every entry the first fetch
// creates is hidden and carries hidden_reason = 'bulk' (spec §13.3), because
// nobody read a subscribe-time backlog.
func TestCreateFeedHidesBacklogWhenRequested(t *testing.T) {
	db := forkTestDB(t)
	configureForkTestOptions(t)
	store := storage.NewStorage(db)

	userID, categoryID := createForkTestUser(t, db, "fork-hide-backlog-checked")

	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		fmt.Fprintf(w, hideBacklogFeedXMLOneItem, server.URL, server.URL)
	}))
	defer server.Close()

	feed, localizedError := CreateFeed(store, userID, &model.FeedCreationRequest{
		FeedURL:             server.URL + "/feed.xml",
		CategoryID:          categoryID,
		HideExistingEntries: true,
	})
	if localizedError != nil {
		t.Fatalf("unable to create feed: %v", localizedError.Error())
	}

	// Establish the positive case first: the fetch actually imported an
	// entry. Asserting "no unread entries" would pass trivially otherwise.
	rows, err := db.Query(`SELECT hidden, hidden_reason, status FROM entries WHERE feed_id=$1`, feed.ID)
	if err != nil {
		t.Fatalf("unable to query entries: %v", err)
	}
	defer rows.Close()

	count := 0
	for rows.Next() {
		count++
		var hidden bool
		var hiddenReason sql.NullString
		var status string
		if err := rows.Scan(&hidden, &hiddenReason, &status); err != nil {
			t.Fatalf("unable to scan entry: %v", err)
		}
		if !hidden {
			t.Fatal("expected the backlog entry to be hidden")
		}
		if !hiddenReason.Valid || hiddenReason.String != model.EntryHiddenReasonBulk {
			t.Fatalf("expected hidden_reason to be %q, got %+v", model.EntryHiddenReasonBulk, hiddenReason)
		}
		if status != model.EntryStatusUnread {
			t.Fatalf("expected the entry to stay unread (hiding is orthogonal to status), got %q", status)
		}
	}
	if count == 0 {
		t.Fatal("expected the feed's first fetch to have imported at least one entry")
	}
}

// TestCreateFeedLeavesBacklogUnhiddenWithoutTheCheckbox is the other half:
// the default (unchecked) behaviour must be completely unaffected.
func TestCreateFeedLeavesBacklogUnhiddenWithoutTheCheckbox(t *testing.T) {
	db := forkTestDB(t)
	configureForkTestOptions(t)
	store := storage.NewStorage(db)

	userID, categoryID := createForkTestUser(t, db, "fork-hide-backlog-unchecked")

	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		fmt.Fprintf(w, hideBacklogFeedXMLOneItem, server.URL, server.URL)
	}))
	defer server.Close()

	feed, localizedError := CreateFeed(store, userID, &model.FeedCreationRequest{
		FeedURL:    server.URL + "/feed.xml",
		CategoryID: categoryID,
		// HideExistingEntries left at its zero value: false.
	})
	if localizedError != nil {
		t.Fatalf("unable to create feed: %v", localizedError.Error())
	}

	rows, err := db.Query(`SELECT hidden, hidden_reason FROM entries WHERE feed_id=$1`, feed.ID)
	if err != nil {
		t.Fatalf("unable to query entries: %v", err)
	}
	defer rows.Close()

	count := 0
	for rows.Next() {
		count++
		var hidden bool
		var hiddenReason sql.NullString
		if err := rows.Scan(&hidden, &hiddenReason); err != nil {
			t.Fatalf("unable to scan entry: %v", err)
		}
		if hidden {
			t.Fatal("expected the backlog entry to stay unhidden when the checkbox was not ticked")
		}
		if hiddenReason.Valid {
			t.Fatalf("expected hidden_reason to stay NULL, got %q", hiddenReason.String)
		}
	}
	if count == 0 {
		t.Fatal("expected the feed's first fetch to have imported at least one entry")
	}
}

// TestRefreshFeedDoesNotHideEntriesFromLaterFetch is what makes this a
// one-time backlog action rather than a persistent feed setting: an entry a
// later refresh of the same feed adds must arrive unread and unhidden, even
// though the feed was subscribed to with the checkbox ticked.
func TestRefreshFeedDoesNotHideEntriesFromLaterFetch(t *testing.T) {
	db := forkTestDB(t)
	configureForkTestOptions(t)
	store := storage.NewStorage(db)

	userID, categoryID := createForkTestUser(t, db, "fork-hide-backlog-refresh")

	var itemCount int32 = 1
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		if atomic.LoadInt32(&itemCount) == 1 {
			fmt.Fprintf(w, hideBacklogFeedXMLOneItem, server.URL, server.URL)
		} else {
			fmt.Fprintf(w, hideBacklogFeedXMLTwoItems, server.URL, server.URL, server.URL)
		}
	}))
	defer server.Close()

	feed, localizedError := CreateFeed(store, userID, &model.FeedCreationRequest{
		FeedURL:             server.URL + "/feed.xml",
		CategoryID:          categoryID,
		HideExistingEntries: true,
	})
	if localizedError != nil {
		t.Fatalf("unable to create feed: %v", localizedError.Error())
	}

	readEntry := func(url string) (hidden bool, hiddenReason sql.NullString) {
		t.Helper()
		if err := db.QueryRow(
			`SELECT hidden, hidden_reason FROM entries WHERE feed_id=$1 AND url=$2`,
			feed.ID, url,
		).Scan(&hidden, &hiddenReason); err != nil {
			t.Fatalf("unable to read back entry %s: %v", url, err)
		}
		return hidden, hiddenReason
	}

	firstHidden, firstReason := readEntry(server.URL + "/first-entry")
	if !firstHidden || !firstReason.Valid || firstReason.String != model.EntryHiddenReasonBulk {
		t.Fatalf("expected the first entry to be hidden with reason 'bulk' after subscribing, got hidden=%v reason=%+v", firstHidden, firstReason)
	}

	// Now the feed publishes a second entry. Force a refresh so it is fetched.
	atomic.StoreInt32(&itemCount, 2)
	if localizedErr := RefreshFeed(store, userID, feed.ID, true); localizedErr != nil {
		t.Fatalf("unable to refresh feed: %v", localizedErr.Error())
	}

	var totalEntries int
	if err := db.QueryRow(`SELECT count(*) FROM entries WHERE feed_id=$1`, feed.ID).Scan(&totalEntries); err != nil {
		t.Fatalf("unable to count entries: %v", err)
	}
	if totalEntries != 2 {
		t.Fatalf("expected the refresh to have added the second entry, got %d entries total", totalEntries)
	}

	secondHidden, secondReason := readEntry(server.URL + "/second-entry")
	if secondHidden {
		t.Fatalf("expected the entry from a later refresh to be unhidden, got hidden=true reason=%+v", secondReason)
	}
	if secondReason.Valid {
		t.Fatalf("expected the entry from a later refresh to carry no hidden_reason, got %q", secondReason.String)
	}

	// The original backlog entry must be untouched by the refresh.
	firstHidden, firstReason = readEntry(server.URL + "/first-entry")
	if !firstHidden || !firstReason.Valid || firstReason.String != model.EntryHiddenReasonBulk {
		t.Fatalf("expected the original backlog entry to remain hidden with reason 'bulk' after a refresh, got hidden=%v reason=%+v", firstHidden, firstReason)
	}
}

// hideBacklogFeedXMLOneItem is an RSS feed with a single item, used as the
// feed's state before the first fetch (and re-served unchanged on refresh
// checks that must not add anything).
const hideBacklogFeedXMLOneItem = `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0">
<channel>
<title>Fork Hide Backlog Test Feed</title>
<link>%s</link>
<item>
	<title>First Entry</title>
	<link>%s/first-entry</link>
	<guid>fork-hide-backlog-first</guid>
	<description>the backlog entry, present since the first fetch</description>
</item>
</channel>
</rss>`

// hideBacklogFeedXMLTwoItems is the same feed after it published a second
// entry, simulating what a later refresh discovers.
const hideBacklogFeedXMLTwoItems = `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0">
<channel>
<title>Fork Hide Backlog Test Feed</title>
<link>%s</link>
<item>
	<title>First Entry</title>
	<link>%s/first-entry</link>
	<guid>fork-hide-backlog-first</guid>
	<description>the backlog entry, present since the first fetch</description>
</item>
<item>
	<title>Second Entry</title>
	<link>%s/second-entry</link>
	<guid>fork-hide-backlog-second</guid>
	<description>a new entry published after subscribing</description>
</item>
</channel>
</rss>`
