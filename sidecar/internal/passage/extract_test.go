// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package passage // import "miniflux.app/v2/sidecar/internal/passage"

import "testing"

func TestExtractTextStripsMarkup(t *testing.T) {
	got := ExtractText(`<p>Hello <strong>world</strong>.</p>`)
	if got != "Hello world." {
		t.Fatalf("got %q", got)
	}
}

func TestExtractTextSeparatesBlockElements(t *testing.T) {
	// Without block separation "one" and "two" would run together as "onetwo"
	// and be indexed as a word that does not exist.
	got := ExtractText(`<p>one</p><p>two</p>`)
	if got != "one two" {
		t.Fatalf("got %q", got)
	}
}

func TestExtractTextDropsScriptAndStyle(t *testing.T) {
	got := ExtractText(`<p>visible</p><script>var x = 1;</script><style>p{color:red}</style>`)
	if got != "visible" {
		t.Fatalf("got %q", got)
	}
}

func TestExtractTextDecodesEntities(t *testing.T) {
	got := ExtractText(`<p>caf&eacute; &amp; bar</p>`)
	if got != "café & bar" {
		t.Fatalf("got %q", got)
	}
}

func TestExtractTextCollapsesWhitespace(t *testing.T) {
	got := ExtractText("<p>a   b\n\n\tc</p>")
	if got != "a b c" {
		t.Fatalf("got %q", got)
	}
}

func TestExtractTextReturnsEmptyForChromeOnly(t *testing.T) {
	// The article-less case P0 already guards against; the sidecar must agree.
	if got := ExtractText(`<div><p id="app"></p></div>`); got != "" {
		t.Fatalf("expected empty, got %q", got)
	}
}

// Go's RE2 \s is ASCII-only, so before the whitespace pattern was widened
// a decoded &nbsp; (U+00A0) survived collapsing as a literal character and
// travelled into passage text, the BM25 index, and the offsets derived
// from it (whole-branch fix wave, finding 7).
func TestExtractTextCollapsesUnicodeWhitespace(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"nbsp entity", "<p>one&nbsp;two</p>", "one two"},
		{"literal nbsp", "<p>one  two</p>", "one two"},
		{"nbsp mixed with ascii space", "<p>one   two</p>", "one two"},
		{"leading and trailing nbsp is trimmed", "<p> one two </p>", "one two"},
		{"ideographic space", "<p>one　two</p>", "one two"},
		{"narrow no-break space", "<p>one two</p>", "one two"},
		{"zero-width no-break space", "<p>one\ufefftwo</p>", "one two"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExtractText(tc.input); got != tc.want {
				t.Fatalf("ExtractText(%q) = %q, want %q (bytes: % x)", tc.input, got, tc.want, got)
			}
		})
	}
}
