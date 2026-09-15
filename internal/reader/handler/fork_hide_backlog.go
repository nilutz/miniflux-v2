// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package handler // import "miniflux.app/v2/internal/reader/handler"

import (
	"log/slog"

	"miniflux.app/v2/internal/storage"
)

// hideBacklogIfRequested hides every entry the feed's first fetch just
// created, when the "hide existing articles" checkbox (spec §13.3) was
// ticked on subscribe.
//
// It can only run after store.CreateFeed has inserted the entries, which is
// why it lives here rather than in the processor — the same constraint that
// shapes markFullTextFetched. Because feedID is brand new at this point,
// every entry belonging to it *is* this first batch; a later refresh of the
// same feed is a separate call that never reaches this function, so its
// entries arrive unread as normal — this is a one-time action on the
// backlog, not a persistent feed setting.
func hideBacklogIfRequested(store *storage.Storage, userID, feedID int64, hideExistingEntries bool) {
	if !hideExistingEntries {
		return
	}

	if err := store.HideFeedBacklog(userID, feedID); err != nil {
		slog.Error("Unable to hide feed backlog",
			slog.Int64("user_id", userID),
			slog.Int64("feed_id", feedID),
			slog.Any("error", err),
		)
	}
}
