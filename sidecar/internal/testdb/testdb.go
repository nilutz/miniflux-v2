// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package testdb resolves the DSN the sidecar's database-backed tests run
// against. It exists for one reason: those tests must never be able to
// reach the database the sidecar itself is configured to serve.
//
// SIDECAR_DATABASE_URL is the variable an operator exports to RUN the
// sidecar, and the sidecar's own docs tell them to point it at their live
// Miniflux database. `make test` runs the whole suite, including
// internal/indexer's, which sweeps the entire entries table and rewrites
// search.passages with whatever embedder the test constructed — a fake
// one, in most of those tests. Reading the production DSN from the tests
// would mean "start the sidecar" and "run the tests" are one export apart
// from each other.
//
// So the tests read SIDECAR_TEST_DATABASE_URL, a variable whose only
// purpose is to be a throwaway database, and DSN refuses outright when it
// has been pointed at the same place as SIDECAR_DATABASE_URL.
package testdb // import "miniflux.app/v2/sidecar/internal/testdb"

import (
	"os"
	"strings"
	"testing"
)

// EnvVar is the environment variable database-backed tests read.
const EnvVar = "SIDECAR_TEST_DATABASE_URL"

// productionEnvVar is the variable the sidecar BINARY reads. Tests must
// never use it, and DSN fails rather than skips when the two name the
// same database — a skip would be the wrong answer to "you are about to
// run a destructive sweep against your live corpus".
const productionEnvVar = "SIDECAR_DATABASE_URL"

// DSN returns the DSN for database-backed tests, or skips t when
// SIDECAR_TEST_DATABASE_URL is unset — keeping `go test ./...` hermetic
// on a machine with no database, exactly as before.
//
// It calls t.Fatal, not t.Skip, when SIDECAR_TEST_DATABASE_URL is set to
// the same value as SIDECAR_DATABASE_URL: that is a misconfiguration
// whose consequence is data loss, and it must stop the run rather than
// be quietly stepped over.
func DSN(t testing.TB) string {
	t.Helper()

	dsn := strings.TrimSpace(os.Getenv(EnvVar))
	if dsn == "" {
		t.Skipf("%s is not set, skipping database test", EnvVar)
	}

	if prod := strings.TrimSpace(os.Getenv(productionEnvVar)); prod != "" && prod == dsn {
		t.Fatalf("%s and %s point at the same database (%s); these tests rewrite search.passages and must never run against the database the sidecar serves",
			EnvVar, productionEnvVar, redact(dsn))
	}

	return dsn
}

// redact strips a DSN's userinfo so a failure message can name the
// database without printing a password into CI logs.
func redact(dsn string) string {
	scheme, rest, ok := strings.Cut(dsn, "://")
	if !ok {
		return dsn
	}
	if _, after, hasUserinfo := strings.Cut(rest, "@"); hasUserinfo {
		rest = after
	}
	return scheme + "://" + rest
}
