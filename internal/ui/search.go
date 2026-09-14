// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui // import "miniflux.app/v2/internal/ui"

import (
	"context"
	"log/slog"
	"net/http"

	"miniflux.app/v2/internal/config"
	"miniflux.app/v2/internal/http/request"
	"miniflux.app/v2/internal/http/response"
	"miniflux.app/v2/internal/model"
	"miniflux.app/v2/internal/searchclient"
	"miniflux.app/v2/internal/ui/view"
)

// searchModes is the mode picker's four values (spec §6.3), in the order
// they are presented in. defaultSearchMode ("hybrid") is what an absent
// or unrecognised "mode" query parameter - a bare /search?q=..., a stale
// bookmark, a typo'd value - resolves to; that mirrors the sidecar's own
// handling of a blank mode (sidecar/internal/web/search_handlers.go's
// parseMode) and means a bad "mode" value degrades to a sensible default
// rather than erroring the whole page.
var searchModes = []string{"keyword", "semantic", "hybrid", "passages"}

const defaultSearchMode = "hybrid"

// parseSearchMode validates raw against searchModes, falling back to
// defaultSearchMode for anything else.
func parseSearchMode(raw string) string {
	for _, mode := range searchModes {
		if raw == mode {
			return mode
		}
	}
	return defaultSearchMode
}

// searchRow is one line item on the search results page: an entry, the
// (possibly absent) highlighted snippet that explains why it matched,
// and - in passages mode only, where the same entry can appear more than
// once via different passages - which passage this row represents.
type searchRow struct {
	Entry    *model.Entry
	Segments []snippetSegment // nil when no snippet is available (the fallback path never has one)
	Ordinal  int              // 1-based passage number; 0 outside passages mode
}

func (h *handler) showSearchPage(w http.ResponseWriter, r *http.Request) {
	user, err := h.store.UserByID(request.UserID(r))
	if err != nil {
		response.HTMLServerError(w, r, err)
		return
	}

	searchQuery := request.QueryStringParam(r, "q", "")
	searchMode := parseSearchMode(request.QueryStringParam(r, "mode", ""))
	unreadOnly := request.QueryBoolParam(r, "unread", false)
	offset := request.QueryIntParam(r, "offset", 0)

	var rows []searchRow
	var entriesCount int
	var degraded bool

	if searchQuery != "" {
		fallback := func() (model.Entries, int, error) {
			builder := h.store.NewEntryQueryBuilder(user.ID).
				WithSearchQuery(searchQuery).
				WithoutContent().
				WithOffset(offset).
				WithLimit(user.EntriesPerPage)

			if unreadOnly {
				builder = builder.WithStatuses(model.EntryStatusUnread)
			}

			return builder.GetEntriesWithCount()
		}

		hydrate := func(entryIDs []int64) (model.Entries, error) {
			if len(entryIDs) == 0 {
				return model.Entries{}, nil
			}
			return h.store.NewEntryQueryBuilder(user.ID).
				WithEntryIDs(entryIDs...).
				WithoutContent().
				GetEntries()
		}

		rows, entriesCount, degraded, err = resolveSearchResults(
			r.Context(),
			config.Opts.SearchSidecarURL(),
			searchQuery,
			searchMode,
			unreadOnly,
			offset,
			user.EntriesPerPage,
			hydrate,
			fallback,
		)
		if err != nil {
			response.HTMLServerError(w, r, err)
			return
		}
	}

	view := view.New(h.tpl, r)
	pagination := getPagination(h.routePath("/search"), entriesCount, offset, user.EntriesPerPage)
	pagination.SearchQuery = searchQuery
	pagination.UnreadOnly = unreadOnly
	pagination.Mode = searchMode

	view.Set("searchQuery", searchQuery)
	view.Set("searchMode", searchMode)
	view.Set("searchModes", searchModes)
	view.Set("searchUnreadOnly", unreadOnly)
	view.Set("searchRows", rows)
	view.Set("total", entriesCount)
	// searchDegraded surfaces the notice spec §8.3 calls for: the sidecar
	// failed (or was never configured to begin with, in which case this
	// is simply false - see resolveSearchResults) and results below came
	// from the built-in fallback search instead.
	view.Set("searchDegraded", degraded)
	view.Set("pagination", pagination)
	view.Set("menu", "search")
	view.Set("user", user)
	navMetadata, _ := h.store.GetNavMetadata(user.ID)
	view.Set("countUnread", navMetadata.CountUnread)
	view.Set("countErrorFeeds", navMetadata.CountErrorFeeds)
	view.Set("hasSaveEntry", navMetadata.HasSaveEntry)

	response.HTML(w, r, view.Render("search"))
}

// resolveSearchResults returns the rows and total count to render on the
// search page.
//
// When sidecarURL is empty, or offset is non-zero (the sidecar's search
// API has no offset parameter, so paging past the first page always uses
// the fallback path, which supports it natively), fallback runs directly
// and degraded is false: neither case is a failure. Fallback rows never
// carry a snippet - only the sidecar computes highlighted snippets - so
// their Segments are left nil; the template simply renders no snippet
// paragraph for those rows.
//
// Otherwise the sidecar is queried in searchMode. ANY failure —
// connection refused, timeout, non-200 status, a malformed body
// (searchclient.Client.Search folds all of these into one error), or a
// failure while hydrating the sidecar's entry ids back into full
// model.Entry values via hydrate — is treated identically: log it, then
// run fallback and report degraded so the caller can show a notice.
// Search gets worse; the reader keeps working (spec §8.3). No error from
// this function ever propagates past a fallback failure itself.
//
// In passages mode a successful sidecar response is aggregated
// differently (spec §6.4): resp.Passages, not resp.Entries, carries the
// ranked hits, one row per passage rather than per entry, and the same
// entry can appear in more than one row.
func resolveSearchResults(
	ctx context.Context,
	sidecarURL string,
	query string,
	searchMode string,
	unreadOnly bool,
	offset int,
	limit int,
	hydrate func([]int64) (model.Entries, error),
	fallback func() (model.Entries, int, error),
) ([]searchRow, int, bool, error) {
	runFallback := func() ([]searchRow, int, error) {
		entries, count, err := fallback()
		return entryRows(entries), count, err
	}

	if sidecarURL == "" || offset > 0 {
		rows, count, err := runFallback()
		return rows, count, false, err
	}

	client := searchclient.NewClient(sidecarURL)
	resp, err := client.Search(ctx, searchclient.SearchRequest{
		Query:      query,
		Mode:       searchMode,
		Limit:      limit,
		UnreadOnly: unreadOnly,
	})
	if err != nil {
		slog.Warn("ui: search sidecar unavailable, falling back to built-in search",
			slog.String("sidecar_url", sidecarURL),
			slog.Any("error", err),
		)
		rows, count, ferr := runFallback()
		return rows, count, true, ferr
	}

	if resp.Passages != nil {
		rows, err := passageRows(resp.Passages, hydrate)
		if err != nil {
			slog.Warn("ui: unable to load entries for sidecar passage results, falling back to built-in search",
				slog.String("sidecar_url", sidecarURL),
				slog.Any("error", err),
			)
			rows, count, ferr := runFallback()
			return rows, count, true, ferr
		}
		return rows, len(rows), false, nil
	}

	entryIDs := make([]int64, len(resp.Entries))
	for i, hit := range resp.Entries {
		entryIDs[i] = hit.EntryID
	}

	hydrated, err := hydrate(entryIDs)
	if err != nil {
		slog.Warn("ui: unable to load entries for sidecar search results, falling back to built-in search",
			slog.String("sidecar_url", sidecarURL),
			slog.Any("error", err),
		)
		rows, count, ferr := runFallback()
		return rows, count, true, ferr
	}

	byID := indexEntriesByID(hydrated)
	rows := make([]searchRow, 0, len(resp.Entries))
	for _, hit := range resp.Entries {
		if e, ok := byID[hit.EntryID]; ok {
			rows = append(rows, searchRow{Entry: e, Segments: buildSnippetSegments(hit.Snippet)})
		}
	}
	return rows, len(rows), false, nil
}

// entryRows wraps plain entries (the fallback path, which has no snippet
// data) into rows with no Segments and no Ordinal.
func entryRows(entries model.Entries) []searchRow {
	rows := make([]searchRow, len(entries))
	for i, e := range entries {
		rows[i] = searchRow{Entry: e}
	}
	return rows
}

// passageRows hydrates the entry referenced by each passage hit and
// builds one row per hit, in the sidecar's ranked order, dropping any
// hit whose entry no longer exists (deleted between the sidecar's search
// and this hydration). Ordinal is 1-based for display; hits.Ordinal is
// the sidecar's 0-based passage index within its entry.
func passageRows(hits []searchclient.PassageResult, hydrate func([]int64) (model.Entries, error)) ([]searchRow, error) {
	ids := make([]int64, len(hits))
	for i, hit := range hits {
		ids[i] = hit.EntryID
	}

	hydrated, err := hydrate(ids)
	if err != nil {
		return nil, err
	}

	byID := indexEntriesByID(hydrated)
	rows := make([]searchRow, 0, len(hits))
	for _, hit := range hits {
		if e, ok := byID[hit.EntryID]; ok {
			rows = append(rows, searchRow{
				Entry:    e,
				Segments: buildSnippetSegments(hit.Snippet),
				Ordinal:  hit.Ordinal + 1,
			})
		}
	}
	return rows, nil
}

// indexEntriesByID returns entries keyed by ID, for looking up hydrated
// entries against a ranked list of ids without trusting the store to
// return rows in the order a WHERE id IN (...) clause listed them.
func indexEntriesByID(entries model.Entries) map[int64]*model.Entry {
	byID := make(map[int64]*model.Entry, len(entries))
	for _, e := range entries {
		byID[e.ID] = e
	}
	return byID
}

// orderEntriesByID returns entries re-ordered to match ids, dropping any
// id with no corresponding entry (e.g. deleted between a sidecar lookup
// and this hydration). Used by the similar-articles block
// (entry_similar.go), which has no per-row snippet to carry alongside
// and so has no need for searchRow.
func orderEntriesByID(entries model.Entries, ids []int64) model.Entries {
	byID := indexEntriesByID(entries)

	ordered := make(model.Entries, 0, len(ids))
	for _, id := range ids {
		if e, ok := byID[id]; ok {
			ordered = append(ordered, e)
		}
	}
	return ordered
}
