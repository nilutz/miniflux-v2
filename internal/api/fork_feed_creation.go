// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package api // import "miniflux.app/v2/internal/api"

import (
	json_parser "encoding/json"
	"io"

	"miniflux.app/v2/internal/config"
	"miniflux.app/v2/internal/model"
)

// decodeFeedCreationRequest decodes a POST /v1/feeds payload, applying
// CRAWLER_ENABLED_BY_DEFAULT when the payload omits "crawler".
//
// Deviates from upstream, where an omitted "crawler" always yields false:
// mobile clients create feeds through this endpoint and never send the
// field, leaving the crawler off and the feed with only RSS excerpts
// forever, which defeats full-text search.
//
// The default is pre-set on the struct rather than applied afterwards
// because encoding/json leaves absent fields untouched, which is what
// keeps an omitted "crawler" distinguishable from an explicit false.
func decodeFeedCreationRequest(body io.Reader, feedCreationRequest *model.FeedCreationRequest) error {
	feedCreationRequest.Crawler = config.Opts.CrawlerEnabledByDefault()

	return json_parser.NewDecoder(body).Decode(feedCreationRequest)
}
