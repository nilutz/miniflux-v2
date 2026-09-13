// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cli // import "miniflux.app/v2/internal/cli"

import (
	"log/slog"

	"miniflux.app/v2/internal/config"
	"miniflux.app/v2/internal/storage"
)

// runCleanupTasks removes expired sessions and orphan icons.
//
// Entry archiving is deliberately absent in this fork: the entry corpus backs
// the search index, so entries are never deleted. ArchiveEntries deletes rows
// and writes entry_tombstones that permanently block re-ingestion, and it is
// disabled only by a negative interval — a zero value deletes everything older
// than one day. Removing the call is safer than relying on configuration.
func runCleanupTasks(store *storage.Storage) {
	if nbWebSessions, err := store.CleanOldWebSessions(config.Opts.CleanupRemoveSessionsInterval()); err != nil {
		slog.Error("Unable to clean old web sessions", slog.Any("error", err))
	} else {
		slog.Info("Sessions cleanup completed",
			slog.Int64("web_sessions_removed", nbWebSessions),
		)
	}

	if nbIcons, err := store.CleanupOrphanIcons(); err != nil {
		slog.Error("Unable to clean orphan icons", slog.Any("error", err))
	} else {
		slog.Info("Orphan icons cleanup completed",
			slog.Int64("orphan_icons_removed", nbIcons),
		)
	}
}
