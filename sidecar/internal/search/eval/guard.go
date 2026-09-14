// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package eval // import "miniflux.app/v2/sidecar/internal/search/eval"

import (
	"database/sql"
	"fmt"
	"math"
	"testing"
)

// normTolerance bounds how far a passage embedding's L2 norm may drift
// from 1.0 and still count as "L2-normalised". Real, correctly-computed
// norms measured against the live corpus come back as
// 0.9999998...1.0000002 — floating-point noise from mean pooling plus
// normalisation, not corruption. 0.02 comfortably tolerates that while
// still catching the failure mode this guard exists for: a fake embedder
// that never normalised at all, whose vectors have arbitrary norms far
// outside this band (measured as ~0 for an all-zero fake embedding, or
// wildly larger otherwise).
const normTolerance = 0.02

// CorpusStats is the small set of facts VerifyCorpusIntegrity's judgment
// is based on. It exists as its own type, separate from the SQL that
// produces it, so that judgment (checkCorpusStats) can be tested
// hermetically against constructed values, without a database, for every
// failure mode this guard exists to catch — see guard_test.go's
// hermetic cases. Only queryCorpusStats and VerifyCorpusIntegrity ever
// touch a live connection.
type CorpusStats struct {
	TotalPassages int64
	TitlePassages int64

	// HasEmbeddings is false when no passage has a non-NULL embedding at
	// all — MinEmbeddingNorm/MaxEmbeddingNorm are meaningless in that
	// case (SQL min()/max() over zero rows is NULL, not 0), so this is
	// tracked separately rather than inferred from the norms being zero.
	HasEmbeddings    bool
	MinEmbeddingNorm float64
	MaxEmbeddingNorm float64
}

// corruptionSuffix is appended to every failure this guard reports. Every
// one of them is "the corpus is corrupted and must be re-indexed" in a
// different shape — an empty table, a title-less backfill, vectors that
// were never normalised — and the fix is always the same, so the message
// says so explicitly rather than making a caller infer it from which
// specific check failed.
const corruptionSuffix = "the corpus is corrupted and must be re-indexed — do not trust recall numbers from this run"

// checkCorpusStats judges a CorpusStats snapshot, pure logic with no I/O.
// It never returns a degraded number: every failure is a hard error naming
// what looked wrong and why the corpus must be re-indexed rather than
// measured, per the task brief's "fail loudly ... never return a degraded
// number".
func checkCorpusStats(stats CorpusStats) error {
	if stats.TotalPassages == 0 {
		return fmt.Errorf("eval: corpus has no passages — %s", corruptionSuffix)
	}

	if stats.TitlePassages == 0 {
		return fmt.Errorf(
			"eval: corpus has %d passages but none with source='title' — a corpus indexed before entry titles "+
				"were added to the index (or backfilled with a stale pipeline version) would silently under-report "+
				"title-only matches with no error of its own; %s",
			stats.TotalPassages, corruptionSuffix,
		)
	}

	if !stats.HasEmbeddings {
		return fmt.Errorf("eval: no passage has an embedding — %s", corruptionSuffix)
	}

	if math.Abs(stats.MinEmbeddingNorm-1.0) > normTolerance || math.Abs(stats.MaxEmbeddingNorm-1.0) > normTolerance {
		return fmt.Errorf(
			"eval: passage embeddings are not L2-normalised (norm range [%.6f, %.6f], want 1.0 ± %.2f) — "+
				"this is exactly the symptom of a fake-embedder sweep overwriting real corpus vectors, which looks "+
				"like plausible-looking bad recall numbers rather than an error; %s",
			stats.MinEmbeddingNorm, stats.MaxEmbeddingNorm, normTolerance, corruptionSuffix,
		)
	}

	return nil
}

// queryCorpusStats reads the facts checkCorpusStats judges from the
// database, doing the aggregation in SQL so no row ever crosses the wire
// to be judged in Go. vector_norm is pgvector's own L2 norm function,
// applied per-row before min()/max() aggregate across the whole table —
// exactly the "assert every passage embedding is L2-normalised" the task
// brief asks for, not a sample.
func queryCorpusStats(db *sql.DB) (CorpusStats, error) {
	var stats CorpusStats

	if err := db.QueryRow(`SELECT count(*) FROM search.passages`).Scan(&stats.TotalPassages); err != nil {
		return CorpusStats{}, fmt.Errorf("eval: corpus integrity: unable to count passages: %w", err)
	}

	if err := db.QueryRow(
		`SELECT count(*) FROM search.passages WHERE source = 'title'`,
	).Scan(&stats.TitlePassages); err != nil {
		return CorpusStats{}, fmt.Errorf("eval: corpus integrity: unable to count title passages: %w", err)
	}

	var minNorm, maxNorm sql.NullFloat64
	if err := db.QueryRow(
		`SELECT min(vector_norm(embedding)), max(vector_norm(embedding))
		 FROM search.passages WHERE embedding IS NOT NULL`,
	).Scan(&minNorm, &maxNorm); err != nil {
		return CorpusStats{}, fmt.Errorf("eval: corpus integrity: unable to compute embedding norms: %w", err)
	}
	stats.HasEmbeddings = minNorm.Valid
	stats.MinEmbeddingNorm = minNorm.Float64
	stats.MaxEmbeddingNorm = maxNorm.Float64

	return stats, nil
}

// VerifyCorpusIntegrity is the precondition every recall-reporting run
// must pass before it runs a single query: it checks that the connected
// corpus's passage embeddings are L2-normalised and that title passages
// exist, per the task brief's corpus-integrity guard.
//
// This exists because the previous task's pre-existing indexer test suite
// swept the entire entries table with a fake embedder and overwrote real
// corpus vectors — caught only because someone happened to check. The
// resulting symptom is not an error, it is plausible-looking bad recall
// numbers: the worst failure mode a measurement tool can have, because
// nothing about running it tells you it lied. VerifyCorpusIntegrity is
// what makes that failure loud instead of silent.
//
// Call this before LoadQueries/RecallAtK/NewReport are driven against a
// live corpus (Task 4's recall runner). It performs read-only SELECTs
// only — it can never itself touch, let alone corrupt, the corpus it is
// checking.
func VerifyCorpusIntegrity(db *sql.DB) error {
	stats, err := queryCorpusStats(db)
	if err != nil {
		return err
	}
	return checkCorpusStats(stats)
}

// RequireIntactCorpus is VerifyCorpusIntegrity wired directly into a test:
// any test that is about to report recall against a live corpus should
// call this first and nothing else, so a corrupted corpus fails the test
// loudly, immediately, and before a single retrieval query runs — rather
// than producing a Report whose MeanRecall merely looks bad.
func RequireIntactCorpus(t *testing.T, db *sql.DB) {
	t.Helper()
	if err := VerifyCorpusIntegrity(db); err != nil {
		t.Fatalf("corpus integrity guard failed, refusing to report recall: %v", err)
	}
}
