// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui // import "miniflux.app/v2/internal/ui"

import (
	json_parser "encoding/json"
	"errors"
	"net/http"

	"miniflux.app/v2/internal/http/request"
	"miniflux.app/v2/internal/http/response"
	"miniflux.app/v2/internal/model"
)

// hideEntries bulk-hides a caller-supplied list of entries, mirroring
// updateEntriesStatus's decode/validate/store shape (entry_update_status.go)
// but for the hidden flag rather than status.
//
// It is the backend for "Mark page as hidden" (app.js
// markPageAsHiddenAction): dismissing a whole page of the unread list at
// once without reading any of it, so it always writes
// hidden_reason = model.EntryHiddenReasonBulk — spec §13.3 reserves NULL for
// a hand judgement on a single entry, which is what entry_toggle_hidden.go
// already covers.
func (h *handler) hideEntries(w http.ResponseWriter, r *http.Request) {
	var entriesStatusUpdateRequest model.EntriesStatusUpdateRequest
	if err := json_parser.NewDecoder(r.Body).Decode(&entriesStatusUpdateRequest); err != nil {
		response.JSONBadRequest(w, r, err)
		return
	}

	if len(entriesStatusUpdateRequest.EntryIDs) == 0 {
		response.JSONBadRequest(w, r, errors.New("the list of entries cannot be empty"))
		return
	}

	if err := h.store.SetEntriesHiddenState(request.UserID(r), entriesStatusUpdateRequest.EntryIDs, true, model.EntryHiddenReasonBulk); err != nil {
		response.JSONServerError(w, r, err)
		return
	}

	response.JSON(w, r, "OK")
}
