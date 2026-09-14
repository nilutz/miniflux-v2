// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui // import "miniflux.app/v2/internal/ui"

import (
	"context"
	"log/slog"

	"miniflux.app/v2/internal/config"
	"miniflux.app/v2/internal/model"
	"miniflux.app/v2/internal/searchclient"
)

// similarEntries returns the entries most similar to entryID (spec §7),
// or nil whenever that block should simply be absent from the page:
// SEARCH_SIDECAR_URL is unset, the sidecar call fails for any reason
// (unreachable, too slow, a bad response), or there is nothing left to
// show after hydrating its hits back into full entries. Every failure is
// logged and swallowed here - never returned as an error - because the
// entry page must never fail, block, or show a stack trace over a
// recommendation sidebar (spec §8.3).
func (h *handler) similarEntries(ctx context.Context, userID, entryID int64) model.Entries {
	sidecarURL := config.Opts.SearchSidecarURL()
	if sidecarURL == "" {
		return nil
	}

	client := searchclient.NewClient(sidecarURL)
	resp, err := client.Similar(ctx, searchclient.SimilarRequest{EntryID: entryID})
	if err != nil {
		slog.Debug("ui: similar-articles sidecar unavailable, omitting the block",
			slog.Int64("entry_id", entryID),
			slog.Any("error", err),
		)
		return nil
	}
	if len(resp.Entries) == 0 {
		return nil
	}

	ids := make([]int64, len(resp.Entries))
	for i, hit := range resp.Entries {
		ids[i] = hit.EntryID
	}

	entries, err := h.store.NewEntryQueryBuilder(userID).
		WithEntryIDs(ids...).
		WithoutContent().
		GetEntries()
	if err != nil {
		slog.Warn("ui: unable to load similar-article entries, omitting the block",
			slog.Int64("entry_id", entryID),
			slog.Any("error", err),
		)
		return nil
	}

	return orderEntriesByID(entries, ids)
}
