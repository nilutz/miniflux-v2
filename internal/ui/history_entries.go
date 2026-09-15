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

func (h *handler) showHistoryPage(w http.ResponseWriter, r *http.Request) {
	user, err := h.store.UserByID(request.UserID(r))
	if err != nil {
		response.HTMLServerError(w, r, err)
		return
	}

	offset := request.QueryIntParam(r, "offset", 0)

	// The per-view sort picker (spec §6.3 amendment, task 10 part B).
	// History's own default has always been "most recently read first"
	// (changed_at, descending) rather than the reader's saved
	// entry_sorting_order/direction - that stays the fallback here, and
	// the picker can override it for this view only.
	listOrder := parseEntryOrder(r, "changed_at")
	listDirection := parseEntryDirection(r, "desc")

	builder := withStableEntrySorting(h.store.NewEntryQueryBuilder(user.ID).WithStatuses(model.EntryStatusRead), listOrder, listDirection)
	entries, count, err := builder.
		WithoutContent().
		WithOffset(offset).
		WithLimit(user.EntriesPerPage).
		GetEntriesWithCount()
	if err != nil {
		response.HTMLServerError(w, r, err)
		return
	}

	view := view.New(h.tpl, r)
	pagination := getPagination(h.routePath("/history"), count, offset, user.EntriesPerPage)
	pagination.Order = listOrder
	pagination.Direction = listDirection
	view.Set("entries", entries)
	view.Set("total", count)
	view.Set("pagination", pagination)
	view.Set("order", listOrder)
	view.Set("direction", listDirection)
	view.Set("sortOrders", searchSortOrders)
	view.Set("menu", "history")
	view.Set("user", user)
	navMetadata, _ := h.store.GetNavMetadata(user.ID)
	view.Set("countUnread", navMetadata.CountUnread)
	view.Set("countErrorFeeds", navMetadata.CountErrorFeeds)
	view.Set("hasSaveEntry", navMetadata.HasSaveEntry)

	response.HTML(w, r, view.Render("history_entries"))
}
