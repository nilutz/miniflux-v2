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

func (h *handler) showCategoryEntriesPage(w http.ResponseWriter, r *http.Request) {
	user, err := h.store.UserByID(request.UserID(r))
	if err != nil {
		response.HTMLServerError(w, r, err)
		return
	}

	categoryID := request.RouteInt64Param(r, "categoryID")
	category, err := h.store.Category(request.UserID(r), categoryID)
	if err != nil {
		response.HTMLServerError(w, r, err)
		return
	}

	if category == nil {
		response.HTMLNotFound(w, r)
		return
	}

	offset := request.QueryIntParam(r, "offset", 0)

	// The per-view sort picker (spec §6.3 amendment, task 10 part B):
	// defaults to the reader's saved preference, overridable via the
	// "order"/"direction" query parameters.
	listOrder := parseEntryOrder(r, user.EntryOrder)
	listDirection := parseEntryDirection(r, user.EntryDirection)

	builder := withStableEntrySorting(h.store.NewEntryQueryBuilder(user.ID).
		WithCategoryID(category.ID).
		WithStatuses(model.EntryStatusUnread).
		WithHidden(false),
		listOrder, listDirection)
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
	pagination := getPagination(h.routePath("/category/%d/entries", category.ID), count, offset, user.EntriesPerPage)
	pagination.Order = listOrder
	pagination.Direction = listDirection
	view.Set("category", category)
	view.Set("total", count)
	view.Set("entries", entries)
	view.Set("pagination", pagination)
	view.Set("order", listOrder)
	view.Set("direction", listDirection)
	view.Set("sortOrders", searchSortOrders)
	view.Set("menu", "categories")
	view.Set("user", user)
	navMetadata, _ := h.store.GetNavMetadata(user.ID)
	view.Set("countUnread", navMetadata.CountUnread)
	view.Set("countErrorFeeds", navMetadata.CountErrorFeeds)
	view.Set("hasSaveEntry", navMetadata.HasSaveEntry)
	view.Set("showOnlyUnreadEntries", true)

	response.HTML(w, r, view.Render("category_entries"))
}
