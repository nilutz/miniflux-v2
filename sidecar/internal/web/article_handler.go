// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// This file implements GET /api/article?entry_id= (task 15): a minimal,
// read-only complement to GET /api/search and GET /api/similar
// (search_handlers.go). Those two return snippets and scores, not full
// text — an agent (or any other caller) that finds a promising result has
// no way to read it. This endpoint closes that gap: given an entry id, it
// returns the entry's title, URL, published date and full content, and
// nothing else — no feed, no category, no read/starred status. It carries
// no sameOriginOrNoOrigin check for the same reason handleSearch and
// handleSimilar do not; see that doc comment in search_handlers.go.
package web // import "miniflux.app/v2/sidecar/internal/web"

import (
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"miniflux.app/v2/sidecar/internal/store"
)

// articleResponseView is GET /api/article's response body.
type articleResponseView struct {
	EntryID     int64     `json:"entry_id"`
	Title       string    `json:"title"`
	URL         string    `json:"url"`
	PublishedAt time.Time `json:"published_at"`
	Content     string    `json:"content"`
}

func toArticleResponseView(d *store.ArticleDetail) articleResponseView {
	return articleResponseView{
		EntryID:     d.ID,
		Title:       d.Title,
		URL:         d.URL,
		PublishedAt: d.PublishedAt,
		Content:     d.Content,
	}
}

// handleArticle serves GET /api/article. See this file's package-level
// doc comment for why it carries no sameOriginOrNoOrigin check.
func (s *Server) handleArticle(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	raw := strings.TrimSpace(q.Get("entry_id"))
	if raw == "" {
		writeAPIError(w, http.StatusBadRequest, `query parameter "entry_id" is required`)
		return
	}
	entryID, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, fmt.Sprintf("entry_id must be a whole number, got %q", raw))
		return
	}

	if s.articles == nil {
		slog.Error("web: article lookup requested but no ArticleLookup is configured")
		writeAPIError(w, http.StatusInternalServerError, "article lookup unavailable")
		return
	}

	article, err := s.articles.EntryArticle(r.Context(), entryID)
	if err != nil {
		// store.Store.EntryArticle wraps sql.ErrNoRows (via %w) for an
		// unknown id, which is a caller error (404): every other error is
		// ours to log and hide, exactly like handleSearch/handleSimilar's
		// own error paths (see search_handlers.go's apiError doc comment)
		// -- the real error never reaches the HTTP response for a 500.
		if errors.Is(err, sql.ErrNoRows) {
			writeAPIError(w, http.StatusNotFound, fmt.Sprintf("entry #%d does not exist", entryID))
			return
		}
		slog.Error("web: article lookup failed",
			slog.Int64("entry_id", entryID),
			slog.Any("error", err),
		)
		writeAPIError(w, http.StatusInternalServerError, "article lookup failed")
		return
	}

	writeJSON(w, toArticleResponseView(article))
}
