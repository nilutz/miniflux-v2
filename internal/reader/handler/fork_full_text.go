// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package handler // import "miniflux.app/v2/internal/reader/handler"

import (
	"log/slog"
	"time"

	"miniflux.app/v2/internal/storage"
)

// markFullTextFetched records full_text_fetched_at for the entries the crawler
// scraped successfully during this refresh.
//
// The column is the signal that tells the search sidecar whether an entry holds
// a real article or only the excerpt the feed carried, and it is what keeps the
// -backfill-full-text command from re-scraping entries the live crawler already
// fetched. It can only be written after the entries exist, which is why this
// runs here rather than inside the processor.
//
// A failure is logged and swallowed: the entry keeps its scraped content and
// simply stays pending, so the backfill picks it up later.
func markFullTextFetched(store *storage.Storage, feedID int64, entryHashes []string) {
	if len(entryHashes) == 0 {
		return
	}

	if err := store.MarkFullTextFetchedByHashes(feedID, entryHashes, time.Now()); err != nil {
		slog.Error("Unable to record which entries hold full text",
			slog.Int64("feed_id", feedID),
			slog.Int("entry_count", len(entryHashes)),
			slog.Any("error", err),
		)
	}
}
