// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package processor // import "miniflux.app/v2/internal/reader/processor"

import "testing"

func TestContainsArticleText(t *testing.T) {
	scenarios := []struct {
		name     string
		content  string
		expected bool
	}{
		{"empty", "", false},
		{"empty paragraph, what readability returns for a page with no article", "<p></p>", false},
		{"markup holding only whitespace", "<div>\n  <p> </p>\n</div>", false},
		{"a real paragraph", "<p>An article about feed readers.</p>", true},
		{"text inside nested markup", "<div><section><span>short</span></section></div>", true},
	}

	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			if got := ContainsArticleText(scenario.content); got != scenario.expected {
				t.Fatalf("ContainsArticleText(%q) = %v, expected %v", scenario.content, got, scenario.expected)
			}
		})
	}
}
