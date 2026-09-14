// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package eval // import "miniflux.app/v2/sidecar/internal/search/eval"

import (
	"database/sql"
	"os"
	"strings"
	"testing"

	_ "github.com/lib/pq"
)

// checkCorpusStats is pure logic — no database — so every failure mode the
// guard exists to catch can be exercised hermetically, without ever
// writing a single corrupting row into the real, shared search.passages
// table. See VerifyCorpusIntegrity's doc comment for why this split
// exists.

func TestCheckCorpusStatsAcceptsAHealthyCorpus(t *testing.T) {
	stats := CorpusStats{
		TotalPassages:    5681,
		TitlePassages:    436,
		HasEmbeddings:    true,
		MinEmbeddingNorm: 0.9999998,
		MaxEmbeddingNorm: 1.0000002,
	}

	if err := checkCorpusStats(stats); err != nil {
		t.Fatalf("expected a healthy corpus to pass, got error: %v", err)
	}
}

func TestCheckCorpusStatsRejectsAnEmptyCorpus(t *testing.T) {
	err := checkCorpusStats(CorpusStats{})
	if err == nil {
		t.Fatal("expected an error for an empty corpus, got nil")
	}
	if !strings.Contains(err.Error(), "no passages") {
		t.Errorf("error %q does not mention the empty-corpus problem", err.Error())
	}
	assertMentionsReindex(t, err)
}

func TestCheckCorpusStatsRejectsMissingTitlePassages(t *testing.T) {
	// The regression this guards: a corpus indexed before Task 1.5 (or a
	// backfill that silently never ran after that migration) has body
	// passages but no title passages, which would silently under-report
	// title-only matches without ever producing an error of its own.
	stats := CorpusStats{
		TotalPassages:    5245,
		TitlePassages:    0,
		HasEmbeddings:    true,
		MinEmbeddingNorm: 1.0,
		MaxEmbeddingNorm: 1.0,
	}

	err := checkCorpusStats(stats)
	if err == nil {
		t.Fatal("expected an error for a corpus with no title passages, got nil")
	}
	if !strings.Contains(err.Error(), "title") {
		t.Errorf("error %q does not mention the missing-title-passages problem", err.Error())
	}
	assertMentionsReindex(t, err)
}

func TestCheckCorpusStatsRejectsMissingEmbeddings(t *testing.T) {
	stats := CorpusStats{
		TotalPassages: 100,
		TitlePassages: 10,
		HasEmbeddings: false,
	}

	err := checkCorpusStats(stats)
	if err == nil {
		t.Fatal("expected an error for a corpus with no embeddings, got nil")
	}
	assertMentionsReindex(t, err)
}

func TestCheckCorpusStatsRejectsNonNormalisedEmbeddings(t *testing.T) {
	// This is the exact failure mode the task brief names: a fake
	// embedder sweep overwrote real corpus vectors with something that
	// is not unit-norm. The symptom is not an error from anything else —
	// only this check catches it.
	stats := CorpusStats{
		TotalPassages:    100,
		TitlePassages:    10,
		HasEmbeddings:    true,
		MinEmbeddingNorm: 0.0,
		MaxEmbeddingNorm: 0.0,
	}

	err := checkCorpusStats(stats)
	if err == nil {
		t.Fatal("expected an error for non-normalised embeddings, got nil")
	}
	if !strings.Contains(err.Error(), "normalis") {
		t.Errorf("error %q does not mention normalisation", err.Error())
	}
	assertMentionsReindex(t, err)
}

func TestCheckCorpusStatsToleratesTinyFloatingPointDrift(t *testing.T) {
	// Real, correctly-normalised pgvector norms measured against the live
	// corpus come back as 0.9999998...1.0000002, never exactly 1.0. The
	// tolerance must accept that without accepting a genuinely corrupted
	// corpus.
	stats := CorpusStats{
		TotalPassages:    100,
		TitlePassages:    10,
		HasEmbeddings:    true,
		MinEmbeddingNorm: 0.9999998211860497,
		MaxEmbeddingNorm: 1.0000001788139183,
	}

	if err := checkCorpusStats(stats); err != nil {
		t.Fatalf("expected tiny floating-point drift to be tolerated, got error: %v", err)
	}
}

func assertMentionsReindex(t *testing.T, err error) {
	t.Helper()
	if !strings.Contains(err.Error(), "corrupt") || !strings.Contains(err.Error(), "re-index") {
		t.Errorf("error %q does not say the corpus is corrupted and must be re-indexed", err.Error())
	}
}

// The remaining tests require a real connection and are the one place
// this package's hermetic contract (see eval.go's package doc) is
// deliberately broken, for the same reason store's own tests break it:
// there is no way to verify VerifyCorpusIntegrity's SQL against anything
// but a real ParadeDB database. They skip without SIDECAR_DATABASE_URL,
// per the plan's global constraints, and they never write anything —
// only SELECT — so there is no way for them to touch, let alone corrupt,
// the corpus.

func testDB(t *testing.T) *sql.DB {
	t.Helper()

	dsn := os.Getenv("SIDECAR_DATABASE_URL")
	if dsn == "" {
		t.Skip("SIDECAR_DATABASE_URL is not set, skipping database test")
	}

	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("unable to open database: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	return db
}

// TestVerifyCorpusIntegrityAcceptsTheRealCorpus is a read-only check that
// the query VerifyCorpusIntegrity actually runs agrees with
// checkCorpusStats' judgment on the real, live corpus. It never inserts,
// updates or deletes a single row.
func TestVerifyCorpusIntegrityAcceptsTheRealCorpus(t *testing.T) {
	db := testDB(t)

	if err := VerifyCorpusIntegrity(db); err != nil {
		t.Fatalf("expected the real corpus to pass the integrity guard, got: %v", err)
	}
}

// TestQueryCorpusStatsMatchesTheRealCorpus pins queryCorpusStats' SQL
// against ground truth read independently in this test, so a bug in the
// query itself (wrong table, wrong column, wrong aggregate) would show up
// here rather than being masked by checkCorpusStats' tolerance.
func TestQueryCorpusStatsMatchesTheRealCorpus(t *testing.T) {
	db := testDB(t)

	var wantTotal, wantTitles int64
	if err := db.QueryRow(`SELECT count(*) FROM search.passages`).Scan(&wantTotal); err != nil {
		t.Fatalf("unable to count passages: %v", err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM search.passages WHERE source = 'title'`).Scan(&wantTitles); err != nil {
		t.Fatalf("unable to count title passages: %v", err)
	}

	stats, err := queryCorpusStats(db)
	if err != nil {
		t.Fatalf("queryCorpusStats: unexpected error: %v", err)
	}

	if stats.TotalPassages != wantTotal {
		t.Errorf("TotalPassages = %d, want %d", stats.TotalPassages, wantTotal)
	}
	if stats.TitlePassages != wantTitles {
		t.Errorf("TitlePassages = %d, want %d", stats.TitlePassages, wantTitles)
	}
}
