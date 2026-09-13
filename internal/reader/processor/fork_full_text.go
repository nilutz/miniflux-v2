// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package processor // import "miniflux.app/v2/internal/reader/processor"

import (
	"strings"

	"miniflux.app/v2/internal/reader/sanitizer"
)

// ContainsArticleText reports whether scraped content holds any text at all.
//
// Readability never fails on a well-formed page: when it finds no article it
// returns markup with no text in it, such as "<p></p>" for a JavaScript-only
// page whose body is an empty mount point. That is not an article, and an
// entry whose content came out of such a scrape must not be recorded as
// holding full text, or the search index would trust an empty document and the
// backfill would never revisit the entry.
func ContainsArticleText(content string) bool {
	return strings.TrimSpace(sanitizer.StripTags(content)) != ""
}
