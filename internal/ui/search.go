// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui // import "miniflux.app/v2/internal/ui"

import (
	"context"
	"log/slog"
	"net/http"

	"miniflux.app/v2/internal/config"
	"miniflux.app/v2/internal/http/request"
	"miniflux.app/v2/internal/http/response"
	"miniflux.app/v2/internal/model"
	"miniflux.app/v2/internal/searchclient"
	"miniflux.app/v2/internal/ui/view"
)

func (h *handler) showSearchPage(w http.ResponseWriter, r *http.Request) {
	user, err := h.store.UserByID(request.UserID(r))
	if err != nil {
		response.HTMLServerError(w, r, err)
		return
	}

	searchQuery := request.QueryStringParam(r, "q", "")
	unreadOnly := request.QueryBoolParam(r, "unread", false)
	offset := request.QueryIntParam(r, "offset", 0)

	var entries model.Entries
	var entriesCount int
	var degraded bool

	if searchQuery != "" {
		fallback := func() (model.Entries, int, error) {
			builder := h.store.NewEntryQueryBuilder(user.ID).
				WithSearchQuery(searchQuery).
				WithoutContent().
				WithOffset(offset).
				WithLimit(user.EntriesPerPage)

			if unreadOnly {
				builder = builder.WithStatuses(model.EntryStatusUnread)
			}

			return builder.GetEntriesWithCount()
		}

		hydrate := func(entryIDs []int64) (model.Entries, error) {
			if len(entryIDs) == 0 {
				return model.Entries{}, nil
			}
			return h.store.NewEntryQueryBuilder(user.ID).
				WithEntryIDs(entryIDs...).
				WithoutContent().
				GetEntries()
		}

		entries, entriesCount, degraded, err = resolveSearchResults(
			r.Context(),
			config.Opts.SearchSidecarURL(),
			searchQuery,
			unreadOnly,
			offset,
			user.EntriesPerPage,
			hydrate,
			fallback,
		)
		if err != nil {
			response.HTMLServerError(w, r, err)
			return
		}
	}

	view := view.New(h.tpl, r)
	pagination := getPagination(h.routePath("/search"), entriesCount, offset, user.EntriesPerPage)
	pagination.SearchQuery = searchQuery
	pagination.UnreadOnly = unreadOnly

	view.Set("searchQuery", searchQuery)
	view.Set("searchUnreadOnly", unreadOnly)
	view.Set("entries", entries)
	view.Set("total", entriesCount)
	// searchDegraded is set for a later UI task (spec §8.2's "mode picker
	// / snippet rendering" line) to surface a user-facing notice; the
	// load-bearing property required by spec §8.3 — the page still
	// renders results when the sidecar fails — does not depend on the
	// template consuming this value, and is covered directly by
	// resolveSearchResults' own tests below.
	view.Set("searchDegraded", degraded)
	view.Set("pagination", pagination)
	view.Set("menu", "search")
	view.Set("user", user)
	navMetadata, _ := h.store.GetNavMetadata(user.ID)
	view.Set("countUnread", navMetadata.CountUnread)
	view.Set("countErrorFeeds", navMetadata.CountErrorFeeds)
	view.Set("hasSaveEntry", navMetadata.HasSaveEntry)

	response.HTML(w, r, view.Render("search"))
}

// resolveSearchResults returns the entries and total count to render on
// the search page.
//
// When sidecarURL is empty, or offset is non-zero (the sidecar's search
// API has no offset parameter, so paging past the first page always uses
// the fallback path, which supports it natively), fallback runs directly
// and degraded is false: neither case is a failure.
//
// Otherwise the sidecar is queried. ANY failure — connection refused,
// timeout, non-200 status, a malformed body (searchclient.Client.Search
// folds all of these into one error), or a failure while hydrating the
// sidecar's entry ids back into full model.Entry values via hydrate — is
// treated identically: log it, then run fallback and report degraded so
// the caller can show a notice. Search gets worse; the reader keeps
// working (spec §8.3). No error from this function ever propagates past
// a fallback failure itself.
func resolveSearchResults(
	ctx context.Context,
	sidecarURL string,
	query string,
	unreadOnly bool,
	offset int,
	limit int,
	hydrate func([]int64) (model.Entries, error),
	fallback func() (model.Entries, int, error),
) (model.Entries, int, bool, error) {
	if sidecarURL == "" || offset > 0 {
		entries, count, err := fallback()
		return entries, count, false, err
	}

	client := searchclient.NewClient(sidecarURL)
	resp, err := client.Search(ctx, searchclient.SearchRequest{
		Query:      query,
		Limit:      limit,
		UnreadOnly: unreadOnly,
	})
	if err != nil {
		slog.Warn("ui: search sidecar unavailable, falling back to built-in search",
			slog.String("sidecar_url", sidecarURL),
			slog.Any("error", err),
		)
		entries, count, ferr := fallback()
		return entries, count, true, ferr
	}

	entryIDs := make([]int64, len(resp.Entries))
	for i, hit := range resp.Entries {
		entryIDs[i] = hit.EntryID
	}

	hydrated, err := hydrate(entryIDs)
	if err != nil {
		slog.Warn("ui: unable to load entries for sidecar search results, falling back to built-in search",
			slog.String("sidecar_url", sidecarURL),
			slog.Any("error", err),
		)
		entries, count, ferr := fallback()
		return entries, count, true, ferr
	}

	ordered := orderEntriesByID(hydrated, entryIDs)
	return ordered, len(ordered), false, nil
}

// orderEntriesByID returns entries re-ordered to match ids, dropping any
// id with no corresponding entry (e.g. deleted between the sidecar's
// search and this hydration). The store has no reason to return rows in
// the order a WHERE id IN (...) clause listed them, but the sidecar's
// ranking — the whole reason to call it — lives in that order.
func orderEntriesByID(entries model.Entries, ids []int64) model.Entries {
	byID := make(map[int64]*model.Entry, len(entries))
	for _, e := range entries {
		byID[e.ID] = e
	}

	ordered := make(model.Entries, 0, len(ids))
	for _, id := range ids {
		if e, ok := byID[id]; ok {
			ordered = append(ordered, e)
		}
	}
	return ordered
}
