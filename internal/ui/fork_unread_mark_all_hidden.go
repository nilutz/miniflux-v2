// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui // import "miniflux.app/v2/internal/ui"

import (
	"net/http"

	"miniflux.app/v2/internal/http/request"
	"miniflux.app/v2/internal/http/response"
)

// markAllAsHidden mirrors markAllAsRead (unread_mark_all_read.go), but hides
// instead of marking read. hidden_reason is always 'bulk' (spec §13.3): this
// dismisses a whole scope of entries at once, not a single one judged by
// hand.
func (h *handler) markAllAsHidden(w http.ResponseWriter, r *http.Request) {
	if err := h.store.MarkGloballyVisibleFeedsAsHidden(request.UserID(r)); err != nil {
		response.JSONServerError(w, r, err)
		return
	}

	response.JSON(w, r, "OK")
}
