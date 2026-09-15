// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mcpserver // import "miniflux.app/v2/sidecar/internal/mcpserver"

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"miniflux.app/v2/sidecar/internal/sidecarclient"
)

// searchDescription is the search tool's description -- the only thing an
// agent has to decide, from among four retrieval modes it cannot inspect
// the implementation of, which one a given query needs. See this
// package's doc comment, item 2, for why every word here earns its
// place: an agent that picks "keyword" for a conceptual query, or
// "passages" when it wanted one ranked list of articles, gets a
// technically-successful, quietly-wrong answer with no error to notice.
const searchDescription = `Search this reader's personal article corpus (previously-read Miniflux articles) for text relevant to a query. Use this to find articles about a topic before reading, citing, or answering questions about them -- results carry short snippets, not full text; call fetch_article with a result's entry_id to read one in full, or call similar to find more articles like it.

Choose "mode" by what the query needs:
- "keyword": exact-term BM25 full-text search. Best when a specific word, name, acronym, or phrase must appear verbatim in the article (a product name, an error message, a person's name, a quote).
- "semantic": vector similarity over meaning, not wording. Best for a conceptual query where the right article may not use the same words (e.g. "ways to cut cloud costs" should also surface an article about "reducing your AWS bill").
- "hybrid" (default): fuses keyword (BM25) and semantic (vector) ranking into one list. The right choice whenever you are not sure which of the two a query needs, or want both exact and conceptual matches ranked together.
- "passages": like hybrid, but returns individual matching paragraph-sized extracts from within articles instead of one snippet per article. Use this when an article is long and you need to know exactly where in it a topic is discussed, or when several distinct passages across many articles could each be relevant (e.g. "every place X is mentioned").

Returns a ranked list of results, each with the article's entry_id (chain it into similar or fetch_article), title, url, a relevance score, and a highlighted snippet -- or, in passages mode, a passage extract plus which article it came from and its position within it. Zero results is reported explicitly as zero, not as an error: it means nothing in the corpus matched this query, which is expected if the corpus is small, empty, or simply does not contain the topic.`

// similarDescription is the similar tool's description.
const similarDescription = `Find articles already in this reader's corpus that are semantically similar to one given article, identified by its entry_id (as returned by search, fetch_article, or Miniflux itself). Use this for "more like this" browsing once you have found one relevant article, to widen a reading list around the same topic without writing a new search query.

Unlike search, results carry no snippet -- there is no query to highlight against, only article-to-article similarity. Each result is the entry_id, title, url, and a similarity score, ranked most-similar first. Zero results is reported explicitly as zero, not as an error.`

// fetchArticleDescription is the fetch_article tool's description.
const fetchArticleDescription = `Fetch one article's full text by its entry_id, as returned by search or similar. Use this once a promising result has been identified and its complete content is needed to read, quote, or accurately answer questions about it -- search and similar only return short snippets and scores, not enough text to do that reliably.

Returns the article's title, source url, published date (RFC3339, empty if unknown), and full plain-text/HTML content as stored in the corpus. Errors if no entry with that id exists.`

// SearchInput is the search tool's input schema.
type SearchInput struct {
	Query string `json:"query" jsonschema:"the text to search for"`
	Mode  string `json:"mode,omitempty" jsonschema:"retrieval mode: keyword, semantic, hybrid (default), or passages -- see the tool description for what each returns and when to use it"`
	Limit int    `json:"limit,omitempty" jsonschema:"maximum number of results to return (sidecar default 10, capped at 100)"`
}

// SearchResultItem is one result within SearchOutput. PassageID, Ordinal
// and Source are only populated in "passages" mode -- see search.Response's
// own Mode-dependent nil convention, mirrored here by simply leaving these
// at their zero values (omitempty) for every other mode.
type SearchResultItem struct {
	EntryID   int64   `json:"entry_id"`
	Title     string  `json:"title"`
	URL       string  `json:"url"`
	Score     float64 `json:"score"`
	Snippet   string  `json:"snippet"`
	PassageID int64   `json:"passage_id,omitempty"`
	Ordinal   int     `json:"ordinal,omitempty"`
	Source    string  `json:"source,omitempty"`
}

// SearchOutput is the search tool's structured output.
type SearchOutput struct {
	Mode        string             `json:"mode"`
	Query       string             `json:"query"`
	ResultCount int                `json:"result_count"`
	Results     []SearchResultItem `json:"results"`
	Message     string             `json:"message,omitempty"`
}

func (ts *toolset) search(ctx context.Context, _ *mcp.CallToolRequest, in SearchInput) (*mcp.CallToolResult, SearchOutput, error) {
	result, err := ts.client.Search(ctx, sidecarclient.SearchParams{Query: in.Query, Mode: in.Mode, Limit: in.Limit})
	if err != nil {
		return nil, SearchOutput{}, err
	}

	entryIDs := make([]int64, 0, len(result.Entries)+len(result.Passages))
	for _, e := range result.Entries {
		entryIDs = append(entryIDs, e.EntryID)
	}
	for _, p := range result.Passages {
		entryIDs = append(entryIDs, p.EntryID)
	}
	meta := ts.enrichArticles(ctx, entryIDs)

	out := SearchOutput{Mode: result.Mode, Query: result.Query}
	for _, e := range result.Entries {
		m := meta[e.EntryID]
		out.Results = append(out.Results, SearchResultItem{
			EntryID: e.EntryID,
			Title:   m.title,
			URL:     m.url,
			Score:   e.Score,
			Snippet: e.Snippet.Text,
		})
	}
	for _, p := range result.Passages {
		m := meta[p.EntryID]
		out.Results = append(out.Results, SearchResultItem{
			EntryID:   p.EntryID,
			Title:     m.title,
			URL:       m.url,
			Score:     p.Score,
			Snippet:   p.Snippet.Text,
			PassageID: p.PassageID,
			Ordinal:   p.Ordinal,
			Source:    p.Source,
		})
	}
	out.ResultCount = len(out.Results)
	if out.ResultCount == 0 {
		out.Message = "No results found for this query. Either nothing in the corpus matches it, or the corpus is empty."
	}

	return nil, out, nil
}

// SimilarInput is the similar tool's input schema.
type SimilarInput struct {
	EntryID int64 `json:"entry_id" jsonschema:"the entry id of the article to find similar articles for"`
	Limit   int   `json:"limit,omitempty" jsonschema:"maximum number of results to return (sidecar default 10, capped at 100)"`
}

// SimilarResultItem is one result within SimilarOutput.
type SimilarResultItem struct {
	EntryID int64   `json:"entry_id"`
	Title   string  `json:"title"`
	URL     string  `json:"url"`
	Score   float64 `json:"score"`
}

// SimilarOutput is the similar tool's structured output.
type SimilarOutput struct {
	EntryID     int64               `json:"entry_id"`
	ResultCount int                 `json:"result_count"`
	Results     []SimilarResultItem `json:"results"`
	Message     string              `json:"message,omitempty"`
}

func (ts *toolset) similar(ctx context.Context, _ *mcp.CallToolRequest, in SimilarInput) (*mcp.CallToolResult, SimilarOutput, error) {
	result, err := ts.client.Similar(ctx, in.EntryID, in.Limit)
	if err != nil {
		return nil, SimilarOutput{}, err
	}

	entryIDs := make([]int64, 0, len(result.Entries))
	for _, e := range result.Entries {
		entryIDs = append(entryIDs, e.EntryID)
	}
	meta := ts.enrichArticles(ctx, entryIDs)

	out := SimilarOutput{EntryID: result.EntryID}
	for _, e := range result.Entries {
		m := meta[e.EntryID]
		out.Results = append(out.Results, SimilarResultItem{
			EntryID: e.EntryID,
			Title:   m.title,
			URL:     m.url,
			Score:   e.Score,
		})
	}
	out.ResultCount = len(out.Results)
	if out.ResultCount == 0 {
		out.Message = "No similar articles found. Either nothing in the corpus is close enough, or the corpus is empty."
	}

	return nil, out, nil
}

// FetchArticleInput is the fetch_article tool's input schema.
type FetchArticleInput struct {
	EntryID int64 `json:"entry_id" jsonschema:"the entry id of the article to fetch, as returned by search or similar"`
}

// FetchArticleOutput is the fetch_article tool's structured output.
type FetchArticleOutput struct {
	EntryID     int64  `json:"entry_id"`
	Title       string `json:"title"`
	URL         string `json:"url"`
	PublishedAt string `json:"published_at"`
	Content     string `json:"content"`
}

func (ts *toolset) fetchArticle(ctx context.Context, _ *mcp.CallToolRequest, in FetchArticleInput) (*mcp.CallToolResult, FetchArticleOutput, error) {
	article, err := ts.client.Article(ctx, in.EntryID)
	if err != nil {
		return nil, FetchArticleOutput{}, fmt.Errorf("fetch_article: %w", err)
	}

	return nil, FetchArticleOutput{
		EntryID:     article.EntryID,
		Title:       article.Title,
		URL:         article.URL,
		PublishedAt: formatPublished(article.PublishedAt),
		Content:     article.Content,
	}, nil
}
