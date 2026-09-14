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

// SimilarResponse is GET /api/similar's response body.
type SimilarResponse struct {
	EntryID int64         `json:"entry_id"`
	Entries []EntryResult `json:"entries"`
}

// SearchRequest is a GET /api/search call. Zero values mean "no opinion"
// for every field: an empty Mode lets the sidecar default to hybrid, a
// zero Limit lets it apply its own default, and zero-value Since/Until
// mean unbounded — matching search_handlers.go's own parsing.
type SearchRequest struct {
	Query       string
	Mode        string // "", "keyword", "semantic", "hybrid", "passages"
	Limit       int
	FeedIDs     []int64
	CategoryIDs []int64
	UnreadOnly  bool
	StarredOnly bool
	Since       time.Time
	Until       time.Time
}

// SimilarRequest is a GET /api/similar call.
type SimilarRequest struct {
	EntryID     int64
	Limit       int
	FeedIDs     []int64
	CategoryIDs []int64
	UnreadOnly  bool
	StarredOnly bool
	Since       time.Time
	Until       time.Time
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
// Exported (rather than an option func) so tests can use a short timeout
// and keep the suite fast without waiting out DefaultTimeout.
func NewClientWithTimeout(baseURL string, timeout time.Duration) *Client {
	return &Client{
		baseURL:    strings.TrimRight(baseURL, "/"),
		httpClient: &http.Client{Timeout: timeout},
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
	FeedIDs     []int64
	CategoryIDs []int64
	UnreadOnly  bool
	StarredOnly bool
	Since       time.Time
	Until       time.Time
}

func setFilterParams(values url.Values, f filterParams) {
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
// ctx is wrapped in its own timeout in addition to httpClient's Timeout
// (belt and suspenders: a caller that passes context.Background(), or a
// context with a much longer deadline than this client's own, still gets
// bounded by c.timeout here) so that no caller can accidentally make a
// reader page wait on the sidecar for longer than this Client was
// configured to allow.
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
