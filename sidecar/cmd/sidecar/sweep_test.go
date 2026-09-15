// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main // import "miniflux.app/v2/sidecar/cmd/sidecar"

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	_ "github.com/lib/pq"

	"miniflux.app/v2/sidecar/internal/embed/onnx"
	"miniflux.app/v2/sidecar/internal/indexer"
	"miniflux.app/v2/sidecar/internal/passage"
	"miniflux.app/v2/sidecar/internal/search"
	"miniflux.app/v2/sidecar/internal/search/eval"
	"miniflux.app/v2/sidecar/internal/store"
	"miniflux.app/v2/sidecar/internal/testdb"
)

// TestChunkingSweep is the repeatable chunking sweep tool the nomic
// migration plan's task 3 brief (and the task 13 brief it supersedes)
// asks for: for a given passage.SplitOptions, re-index a corpus from
// scratch and report per-mode recall@10 alongside passage count and
// passages-per-entry, so a sweep across candidate SplitOptions is one
// command per configuration rather than a one-off measurement redone by
// hand each time.
//
// It lives here, not in internal/search or internal/passage, for the
// identical reason TestEvalRecall does (see that file's own doc comment):
// cmd/sidecar is documented as the only package in this module that
// imports internal/embed/onnx, and this tool needs a real embedder to
// produce a real corpus to score.
//
// # Why this is a gated test, not a plain `go test ./...` citizen
//
// This re-indexes an entire corpus — deleting every row in
// search.passages and search.entry_index_state first, unconditionally —
// which is destructive by design (a sweep has to start from a known,
// empty state to measure a *specific* SplitOptions rather than whatever
// was indexed last). testdb.DSN already refuses to run against the
// database the sidecar itself serves, but that alone is not enough here:
// TestEvalRecall's two gates (SIDECAR_TEST_DATABASE_URL,
// SIDECAR_MODEL_PATH) are also satisfied by anyone already set up to run
// the OTHER gated tests in this package, and none of those expect the
// database they point at to be truncated out from under them. So this
// test has a THIRD, own-purpose gate — SIDECAR_SWEEP=1 — that nothing
// else in this module sets or needs, specifically so having the other two
// configured is never enough, by itself, to trigger 15-20 minutes of
// destructive re-indexing by accident.
//
// # Invocation
//
// One command per configuration, mirroring TestEvalRecall's own
// documented invocation:
//
//	CGO_LDFLAGS="-L<dir-with-libtokenizers.a>" \
//	DYLD_LIBRARY_PATH=/opt/homebrew/opt/onnxruntime/lib \
//	SIDECAR_TEST_DATABASE_URL=postgres://miniflux:REDACTED-DEV-PASSWORD@127.0.0.1:5432/<throwaway>?sslmode=disable \
//	SIDECAR_MODEL_PATH=<path-to-model_quantized.onnx> \
//	SIDECAR_ONNX_LIB_DIR=/opt/homebrew/opt/onnxruntime/lib \
//	SIDECAR_SWEEP=1 \
//	SIDECAR_SWEEP_TARGET_TOKENS=1500 SIDECAR_SWEEP_MAX_TOKENS=2200 SIDECAR_SWEEP_OVERLAP_TOKENS=200 \
//	  go test -tags ORT ./cmd/sidecar/ -run TestChunkingSweep -v -timeout 30m
//
// SIDECAR_SWEEP_TARGET_TOKENS / _MAX_TOKENS / _OVERLAP_TOKENS default to
// passage.DefaultSplitOptions()'s own values when unset, so
// SIDECAR_SWEEP=1 alone re-measures the current defaults — useful as a
// sanity check that the tool reproduces a known baseline before trusting
// it on a candidate configuration.
//
// The target database must already hold the entries to index (see
// sidecar/scripts/seed-eval-corpus.sh and
// internal/search/eval/README.md's "The corpus" section) and
// testdata/queries.json's relevant-entry-id labels must actually name
// entries present in it — this tool re-indexes whatever corpus it finds,
// it does not create or label one. Skips (not fails) without
// SIDECAR_TEST_DATABASE_URL or SIDECAR_MODEL_PATH; skips (not fails)
// without SIDECAR_SWEEP=1.
//
// # What "refuse to run against a corpus failing the integrity guard" means here
//
// The task brief requires refusing to run against a corpus that fails
// eval.VerifyCorpusIntegrity. Since this tool always re-indexes fully
// before scoring anything, checking that guard BEFORE the re-index would
// only ever observe leftover state from whatever ran here last — on a
// freshly created throwaway database, search.passages starts empty, which
// the guard already (correctly) treats as corrupt, so a before-reindex
// check would refuse to run on the very first invocation ever made
// against a new database. The guard is therefore checked once, after the
// re-index completes and before a single recall query runs — the same
// point TestEvalRecall already checks it at, and the point at which "is
// this corpus trustworthy" is actually decidable: a half-indexed or
// zero-passage corpus at THAT point is a real failure (the embedder
// errored out partway, say), not merely leftover history, and this still
// refuses before producing a single recall number. See guard.go's own
// doc comment for why a degraded number is worse than a hard failure
// here.
//
// # Baseline to compare against
//
// Measured once, on the corpus this project's eval harness was built
// against (436 entries, nine tech/security blogs — see
// internal/search/eval/README.md), at the CURRENT (pre-task-3, still
// word-counted) defaults — TargetTokens=320, MaxTokens=512,
// OverlapTokens=64 — 13 passages/entry:
//
//	keyword=0.828  semantic=0.955  hybrid=0.932  passages=0.919
//
// That corpus and its database no longer exist (deleted at the operator's
// request, 2026-09-15) and testdata/queries.json's 32 relevant-entry-id
// labels were written against ITS entry ids, which a freshly re-seeded
// corpus will not reproduce (autoincrementing ids start over). This tool
// cannot be run meaningfully today without first: (1) seeding a fresh
// corpus (sidecar/scripts/seed-eval-corpus.sh) and letting it fully
// index, and (2) re-deriving testdata/queries.json's relevant_entry_ids
// against whatever ids that seeding actually produces — by hand, reading
// each candidate entry, exactly as the original 32 were labelled (see
// internal/search/eval/README.md's "The labels" section). Skipping step
// (2) would not fail loudly: RecallAtK would just silently score every
// query against entry ids that happen not to exist in the results,
// producing plausible-looking near-zero recall for every mode — exactly
// the "worse than none" failure guard.go's own doc comment warns about,
// just one hop upstream of what the guard itself can check.
func TestChunkingSweep(t *testing.T) {
	dsn := testdb.DSN(t)

	modelPath := os.Getenv("SIDECAR_MODEL_PATH")
	if modelPath == "" {
		t.Skip("SIDECAR_MODEL_PATH is not set, skipping model test")
	}

	if os.Getenv("SIDECAR_SWEEP") != "1" {
		t.Skip("SIDECAR_SWEEP is not set to \"1\" -- this test re-indexes the entire corpus at the configured DSN " +
			"and is never run implicitly just because a database and model are configured for the OTHER gated " +
			"tests in this package; see this test's own doc comment")
	}

	opts, err := sweepSplitOptionsFromEnv()
	if err != nil {
		t.Fatalf("reading SIDECAR_SWEEP_* configuration: %v", err)
	}

	overallStart := time.Now()

	// A direct, second connection for the destructive wipe and the
	// integrity guard -- deliberately independent of the *store.Store
	// connection the indexing/search path below runs through, the same
	// separation TestEvalRecall's own guard connection uses. store.Store
	// exposes no Exec (see store.Reader's own doc comment for why:
	// keeping it the sole writer to search.passages in production code),
	// so a raw connection is the only way anything outside package store
	// can issue the TRUNCATE this tool needs.
	rawDB, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("opening raw database connection: %v", err)
	}
	defer rawDB.Close()

	s, err := store.New(dsn)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	defer s.Close()
	if err := s.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// TRUNCATE, not a content-hash-driven re-offer: changing SplitOptions
	// changes no entry's title or content, so contentHash (which folds in
	// PipelineVersion and the embedder's identity, never SplitOptions —
	// see internal/store/entries.go) would not consider a single entry
	// pending after a mere chunking change. This is the sweep tool's own
	// reason to exist rather than relying on the production re-offer
	// mechanism: it must force a full re-index under a NEW SplitOptions
	// on demand, not only when a model, pipeline or entry actually
	// changed.
	if _, err := rawDB.Exec(`TRUNCATE search.passages, search.entry_index_state`); err != nil {
		t.Fatalf("truncating search.passages/search.entry_index_state before re-indexing: %v", err)
	}

	embedder, err := onnx.NewONNX(onnx.ONNXConfig{
		ModelPath:      modelPath,
		ONNXLibraryDir: os.Getenv("SIDECAR_ONNX_LIB_DIR"),
	})
	if err != nil {
		t.Fatalf("creating embedder: %v", err)
	}
	defer embedder.Close()

	idx := indexer.New(s, embedder)
	idx.SetSplitOptions(opts)
	defer idx.Close()

	// StopWhenDrained: true is what makes Start() a "re-index once and
	// return" call instead of the production lane's "keep sweeping
	// forever" behaviour -- the same knob backfillTestConfig() in
	// internal/indexer's own tests uses for exactly this reason. PageSize
	// is raised well past DefaultBackfillPageSize (20): that default is
	// sized for a lane polling gently in the background over hours, not
	// for driving a one-shot re-index to completion as fast as the
	// embedder allows.
	const sweepPageSize = 200
	controller := indexer.NewController(indexer.DefaultControllerConfig())
	backfill := indexer.NewBackfill(idx, controller, indexer.BackfillConfig{
		PageSize:        sweepPageSize,
		StopWhenDrained: true,
	})

	t.Logf("sweep: re-indexing with %s", describeSplitOptions(opts))
	reindexStart := time.Now()
	if err := backfill.Start(context.Background()); err != nil {
		t.Fatalf("backfill did not complete: %v", err)
	}
	reindexDuration := time.Since(reindexStart)

	stats := backfill.Stats()
	t.Logf("sweep: re-index complete in %s -- indexed=%d skipped=%d failed=%d",
		reindexDuration, stats.Indexed, stats.Skipped, stats.Failed)
	if stats.Failed > 0 {
		t.Logf("sweep: %d entries failed to index (by reason: %v) -- recall numbers below still reflect whatever DID index", stats.Failed, stats.FailedByReason)
	}

	// See this test's own doc comment for why the guard is checked HERE,
	// after the re-index, rather than before it.
	if err := eval.VerifyCorpusIntegrity(rawDB); err != nil {
		t.Fatalf("refusing to report recall: %v", err)
	}

	passageCount, entryCount, err := sweepPassageStats(rawDB)
	if err != nil {
		t.Fatalf("querying passage stats: %v", err)
	}
	var passagesPerEntry float64
	if entryCount > 0 {
		passagesPerEntry = float64(passageCount) / float64(entryCount)
	}

	searcher := search.NewSearcher(s, search.WithEmbedder(embedder))
	queries, err := eval.LoadQueries(evalQueriesPath)
	if err != nil {
		t.Fatalf("loading eval queries: %v", err)
	}

	ctx := context.Background()
	evalStart := time.Now()
	reports := runRecallEval(t, ctx, searcher, queries)
	evalDuration := time.Since(evalStart)

	overallDuration := time.Since(overallStart)

	t.Logf("=== SWEEP RESULT ===")
	t.Logf("split options:      %s", describeSplitOptions(opts))
	t.Logf("passages:           %d total, %d entries indexed, %.2f passages/entry", passageCount, entryCount, passagesPerEntry)
	t.Logf("recall@%d by mode:", evalK)
	for _, m := range evalModes {
		t.Logf("  %-10s mean recall@%d = %.4f", m.name, evalK, reports[m.name].MeanRecall)
	}
	t.Logf("timing:             reindex=%s eval=%s total=%s", reindexDuration, evalDuration, overallDuration)
	t.Logf("baseline (436-entry corpus, defaults 320/512/64, 13 passages/entry, now unreproducible -- see doc comment):")
	t.Logf("  keyword=0.8280  semantic=0.9550  hybrid=0.9320  passages=0.9190")
}

// sweepSplitOptionsFromEnv reads SIDECAR_SWEEP_TARGET_TOKENS,
// SIDECAR_SWEEP_MAX_TOKENS and SIDECAR_SWEEP_OVERLAP_TOKENS, each
// defaulting to passage.DefaultSplitOptions()'s own value when unset --
// so a sweep run can override exactly the fields it cares about and leave
// the rest at the production default, and SIDECAR_SWEEP=1 alone
// re-measures the current defaults. TokenCounter is deliberately left
// nil: the sweep tool measures the SAME word-estimated chunking
// production indexing actually does today, not an idealised real-token
// version of it -- see TestSplitPassageAtCapNeverExceedsNomicsRealTokenLimit
// for where a real TokenCounter is exercised instead.
func sweepSplitOptionsFromEnv() (passage.SplitOptions, error) {
	opts := passage.DefaultSplitOptions()

	fields := []struct {
		name string
		dst  *int
	}{
		{"SIDECAR_SWEEP_TARGET_TOKENS", &opts.TargetTokens},
		{"SIDECAR_SWEEP_MAX_TOKENS", &opts.MaxTokens},
		{"SIDECAR_SWEEP_OVERLAP_TOKENS", &opts.OverlapTokens},
	}
	for _, f := range fields {
		raw, ok := os.LookupEnv(f.name)
		if !ok {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(raw))
		if err != nil {
			return passage.SplitOptions{}, fmt.Errorf("%s must be a whole number, got %q", f.name, raw)
		}
		*f.dst = n
	}

	return opts, nil
}

func describeSplitOptions(opts passage.SplitOptions) string {
	return fmt.Sprintf("TargetTokens=%d MaxTokens=%d OverlapTokens=%d", opts.TargetTokens, opts.MaxTokens, opts.OverlapTokens)
}

// sweepPassageStats returns the total passage count and the number of
// distinct entries actually represented in search.passages after a
// re-index -- the two numbers passagesPerEntry is derived from. Counting
// DISTINCT entry_id from search.passages itself, rather than counting
// public.entries, deliberately excludes entries that were skipped
// (empty/unusable content) from the denominator: passages-per-entry is
// meant to describe the chunking of entries that actually produced
// passages, not be diluted by entries that contributed none.
func sweepPassageStats(db *sql.DB) (passageCount, entryCount int64, err error) {
	if err := db.QueryRow(`SELECT count(*), count(DISTINCT entry_id) FROM search.passages`).Scan(&passageCount, &entryCount); err != nil {
		return 0, 0, fmt.Errorf("counting search.passages: %w", err)
	}
	return passageCount, entryCount, nil
}
