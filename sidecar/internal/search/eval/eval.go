// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package eval implements the retrieval-quality metric spec §11 requires:
// recall@k against a small, hand-labelled set of query -> relevant-entry-id
// pairs. It is deliberately hermetic — no database, no embedder, no
// network — so that RecallAtK's arithmetic can be trusted independently of
// whatever runs it against real retrieval later (Task 4).
//
// See README.md for what testdata/queries.json actually measures and what
// it does not.
package eval // import "miniflux.app/v2/sidecar/internal/search/eval"

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// Query is one hand-labelled evaluation case: a search query and the set of
// entry ids a human judged relevant to it by reading them.
type Query struct {
	Text             string  `json:"text"`
	RelevantEntryIDs []int64 `json:"relevant_entry_ids"`

	// Note documents why this query is in the set — e.g. which of the
	// task brief's categories (close wording, paraphrase, rare
	// discriminating term, short vs. sentence-length) it was chosen to
	// cover. Purely documentary: nothing in this package reads it.
	Note string `json:"note,omitempty"`
}

// LoadQueries reads a labelled query set from a JSON file: an array of
// Query objects.
//
// A query with an empty RelevantEntryIDs is rejected rather than loaded.
// Such a query is a labelling error, not a legitimately hard query — see
// RecallAtK's own doc comment for why silently scoring it would be worse
// than refusing to load it at all: it would drag the mean recall down
// while looking exactly like ordinary retrieval failure.
func LoadQueries(path string) ([]Query, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("eval: reading %s: %w", path, err)
	}

	var queries []Query
	if err := json.Unmarshal(data, &queries); err != nil {
		return nil, fmt.Errorf("eval: parsing %s: %w", path, err)
	}

	for i, q := range queries {
		if strings.TrimSpace(q.Text) == "" {
			return nil, fmt.Errorf("eval: %s: query %d has blank text", path, i)
		}
		if len(q.RelevantEntryIDs) == 0 {
			return nil, fmt.Errorf(
				"eval: %s: query %d (%q) has an empty relevant set — a query nobody labelled is a labelling error, not a score of zero; label it with at least one relevant entry id or remove it from the set",
				path, i, q.Text,
			)
		}
	}

	return queries, nil
}

// RecallAtK is the fraction of a query's distinct relevant entries that
// appear anywhere in the top k of results. results is a ranked list of
// entry ids, most relevant first; relevant is the set of entry ids a human
// judged relevant to the query, order irrelevant and duplicates tolerated.
//
// It returns -1, a sentinel not a score, when relevant is empty. A query
// with nothing labelled relevant is a labelling error: scoring it 0 would
// silently pull down the mean recall over a whole evaluation run while
// looking exactly like a retrieval failure, when the actual problem is
// that nobody recorded what the right answer was. LoadQueries refuses to
// load such a query in the first place, so in practice this case should
// only ever come from a Query built by hand (e.g. in a test) — the
// sentinel is what makes that visible rather than silently averaged away.
//
// k is clamped to len(results): asking for recall@100 against 3 results
// scores against the 3 that exist rather than panicking or padding with
// nothing.
func RecallAtK(results []int64, relevant []int64, k int) float64 {
	if len(relevant) == 0 {
		return -1
	}

	relevantSet := make(map[int64]struct{}, len(relevant))
	for _, id := range relevant {
		relevantSet[id] = struct{}{}
	}

	if k < 0 {
		k = 0
	}
	if k > len(results) {
		k = len(results)
	}

	found := make(map[int64]struct{})
	for _, id := range results[:k] {
		if _, ok := relevantSet[id]; ok {
			found[id] = struct{}{}
		}
	}

	return float64(len(found)) / float64(len(relevantSet))
}

// PerQueryResult is one query's recall outcome within a Report.
type PerQueryResult struct {
	Query  Query
	Recall float64
}

// Report summarises a full evaluation run: every query's individual
// recall alongside the mean across all of them. Task 4 builds one per
// retrieval mode (keyword, semantic, hybrid, passages) by running each
// query through that mode's retrieval, scoring it with RecallAtK, and
// passing the resulting []PerQueryResult to NewReport.
type Report struct {
	PerQuery   []PerQueryResult
	MeanRecall float64
}

// NewReport builds a Report from a completed set of per-query results,
// computing MeanRecall as their arithmetic mean. An empty perQuery yields
// MeanRecall 0, not NaN.
//
// perQuery is expected to hold real recall values (RecallAtK's ordinary
// [0,1] output), never the -1 sentinel: LoadQueries already refuses to
// load a query that could produce one. NewReport does not itself special-
// case -1, so a caller that bypasses LoadQueries and feeds one in will see
// it pull the mean down sharply rather than being silently excluded —
// visible for the same reason RecallAtK's own sentinel is, rather than
// papered over here.
func NewReport(perQuery []PerQueryResult) Report {
	if len(perQuery) == 0 {
		return Report{}
	}

	var sum float64
	for _, r := range perQuery {
		sum += r.Recall
	}

	return Report{
		PerQuery:   perQuery,
		MeanRecall: sum / float64(len(perQuery)),
	}
}
