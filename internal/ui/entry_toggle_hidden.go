// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui // import "miniflux.app/v2/internal/ui"

import (
	"net/http"

	"miniflux.app/v2/internal/http/request"
	"miniflux.app/v2/internal/http/response"
)

// toggleHidden flips an entry's hidden flag, mirroring toggleStarred.
//
// Spec §13.3: hidden is a separate boolean, exactly like starred, so it
// gets the same toggle-and-report-OK handling rather than a status value.
func (h *handler) toggleHidden(w http.ResponseWriter, r *http.Request) {
	entryID := request.RouteInt64Param(r, "entryID")
	if err := h.store.ToggleHidden(request.UserID(r), entryID); err != nil {
		response.JSONServerError(w, r, err)
		return
	}

	response.JSON(w, r, "OK")
}
