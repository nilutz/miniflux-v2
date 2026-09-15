// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package mcpserver builds the MCP server cmd/mcp runs over stdio (task
// 15): three tools -- search, similar and fetch_article -- that wrap the
// sidecar's own read-only HTTP API (internal/sidecarclient) so a Claude
// Code agent can search and read this reader's article corpus.
//
// Every tool call is, ultimately, one or more HTTP requests to the
// sidecar. Three things happen to every result before it reaches the
// agent, deliberately:
//
//  1. Zero matches is reported as a normal, successful result with
//     ResultCount 0 and an explanatory Message -- never as a tool error.
//     The corpus is empty right now, and "nothing matched" must not look
//     like a failure (see this package's own tests, e.g.
//     TestSearchZeroResultsIsReportedNotAsAnError).
//  2. A sidecar the client cannot even reach, a sidecar that rejects the
//     configured API key, and a sidecar that rejects the request itself
//     (a bad mode, an unknown entry id) are three different problems with
//     three different fixes -- see internal/sidecarclient's
//     UnreachableError/AuthError/APIError. This package does not
//     re-wrap or flatten those messages: returning the error as-is from a
//     tool handler is enough, because [mcp.AddTool]'s generated handler
//     already turns a returned error into a CallToolResult with IsError
//     set and the error text as the tool's content (see the go-sdk's
//     ToolHandlerFor doc comment) -- an MCP protocol error would instead
//     hide that text from a client that only surfaces tool results.
//  3. search and similar results carry title/url alongside the sidecar's
//     own entry_id/score/snippet, even though GET /api/search and GET
//     /api/similar do not return either (see search_handlers.go's
//     entryResultView/similarEntryResultView) -- the requirement is that
//     a result must be actionable without a second round trip to figure
//     out what it even is. This package fills them in itself, by calling
//     GET /api/article (the same endpoint fetch_article wraps) once per
//     distinct entry id in a result set, concurrently and best-effort: an
//     enrichment failure for one entry degrades that one result's
//     title/url to empty strings rather than failing the whole call (see
//     enrichArticles below).
package mcpserver // import "miniflux.app/v2/sidecar/internal/mcpserver"

import (
	"context"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"miniflux.app/v2/sidecar/internal/sidecarclient"
)

// serverName and serverVersion identify this server in MCP's initialize
// handshake (mcp.Implementation).
const serverName = "miniflux-search"

// enrichConcurrency bounds how many GET /api/article requests run at once
// while filling in title/url for a search or similar result set. The
// sidecar is loopback/LAN-local and webMaxSearchLimit
// (internal/web/search_handlers.go) caps any one result set at 100, so a
// small, fixed bound is enough to keep this fast without opening 100
// simultaneous connections to a single-process sidecar.
const enrichConcurrency = 8

// toolset holds the sidecar client every tool handler shares.
type toolset struct {
	client *sidecarclient.Client
}

// New builds the MCP server: three tools (search, similar, fetch_article)
// backed by requests to baseURL via a *sidecarclient.Client constructed
// with apiKey. version is reported in the initialize handshake.
func New(baseURL, apiKey, version string) *mcp.Server {
	ts := &toolset{client: sidecarclient.New(baseURL, apiKey)}

	server := mcp.NewServer(&mcp.Implementation{Name: serverName, Version: version}, nil)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "search",
		Description: searchDescription,
	}, ts.search)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "similar",
		Description: similarDescription,
	}, ts.similar)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "fetch_article",
		Description: fetchArticleDescription,
	}, ts.fetchArticle)

	return server
}

// articleMeta is the title/url this package fills into search and similar
// results, by entry id.
type articleMeta struct {
	title string
	url   string
}

// enrichArticles fetches title/url for every distinct id in entryIDs by
// calling GET /api/article (client.Article) concurrently, bounded by
// enrichConcurrency, and returns whatever it managed to fetch. An id that
// fails to fetch (the entry was deleted since the search ran, a
// transient error) is simply absent from the returned map -- callers
// render that as an empty title/url, exactly like
// internal/web/search_handlers.go's own EntryLookup degrades a snippet
// build it cannot complete (see entrySnapshots.get's doc comment there)
// rather than failing the whole response over one result.
func (ts *toolset) enrichArticles(ctx context.Context, entryIDs []int64) map[int64]articleMeta {
	out := make(map[int64]articleMeta, len(entryIDs))
	var mu sync.Mutex

	seen := make(map[int64]bool, len(entryIDs))
	sem := make(chan struct{}, enrichConcurrency)
	var wg sync.WaitGroup

	for _, id := range entryIDs {
		if seen[id] {
			continue
		}
		seen[id] = true

		wg.Add(1)
		sem <- struct{}{}
		go func(id int64) {
			defer wg.Done()
			defer func() { <-sem }()

			article, err := ts.client.Article(ctx, id)
			if err != nil {
				return
			}
			mu.Lock()
			out[id] = articleMeta{title: article.Title, url: article.URL}
			mu.Unlock()
		}(id)
	}
	wg.Wait()
	return out
}

// formatPublished renders a published-date value the way every tool
// output uses: RFC3339 when known, "" when the sidecar never returned
// one (a lookup failure during enrichment, or a genuinely zero value) --
// never a zero time.Time's default "0001-01-01T00:00:00Z" string, which
// would read to an agent as a real, absurd date rather than "unknown".
func formatPublished(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339)
}
