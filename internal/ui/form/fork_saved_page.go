// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package form // import "miniflux.app/v2/internal/ui/form"

import (
	"net/http"

	"miniflux.app/v2/internal/locale"
	"miniflux.app/v2/internal/urllib"
)

// SavedPageForm carries the URL a user pastes to save a page that has no
// feed as a searchable entry. See spec §13.4.
type SavedPageForm struct {
	URL string
}

// Validate makes sure the form values are valid.
func (s *SavedPageForm) Validate() *locale.LocalizedError {
	if s.URL == "" {
		return locale.NewLocalizedError("error.saved_page_url_required")
	}

	if !urllib.IsAbsoluteURL(s.URL) {
		return locale.NewLocalizedError("error.saved_page_invalid_url")
	}

	return nil
}

// NewSavedPageForm returns a new SavedPageForm.
func NewSavedPageForm(r *http.Request) *SavedPageForm {
	return &SavedPageForm{URL: r.FormValue("url")}
}
