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

func (h *handler) showUnreadPage(w http.ResponseWriter, r *http.Request) {
	user, err := h.store.UserByID(request.UserID(r))
	if err != nil {
		response.HTMLServerError(w, r, err)
		return
	}

	offset := request.QueryIntParam(r, "offset", 0)

	// The per-view sort picker (spec §6.3 amendment, task 10 part B):
	// defaults to the reader's saved preference, overridable via the
	// "order"/"direction" query parameters. See parseEntryOrder's doc
	// comment for why an invalid value falls back rather than reaching
	// WithSorting unvalidated.
	listOrder := parseEntryOrder(r, user.EntryOrder)
	listDirection := parseEntryDirection(r, user.EntryDirection)

	builder := h.store.NewEntryQueryBuilder(user.ID).
		WithStatuses(model.EntryStatusUnread).
		WithHidden(false)
	builder = withStableEntrySorting(builder, listOrder, listDirection)

	entries, countUnread, err := builder.
		WithOffset(offset).
		WithLimit(user.EntriesPerPage).
		WithGloballyVisible().
		WithoutContent().
		GetEntriesWithCount()
	if err != nil {
		response.HTMLServerError(w, r, err)
		return
	}

	if offset >= countUnread && countUnread > 0 {
		offset = 0

		retryBuilder := h.store.NewEntryQueryBuilder(user.ID).
			WithStatuses(model.EntryStatusUnread).
			WithHidden(false)
		retryBuilder = withStableEntrySorting(retryBuilder, listOrder, listDirection)

		entries, countUnread, err = retryBuilder.
			WithLimit(user.EntriesPerPage).
			WithGloballyVisible().
			WithoutContent().
			GetEntriesWithCount()
		if err != nil {
			response.HTMLServerError(w, r, err)
			return
		}
	}

	view := view.New(h.tpl, r)
	pagination := getPagination(h.routePath("/unread"), countUnread, offset, user.EntriesPerPage)
	pagination.Order = listOrder
	pagination.Direction = listDirection
	view.Set("entries", entries)
	view.Set("pagination", pagination)
	view.Set("order", listOrder)
	view.Set("direction", listDirection)
	view.Set("sortOrders", searchSortOrders)
	view.Set("menu", "unread")
	view.Set("user", user)
	navMetadata, _ := h.store.GetNavMetadata(user.ID)
	view.Set("countUnread", countUnread)
	view.Set("countErrorFeeds", navMetadata.CountErrorFeeds)
	view.Set("hasSaveEntry", navMetadata.HasSaveEntry)

	response.HTML(w, r, view.Render("unread_entries"))
}
