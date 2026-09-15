// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package sidecarclient is a small HTTP client for the sidecar's own
// read-only search API (internal/web/search_handlers.go and
// article_handler.go): GET /api/search, GET /api/similar and
// GET /api/article. It exists so cmd/mcp can talk to a running sidecar as
// a plain HTTP client, with no dependency on internal/web, internal/store,
// internal/search or anything else that would pull database or (via
// cmd/sidecar's own sibling packages) native ONNX Runtime code into an MCP
// binary that never needs either — see cmd/mcp's own package doc comment.
//
// Every request carries an X-Auth-Token header when a token is configured
// (New's apiKey parameter), matching the header internal/api/middleware.go
// reads on the main Miniflux fork's own API
// (validateAPIKeyAuth, `token := r.Header.Get("X-Auth-Token")`). The
// sidecar does not check this header today -- GET /api/search,
// /api/similar and /api/article are unauthenticated, by design (see
// internal/web/search_handlers.go's package doc comment) -- but a
// follow-on task will have it validate the token against
// public.api_keys, the same table and header the fork's own API already
// uses. Sending it unconditionally now means that follow-on task requires
// no change here: an unrecognised header is simply ignored by today's
// sidecar.
package sidecarclient // import "miniflux.app/v2/sidecar/internal/sidecarclient"

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// DefaultBaseURL is used when no SIDECAR_URL environment variable is set
// (cmd/mcp/main.go) -- matching internal/web.DefaultAddr, the sidecar's
// own default bind address, spelled out as a URL.
const DefaultBaseURL = "http://localhost:8081"

// defaultTimeout bounds every request this client makes. The sidecar is
// loopback/LAN-local and every one of these endpoints is a bounded read
// (see webMaxSearchLimit in internal/web/search_handlers.go), so a slow
// response past this is a hung or overloaded sidecar, not a naturally
// long-running request -- an MCP tool call should fail fast and say so
// rather than leave the calling agent waiting indefinitely.
const defaultTimeout = 30 * time.Second

// authTokenHeader is the header name the fork's own API middleware reads
// (internal/api/middleware.go:41, `r.Header.Get("X-Auth-Token")`). Kept as
// a constant so client.go and its tests never risk drifting from the
// production header name by way of a typo in a string literal.
const authTokenHeader = "X-Auth-Token"

// Client is a minimal HTTP client for the sidecar's search API.
type Client struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

// New builds a Client against baseURL (e.g. "http://localhost:8081").
// apiKey, when non-empty, is sent as the X-Auth-Token header on every
// request (see this package's doc comment); pass "" when none is
// configured -- every request still succeeds against today's sidecar,
// which does not yet check the header.
func New(baseURL, apiKey string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		http:    &http.Client{Timeout: defaultTimeout},
	}
}

// UnreachableError means the request never got an HTTP response at all --
// connection refused, DNS failure, timeout, or any other transport-level
// failure. It is deliberately a distinct type from AuthError and APIError
// below: "the sidecar is not running" and "the sidecar rejected the
// request" are different problems with different fixes, and a caller (an
// MCP tool handler, ultimately an agent) that conflates them tells the
// user the wrong thing to go check.
type UnreachableError struct {
	BaseURL string
	Err     error
}

func (e *UnreachableError) Error() string {
	return fmt.Sprintf("sidecar unreachable at %s: %v", e.BaseURL, e.Err)
}

func (e *UnreachableError) Unwrap() error { return e.Err }

// AuthError means the sidecar responded but rejected the request's
// credentials: HTTP 401 (no/invalid token) or 403 (valid token, not
// permitted). Distinct from APIError (below) for the same reason
// UnreachableError is distinct from both: "your API key is wrong" and
// "your query was malformed" call for different fixes. The sidecar does
// not return either status today -- GET /api/search, /api/similar and
// /api/article are unauthenticated -- so this case is unreachable against
// the current sidecar and exists for the auth it is documented to grow;
// see this package's own doc comment.
type AuthError struct {
	Status  int
	Message string
}

func (e *AuthError) Error() string {
	return fmt.Sprintf("sidecar rejected the API key (HTTP %d): %s -- set MINIFLUX_API_KEY to a valid Miniflux API key", e.Status, e.Message)
}

// APIError means the sidecar responded with a non-2xx status that was
// neither 401 nor 403 -- a caller error such as an unknown search mode
// (400) or a missing entry (404), or a server-side failure (500). Message
// is the sidecar's own {"error": "..."} body text (see apiError in
// internal/web/search_handlers.go), which that package's own doc comment
// guarantees is always safe to show a caller.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("sidecar returned HTTP %d: %s", e.Status, e.Message)
}

// apiErrorBody mirrors internal/web/search_handlers.go's apiError wire
// type: {"error": "<message>"}.
type apiErrorBody struct {
	Error string `json:"error"`
}

// get performs a GET against path+query, decoding a 200 JSON body into
// out. Every non-2xx and every transport failure returns one of
// UnreachableError, AuthError or APIError above -- never a bare error a
// caller has to string-match to classify.
func (c *Client) get(ctx context.Context, path string, query url.Values, out any) error {
	full := c.baseURL + path
	if len(query) > 0 {
		full += "?" + query.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, full, nil)
	if err != nil {
		return fmt.Errorf("sidecarclient: unable to build request: %w", err)
	}
	if c.apiKey != "" {
		req.Header.Set(authTokenHeader, c.apiKey)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return &UnreachableError{BaseURL: c.baseURL, Err: err}
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return &UnreachableError{BaseURL: c.baseURL, Err: fmt.Errorf("reading response body: %w", err)}
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var eb apiErrorBody
		_ = json.Unmarshal(body, &eb) // best-effort; fall back to a generic message below
		msg := eb.Error
		if msg == "" {
			msg = strings.TrimSpace(string(body))
		}
		if msg == "" {
			msg = resp.Status
		}
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return &AuthError{Status: resp.StatusCode, Message: msg}
		}
		return &APIError{Status: resp.StatusCode, Message: msg}
	}

	if out != nil {
		if err := json.Unmarshal(body, out); err != nil {
			return fmt.Errorf("sidecarclient: unable to decode response from %s: %w", full, err)
		}
	}
	return nil
}

// Range is search.Range reshaped for JSON, mirrored from
// internal/web/search_handlers.go's rangeView.
type Range struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

// Snippet is search.Snippet reshaped for JSON, mirrored from
// internal/web/search_handlers.go's snippetView.
type Snippet struct {
	Text       string  `json:"text"`
	Highlights []Range `json:"highlights"`
}

// EntryResult is one keyword/semantic/hybrid search result, mirrored from
// internal/web/search_handlers.go's entryResultView.
type EntryResult struct {
	EntryID int64   `json:"entry_id"`
	Score   float64 `json:"score"`
	Snippet Snippet `json:"snippet"`
}

// PassageResult is one ModePassages search result, mirrored from
// internal/web/search_handlers.go's passageResultView.
type PassageResult struct {
	EntryID   int64   `json:"entry_id"`
	PassageID int64   `json:"passage_id"`
	Ordinal   int     `json:"ordinal"`
	Source    string  `json:"source"`
	Score     float64 `json:"score"`
	Snippet   Snippet `json:"snippet"`
}

// SearchResult is GET /api/search's response body, mirrored from
// internal/web/search_handlers.go's searchResponseView. Entries is
// populated for keyword/semantic/hybrid, Passages for passages -- never
// both, matching that type's own doc comment.
type SearchResult struct {
	Mode     string          `json:"mode"`
	Query    string          `json:"query"`
	Entries  []EntryResult   `json:"entries,omitempty"`
	Passages []PassageResult `json:"passages,omitempty"`
}

// SearchParams are GET /api/search's query parameters that this client
// exposes. Mode is passed through verbatim to the sidecar, which rejects
// an unrecognised value with a 400 (parseMode in search_handlers.go) --
// this client does not re-validate it. An empty Mode means "no opinion",
// which the sidecar resolves as hybrid, the same default parseMode
// documents.
type SearchParams struct {
	Query string
	Mode  string
	Limit int
}

// Search wraps GET /api/search.
func (c *Client) Search(ctx context.Context, p SearchParams) (SearchResult, error) {
	q := url.Values{"q": {p.Query}}
	if p.Mode != "" {
		q.Set("mode", p.Mode)
	}
	if p.Limit > 0 {
		q.Set("limit", strconv.Itoa(p.Limit))
	}

	var result SearchResult
	if err := c.get(ctx, "/api/search", q, &result); err != nil {
		return SearchResult{}, err
	}
	return result, nil
}

// SimilarEntry is one GET /api/similar result, mirrored from
// internal/web/search_handlers.go's similarEntryResultView. It carries no
// snippet, by that endpoint's own deliberate design -- see
// similarEntryResultView's doc comment for why.
type SimilarEntry struct {
	EntryID int64   `json:"entry_id"`
	Score   float64 `json:"score"`
}

// SimilarResult is GET /api/similar's response body, mirrored from
// internal/web/search_handlers.go's similarResponseView.
type SimilarResult struct {
	EntryID int64          `json:"entry_id"`
	Entries []SimilarEntry `json:"entries"`
}

// Similar wraps GET /api/similar.
func (c *Client) Similar(ctx context.Context, entryID int64, limit int) (SimilarResult, error) {
	q := url.Values{"entry_id": {strconv.FormatInt(entryID, 10)}}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}

	var result SimilarResult
	if err := c.get(ctx, "/api/similar", q, &result); err != nil {
		return SimilarResult{}, err
	}
	return result, nil
}

// Article is GET /api/article's response body, mirrored from
// internal/web/article_handler.go's articleResponseView.
type Article struct {
	EntryID     int64     `json:"entry_id"`
	Title       string    `json:"title"`
	URL         string    `json:"url"`
	PublishedAt time.Time `json:"published_at"`
	Content     string    `json:"content"`
}

// Article wraps GET /api/article.
func (c *Client) Article(ctx context.Context, entryID int64) (Article, error) {
	q := url.Values{"entry_id": {strconv.FormatInt(entryID, 10)}}

	var result Article
	if err := c.get(ctx, "/api/article", q, &result); err != nil {
		return Article{}, err
	}
	return result, nil
}
