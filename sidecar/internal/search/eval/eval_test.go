// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package eval // import "miniflux.app/v2/sidecar/internal/search/eval"

import (
	"math"
	"os"
	"path/filepath"
	"testing"
)

// This file is hermetic on purpose (spec §11): no database, no
// embedder, no network. It must pass with `go test ./internal/search/eval/`
// alone, without -tags ORT and without SIDECAR_TEST_DATABASE_URL set.

func TestRecallAtKCountsOnlyTheTopK(t *testing.T) {
	relevant := []int64{10, 20, 30}
	results := []int64{99, 10, 98, 20, 97, 30}

	if got := RecallAtK(results, relevant, 3); got != 1.0/3.0 {
		t.Fatalf("recall@3: got %v, want 1/3", got)
	}
	if got := RecallAtK(results, relevant, 6); got != 1.0 {
		t.Fatalf("recall@6: got %v, want 1", got)
	}
}

func TestRecallAtKIsZeroWhenNothingRelevantRetrieved(t *testing.T) {
	if got := RecallAtK([]int64{1, 2, 3}, []int64{9}, 3); got != 0 {
		t.Fatalf("got %v, want 0", got)
	}
}

func TestRecallAtKHandlesEmptyRelevantSet(t *testing.T) {
	// A query with no relevant documents is a labelling error, not a score of
	// zero — it must be visible rather than silently dragging the mean down.
	if got := RecallAtK([]int64{1}, nil, 3); got != -1 {
		t.Fatalf("got %v, want -1 sentinel", got)
	}
}

func TestRecallAtKClampsKToResultsLength(t *testing.T) {
	// k larger than len(results) must not panic and must fall back to
	// scoring against every result actually available.
	results := []int64{1, 2, 3}
	relevant := []int64{2, 99}

	if got := RecallAtK(results, relevant, 100); got != 0.5 {
		t.Fatalf("got %v, want 0.5 (1 of 2 relevant found in the only 3 results)", got)
	}
}

func TestRecallAtKZeroReturnsZeroNotPanic(t *testing.T) {
	if got := RecallAtK([]int64{1, 2, 3}, []int64{1}, 0); got != 0 {
		t.Fatalf("got %v, want 0", got)
	}
}

func TestRecallAtKDedupesTheRelevantSet(t *testing.T) {
	// A relevant set listing the same id twice must not inflate the
	// denominator — the true count of distinct relevant documents is 2,
	// not 3.
	results := []int64{1, 2, 3}
	relevant := []int64{1, 1, 2}

	if got := RecallAtK(results, relevant, 3); got != 1.0 {
		t.Fatalf("got %v, want 1 (both distinct relevant ids found)", got)
	}
}

func TestLoadQueriesRejectsAnEmptyRelevantSet(t *testing.T) {
	// Same reasoning, enforced at load time: a query with no relevant
	// documents recorded must fail loudly rather than load silently as a
	// query that will always score the -1 sentinel.
	dir := t.TempDir()
	path := filepath.Join(dir, "queries.json")
	writeJSON(t, path, `[
		{"text": "has a relevant set", "relevant_entry_ids": [1]},
		{"text": "empty relevant set", "relevant_entry_ids": []}
	]`)

	if _, err := LoadQueries(path); err == nil {
		t.Fatal("LoadQueries: got no error for a query with an empty relevant set, want an error")
	}
}

func TestLoadQueriesLoadsWellFormedQueries(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "queries.json")
	writeJSON(t, path, `[
		{"text": "first query", "relevant_entry_ids": [1, 2]},
		{"text": "second query", "relevant_entry_ids": [3]}
	]`)

	queries, err := LoadQueries(path)
	if err != nil {
		t.Fatalf("LoadQueries: unexpected error: %v", err)
	}
	if len(queries) != 2 {
		t.Fatalf("got %d queries, want 2", len(queries))
	}
	if queries[0].Text != "first query" {
		t.Errorf("queries[0].Text = %q, want %q", queries[0].Text, "first query")
	}
	if got, want := queries[0].RelevantEntryIDs, []int64{1, 2}; !int64SlicesEqual(got, want) {
		t.Errorf("queries[0].RelevantEntryIDs = %v, want %v", got, want)
	}
}

func TestLoadQueriesRejectsMissingFile(t *testing.T) {
	if _, err := LoadQueries(filepath.Join(t.TempDir(), "does-not-exist.json")); err == nil {
		t.Fatal("LoadQueries: got no error for a missing file, want an error")
	}
}

func TestLoadQueriesRejectsMalformedJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "queries.json")
	writeJSON(t, path, `not json`)

	if _, err := LoadQueries(path); err == nil {
		t.Fatal("LoadQueries: got no error for malformed JSON, want an error")
	}
}

func TestLoadQueriesRejectsBlankText(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "queries.json")
	writeJSON(t, path, `[{"text": "  ", "relevant_entry_ids": [1]}]`)

	if _, err := LoadQueries(path); err == nil {
		t.Fatal("LoadQueries: got no error for a blank query text, want an error")
	}
}

func TestNewReportComputesTheMeanOverPerQueryRecall(t *testing.T) {
	perQuery := []PerQueryResult{
		{Query: Query{Text: "a", RelevantEntryIDs: []int64{1}}, Recall: 1.0},
		{Query: Query{Text: "b", RelevantEntryIDs: []int64{2}}, Recall: 0.0},
		{Query: Query{Text: "c", RelevantEntryIDs: []int64{3}}, Recall: 0.5},
	}

	report := NewReport(perQuery)

	if len(report.PerQuery) != 3 {
		t.Fatalf("got %d PerQuery entries, want 3", len(report.PerQuery))
	}
	want := (1.0 + 0.0 + 0.5) / 3.0
	if math.Abs(report.MeanRecall-want) > 1e-9 {
		t.Fatalf("MeanRecall = %v, want %v", report.MeanRecall, want)
	}
}

func TestNewReportOfNoQueriesIsZeroNotNaN(t *testing.T) {
	report := NewReport(nil)
	if report.MeanRecall != 0 {
		t.Fatalf("MeanRecall of an empty report = %v, want 0 (not NaN)", report.MeanRecall)
	}
}

// TestLoadQueriesLoadsTheRealEvalSet ties this hermetic test file to the
// actual committed dataset: it is the one check that would fail if
// testdata/queries.json regressed below the required 25-query minimum, or
// picked up a labelling error LoadQueries itself would reject. Still
// hermetic — testdata/queries.json is a static file, not a live corpus.
func TestLoadQueriesLoadsTheRealEvalSet(t *testing.T) {
	queries, err := LoadQueries("testdata/queries.json")
	if err != nil {
		t.Fatalf("LoadQueries(testdata/queries.json): unexpected error: %v", err)
	}
	const minQueries = 25
	if len(queries) < minQueries {
		t.Fatalf("got %d queries in testdata/queries.json, want at least %d", len(queries), minQueries)
	}
	for i, q := range queries {
		if len(q.RelevantEntryIDs) == 0 {
			t.Errorf("query %d (%q) has no relevant entry ids", i, q.Text)
		}
	}
}

func writeJSON(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing fixture %s: %v", path, err)
	}
}

func int64SlicesEqual(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
