// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui // import "miniflux.app/v2/internal/ui"

type pagination struct {
	Route       string
	SearchQuery string
	// Mode carries the search mode picker's selection (spec §6.3) across
	// pagination links. It is only ever set by the search page - every
	// other caller of getPagination leaves it at its zero value, which
	// queryString (internal/template/functions.go) omits from the query
	// string entirely, so this field changes nothing for any other page.
	Mode string
	// ExcludeHidden, Order and Direction carry the search page's hidden
	// filter and sort picker (task 10, parts A and B) across pagination
	// links, the same way Mode already carries the mode picker. Only the
	// search page ever sets them; every other caller of getPagination
	// leaves them at their zero value, which queryString omits, so this
	// changes nothing for any other page.
	ExcludeHidden bool
	Order         string
	Direction     string
	Total         int
	Offset        int
	ItemsPerPage  int
	NextOffset    int
	LastOffset    int
	PrevOffset    int
	FirstOffset   int
	UnreadOnly    bool
	ShowNext      bool
	ShowLast      bool
	ShowFirst     bool
	ShowPrev      bool
}

func getPagination(route string, total, offset, nbItemsPerPage int) pagination {
	nextOffset := 0
	prevOffset := 0

	firstOffset := 0
	lastOffset := (total / nbItemsPerPage) * nbItemsPerPage
	if lastOffset == total {
		lastOffset -= nbItemsPerPage
	}

	showNext := (total - offset) > nbItemsPerPage
	showPrev := offset > 0
	showLast := showNext
	showFirst := showPrev

	if showNext {
		nextOffset = offset + nbItemsPerPage
	}

	if showPrev {
		prevOffset = offset - nbItemsPerPage
	}

	return pagination{
		Route:        route,
		Total:        total,
		Offset:       offset,
		ItemsPerPage: nbItemsPerPage,
		ShowNext:     showNext,
		ShowLast:     showLast,
		NextOffset:   nextOffset,
		LastOffset:   lastOffset,
		ShowPrev:     showPrev,
		ShowFirst:    showFirst,
		PrevOffset:   prevOffset,
		FirstOffset:  firstOffset,
	}
}
