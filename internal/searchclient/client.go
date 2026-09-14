// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package searchclient is the fork's HTTP client for the search sidecar
// (sidecar/internal/web/search_handlers.go). It is the ONLY thing that
// crosses the fork/sidecar boundary, and it does so as a plain HTTP+JSON
// client: this package must never import anything under
// "miniflux.app/v2/sidecar/...". The response shapes below are this
// package's own copy of the sidecar's wire contract, not a shared type.
//
// See docs/superpowers/specs/2026-09-12-miniflux-search-design.md §8.2-8.3:
// the sidecar is optional infrastructure the reader must never depend on
// to keep working, so every method here has a short, explicit timeout and
// folds every failure mode (connection refused, timeout, non-200,
// malformed body) into a single error a caller can fall back on.
package searchclient // import "miniflux.app/v2/internal/searchclient"

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// DefaultTimeout bounds a Client created with NewClient. A couple of
// seconds, per spec §8.3: long enough for a healthy, loopback-bound
// sidecar to answer, short enough that a wedged one never makes a reader
// page wait noticeably.
const DefaultTimeout = 3 * time.Second

// SimilarTimeout bounds the similar-articles call, and is deliberately
// shorter than DefaultTimeout.
//
// The two calls are not worth the same wait. A search page that renders
// without its results has failed at the thing the reader asked for, so
// it is worth three seconds to get them. The entry page's "similar
// articles" block is a sidebar beside an article the reader is already
// reading: the page is complete without it, and every millisecond spent
// waiting for it is a millisecond the article itself is not on screen.
// This call sits on the synchronous render path of every entry view, so
// its ceiling is what a wedged sidecar costs a reader who never asked
// for a recommendation.
//
// 1.5s is chosen against what the work actually costs: Similar issues up
// to maxSeedPassages (32) sequential nearest-neighbour queries, measured
// at roughly 26ms each on this corpus, so a healthy sidecar answers even
// the worst-case entry inside about 850ms. Anything slower than 1.5s is
// not a slow entry, it is a sidecar in trouble - and the right answer to
// that is to drop the block, which is exactly what the caller does.
const SimilarTimeout = 1500 * time.Millisecond

// Range is a highlighted span within a Snippet's Text, in byte offsets.
type Range struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

// Snippet is a highlighted excerpt of an entry or passage's text.
type Snippet struct {
	Text       string  `json:"text"`
	Highlights []Range `json:"highlights"`
}

// EntryResult is one search or similar-article hit at entry granularity.
type EntryResult struct {
	EntryID int64   `json:"entry_id"`
	Score   float64 `json:"score"`
	Snippet Snippet `json:"snippet"`
}

// PassageResult is one ModePassages search hit.
type PassageResult struct {
	EntryID   int64   `json:"entry_id"`
	PassageID int64   `json:"passage_id"`
	Ordinal   int     `json:"ordinal"`
	Source    string  `json:"source"`
	Score     float64 `json:"score"`
	Snippet   Snippet `json:"snippet"`
}

// SearchResponse is GET /api/search's response body. Entries is populated
// for the keyword/semantic/hybrid modes, Passages for passages mode —
// mirroring the sidecar's own mode-dependent convention, so callers
// should branch on which slice is non-empty rather than on Mode alone.
type SearchResponse struct {
	Mode     string          `json:"mode"`
	Query    string          `json:"query"`
	Entries  []EntryResult   `json:"entries,omitempty"`
	Passages []PassageResult `json:"passages,omitempty"`
}

// SimilarResult is one GET /api/similar hit. It carries no snippet, and
// that is the contract, not an omission: the entry page's similar block
// renders an entry's title, feed and category, all of which the fork
// already holds in its own database, so the sidecar does not build a
// snippet for this endpoint at all (see its
// internal/web/search_handlers.go's similarEntryResultView).
type SimilarResult struct {
	EntryID int64   `json:"entry_id"`
	Score   float64 `json:"score"`
}

// SimilarResponse is GET /api/similar's response body.
type SimilarResponse struct {
	EntryID int64           `json:"entry_id"`
	Entries []SimilarResult `json:"entries"`
}

// SearchRequest is a GET /api/search call. Zero values mean "no opinion"
// for every field: an empty Mode lets the sidecar default to hybrid, a
// zero Limit lets it apply its own default, and zero-value Since/Until
// mean unbounded — matching search_handlers.go's own parsing.
type SearchRequest struct {
	Query string
	Mode  string // "", "keyword", "semantic", "hybrid", "passages"
	Limit int

	// UserID scopes the search to one Miniflux user's entries. Unlike
	// every other field here, leaving it zero is not a neutral "no
	// opinion": the sidecar's index is global, so an unscoped search
	// draws its candidate set from every user's content and the caller
	// silently gets a shorter page of their own. Every caller in this
	// fork sets it.
	UserID int64

	FeedIDs     []int64
	CategoryIDs []int64
	UnreadOnly  bool
	StarredOnly bool
	Since       time.Time
	Until       time.Time
}

// SimilarRequest is a GET /api/similar call.
type SimilarRequest struct {
	EntryID int64
	Limit   int

	// UserID scopes the lookup to one Miniflux user's entries. See
	// SearchRequest.UserID: leaving it zero is not neutral.
	UserID int64

	FeedIDs     []int64
	CategoryIDs []int64
	UnreadOnly  bool
	StarredOnly bool
	Since       time.Time
	Until       time.Time
}

// sharedHTTPClient is the one *http.Client every Client in this process
// uses.
//
// A Client is constructed per request — the UI handlers call NewClient on
// each page view rather than holding one — and a fresh *http.Client means
// a fresh Transport, which means a fresh connection pool that is thrown
// away after a single use. Every search and every entry page view paid
// for a new TCP handshake to a service on loopback. Hoisting the client
// here lets keep-alive actually keep something alive across requests,
// while a per-Client timeout stays per-Client: get wraps each request's
// context in its own deadline, which bounds connect, response and body
// read alike, so the bound never needed to live on the *http.Client.
var sharedHTTPClient = &http.Client{
	Transport: &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		MaxIdleConns:        8,
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     90 * time.Second,
	},
}

// Client is a small HTTP client for one sidecar instance.
type Client struct {
	baseURL    string
	httpClient *http.Client
	timeout    time.Duration
}

// NewClient returns a Client bounded by DefaultTimeout.
func NewClient(baseURL string) *Client {
	return NewClientWithTimeout(baseURL, DefaultTimeout)
}

// NewClientWithTimeout returns a Client bounded by an explicit timeout.
// Exported (rather than an option func) so callers with a different
// tolerance can say so — the similar-articles block uses SimilarTimeout
// — and so tests can use a short timeout and keep the suite fast without
// waiting out DefaultTimeout.
func NewClientWithTimeout(baseURL string, timeout time.Duration) *Client {
	return &Client{
		baseURL:    strings.TrimRight(baseURL, "/"),
		httpClient: sharedHTTPClient,
		timeout:    timeout,
	}
}

// Search calls GET /api/search. Every failure mode — the sidecar is
// unreachable, it is too slow to answer within the client's timeout, it
// answers with a non-200 status, or its body does not parse as
// SearchResponse — is returned as a single error a caller can treat
// uniformly: fall back to the built-in search path (spec §8.3).
func (c *Client) Search(ctx context.Context, req SearchRequest) (SearchResponse, error) {
	values := url.Values{}
	values.Set("q", req.Query)
	if req.Mode != "" {
		values.Set("mode", req.Mode)
	}
	if req.Limit > 0 {
		values.Set("limit", strconv.Itoa(req.Limit))
	}
	setFilterParams(values, filterParams{
		UserID:      req.UserID,
		FeedIDs:     req.FeedIDs,
		CategoryIDs: req.CategoryIDs,
		UnreadOnly:  req.UnreadOnly,
		StarredOnly: req.StarredOnly,
		Since:       req.Since,
		Until:       req.Until,
	})

	var out SearchResponse
	if err := c.get(ctx, "/api/search", values, &out); err != nil {
		return SearchResponse{}, fmt.Errorf("searchclient: search: %w", err)
	}
	return out, nil
}

// Similar calls GET /api/similar. See Search's doc comment: every failure
// mode is folded into one error.
func (c *Client) Similar(ctx context.Context, req SimilarRequest) (SimilarResponse, error) {
	values := url.Values{}
	values.Set("entry_id", strconv.FormatInt(req.EntryID, 10))
	if req.Limit > 0 {
		values.Set("limit", strconv.Itoa(req.Limit))
	}
	setFilterParams(values, filterParams{
		UserID:      req.UserID,
		FeedIDs:     req.FeedIDs,
		CategoryIDs: req.CategoryIDs,
		UnreadOnly:  req.UnreadOnly,
		StarredOnly: req.StarredOnly,
		Since:       req.Since,
		Until:       req.Until,
	})

	var out SimilarResponse
	if err := c.get(ctx, "/api/similar", values, &out); err != nil {
		return SimilarResponse{}, fmt.Errorf("searchclient: similar: %w", err)
	}
	return out, nil
}

// filterParams is the set of optional filters shared by SearchRequest and
// SimilarRequest, factored out so setFilterParams has one implementation.
type filterParams struct {
	UserID      int64
	FeedIDs     []int64
	CategoryIDs []int64
	UnreadOnly  bool
	StarredOnly bool
	Since       time.Time
	Until       time.Time
}

func setFilterParams(values url.Values, f filterParams) {
	if f.UserID > 0 {
		values.Set("user", strconv.FormatInt(f.UserID, 10))
	}
	for _, id := range f.FeedIDs {
		values.Add("feed", strconv.FormatInt(id, 10))
	}
	for _, id := range f.CategoryIDs {
		values.Add("category", strconv.FormatInt(id, 10))
	}
	if f.UnreadOnly {
		values.Set("unread", "true")
	}
	if f.StarredOnly {
		values.Set("starred", "true")
	}
	if !f.Since.IsZero() {
		values.Set("since", f.Since.UTC().Format(time.RFC3339))
	}
	if !f.Until.IsZero() {
		values.Set("until", f.Until.UTC().Format(time.RFC3339))
	}
}

// get issues one bounded GET request and decodes its JSON body into out.
// ctx is wrapped in this Client's own timeout, which is the ONLY bound on
// the call now that the *http.Client is shared process-wide and carries
// no Timeout of its own (see sharedHTTPClient). A context deadline covers
// more than http.Client.Timeout did anyway — connect, response headers
// and body read alike — so no caller can make a reader page wait on the
// sidecar for longer than this Client was configured to allow, whatever
// context it passes in.
func (c *Client) get(ctx context.Context, path string, values url.Values, out any) error {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	u := c.baseURL + path
	if len(values) > 0 {
		u += "?" + values.Encode()
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return fmt.Errorf("building request: %w", err)
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status %d", resp.StatusCode)
	}

	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decoding response: %w", err)
	}

	return nil
}
