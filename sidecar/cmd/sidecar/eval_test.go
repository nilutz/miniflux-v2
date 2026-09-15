// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main // import "miniflux.app/v2/sidecar/cmd/sidecar"

import (
	"context"
	"database/sql"
	"os"
	"testing"

	_ "github.com/lib/pq"

	"miniflux.app/v2/sidecar/internal/embed/onnx"
	"miniflux.app/v2/sidecar/internal/search"
	"miniflux.app/v2/sidecar/internal/search/eval"
	"miniflux.app/v2/sidecar/internal/store"
	"miniflux.app/v2/sidecar/internal/testdb"
)

// This file, not internal/search, is deliberately where the task 4
// evaluation run against the real corpus lives. cmd/sidecar is documented
// (main.go's own package comment, README.md's "The ORT build tag" section)
// as the ONLY package in this module that imports internal/embed/onnx --
// everything else, internal/search included, depends solely on the
// pure-Go embed.Embedder interface, which is what lets
// `go test -tags ORT ./internal/search/` link and run without
// libtokenizers.a. Putting a real-embedder-driven test inside
// internal/search itself, even gated by -tags ORT, would break that
// invariant for the whole package. Driving internal/search's exported
// Searcher/eval API from here keeps the dependency exactly where the
// rest of the module already puts it.
//
// Run with:
//
//	CGO_LDFLAGS="-L<dir-with-libtokenizers.a>" \
//	DYLD_LIBRARY_PATH=/opt/homebrew/opt/onnxruntime/lib \
//	SIDECAR_TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:5434/miniflux2?sslmode=disable \
//	SIDECAR_MODEL_PATH=<path-to-model_quantized.onnx> \
//	SIDECAR_ONNX_LIB_DIR=/opt/homebrew/opt/onnxruntime/lib \
//	  go test -tags ORT ./cmd/sidecar/ -run TestEvalRecall -v
//
// Skips (not fails) without SIDECAR_TEST_DATABASE_URL or SIDECAR_MODEL_PATH,
// exactly like every other database/model-backed test in this module.

// evalQueriesPath locates the committed, hand-labelled query set relative
// to this package's directory.
const evalQueriesPath = "../../internal/search/eval/testdata/queries.json"

// evalK is the k this evaluation run scores recall@k against, matching
// the task brief's recall@10.
const evalK = 10

func TestEvalRecall(t *testing.T) {
	dsn := testdb.DSN(t)
	modelPath := os.Getenv("SIDECAR_MODEL_PATH")
	if modelPath == "" {
		t.Skip("SIDECAR_MODEL_PATH is not set, skipping model test")
	}

	// The corpus integrity guard runs first, before a single retrieval
	// query, and is a hard failure -- see eval.RequireIntactCorpus's own
	// doc comment for why a corrupted corpus must fail loudly here rather
	// than silently producing plausible-looking bad recall numbers. It
	// opens its own direct connection: read-only, and independent of the
	// store.Store connection Search itself runs through.
	guardDB, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("opening corpus-integrity guard connection: %v", err)
	}
	defer guardDB.Close()
	eval.RequireIntactCorpus(t, guardDB)

	s, err := store.New(dsn)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	defer s.Close()
	if err := s.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	embedder, err := onnx.NewONNX(onnx.ONNXConfig{
		ModelPath:      modelPath,
		ONNXLibraryDir: os.Getenv("SIDECAR_ONNX_LIB_DIR"),
	})
	if err != nil {
		t.Fatalf("creating embedder: %v", err)
	}
	defer embedder.Close()

	searcher := search.NewSearcher(s, search.WithEmbedder(embedder))

	queries, err := eval.LoadQueries(evalQueriesPath)
	if err != nil {
		t.Fatalf("loading eval queries: %v", err)
	}

	ctx := context.Background()
	reports := runRecallEval(t, ctx, searcher, queries)

	t.Logf("recall@%d by mode (%d queries):", evalK, len(queries))
	for _, m := range evalModes {
		r := reports[m.name]
		t.Logf("  %-10s mean recall@%d = %.4f", m.name, evalK, r.MeanRecall)
	}

	for _, m := range evalModes {
		r := reports[m.name]
		for _, pq := range r.PerQuery {
			t.Logf("    [%-8s] recall=%.3f  %-70s  (%s)", m.name, pq.Recall, pq.Query.Text, pq.Query.Note)
		}
	}

	hybrid := reports["hybrid"].MeanRecall
	keyword := reports["keyword"].MeanRecall
	semantic := reports["semantic"].MeanRecall
	passages := reports["passages"].MeanRecall
	t.Logf("summary: keyword=%.4f semantic=%.4f hybrid=%.4f passages=%.4f", keyword, semantic, hybrid, passages)

	if hybrid+1e-9 < keyword || hybrid+1e-9 < semantic {
		t.Logf("FINDING: hybrid (%.4f) did NOT beat both single-mode retrievals (keyword=%.4f, semantic=%.4f) on this eval set -- spec §6.3's central claim does not hold here; see the task report.", hybrid, keyword, semantic)
	}
}

// evalModes is the fixed set of retrieval modes every recall-reporting run
// in this package scores -- shared by TestEvalRecall and TestChunkingSweep
// (task 3's sweep tool) so the two can never silently drift into scoring a
// different set of modes from one another.
var evalModes = []struct {
	name string
	mode search.Mode
}{
	{"keyword", search.ModeKeyword},
	{"semantic", search.ModeSemantic},
	{"hybrid", search.ModeHybrid},
	{"passages", search.ModePassages},
}

// runRecallEval runs every query in queries through every mode in
// evalModes against searcher, scoring each with eval.RecallAtK at evalK,
// and returns one eval.Report per mode name. A Search failure is a hard
// t.Fatalf: an error here means the run cannot be trusted at all, not a
// data point to fold into the mean.
func runRecallEval(t *testing.T, ctx context.Context, searcher *search.Searcher, queries []eval.Query) map[string]eval.Report {
	t.Helper()

	reports := make(map[string]eval.Report, len(evalModes))
	for _, m := range evalModes {
		perQuery := make([]eval.PerQueryResult, 0, len(queries))

		for _, q := range queries {
			resp, err := searcher.Search(ctx, search.Request{
				Query: q.Text,
				Mode:  m.mode,
				Limit: evalK,
			})
			if err != nil {
				t.Fatalf("mode %s, query %q: Search failed: %v", m.name, q.Text, err)
			}

			entryIDs := responseEntryIDs(resp)
			recall := eval.RecallAtK(entryIDs, q.RelevantEntryIDs, evalK)
			perQuery = append(perQuery, eval.PerQueryResult{Query: q, Recall: recall})
		}

		reports[m.name] = eval.NewReport(perQuery)
	}
	return reports
}

// responseEntryIDs extracts a ranked list of distinct entry ids from resp,
// reading Entries for the entry-aggregated modes and de-duplicating
// Passages (preserving first-seen, i.e. best-ranked, order) for
// ModePassages, whose Response carries a flat passage list instead.
func responseEntryIDs(resp search.Response) []int64 {
	if resp.Mode == search.ModePassages {
		seen := make(map[int64]bool, len(resp.Passages))
		ids := make([]int64, 0, len(resp.Passages))
		for _, p := range resp.Passages {
			if seen[p.EntryID] {
				continue
			}
			seen[p.EntryID] = true
			ids = append(ids, p.EntryID)
		}
		return ids
	}

	ids := make([]int64, 0, len(resp.Entries))
	for _, e := range resp.Entries {
		ids = append(ids, e.EntryID)
	}
	return ids
}
