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

func (h *handler) showReadEntryPage(w http.ResponseWriter, r *http.Request) {
	user, err := h.store.UserByID(request.UserID(r))
	if err != nil {
		response.HTMLServerError(w, r, err)
		return
	}

	entryID := request.RouteInt64Param(r, "entryID")

	entry, err := h.store.NewEntryQueryBuilder(user.ID).
		WithEntryIDs(entryID).
		GetEntry()
	if err != nil {
		response.HTMLServerError(w, r, err)
		return
	}

	if entry == nil {
		response.HTMLNotFound(w, r)
		return
	}

	if user.AlwaysOpenExternalLinks {
		response.HTMLRedirect(w, r, entry.URL)
		return
	}

	// Must match showHistoryPage's effective order exactly, or prev/next
	// walks a different list than the one the reader was just looking at.
	listOrder := parseEntryOrder(r, "changed_at")
	listDirection := parseEntryDirection(r, "desc")

	prevEntry, nextEntry, err := h.store.NewEntryPaginationBuilder(user.ID, entry.ID, listOrder, listDirection).
		WithStatus(model.EntryStatusRead).
		Entries()
	if err != nil {
		response.HTMLServerError(w, r, err)
		return
	}

	nextEntryRoute := ""
	if nextEntry != nil {
		nextEntryRoute = h.routePath("/history/entry/%d", nextEntry.ID)
	}

	prevEntryRoute := ""
	if prevEntry != nil {
		prevEntryRoute = h.routePath("/history/entry/%d", prevEntry.ID)
	}

	view := view.New(h.tpl, r)
	view.Set("entry", entry)
	view.Set("prevEntry", prevEntry)
	view.Set("nextEntry", nextEntry)
	view.Set("nextEntryRoute", nextEntryRoute)
	view.Set("prevEntryRoute", prevEntryRoute)
	view.Set("order", listOrder)
	view.Set("direction", listDirection)
	// similarEntries is nil (block simply absent) whenever the search
	// sidecar is unavailable or unconfigured - see entry_similar.go.
	view.Set("similarEntries", h.similarEntries(r.Context(), user.ID, entry.ID))
	view.Set("menu", "history")
	view.Set("user", user)
	navMetadata, _ := h.store.GetNavMetadata(user.ID)
	view.Set("countUnread", navMetadata.CountUnread)
	view.Set("countErrorFeeds", navMetadata.CountErrorFeeds)
	view.Set("hasSaveEntry", navMetadata.HasSaveEntry)

	response.HTML(w, r, view.Render("entry"))
}
