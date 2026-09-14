// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package passage // import "miniflux.app/v2/sidecar/internal/passage"

import (
	"regexp"
	"strings"

	"golang.org/x/net/html"
)

// blockElements lists the HTML elements that should force a word break in
// the extracted plaintext. Miniflux stores sanitised article HTML, so this
// only needs to cover the tags a sanitised article can plausibly contain —
// not the full HTML5 element set.
var blockElements = map[string]bool{
	"p": true, "div": true, "br": true, "li": true,
	"ul": true, "ol": true, "dl": true, "dt": true, "dd": true,
	"h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true,
	"blockquote": true, "pre": true,
	"table": true, "thead": true, "tbody": true, "tfoot": true, "tr": true, "td": true, "th": true,
	"section": true, "article": true, "header": true, "footer": true,
	"nav": true, "aside": true, "figure": true, "figcaption": true,
	"hr": true, "form": true, "fieldset": true, "address": true,
	"main": true, "details": true, "summary": true,
}

// skippedElements lists elements whose text content must never reach the
// index — script bodies and stylesheets are not article content.
var skippedElements = map[string]bool{
	"script": true, "style": true, "noscript": true,
}

// whitespaceRun matches a run of whitespace to collapse. Go's RE2 \s is
// ASCII-only ([\t\n\f\r ]), which is not enough for article HTML: a
// sanitised entry routinely contains &nbsp;, which the HTML parser decodes
// to U+00A0, and U+00A0 would survive collapsing as a literal character —
// landing inside passage text, inside the BM25 index, and inside the
// char_start/char_end offsets derived from it. \p{Z} covers U+00A0 and
// every other Unicode space separator; U+FEFF (a zero-width no-break
// space, category Cf) is added explicitly because it is a common BOM
// leftover in scraped content and is not in \p{Z}.
//
// strings.TrimSpace already trims all of these — unicode.IsSpace includes
// U+00A0 and U+0085 — so trimming and collapsing now agree.
var whitespaceRun = regexp.MustCompile(`[\s\p{Z}\x{feff}]+`)

// ExtractText converts sanitised article HTML into plain text. Block-level
// elements are separated by a space so adjacent blocks never run their words
// together; script, style and noscript subtrees and comments are dropped
// entirely; entities are decoded by the HTML parser itself. All whitespace
// runs — Unicode included, see whitespaceRun — collapse to a single ASCII
// space and the result is trimmed.
func ExtractText(input string) string {
	doc, err := html.Parse(strings.NewReader(input))
	if err != nil {
		return ""
	}

	var sb strings.Builder
	extractNode(doc, &sb)

	collapsed := whitespaceRun.ReplaceAllString(sb.String(), " ")
	return strings.TrimSpace(collapsed)
}

func extractNode(n *html.Node, sb *strings.Builder) {
	switch n.Type {
	case html.TextNode:
		sb.WriteString(n.Data)
	case html.CommentNode:
		// Comment content is never article text.
	case html.ElementNode:
		if skippedElements[n.Data] {
			return
		}
		block := blockElements[n.Data]
		if block {
			sb.WriteByte(' ')
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			extractNode(c, sb)
		}
		if block {
			sb.WriteByte(' ')
		}
	default:
		// DocumentNode, DoctypeNode: no text of their own, just recurse.
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			extractNode(c, sb)
		}
	}
}
