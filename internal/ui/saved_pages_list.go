// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui // import "miniflux.app/v2/internal/ui"

import (
	"net/http"

	"miniflux.app/v2/internal/http/request"
	"miniflux.app/v2/internal/http/response"
	"miniflux.app/v2/internal/model"
	"miniflux.app/v2/internal/ui/view"
)

// showSavedPagesPage lists the entries in the user's "save a page" feed
// (spec §13.4), reachable from the main navigation instead of only by
// recognising the synthetic feed among real subscriptions.
//
// It must never create that feed: h.store.SavedPagesFeed is a read-only
// lookup, unlike storage.GetOrCreateSavedPagesFeed which submitSavedPage
// uses. A user who has never saved a page sees an empty state here, and
// this handler leaves no row behind for them in the feeds table.
func (h *handler) showSavedPagesPage(w http.ResponseWriter, r *http.Request) {
	user, err := h.store.UserByID(request.UserID(r))
	if err != nil {
		response.HTMLServerError(w, r, err)
		return
	}

	feed, err := h.store.SavedPagesFeed(user.ID)
	if err != nil {
		response.HTMLServerError(w, r, err)
		return
	}

	offset := request.QueryIntParam(r, "offset", 0)

	// The per-view sort picker and hidden filter (spec §6.3 amendment and
	// §13.3, task 10 parts A and B): default to the reader's saved
	// preference, overridable via the "order"/"direction"/"excludeHidden"
	// query parameters. Routed through parseEntryOrder/parseEntryDirection
	// so an unvalidated request value never reaches WithSorting.
	listOrder := parseEntryOrder(r, user.EntryOrder)
	listDirection := parseEntryDirection(r, user.EntryDirection)
	excludeHidden := request.QueryBoolParam(r, "excludeHidden", false)

	var entries model.Entries
	var count int
	if feed != nil {
		builder := withStableEntrySorting(h.store.NewEntryQueryBuilder(user.ID).WithFeedID(feed.ID), listOrder, listDirection)
		if excludeHidden {
			builder = builder.WithHidden(false)
		}

		entries, count, err = builder.
			WithOffset(offset).
			WithLimit(user.EntriesPerPage).
			WithoutContent().
			GetEntriesWithCount()
		if err != nil {
			response.HTMLServerError(w, r, err)
			return
		}
	}

	v := view.New(h.tpl, r)
	pagination := getPagination(h.routePath("/saved-pages"), count, offset, user.EntriesPerPage)
	pagination.Order = listOrder
	pagination.Direction = listDirection
	pagination.ExcludeHidden = excludeHidden
	v.Set("total", count)
	v.Set("entries", entries)
	v.Set("pagination", pagination)
	v.Set("order", listOrder)
	v.Set("direction", listDirection)
	v.Set("excludeHidden", excludeHidden)
	v.Set("sortOrders", searchSortOrders)
	v.Set("menu", "saved-pages")
	v.Set("user", user)
	navMetadata, _ := h.store.GetNavMetadata(user.ID)
	v.Set("countUnread", navMetadata.CountUnread)
	v.Set("countErrorFeeds", navMetadata.CountErrorFeeds)
	v.Set("hasSaveEntry", navMetadata.HasSaveEntry)

	response.HTML(w, r, v.Render("saved_pages_entries"))
}
