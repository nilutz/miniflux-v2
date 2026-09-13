// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package api // import "miniflux.app/v2/internal/api"

import (
	"strings"
	"testing"

	"miniflux.app/v2/internal/config"
	"miniflux.app/v2/internal/model"
)

// TestDecodeFeedCreationRequestCrawlerDefault pins the one place where this
// fork deviates from upstream's POST /v1/feeds contract: a payload that omits
// "crawler" gets CRAWLER_ENABLED_BY_DEFAULT, while a payload that sends the
// field explicitly is always obeyed.
func TestDecodeFeedCreationRequestCrawlerDefault(t *testing.T) {
	scenarios := []struct {
		name                        string
		crawlerByDefaultConfigValue string
		payload                     string
		expectedCrawler             bool
	}{
		{"omitted with the default enabled", "1", `{"feed_url": "https://example.org/feed.xml"}`, true},
		{"omitted with the default disabled", "0", `{"feed_url": "https://example.org/feed.xml"}`, false},
		{"explicit false wins over the default", "1", `{"feed_url": "https://example.org/feed.xml", "crawler": false}`, false},
		{"explicit true with the default disabled", "0", `{"feed_url": "https://example.org/feed.xml", "crawler": true}`, true},
	}

	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			t.Setenv("CRAWLER_ENABLED_BY_DEFAULT", scenario.crawlerByDefaultConfigValue)

			parsedOptions, err := config.NewConfigParser().ParseEnvironmentVariables()
			if err != nil {
				t.Fatalf("unable to configure test options: %v", err)
			}

			previousOptions := config.Opts
			config.Opts = parsedOptions
			t.Cleanup(func() {
				config.Opts = previousOptions
			})

			var feedCreationRequest model.FeedCreationRequest
			if err := decodeFeedCreationRequest(strings.NewReader(scenario.payload), &feedCreationRequest); err != nil {
				t.Fatalf("unable to decode the payload: %v", err)
			}

			if feedCreationRequest.Crawler != scenario.expectedCrawler {
				t.Fatalf("expected crawler=%v, got %v", scenario.expectedCrawler, feedCreationRequest.Crawler)
			}

			if feedCreationRequest.FeedURL != "https://example.org/feed.xml" {
				t.Fatalf("expected the rest of the payload to be decoded, got feed_url=%q", feedCreationRequest.FeedURL)
			}
		})
	}
}
