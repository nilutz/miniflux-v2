// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui // import "miniflux.app/v2/internal/ui"

import (
	"net/http"

	"miniflux.app/v2/internal/http/request"
	"miniflux.app/v2/internal/http/response"
)

// markFeedAsHidden mirrors markFeedAsRead (feed_mark_as_read.go), but hides
// instead of marking read. hidden_reason is always 'bulk' (spec §13.3).
func (h *handler) markFeedAsHidden(w http.ResponseWriter, r *http.Request) {
	feedID := request.RouteInt64Param(r, "feedID")
	userID := request.UserID(r)

	checkedAt, err := h.store.CheckedAt(userID, feedID)
	if err != nil {
		response.HTMLNotFound(w, r)
		return
	}

	if err = h.store.MarkFeedAsHidden(userID, feedID, checkedAt); err != nil {
		response.HTMLServerError(w, r, err)
		return
	}

	response.HTMLRedirect(w, r, h.routePath("/feeds"))
}
