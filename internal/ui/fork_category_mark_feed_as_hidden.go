// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui // import "miniflux.app/v2/internal/ui"

import (
	"net/http"

	"miniflux.app/v2/internal/http/request"
	"miniflux.app/v2/internal/http/response"
)

// markCategoryFeedAsHidden mirrors markCategoryFeedAsRead
// (category_mark_feed_as_read.go), but hides instead of marking read. It
// reuses MarkFeedAsHidden, exactly as its read counterpart reuses
// MarkFeedAsRead: the scope (one feed) is identical, only the entry point
// differs. hidden_reason is always 'bulk' (spec §13.3).
func (h *handler) markCategoryFeedAsHidden(w http.ResponseWriter, r *http.Request) {
	feedID := request.RouteInt64Param(r, "feedID")
	categoryID := request.RouteInt64Param(r, "categoryID")
	userID := request.UserID(r)

	exists, err := h.store.CategoryFeedExists(userID, categoryID, feedID)
	if err != nil {
		response.HTMLServerError(w, r, err)
		return
	}
	if !exists {
		response.HTMLNotFound(w, r)
		return
	}

	checkedAt, err := h.store.CheckedAt(userID, feedID)
	if err != nil {
		response.HTMLNotFound(w, r)
		return
	}

	if err = h.store.MarkFeedAsHidden(userID, feedID, checkedAt); err != nil {
		response.HTMLServerError(w, r, err)
		return
	}

	response.HTMLRedirect(w, r, h.routePath("/category/%d/feeds", categoryID))
}
