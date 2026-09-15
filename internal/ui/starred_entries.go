// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui // import "miniflux.app/v2/internal/ui"

import (
	"net/http"

	"miniflux.app/v2/internal/http/request"
	"miniflux.app/v2/internal/http/response"
	"miniflux.app/v2/internal/ui/view"
)

func (h *handler) showStarredPage(w http.ResponseWriter, r *http.Request) {
	user, err := h.store.UserByID(request.UserID(r))
	if err != nil {
		response.HTMLServerError(w, r, err)
		return
	}

	offset := request.QueryIntParam(r, "offset", 0)

	// The per-view sort picker (spec §6.3 amendment, task 10 part B):
	// defaults to the reader's saved preference, overridable via the
	// "order"/"direction" query parameters.
	listOrder := parseEntryOrder(r, user.EntryOrder)
	listDirection := parseEntryDirection(r, user.EntryDirection)

	builder := withStableEntrySorting(h.store.NewEntryQueryBuilder(user.ID).WithStarred(true), listOrder, listDirection)
	entries, count, err := builder.
		WithOffset(offset).
		WithLimit(user.EntriesPerPage).
		WithoutContent().
		GetEntriesWithCount()
	if err != nil {
		response.HTMLServerError(w, r, err)
		return
	}

	view := view.New(h.tpl, r)
	pagination := getPagination(h.routePath("/starred"), count, offset, user.EntriesPerPage)
	pagination.Order = listOrder
	pagination.Direction = listDirection
	view.Set("total", count)
	view.Set("entries", entries)
	view.Set("pagination", pagination)
	view.Set("order", listOrder)
	view.Set("direction", listDirection)
	view.Set("sortOrders", searchSortOrders)
	view.Set("menu", "starred")
	view.Set("user", user)
	navMetadata, _ := h.store.GetNavMetadata(user.ID)
	view.Set("countUnread", navMetadata.CountUnread)
	view.Set("countErrorFeeds", navMetadata.CountErrorFeeds)
	view.Set("hasSaveEntry", navMetadata.HasSaveEntry)

	response.HTML(w, r, view.Render("starred_entries"))
}
