// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// This file implements the sidecar's read-only search HTTP API (task 7):
//
//	GET /api/search?q=&mode=&limit=&user=&feed=&category=&unread=&starred=&since=&until=
//	GET /api/similar?entry_id=&limit=&user=&feed=&category=&unread=&starred=&since=&until=
//
// Why these two endpoints do NOT carry sameOriginOrNoOrigin's check
// (server.go), unlike POST /api/backfill/pause|resume|config:
//
// That check exists to stop Cross-Site Request Forgery — a page open in
// the operator's own browser, on any origin, silently firing a
// state-changing request at this loopback-bound service, riding whatever
// ambient credential (task 18's MinifluxSessionID cookie) the browser
// attaches automatically. CSRF is specifically an attack on STATE
// CHANGE: it works by getting the victim's browser to *cause an effect* (pause the backfill, change its
// concurrency) using credentials/network-position the attacker doesn't
// have themselves. A GET against /api/search or /api/similar changes
// nothing server-side — it returns a read of already-indexed public.entries
// content back to whatever page asked for it. The worst a hostile page
// could do by firing this request cross-origin is *read search results
// back into itself*, which is a real information-disclosure concern in
// general (this is why browsers' own Fetch/CORS model still blocks a
// cross-origin page from reading the *response* body of a fetch() call
// unless the server opts in) — but the browser's own same-origin policy
// already provides that protection for a page's own script, independent
// of anything this server does. sameOriginOrNoOrigin is this server's OWN
// defence against a *simple, credential-less* cross-site request causing
// a side effect; it has nothing to add on top of the browser's built-in
// protection for a request whose entire "effect" is its response body.
// A curl/CLI caller (RunLive's own health checks, a future fork client,
// task 8's searchclient) never sends an Origin header either way, so this
// check would not protect them regardless.
//
// They still live on the same loopback-bound Server as the control
// endpoints (New's caller, cmd/sidecar/main.go, binds it to 127.0.0.1 by
// default per web.DefaultAddr) — loopback binding, not the Origin check,
// is what stops a remote attacker from reaching either kind of endpoint
// at all.
package web // import "miniflux.app/v2/sidecar/internal/web"

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"miniflux.app/v2/sidecar/internal/search"
	"miniflux.app/v2/sidecar/internal/store"
)

// SearchService is the subset of *search.Searcher the search HTTP API
// needs. Defined as an interface, rather than depending on
// *search.Searcher directly, so this package's own tests can inject a
// fake with no store, no embedder and no database connection (see
// search_handlers_test.go's fakeSearcher) — exactly the same reason
// BackfillController exists alongside *indexer.Backfill.
type SearchService interface {
	Search(ctx context.Context, req search.Request) (search.Response, error)
	Similar(ctx context.Context, entryID int64, limit int, f search.Filters) ([]search.EntryHit, error)
}

// EntryLookup is the subset of *store.Store the search HTTP API needs to
// build a highlighted search.Snippet (search.BuildSnippet) for each
// result: the entry's current title/content/hash, and what was recorded
// the last time it was actually indexed. *store.Store satisfies this
// with no adaptation; tests use an in-memory fakeEntries instead.
type EntryLookup interface {
	EntryForIndexing(ctx context.Context, entryID int64) (*store.Entry, error)
	EntryIndexState(ctx context.Context, entryID int64) (*store.IndexState, error)
}

// webMaxSearchLimit is the highest `limit` this API will ever honour,
// regardless of what a caller asks for. It matches the "Request.Limit=100"
// case fuse.go's own candidateMultiplier comment already reasons about as
// the top of a sane range (2500 index candidates once both retrieval
// multipliers compound) — asking for more than this from an HTTP client is
// treated as a caller error worth clamping quietly, not a request this
// service should honour verbatim.
const webMaxSearchLimit = 100

// apiError is the search API's uniform error body: {"error": "<message>"}.
// Every 4xx from this file is safe, caller-facing text describing what was
// wrong with THEIR request (a bad mode, a bad limit, a missing query) —
// never a wrapped internal error. Every 5xx carries a fixed, generic
// message; the real error is logged server-side via slog and never placed
// in a response body. See writeAPIError.
type apiError struct {
	Error string `json:"error"`
}

// writeAPIError writes status with a JSON {"error": message} body. Callers
// of this function must pass only text that is safe to hand to an HTTP
// client: a caller-facing explanation for 4xx, or a fixed generic string
// for 5xx (e.g. "search failed") — never fmt.Errorf-wrapped internal
// error text, a driver error, or a SQL fragment. See handleSearch and
// handleSimilar's own error paths for where that boundary is enforced.
func writeAPIError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(apiError{Error: message}); err != nil {
		slog.Error("web: unable to encode error response", slog.Any("error", err))
	}
}

// rangeView is search.Range reshaped for JSON: exported field names
// rather than search.Range's own Start/End, kept as a distinct wire type
// so a change to search.Range's internal shape can never silently change
// this API's JSON contract.
type rangeView struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

// snippetView is search.Snippet reshaped for JSON.
type snippetView struct {
	Text       string      `json:"text"`
	Highlights []rangeView `json:"highlights"`
}

func toSnippetView(sn search.Snippet) snippetView {
	highlights := make([]rangeView, len(sn.Highlights))
	for i, r := range sn.Highlights {
		highlights[i] = rangeView{Start: r.Start, End: r.End}
	}
	return snippetView{Text: sn.Text, Highlights: highlights}
}

// entryResultView is one search or similar-article result, reshaped for
// JSON. Score is carried verbatim from search.EntryHit — see that type's
// own doc comment before comparing or sorting by it across modes; this
// API does not re-sort, it serialises Search/Similar's own already-correct
// order.
type entryResultView struct {
	EntryID int64       `json:"entry_id"`
	Score   float64     `json:"score"`
	Snippet snippetView `json:"snippet"`
}

// passageResultView is one ModePassages result, reshaped for JSON.
type passageResultView struct {
	EntryID   int64       `json:"entry_id"`
	PassageID int64       `json:"passage_id"`
	Ordinal   int         `json:"ordinal"`
	Source    string      `json:"source"`
	Score     float64     `json:"score"`
	Snippet   snippetView `json:"snippet"`
}

// searchResponseView is GET /api/search's response body. Entries is
// populated for ModeKeyword/ModeSemantic/ModeHybrid, Passages for
// ModePassages — mirroring search.Response's own Mode-dependent nil
// convention (see that type's doc comment) — and both are tagged
// omitempty so the response never carries a spurious empty array for
// the field the current mode does not use.
type searchResponseView struct {
	Mode     string              `json:"mode"`
	Query    string              `json:"query"`
	Entries  []entryResultView   `json:"entries,omitempty"`
	Passages []passageResultView `json:"passages,omitempty"`
}

// similarEntryResultView is one GET /api/similar result. It is
// deliberately NOT entryResultView: it carries no snippet.
//
// /api/search's snippet answers "why did this match?", which a result
// list has to show. "More like this" has no query to highlight and no
// match to explain — the fork's entry page renders a similar article as
// its title, feed and category, all of which it already has in its own
// database — so every snippet /api/similar used to build was discarded
// by its only consumer.
//
// Building them was not free. One similar-article response is up to
// limit results; each snippet costs an EntryForIndexing (the entry's
// full HTML content, detoasted) plus an EntryIndexState round trip and
// a full ExtractText pass over that HTML, all of it on the entry page's
// synchronous render path. Measured against this corpus that was
// roughly 430 KB of article HTML over 20 round trips and 10 HTML
// extractions per entry page view, for output nothing read.
//
// Dropping the field from the wire type, rather than leaving it present
// and empty, is what stops it quietly coming back: a future caller that
// wants a snippet here has to add it deliberately, and pay for it
// deliberately.
type similarEntryResultView struct {
	EntryID int64   `json:"entry_id"`
	Score   float64 `json:"score"`
}

// similarResponseView is GET /api/similar's response body.
type similarResponseView struct {
	EntryID int64                    `json:"entry_id"`
	Entries []similarEntryResultView `json:"entries"`
}

// entrySnapshot is one entry's current state plus its last-recorded index
// state, cached per request (entrySnapshots) so an entry appearing more
// than once in a single response — every passage of one EntryHit shares
// an entry, and the same entry can recur across a passage-mode result
// list — is only ever fetched from EntryLookup once.
type entrySnapshot struct {
	entry   *store.Entry
	indexed *store.IndexState
	ok      bool
}

// entrySnapshots memoizes entrySnapshot lookups within one handler call.
type entrySnapshots struct {
	lookup EntryLookup
	cache  map[int64]entrySnapshot
}

func newEntrySnapshots(lookup EntryLookup) *entrySnapshots {
	return &entrySnapshots{lookup: lookup, cache: map[int64]entrySnapshot{}}
}

// get returns entryID's cached snapshot, fetching it on first use. A
// lookup failure (entry deleted since the search ran, a transient store
// error) is logged and cached as "not ok" rather than returned to the
// caller: it degrades the ONE result's snippet to its raw, unhighlighted
// PassageHit.Text (see buildSnippetView) instead of failing the whole
// search response over what is, at that point, a display nicety.
func (s *entrySnapshots) get(ctx context.Context, entryID int64) entrySnapshot {
	if snap, ok := s.cache[entryID]; ok {
		return snap
	}

	var snap entrySnapshot
	if s.lookup == nil {
		s.cache[entryID] = snap
		return snap
	}

	entry, err := s.lookup.EntryForIndexing(ctx, entryID)
	if err != nil {
		slog.Warn("web: unable to load entry for search snippet; degrading to unhighlighted text",
			slog.Int64("entry_id", entryID), slog.Any("error", err))
		s.cache[entryID] = snap
		return snap
	}

	indexed, err := s.lookup.EntryIndexState(ctx, entryID)
	if err != nil {
		slog.Warn("web: unable to load index state for search snippet; degrading to unhighlighted text",
			slog.Int64("entry_id", entryID), slog.Any("error", err))
		s.cache[entryID] = snap
		return snap
	}

	snap = entrySnapshot{entry: entry, indexed: indexed, ok: true}
	s.cache[entryID] = snap
	return snap
}

// buildSnippetView builds hit's snippet via search.BuildSnippet using
// snap's entry state, or degrades to hit's own raw, unhighlighted text
// when snap has no entry to build one from (lookup failed, or this
// Server was built without an EntryLookup at all).
func buildSnippetView(hit search.PassageHit, snap entrySnapshot, query string) snippetView {
	if !snap.ok {
		return snippetView{Text: hit.Text}
	}
	return toSnippetView(search.BuildSnippet(hit, *snap.entry, snap.indexed, query))
}

func (s *Server) buildEntryViews(ctx context.Context, hits []search.EntryHit, query string) []entryResultView {
	snaps := newEntrySnapshots(s.entries)
	views := make([]entryResultView, 0, len(hits))
	for _, h := range hits {
		views = append(views, entryResultView{
			EntryID: h.EntryID,
			Score:   h.Score,
			Snippet: buildSnippetView(h.Best, snaps.get(ctx, h.EntryID), query),
		})
	}
	return views
}

// buildSimilarEntryViews serialises similar-article hits with no snippet
// work at all — no EntryLookup round trips, no HTML extraction. See
// similarEntryResultView for why.
func buildSimilarEntryViews(hits []search.EntryHit) []similarEntryResultView {
	views := make([]similarEntryResultView, 0, len(hits))
	for _, h := range hits {
		views = append(views, similarEntryResultView{EntryID: h.EntryID, Score: h.Score})
	}
	return views
}

func (s *Server) buildPassageViews(ctx context.Context, hits []search.PassageHit, query string) []passageResultView {
	snaps := newEntrySnapshots(s.entries)
	views := make([]passageResultView, 0, len(hits))
	for _, h := range hits {
		views = append(views, passageResultView{
			EntryID:   h.EntryID,
			PassageID: h.PassageID,
			Ordinal:   h.Ordinal,
			Source:    h.Source,
			Score:     h.Score,
			Snippet:   buildSnippetView(h, snaps.get(ctx, h.EntryID), query),
		})
	}
	return views
}

// parseMode maps the API's "mode" query parameter to a search.Mode.
// Unrecognised text is a caller error (400), never a silent fallback to
// ModeHybrid: a typo'd mode string ("hybird", a stale client's old value)
// must be visible to whoever sent it, not quietly answered by a different
// mode than they asked for. An absent/blank value legitimately means "the
// caller has no opinion", which spec §6.3 resolves as hybrid — that is a
// documented default, not a masked mistake, so it is the one case that
// does not error.
func parseMode(raw string) (search.Mode, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "":
		return search.ModeHybrid, nil
	case "hybrid":
		return search.ModeHybrid, nil
	case "keyword":
		return search.ModeKeyword, nil
	case "semantic":
		return search.ModeSemantic, nil
	case "passages":
		return search.ModePassages, nil
	default:
		return 0, fmt.Errorf("unknown mode %q: want one of keyword, semantic, hybrid, passages", raw)
	}
}

// parseLimit parses the API's "limit" query parameter.
//
//   - absent/blank -> 0, nil: "no opinion", and Search/Similar already
//     treat a zero-or-negative Limit as their own defaultSearchLimit
//     (fuse.go, similar.go), so leaving it at zero here reuses that
//     default rather than restating it.
//   - present but not a whole number, or <= 0 -> an error: a caller who
//     explicitly wrote limit=0 or limit=-5 has a bug worth surfacing as
//     a 400, unlike a caller who simply left it out.
//   - present and > webMaxSearchLimit -> silently clamped down, not
//     rejected: asking for "too many" is not a caller mistake the way
//     asking for "none" or "negative" is, it is just met with a cap.
func parseLimit(raw string) (int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("limit must be a whole number, got %q", raw)
	}
	if n <= 0 {
		return 0, fmt.Errorf("limit must be positive, got %d", n)
	}
	if n > webMaxSearchLimit {
		n = webMaxSearchLimit
	}
	return n, nil
}

// parseIDList parses a "feed" or "category" query parameter's values into
// a slice of ids. Both a repeated parameter (?feed=1&feed=2) and a
// comma-separated one (?feed=1,2) are accepted, and the two forms may be
// mixed, since either is a natural way for a caller to write a list in a
// query string and rejecting one in favour of the other would only be a
// caller-hostile surprise.
func parseIDList(values []string, name string) ([]int64, error) {
	var ids []int64
	for _, v := range values {
		for _, part := range strings.Split(v, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			id, err := strconv.ParseInt(part, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("%s: invalid id %q", name, part)
			}
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// parseBoolParam parses an optional boolean query parameter, defaulting
// to false when absent/blank; strconv.ParseBool accepts "1", "t", "T",
// "TRUE", "true", "True" and their false counterparts.
func parseBoolParam(raw, name string) (bool, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return false, nil
	}
	b, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("%s must be a boolean, got %q", name, raw)
	}
	return b, nil
}

// parseDateParam parses an optional "since"/"until" query parameter,
// accepting either full RFC3339 or a bare "YYYY-MM-DD" date (taken as UTC
// midnight of that day — so a date-only "until" bounds through the START
// of that day, not its end; a caller wanting an inclusive whole-day
// upper bound should pass the following day, or a full RFC3339 timestamp).
// An absent/blank value returns the zero time.Time, which Filters treats
// as "no bound" (see search.Filters' own doc comment).
func parseDateParam(raw, name string) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t, nil
	}
	if t, err := time.Parse("2006-01-02", raw); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("%s must be RFC3339 or YYYY-MM-DD, got %q", name, raw)
}

// parseUserID parses the optional "user" query parameter into
// search.Filters.UserID.
//
// Since task 17, GET /api/search and /api/similar are authenticated, and
// resolveAuthenticatedUserID (below) always overwrites whatever this
// returns with the id the caller's own API key resolved to before a
// request reaches the Searcher — so a value parsed here is no longer the
// thing that decides scope. It still has two jobs: a malformed value (not
// a whole number, zero, negative) is still rejected here with a 400
// before authentication's own scope check ever runs, and a well-formed
// value that disagrees with the authenticated key is what
// resolveAuthenticatedUserID rejects. Absent/blank still means 0 at this
// parsing stage — resolveAuthenticatedUserID reads that as "the caller
// expressed no opinion" and fills in the authenticated user unconditionally,
// never as "every user's content is eligible" the way it did before
// authentication existed (see search.Filters.UserID's own doc comment for
// why an unscoped search was already a footgun even before this task).
//
// A present-but-nonsensical value (not a whole number, zero, negative)
// is a 400 rather than a silent fall back to "unscoped": a caller that
// meant to scope and got it wrong must not be quietly answered with the
// whole corpus.
func parseUserID(raw string) (int64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("user must be a whole number, got %q", raw)
	}
	if id <= 0 {
		return 0, fmt.Errorf("user must be positive, got %d", id)
	}
	return id, nil
}

// parseFilters parses every filter query parameter shared by
// GET /api/search and GET /api/similar into a search.Filters.
func parseFilters(q url.Values) (search.Filters, error) {
	var f search.Filters

	userID, err := parseUserID(q.Get("user"))
	if err != nil {
		return f, err
	}
	f.UserID = userID

	feedIDs, err := parseIDList(q["feed"], "feed")
	if err != nil {
		return f, err
	}
	categoryIDs, err := parseIDList(q["category"], "category")
	if err != nil {
		return f, err
	}
	f.FeedIDs = feedIDs
	f.CategoryIDs = categoryIDs

	if f.UnreadOnly, err = parseBoolParam(q.Get("unread"), "unread"); err != nil {
		return f, err
	}
	if f.StarredOnly, err = parseBoolParam(q.Get("starred"), "starred"); err != nil {
		return f, err
	}
	if f.Since, err = parseDateParam(q.Get("since"), "since"); err != nil {
		return f, err
	}
	if f.Until, err = parseDateParam(q.Get("until"), "until"); err != nil {
		return f, err
	}

	return f, nil
}

// resolveAuthenticatedUserID is task 17's "second problem" fix, applied to
// both GET /api/search and GET /api/similar: it reconciles f.UserID (as
// parsed by parseFilters, above, from the caller-supplied "user" query
// parameter) against the user id the caller's own API key resolved to
// (requireAuthenticatedUser in auth.go, always run first on both routes).
//
//   - f.UserID == 0 (the caller expressed no opinion): filled in from the
//     authenticated user unconditionally. Once a request is authenticated
//     there is no longer such a thing as an intentionally unscoped
//     search through this endpoint — the key IS the scope.
//   - f.UserID != 0 and it matches the authenticated user: left as is (a
//     no-op — this is the same value resolveAuthenticatedUserID would
//     have filled in anyway).
//   - f.UserID != 0 and it does NOT match: rejected with 400. This is a
//     deliberate choice among three defensible ones (ignore it, remove
//     the parameter entirely, or reject a disagreement) — rejecting
//     follows the same fail-loud policy parseUserID's own doc comment
//     already established for a malformed value, and it is what actually
//     closes the hole this task exists to close: before this task, the
//     "user" parameter was the caller's unchecked assertion of identity;
//     after it, a caller can no longer make that assertion at all, loudly
//     or quietly — the key is what decides who they are.
//
// Writes a 400 or 500 response and returns false when it refuses the
// request; returns true, with f.UserID authoritatively set to the
// authenticated user, otherwise.
func resolveAuthenticatedUserID(w http.ResponseWriter, r *http.Request, f *search.Filters) bool {
	authUserID, ok := authenticatedUserID(r)
	if !ok {
		// Unreachable through the registered routes: handleSearch and
		// handleSimilar are only ever invoked wrapped by requireAuthenticatedUser,
		// which always sets this before calling through. Fail closed
		// rather than silently search unscoped if that invariant is ever
		// broken by a future refactor.
		slog.Error("web: resolveAuthenticatedUserID called with no authenticated user id in context")
		writeAPIError(w, http.StatusInternalServerError, "authentication context missing")
		return false
	}
	if f.UserID != 0 && f.UserID != authUserID {
		writeAPIError(w, http.StatusBadRequest, `the "user" parameter does not match the user your API key belongs to`)
		return false
	}
	f.UserID = authUserID
	return true
}

// handleSearch serves GET /api/search. See this file's package-level doc
// comment for why it carries no sameOriginOrNoOrigin check.
func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	query := strings.TrimSpace(q.Get("q"))
	if query == "" {
		writeAPIError(w, http.StatusBadRequest, `query parameter "q" is required`)
		return
	}

	mode, err := parseMode(q.Get("mode"))
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}

	limit, err := parseLimit(q.Get("limit"))
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}

	filters, err := parseFilters(q)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !resolveAuthenticatedUserID(w, r, &filters) {
		return
	}

	resp, err := s.searcher.Search(r.Context(), search.Request{
		Query:   query,
		Mode:    mode,
		Limit:   limit,
		Filters: filters,
	})
	if err != nil {
		// The real error (which may embed a driver/SQL error, per
		// fuse.go's own %w-wrapped errors) is logged here, server-side,
		// with structured attributes — and stops here. writeAPIError
		// below gets a fixed, generic string, never err.Error(): a
		// database error or SQL fragment must never reach an HTTP
		// client (see this file's apiError doc comment).
		slog.Error("web: search failed",
			slog.String("query", query),
			slog.String("mode", mode.String()),
			slog.Any("error", err),
		)
		writeAPIError(w, http.StatusInternalServerError, "search failed")
		return
	}

	view := searchResponseView{Mode: resp.Mode.String(), Query: query}
	if resp.Entries != nil {
		view.Entries = s.buildEntryViews(r.Context(), resp.Entries, query)
	}
	if resp.Passages != nil {
		view.Passages = s.buildPassageViews(r.Context(), resp.Passages, query)
	}
	writeJSON(w, view)
}

// handleSimilar serves GET /api/similar. See this file's package-level
// doc comment for why it carries no sameOriginOrNoOrigin check.
func (s *Server) handleSimilar(w http.ResponseWriter, r *http.Request) {
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

	limit, err := parseLimit(q.Get("limit"))
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}

	filters, err := parseFilters(q)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !resolveAuthenticatedUserID(w, r, &filters) {
		return
	}

	hits, err := s.searcher.Similar(r.Context(), entryID, limit, filters)
	if err != nil {
		slog.Error("web: similar-article lookup failed",
			slog.Int64("entry_id", entryID),
			slog.Any("error", err),
		)
		writeAPIError(w, http.StatusInternalServerError, "similar-article lookup failed")
		return
	}

	// No snippets here, by design: see similarEntryResultView.
	writeJSON(w, similarResponseView{
		EntryID: entryID,
		Entries: buildSimilarEntryViews(hits),
	})
}
