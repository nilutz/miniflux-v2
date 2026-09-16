// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package template // import "miniflux.app/v2/internal/template"

import (
	"strings"
	"testing"

	"miniflux.app/v2/internal/model"
)

// TestSavedPagesEntriesTemplateRendersEmptyState is the template-level
// half of the "no saved pages yet" requirement: showSavedPagesPage (in
// internal/ui, not reachable from here without an import cycle) must
// render this exact empty state whenever it is called with no entries -
// this proves the template side of that contract renders correctly and
// does not, for instance, silently render an empty items list instead.
func TestSavedPagesEntriesTemplateRendersEmptyState(t *testing.T) {
	engine := NewEngine("")
	engine.ParseTemplates()

	data := map[string]any{
		"language":        "en_US",
		"theme":           "system_serif",
		"total":           0,
		"entries":         nil,
		"pagination":      fakePagination{Route: "/saved-pages"},
		"order":           "published_at",
		"direction":       "desc",
		"excludeHidden":   false,
		"sortOrders":      []string{"published_at", "title"},
		"user":            &model.User{},
		"hasSaveEntry":    false,
		"menu":            "saved-pages",
		"countUnread":     0,
		"countErrorFeeds": 0,
	}

	out := string(engine.Render("saved_pages_entries.html", data))

	if !strings.Contains(out, "You haven&#39;t saved any pages yet.") {
		t.Fatalf("expected the empty-state alert to render, got:\n%s", out)
	}
	if strings.Contains(out, `class="items"`) {
		t.Fatalf("expected no items list to render alongside the empty state, got:\n%s", out)
	}
}

// TestSavedPagesEntriesTemplateEntryLinkCarriesSortAndHiddenParams proves
// the sort picker's order/direction and the hidden filter round-trip
// through the list's per-entry links (task 10 part A/B's own convention,
// spec §6.3 amendment and §13.3), at the template level: the href
// pagination.go/showSavedPagesPage build is only ever correct if the
// template actually threads the three view variables into
// routePath+queryString the way this asserts.
func TestSavedPagesEntriesTemplateEntryLinkCarriesSortAndHiddenParams(t *testing.T) {
	engine := NewEngine("")
	engine.ParseTemplates()

	entry := newFakeEntry(42, "Round trip entry")

	data := map[string]any{
		"language":        "en_US",
		"theme":           "system_serif",
		"total":           1,
		"entries":         model.Entries{entry},
		"pagination":      fakePagination{Route: "/saved-pages"},
		"order":           "title",
		"direction":       "asc",
		"excludeHidden":   true,
		"sortOrders":      []string{"published_at", "title"},
		"user":            &model.User{},
		"hasSaveEntry":    false,
		"menu":            "saved-pages",
		"countUnread":     0,
		"countErrorFeeds": 0,
	}

	out := string(engine.Render("saved_pages_entries.html", data))

	wantHref := `href="/saved-pages/entry/42?direction=asc&amp;excludeHidden=1&amp;order=title"`
	if !strings.Contains(out, wantHref) {
		t.Fatalf("expected the entry link %s, got:\n%s", wantHref, out)
	}
}
