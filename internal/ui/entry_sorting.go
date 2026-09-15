// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui // import "miniflux.app/v2/internal/ui"

import (
	"net/http"

	"miniflux.app/v2/internal/http/request"
	"miniflux.app/v2/internal/storage"
	"miniflux.app/v2/internal/validator"
)

// searchSortOrders is the curated set of validator.ValidateEntryOrder
// values offered by the search page's per-view sort picker. The validator
// accepts a broader set (id, category_id, ...) shared with the REST API;
// the picker only offers columns a reader would recognise.
var searchSortOrders = []string{
	"published_at", "created_at", "changed_at", "title", "author",
	"category_title", "status", "starred", "hidden",
}

// parseEntryOrder resolves the "order" query parameter against
// validator.ValidateEntryOrder, falling back to fallbackOrder for an
// absent or invalid value. WithSorting passes its column through
// pq.QuoteIdentifier, which happily quotes any string and hands Postgres
// a column that may not exist - so an unvalidated request value must
// never reach it. A crafted, unrecognised ?order= therefore renders the
// page on fallbackOrder (typically the user's saved preference) rather
// than erroring the whole page.
func parseEntryOrder(r *http.Request, fallbackOrder string) string {
	raw := request.QueryStringParam(r, "order", "")
	if raw == "" {
		return fallbackOrder
	}
	if err := validator.ValidateEntryOrder(raw); err != nil {
		return fallbackOrder
	}
	return raw
}

// parseEntryDirection resolves the "direction" query parameter against
// validator.ValidateDirection, with the same invalid-falls-back-to-default
// behaviour as parseEntryOrder, for the same reason: WithSorting trusts
// its direction argument as much as its column argument.
func parseEntryDirection(r *http.Request, fallbackDirection string) string {
	raw := request.QueryStringParam(r, "direction", "")
	if raw == "" {
		return fallbackDirection
	}
	if err := validator.ValidateDirection(raw); err != nil {
		return fallbackDirection
	}
	return raw
}

// withStableEntrySorting adds order/direction to builder, then a stable
// secondary sort by published_at and id, and returns it for chaining.
// Sorting by a grouping column - starred, hidden or status - only orders
// entries into buckets; within a bucket Postgres may return rows in any
// order, which can change between requests and makes offset pagination
// skip or repeat entries. The redundant secondary is skipped when the
// primary column already is it.
func withStableEntrySorting(builder *storage.EntryQueryBuilder, order, direction string) *storage.EntryQueryBuilder {
	builder = builder.WithSorting(order, direction)
	if order != "published_at" {
		builder = builder.WithSorting("published_at", "desc")
	}
	if order != "id" {
		builder = builder.WithSorting("id", "desc")
	}
	return builder
}
