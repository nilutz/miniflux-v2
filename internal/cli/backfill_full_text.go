// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cli // import "miniflux.app/v2/internal/cli"

import (
	"log/slog"
	"time"

	"miniflux.app/v2/internal/reader/processor"
	"miniflux.app/v2/internal/storage"
)

// backfillBatchSize is the number of entry ids fetched per database round trip.
const backfillBatchSize = 100

// backfillFullText scrapes the original web page for entries that have never
// been fetched, one at a time, and records progress on each entry so the
// command can be interrupted and resumed.
//
// Enabling the crawler only affects new entries, so this is the only way to
// obtain full text for entries that already exist.
func backfillFullText(store *storage.Storage, delay time.Duration) {
	var lastID int64
	var processed, succeeded, failed int

	for {
		entryIDs, err := store.EntryIDsWithoutFullText(lastID, backfillBatchSize)
		if err != nil {
			printErrorAndExit(err)
		}

		if len(entryIDs) == 0 {
			break
		}

		for _, entryID := range entryIDs {
			lastID = entryID
			processed++

			if backfillEntry(store, entryID) {
				succeeded++
			} else {
				failed++
			}

			if delay > 0 {
				time.Sleep(delay)
			}
		}

		slog.Info("Full text backfill progress",
			slog.Int("processed", processed),
			slog.Int("succeeded", succeeded),
			slog.Int("failed", failed),
			slog.Int64("last_entry_id", lastID),
		)
	}

	slog.Info("Full text backfill completed",
		slog.Int("processed", processed),
		slog.Int("succeeded", succeeded),
		slog.Int("failed", failed),
	)
}

// backfillEntry scrapes a single entry. It returns false when the entry could
// not be fetched, in which case full_text_fetched_at is left null so a later
// run retries it.
func backfillEntry(store *storage.Storage, entryID int64) bool {
	userID, feedID, err := store.EntryOwner(entryID)
	if err != nil {
		slog.Warn("Unable to find entry owner", slog.Int64("entry_id", entryID), slog.Any("error", err))
		return false
	}

	entry, err := store.NewEntryQueryBuilder(userID).WithEntryIDs(entryID).GetEntry()
	if err != nil {
		slog.Warn("Unable to load entry", slog.Int64("entry_id", entryID), slog.Any("error", err))
		return false
	}
	if entry == nil {
		slog.Warn("Entry disappeared before it could be scraped", slog.Int64("entry_id", entryID))
		return false
	}

	user, err := store.UserByID(userID)
	if err != nil || user == nil {
		slog.Warn("Unable to load user", slog.Int64("entry_id", entryID), slog.Any("error", err))
		return false
	}

	feed, err := store.FeedByID(userID, feedID)
	if err != nil || feed == nil {
		slog.Warn("Unable to load feed", slog.Int64("entry_id", entryID), slog.Any("error", err))
		return false
	}

	// ProcessEntryWebPage reports no error when the page is fetched but holds
	// no extractable article (a paywall, a JavaScript-only page, a link farm).
	// Readability does not fail on such a page: it returns either nothing, in
	// which case the processor leaves entry.Content alone, or markup with no
	// text in it such as "<p></p>". Comparing the content before and after,
	// and requiring the result to hold text, is the only signal available here
	// that full text was really obtained.
	originalContent := entry.Content

	if err := processor.ProcessEntryWebPage(feed, entry, user); err != nil {
		slog.Warn("Unable to scrape entry",
			slog.Int64("entry_id", entryID),
			slog.String("entry_url", entry.URL),
			slog.Any("error", err),
		)
		return false
	}

	if entry.Content == originalContent || !processor.ContainsArticleText(entry.Content) {
		slog.Warn("Scraper returned no article content, leaving entry pending",
			slog.Int64("entry_id", entryID),
			slog.String("entry_url", entry.URL),
		)
		return false
	}

	if err := store.UpdateEntryTitleAndContent(entry); err != nil {
		slog.Warn("Unable to store entry content", slog.Int64("entry_id", entryID), slog.Any("error", err))
		return false
	}

	if err := store.MarkFullTextFetched(entryID, time.Now()); err != nil {
		slog.Warn("Unable to mark entry as fetched", slog.Int64("entry_id", entryID), slog.Any("error", err))
		return false
	}

	return true
}
