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

// showSavedPagesEntryPage is showSavedPagesPage's single-entry
// counterpart, mirroring showStarredEntryPage's shape. The entry lookup
// itself is scoped to the requesting user by NewEntryQueryBuilder(user.ID)
// as usual, so one user's saved pages are never reachable by another.
func (h *handler) showSavedPagesEntryPage(w http.ResponseWriter, r *http.Request) {
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

	if entry.ShouldMarkAsReadOnView(user) {
		if err := h.store.SetEntriesStatus(user.ID, []int64{entry.ID}, model.EntryStatusRead); err != nil {
			response.HTMLServerError(w, r, err)
			return
		}

		entry.Status = model.EntryStatusRead
	}

	if user.AlwaysOpenExternalLinks {
		response.HTMLRedirect(w, r, entry.URL)
		return
	}

	// Must match showSavedPagesPage's effective order/filter exactly, or
	// prev/next walks a different list than the one the reader was just
	// looking at (task 10 shipped this wrong once for search and had to
	// fix it in a second round).
	listOrder := parseEntryOrder(r, user.EntryOrder)
	listDirection := parseEntryDirection(r, user.EntryDirection)
	excludeHidden := request.QueryBoolParam(r, "excludeHidden", false)

	entryPaginationBuilder := h.store.NewEntryPaginationBuilder(user.ID, entry.ID, listOrder, listDirection).
		WithFeedID(entry.FeedID)
	if excludeHidden {
		entryPaginationBuilder = entryPaginationBuilder.WithNotHiddenOrEntryID(entry.ID)
	}

	prevEntry, nextEntry, err := entryPaginationBuilder.Entries()
	if err != nil {
		response.HTMLServerError(w, r, err)
		return
	}

	nextEntryRoute := ""
	if nextEntry != nil {
		nextEntryRoute = h.routePath("/saved-pages/entry/%d", nextEntry.ID)
	}

	prevEntryRoute := ""
	if prevEntry != nil {
		prevEntryRoute = h.routePath("/saved-pages/entry/%d", prevEntry.ID)
	}

	v := view.New(h.tpl, r)
	v.Set("entry", entry)
	v.Set("prevEntry", prevEntry)
	v.Set("nextEntry", nextEntry)
	v.Set("nextEntryRoute", nextEntryRoute)
	v.Set("prevEntryRoute", prevEntryRoute)
	v.Set("order", listOrder)
	v.Set("direction", listDirection)
	// searchExcludeHidden is the generic key entry.html's pagination dict
	// reads for every single-entry view, not just search's - see
	// entry_search.go, which sets the same key for the same reason.
	v.Set("searchExcludeHidden", excludeHidden)
	// similarEntries is nil (block simply absent) whenever the search
	// sidecar is unavailable or unconfigured - see entry_similar.go.
	v.Set("similarEntries", h.similarEntries(r.Context(), user.ID, entry.ID))
	v.Set("menu", "saved-pages")
	v.Set("user", user)
	navMetadata, _ := h.store.GetNavMetadata(user.ID)
	v.Set("countUnread", navMetadata.CountUnread)
	v.Set("countErrorFeeds", navMetadata.CountErrorFeeds)
	v.Set("hasSaveEntry", navMetadata.HasSaveEntry)

	response.HTML(w, r, v.Render("entry"))
}
