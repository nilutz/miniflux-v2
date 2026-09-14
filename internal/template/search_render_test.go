// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package template // import "miniflux.app/v2/internal/template"

import (
	"strings"
	"testing"

	"miniflux.app/v2/internal/config"
	"miniflux.app/v2/internal/model"
)

func init() {
	// layout.html (which search.html and entry.html both extend) calls
	// several config.Opts accessors (hasAuthProxy, apiEnabled, ...); the
	// package-level Opts is nil until something loads it, which nothing
	// in this test binary otherwise does.
	if config.Opts == nil {
		config.Opts = config.NewConfigOptions()
	}
}

// fakeSegment/fakeRow mirror internal/ui's snippetSegment/searchRow field
// names exactly (that package cannot be imported here without a cycle,
// since it imports this one) so search.html's field access resolves the
// same way it would against the real types.
type fakeSegment struct {
	Text      string
	Highlight bool
}
type fakeRow struct {
	Entry    *model.Entry
	Segments []fakeSegment
	Ordinal  int
}
type fakePagination struct {
	Route                                           string
	SearchQuery, Mode                               string
	Total, Offset, ItemsPerPage                     int
	NextOffset, LastOffset, PrevOffset, FirstOffset int
	UnreadOnly                                      bool
	ShowNext, ShowLast, ShowFirst, ShowPrev         bool
}

func newFakeEntry(id int64, title string) *model.Entry {
	e := model.NewEntry()
	e.ID = id
	e.Title = title
	e.Status = "unread"
	e.Feed.ID = 2
	e.Feed.Title = "Feed"
	e.Feed.Category.ID = 3
	e.Feed.Category.Title = "Cat"
	return e
}

// TestSearchTemplateRendersSnippetsWithoutRawHTML is the render-level
// proof of this task's core security constraint: a highlighted snippet
// segment containing HTML-significant characters (worst case, a script
// tag - snippet text is article content from the open internet) must
// come out of the template escaped, wrapped in a literal <mark> the
// template itself writes - never as raw markup the segment supplied.
func TestSearchTemplateRendersSnippetsWithoutRawHTML(t *testing.T) {
	engine := NewEngine("")
	engine.ParseTemplates()

	entry := newFakeEntry(1, "Hello")
	rows := []fakeRow{
		{
			Entry: entry,
			Segments: []fakeSegment{
				{Text: "before "},
				{Text: "<script>alert(1)</script>", Highlight: true},
				{Text: " after"},
			},
		},
		{
			Entry:    entry,
			Segments: []fakeSegment{{Text: "passage text"}},
			Ordinal:  3,
		},
	}

	data := map[string]any{
		"language":         "en_US",
		"theme":            "system_serif",
		"searchQuery":      "hello",
		"searchMode":       "hybrid",
		"searchModes":      []string{"keyword", "semantic", "hybrid", "passages"},
		"searchUnreadOnly": false,
		"searchDegraded":   true,
		"searchRows":       rows,
		"total":            2,
		"pagination":       fakePagination{Route: "/search"},
		"user":             &model.User{},
		"hasSaveEntry":     false,
		"menu":             "search",
		"countUnread":      0,
		"countErrorFeeds":  0,
	}

	out := string(engine.Render("search.html", data))

	if !strings.Contains(out, "<mark>&lt;script&gt;alert(1)&lt;/script&gt;</mark>") {
		t.Fatalf("expected the highlighted segment to be escaped inside <mark>, got:\n%s", out)
	}
	if strings.Contains(out, "<script>alert(1)</script>") {
		t.Fatalf("found UNESCAPED script content in rendered output - highlight rendering is unsafe:\n%s", out)
	}
	if !strings.Contains(out, "Search mode") {
		t.Fatalf("expected the mode picker label to render, got:\n%s", out)
	}
	if !strings.Contains(out, "Advanced search is temporarily unavailable") {
		t.Fatalf("expected the degraded notice to render, got:\n%s", out)
	}
	if !strings.Contains(out, "Passage 3") {
		t.Fatalf("expected the passage-mode ordinal to render, got:\n%s", out)
	}
}

// TestEntryTemplateRendersSimilarBlock proves the similar-articles block
// renders when populated, and that its entry titles - also article
// content - are escaped rather than trusted.
func TestEntryTemplateRendersSimilarBlock(t *testing.T) {
	engine := NewEngine("")
	engine.ParseTemplates()

	entry := newFakeEntry(1, "Main entry")
	similar := model.Entries{newFakeEntry(9, "<b>Related</b>")}

	data := map[string]any{
		"language":        "en_US",
		"theme":           "system_serif",
		"entry":           entry,
		"user":            &model.User{},
		"hasSaveEntry":    false,
		"similarEntries":  similar,
		"menu":            "search",
		"countUnread":     0,
		"countErrorFeeds": 0,
	}

	out := string(engine.Render("entry.html", data))

	if !strings.Contains(out, "Similar articles") {
		t.Fatalf("expected the similar-articles heading to render, got:\n%s", out)
	}
	if !strings.Contains(out, "&lt;b&gt;Related&lt;/b&gt;") {
		t.Fatalf("expected the similar entry's title to be escaped, got:\n%s", out)
	}
	if strings.Contains(out, "<b>Related</b>") {
		t.Fatalf("found UNESCAPED entry title in rendered output:\n%s", out)
	}
}

// TestEntryTemplateOmitsSimilarBlockWhenNil proves the block is simply
// absent - not an empty box, not an error - when similarEntries was
// never set (spec §8.3: the sidecar being down must degrade silently).
func TestEntryTemplateOmitsSimilarBlockWhenNil(t *testing.T) {
	engine := NewEngine("")
	engine.ParseTemplates()

	entry := newFakeEntry(1, "Main entry")

	data := map[string]any{
		"language":        "en_US",
		"theme":           "system_serif",
		"entry":           entry,
		"user":            &model.User{},
		"hasSaveEntry":    false,
		"menu":            "search",
		"countUnread":     0,
		"countErrorFeeds": 0,
	}

	out := string(engine.Render("entry.html", data))

	if strings.Contains(out, "entry-similar") {
		t.Fatalf("expected no similar-articles block when similarEntries is absent, got:\n%s", out)
	}
}
