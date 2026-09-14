# P1a Indexing Pipeline Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Turn the retained article corpus into a populated, queryable hybrid index — passages, embeddings, BM25 — kept current by a live lane and filled by a throttled backfill lane.

**Architecture:** A Go sidecar in its own module, sharing one Postgres with Miniflux. It owns the `search` schema, reads `public.entries` read-only, and never writes to it. Embedding runs in-process through ONNX Runtime via CGO. The search API and fork UI are a separate plan (P1b); this plan ends with an index you can query by hand.

**Tech Stack:** Go 1.26, PostgreSQL 18 via ParadeDB (pgvector + pg_search), hugot/ONNX Runtime, `database/sql` with `lib/pq`.

**Spec:** `docs/superpowers/specs/2026-09-12-miniflux-search-design.md` — sections 4, 6, 9 and 10 bind this plan. §6.7 records the spike measurements every performance decision here rests on.

## Global Constraints

- **The sidecar is a separate Go module at `sidecar/`** in this repository. New top-level directories never conflict when rebasing onto upstream Miniflux, and one repo keeps the schema contract visible to both halves. It has its own `go.mod`; the root module must not depend on it, and it must not import `miniflux.app/v2/...`.
- **Miniflux code is not touched by this plan.** The sidecar reads `public.entries` through SQL. If you find yourself editing a file outside `sidecar/`, stop and report it.
- Every new `.go` file starts with the two-line SPDX header and a package import comment, matching the fork's convention:
  ```go
  // SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
  // SPDX-License-Identifier: Apache-2.0

  package foo // import "miniflux.app/v2/sidecar/foo"
  ```
- Commit messages follow conventional commits: `^(build|chore|ci|docs|feat|fix|perf|refactor|revert|security|style|test)(\(scope\))?!?: <subject>`.
- Logging is `log/slog` with structured attributes.
- **Pin these exactly** (measured in spec §6.7, not chosen by reputation):

  | Component | Version |
  |---|---|
  | `github.com/knights-analytics/hugot` | v0.7.8, build tag `ORT` |
  | `github.com/yalue/onnxruntime_go` | v1.35.0 |
  | `github.com/daulet/tokenizers` | v1.27.0 |
  | ONNX Runtime (native) | v1.30.0 |
  | Model | `Xenova/bge-small-en-v1.5`, int8 ONNX, 384 dims |

- **Build requires `CGO_ENABLED=1` and `-tags ORT`.** Omitting the tag silently builds the far slower pure-Go GoMLX backend and produces *no error*. Task 2 adds a guard against this; never remove it.
- Tests needing Postgres skip when `SIDECAR_DATABASE_URL` is unset. Tests needing the ONNX model skip when `SIDECAR_MODEL_PATH` is unset. Both keep `go test ./...` hermetic.
- Run `go vet ./...` and `gofmt -l .` before every commit. `golangci-lint` is not installed on this machine.

## Performance facts that bind the design

From spec §6.7 — these were measured, and two of them contradict the obvious approach:

- Best throughput is **33.9 passages/sec**: 2 workers, batch 8, **unconstrained** ORT threading, int8.
- hugot's documented tuning (1 intra-op thread per worker) measured **11.7/sec — 3× slower**. The controller varies *worker count against one shared unconstrained session*. Never pin threads.
- Batch 64 (24.3/s) is **slower** than batch 8 (33.9/s). Default 8–32; treat larger as a regression.
- Backfill budget: ~4.1 hours per 0.5M passages, ~41 hours per 5M.

---

### Task 1: Sidecar skeleton, dev database, and the `search` schema

**Files:**
- Create: `sidecar/go.mod`, `sidecar/go.sum`
- Create: `sidecar/internal/store/store.go`, `sidecar/internal/store/migrations.go`, `sidecar/internal/store/migrations_test.go`
- Create: `sidecar/Makefile`, `sidecar/README.md`
- Create: `sidecar/docker-compose.dev.yml`

**Interfaces:**
- Consumes: nothing
- Produces: `store.New(dsn string) (*store.Store, error)`; `(*Store).Migrate() error`; the `search` schema; the test helper `testStore(t *testing.T) *Store`

**Why ParadeDB:** stock Postgres has neither extension — verified: a `postgres:17-alpine` lists 59 available extensions, and neither `vector` nor `pg_search` is among them. `paradedb/paradedb:latest` ships `vector` 0.8.4 and `pg_search` 0.25.9 on PostgreSQL 18, and both were confirmed working together on one table with a fused RRF query.

- [x] **Step 1: Create the dev database compose file**

`sidecar/docker-compose.dev.yml`:

```yaml
services:
  db:
    image: paradedb/paradedb:latest
    environment:
      POSTGRES_USER: postgres
      POSTGRES_PASSWORD: postgres
      POSTGRES_DB: miniflux2
    ports:
      - "5434:5432"
    volumes:
      - paradedb-data:/var/lib/postgresql/data
volumes:
  paradedb-data:
```

Port 5434 deliberately avoids 5432 (the existing Miniflux dev database) and 5433.

Start it and wait for readiness:

```bash
cd sidecar && docker compose -f docker-compose.dev.yml up -d
until PGPASSWORD=postgres pg_isready -h 127.0.0.1 -p 5434 -U postgres; do sleep 2; done
```

- [x] **Step 2: Write the failing migration test**

`sidecar/internal/store/migrations_test.go`:

```go
// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package store // import "miniflux.app/v2/sidecar/internal/store"

import (
	"os"
	"testing"
)

func testStore(t *testing.T) *Store {
	t.Helper()

	dsn := os.Getenv("SIDECAR_DATABASE_URL")
	if dsn == "" {
		t.Skip("SIDECAR_DATABASE_URL is not set, skipping database test")
	}

	s, err := New(dsn)
	if err != nil {
		t.Fatalf("unable to open database: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	return s
}

func TestMigrateCreatesSearchSchema(t *testing.T) {
	s := testStore(t)

	if err := s.Migrate(); err != nil {
		t.Fatalf("migrate failed: %v", err)
	}

	for _, table := range []string{"passages", "entry_index_state"} {
		var exists bool
		err := s.db.QueryRow(`
			SELECT EXISTS (
				SELECT 1 FROM information_schema.tables
				WHERE table_schema='search' AND table_name=$1
			)`, table).Scan(&exists)
		if err != nil {
			t.Fatalf("unable to inspect tables: %v", err)
		}
		if !exists {
			t.Fatalf("expected table search.%s to exist", table)
		}
	}
}

func TestMigrateInstallsBothExtensions(t *testing.T) {
	s := testStore(t)

	if err := s.Migrate(); err != nil {
		t.Fatalf("migrate failed: %v", err)
	}

	for _, extension := range []string{"vector", "pg_search"} {
		var installed bool
		err := s.db.QueryRow(
			`SELECT EXISTS (SELECT 1 FROM pg_extension WHERE extname=$1)`, extension,
		).Scan(&installed)
		if err != nil {
			t.Fatalf("unable to inspect extensions: %v", err)
		}
		if !installed {
			t.Fatalf("expected extension %q to be installed; is this the ParadeDB image?", extension)
		}
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	s := testStore(t)

	if err := s.Migrate(); err != nil {
		t.Fatalf("first migrate failed: %v", err)
	}
	if err := s.Migrate(); err != nil {
		t.Fatalf("second migrate failed: %v", err)
	}
}
```

- [x] **Step 3: Run the test to verify it fails**

```bash
cd sidecar && go test ./internal/store/ -run TestMigrate -v
```
Expected: FAIL — `undefined: New`, `undefined: Store`.

- [x] **Step 4: Implement the store and migrations**

`sidecar/internal/store/store.go`:

```go
// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package store // import "miniflux.app/v2/sidecar/internal/store"

import (
	"database/sql"
	"fmt"

	_ "github.com/lib/pq"
)

// Store owns the `search` schema and reads Miniflux's `public` schema
// read-only.
type Store struct {
	db *sql.DB
}

func New(dsn string) (*Store, error) {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: unable to open database: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Ping() error { return s.db.Ping() }
```

`sidecar/internal/store/migrations.go`:

```go
// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package store // import "miniflux.app/v2/sidecar/internal/store"

import (
	"database/sql"
	"fmt"
	"log/slog"
)

// migrations are owned entirely by the sidecar and tracked in
// search.schema_version, which is independent of both Miniflux's
// schema_version and the fork's fork_schema_version. Order matters: append
// only, never reorder.
var migrations = [...]func(tx *sql.Tx) error{
	func(tx *sql.Tx) error {
		_, err := tx.Exec(`
			CREATE EXTENSION IF NOT EXISTS vector;
			CREATE EXTENSION IF NOT EXISTS pg_search;

			CREATE TABLE search.passages (
				id         bigserial PRIMARY KEY,
				entry_id   bigint NOT NULL,
				ordinal    int NOT NULL,
				text       text NOT NULL,
				char_start int NOT NULL,
				char_end   int NOT NULL,
				embedding  vector(384),
				UNIQUE (entry_id, ordinal)
			);

			CREATE INDEX passages_entry_id_idx ON search.passages (entry_id);

			CREATE TABLE search.entry_index_state (
				entry_id     bigint PRIMARY KEY,
				content_hash text NOT NULL,
				indexed_at   timestamptz,
				status       text NOT NULL,
				reason       text
			);

			CREATE INDEX entry_index_state_status_idx
				ON search.entry_index_state (status);
		`)
		return err
	},
	func(tx *sql.Tx) error {
		// Built separately from the table so a reindex can drop and rebuild
		// them without touching the data.
		_, err := tx.Exec(`
			CREATE INDEX passages_embedding_idx
				ON search.passages USING hnsw (embedding vector_cosine_ops);

			CREATE INDEX passages_bm25_idx
				ON search.passages USING bm25 (id, text)
				WITH (key_field='id');
		`)
		return err
	},
}

var schemaVersion = len(migrations)

// Migrate creates the search schema and applies pending migrations.
func (s *Store) Migrate() error {
	if _, err := s.db.Exec(`CREATE SCHEMA IF NOT EXISTS search`); err != nil {
		return fmt.Errorf("store: unable to create schema: %w", err)
	}
	if _, err := s.db.Exec(
		`CREATE TABLE IF NOT EXISTS search.schema_version (version int not null)`,
	); err != nil {
		return fmt.Errorf("store: unable to create schema_version: %w", err)
	}

	var currentVersion int
	s.db.QueryRow(`SELECT version FROM search.schema_version`).Scan(&currentVersion)

	slog.Info("Running sidecar migrations",
		slog.Int("current_version", currentVersion),
		slog.Int("latest_version", schemaVersion),
	)

	for version := currentVersion; version < schemaVersion; version++ {
		newVersion := version + 1

		tx, err := s.db.Begin()
		if err != nil {
			return fmt.Errorf("[sidecar migration v%d] %w", newVersion, err)
		}

		if err := migrations[version](tx); err != nil {
			tx.Rollback()
			return fmt.Errorf("[sidecar migration v%d] %w", newVersion, err)
		}
		if _, err := tx.Exec(`TRUNCATE search.schema_version`); err != nil {
			tx.Rollback()
			return fmt.Errorf("[sidecar migration v%d] %w", newVersion, err)
		}
		if _, err := tx.Exec(
			`INSERT INTO search.schema_version (version) VALUES ($1)`, newVersion,
		); err != nil {
			tx.Rollback()
			return fmt.Errorf("[sidecar migration v%d] %w", newVersion, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("[sidecar migration v%d] %w", newVersion, err)
		}
	}

	return nil
}
```

- [x] **Step 5: Initialise the module and run the tests**

```bash
cd sidecar
go mod init miniflux.app/v2/sidecar
go get github.com/lib/pq@v1.12.3
SIDECAR_DATABASE_URL="postgres://postgres:postgres@127.0.0.1:5434/miniflux2?sslmode=disable" \
  go test ./internal/store/ -run TestMigrate -v
```
Expected: all three PASS.

Then confirm hermeticity — with the variable unset, the tests must SKIP:

```bash
go test ./internal/store/ -v 2>&1 | grep -c SKIP
```

- [x] **Step 6: Write the Makefile and README**

`sidecar/Makefile` must carry the build tag in every target, because omitting it is silent:

```make
export CGO_ENABLED := 1
GO_TAGS := ORT

dev-db:
	docker compose -f docker-compose.dev.yml up -d

build:
	go build -tags $(GO_TAGS) -o bin/sidecar ./cmd/sidecar

test:
	go test -tags $(GO_TAGS) -race ./...

lint:
	go vet -tags $(GO_TAGS) ./...
	test -z "$$(gofmt -l .)"
```

`sidecar/README.md` documents: the ParadeDB requirement, the two native dependencies from spec §6.7 (ONNX Runtime dylib and `libtokenizers.a`), the mandatory build tag and what silently happens without it, and both test environment variables.

- [x] **Step 7: Commit**

```bash
cd sidecar && make lint
git add sidecar/
git commit -m "feat(sidecar): add module skeleton and the search schema"
```

---

### Task 2: The Embedder, and a guard against the silent-backend footgun

**Files:**
- Create: `sidecar/internal/embed/embed.go` (the interface)
- Create: `sidecar/internal/embed/onnx/onnx.go` (the ORT implementation)
- Create: `sidecar/internal/embed/onnx/onnx_test.go`
- Create: `sidecar/internal/embed/onnx/backend_ort.go`, `sidecar/internal/embed/onnx/backend_noort.go`
- Modify: `sidecar/README.md`

> **As built:** the ONNX implementation landed in `internal/embed/` first and
> was moved to the leaf package `internal/embed/onnx/` by Task 4.5 (inserted
> after Task 4). The paths above are the final ones; see Task 4.5 for why.

**Interfaces:**
- Consumes: nothing
- Produces: `embed.Embedder` interface; `embed.NewONNX(cfg ONNXConfig) (Embedder, error)`; `embed.BackendName() string`

**The footgun this task defends against:** `go build` without `-tags ORT` silently produces the pure-Go GoMLX backend, roughly 10× slower, with no error. A build that is accidentally 10× slow in production is the single most likely way this project quietly fails, so the guard is a test, not a comment.

- [x] **Step 1: Write the build-tag guard**

`sidecar/internal/embed/backend_ort.go`:

```go
// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build ORT

package embed // import "miniflux.app/v2/sidecar/internal/embed"

// BackendName reports which inference backend this binary was compiled with.
func BackendName() string { return "ORT" }
```

`sidecar/internal/embed/backend_noort.go`:

```go
// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !ORT

package embed // import "miniflux.app/v2/sidecar/internal/embed"

// BackendName reports which inference backend this binary was compiled with.
//
// Reaching this file means the ORT build tag was omitted, which silently
// selects the pure-Go GoMLX backend — roughly an order of magnitude slower
// (spec §6.7). TestBackendIsORT fails loudly rather than letting that reach
// production.
func BackendName() string { return "GoMLX" }
```

- [x] **Step 2: Write the failing tests**

`sidecar/internal/embed/onnx_test.go`:

```go
// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package embed // import "miniflux.app/v2/sidecar/internal/embed"

import (
	"context"
	"math"
	"os"
	"testing"
)

// TestBackendIsORT fails when the binary was built without -tags ORT, which
// silently substitutes a far slower backend instead of erroring.
func TestBackendIsORT(t *testing.T) {
	if got := BackendName(); got != "ORT" {
		t.Fatalf("built with the %q backend; rebuild with -tags ORT (see spec §6.7)", got)
	}
}

func testEmbedder(t *testing.T) Embedder {
	t.Helper()

	modelPath := os.Getenv("SIDECAR_MODEL_PATH")
	if modelPath == "" {
		t.Skip("SIDECAR_MODEL_PATH is not set, skipping model test")
	}

	e, err := NewONNX(ONNXConfig{
		ModelPath:      modelPath,
		ONNXLibraryDir: os.Getenv("SIDECAR_ONNX_LIB_DIR"),
	})
	if err != nil {
		t.Fatalf("unable to create embedder: %v", err)
	}
	t.Cleanup(func() { e.Close() })

	return e
}

func cosine(a, b []float32) float64 {
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

func TestEmbedReturns384Dimensions(t *testing.T) {
	e := testEmbedder(t)

	vectors, err := e.Embed(context.Background(), []string{"hello world"})
	if err != nil {
		t.Fatalf("embed failed: %v", err)
	}
	if len(vectors) != 1 {
		t.Fatalf("expected 1 vector, got %d", len(vectors))
	}
	if len(vectors[0]) != 384 {
		t.Fatalf("expected 384 dimensions, got %d", len(vectors[0]))
	}
	if e.Dimensions() != 384 {
		t.Fatalf("expected Dimensions() == 384, got %d", e.Dimensions())
	}
}

// TestEmbedSeparatesParaphraseFromUnrelated is the sanity check from spec §6.7.
// It catches a wrong model, wrong pooling, or missing normalisation — all of
// which yield vectors that look fine but rank meaninglessly.
func TestEmbedSeparatesParaphraseFromUnrelated(t *testing.T) {
	e := testEmbedder(t)

	vectors, err := e.Embed(context.Background(), []string{
		"The cat sat quietly on the warm windowsill in the afternoon sun.",
		"A cat was resting peacefully on the sunny windowsill that afternoon.",
		"The stock market fell sharply after the central bank raised interest rates.",
	})
	if err != nil {
		t.Fatalf("embed failed: %v", err)
	}

	paraphrase := cosine(vectors[0], vectors[1])
	unrelated := cosine(vectors[0], vectors[2])

	// The spike measured 0.87 against 0.31. A margin of 0.2 catches a broken
	// pipeline without being brittle about model revisions.
	if paraphrase <= unrelated+0.2 {
		t.Fatalf("paraphrase %.4f should clearly exceed unrelated %.4f", paraphrase, unrelated)
	}
}

func TestEmbedRejectsEmptyBatch(t *testing.T) {
	e := testEmbedder(t)

	vectors, err := e.Embed(context.Background(), nil)
	if err != nil {
		t.Fatalf("an empty batch should be a no-op, got error: %v", err)
	}
	if len(vectors) != 0 {
		t.Fatalf("expected no vectors, got %d", len(vectors))
	}
}
```

- [x] **Step 3: Run to verify failure**

```bash
cd sidecar && go test -tags ORT ./internal/embed/ -v
```
Expected: FAIL — `undefined: Embedder`, `undefined: NewONNX`.

Also confirm the guard works:
```bash
go test ./internal/embed/ -run TestBackendIsORT -v
```
Expected: FAIL with "built with the \"GoMLX\" backend" — proving the guard bites.

- [x] **Step 4: Write the interface**

`sidecar/internal/embed/embed.go`:

```go
// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package embed // import "miniflux.app/v2/sidecar/internal/embed"

import "context"

// Embedder turns text into dense vectors. The ONNX implementation is the only
// one today; the interface exists so a local inference service or a pure-Go
// backend can be substituted without touching the pipeline (spec §6.6).
type Embedder interface {
	// Embed returns one vector per input text, each of Dimensions() length.
	// An empty input returns no vectors and no error.
	Embed(ctx context.Context, texts []string) ([][]float32, error)

	// Dimensions is the fixed width of every vector this Embedder returns.
	Dimensions() int

	// Close releases the underlying session.
	Close() error
}
```

- [x] **Step 5: Implement the ONNX embedder**

`sidecar/internal/embed/onnx/onnx.go` wraps hugot. Consult the spike's working code at
`/private/tmp/claude-501/-Users-nico-Dev-miniflux-v2/15940a9c-8c0f-45a5-b91c-1f83d1d2a02f/scratchpad/spike-embedding/`
for the exact call sequence that was measured working — **read it before writing this file.** It is throwaway code, so copy the approach, not the structure.

Requirements the spike established:

- Build the session with `options.WithOnnxLibraryPath(cfg.ONNXLibraryDir)` when that directory is non-empty. hugot defaults to `/usr/lib`, a Linux path, so macOS fails without it.
- Use a feature-extraction pipeline with `pipelines.WithNormalization()`. Without L2 normalisation the cosine comparisons in the tests are meaningless.
- **Do not** set `WithIntraOpNumThreads` or `WithInterOpNumThreads`. Leaving ORT threading unconstrained measured 33.9 passages/sec against 11.7 when pinned — see spec §6.7. If you believe pinning is correct, measure it before changing it.
- One shared session, safe for concurrent `Embed` calls from multiple goroutines. The controller in Task 6 relies on this.
- `Close()` must release the session.

`ONNXConfig` carries `ModelPath` and `ONNXLibraryDir` and nothing else for now.

- [x] **Step 6: Fetch the native dependencies and the model**

Document each in the README as you go — these are the steps most likely to trip up a future deployment:

```bash
brew install onnxruntime                      # native ORT v1.30.0
# libtokenizers.a, matched to daulet/tokenizers v1.27.0, from its GitHub releases
# model: Xenova/bge-small-en-v1.5, onnx/model_quantized.onnx (~32MB)
```

- [x] **Step 7: Run the tests**

```bash
cd sidecar
CGO_LDFLAGS="-L<dir-with-libtokenizers.a>" \
DYLD_LIBRARY_PATH=/opt/homebrew/opt/onnxruntime/lib \
SIDECAR_MODEL_PATH=<path-to-model_quantized.onnx> \
SIDECAR_ONNX_LIB_DIR=/opt/homebrew/opt/onnxruntime/lib \
  go test -tags ORT ./internal/embed/ -v
```
Expected: all PASS, with the paraphrase margin comfortably above 0.2.

- [x] **Step 8: Commit**

```bash
cd sidecar && make lint
git add sidecar/
git commit -m "feat(sidecar): add the ONNX embedder and a backend guard"
```

---

### Task 3: Plaintext extraction and passage splitting

**Files:**
- Create: `sidecar/internal/passage/extract.go`, `sidecar/internal/passage/extract_test.go`
- Create: `sidecar/internal/passage/split.go`, `sidecar/internal/passage/split_test.go`

**Interfaces:**
- Consumes: nothing
- Produces: `passage.ExtractText(html string) string`; `passage.Split(text string, opts SplitOptions) []Passage`; `passage.Passage{Text string; CharStart, CharEnd int}`

**Why offsets matter:** P1b highlights matched passages inside the article. If `CharStart`/`CharEnd` do not index exactly into the string `ExtractText` returned, highlighting silently lands on the wrong words. Round-tripping is therefore a correctness test, not a nicety.

- [x] **Step 1: Write the failing extraction tests**

`sidecar/internal/passage/extract_test.go`:

```go
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
```

- [x] **Step 2: Write the failing splitting tests**

`sidecar/internal/passage/split_test.go`:

```go
// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package passage // import "miniflux.app/v2/sidecar/internal/passage"

import (
	"strings"
	"testing"
)

func TestSplitOffsetsRoundTripExactly(t *testing.T) {
	text := strings.TrimSpace(strings.Repeat("This is a sentence about search. ", 200))

	for _, p := range Split(text, DefaultSplitOptions()) {
		if p.CharStart < 0 || p.CharEnd > len(text) || p.CharStart >= p.CharEnd {
			t.Fatalf("offsets out of range: %d..%d (len %d)", p.CharStart, p.CharEnd, len(text))
		}
		if got := text[p.CharStart:p.CharEnd]; got != p.Text {
			t.Fatalf("offset mismatch:\n slice: %q\n  text: %q", got, p.Text)
		}
	}
}

func TestSplitCoversTheWholeText(t *testing.T) {
	text := strings.TrimSpace(strings.Repeat("Sentences make up a document. ", 200))

	passages := Split(text, DefaultSplitOptions())
	if len(passages) < 2 {
		t.Fatalf("expected several passages, got %d", len(passages))
	}
	if passages[0].CharStart != 0 {
		t.Fatalf("first passage should start at 0, got %d", passages[0].CharStart)
	}
	if last := passages[len(passages)-1]; last.CharEnd != len(text) {
		t.Fatalf("last passage should end at %d, got %d", len(text), last.CharEnd)
	}
}

func TestSplitConsecutivePassagesOverlap(t *testing.T) {
	text := strings.TrimSpace(strings.Repeat("Overlap keeps context across boundaries. ", 200))

	passages := Split(text, DefaultSplitOptions())
	if len(passages) < 2 {
		t.Fatalf("expected several passages, got %d", len(passages))
	}
	for i := 1; i < len(passages); i++ {
		if passages[i].CharStart >= passages[i-1].CharEnd {
			t.Fatalf("passage %d does not overlap its predecessor", i)
		}
	}
}

func TestSplitShortTextYieldsOnePassage(t *testing.T) {
	text := "A single short sentence."

	passages := Split(text, DefaultSplitOptions())
	if len(passages) != 1 {
		t.Fatalf("expected 1 passage, got %d", len(passages))
	}
	if passages[0].Text != text {
		t.Fatalf("got %q", passages[0].Text)
	}
}

func TestSplitEmptyTextYieldsNothing(t *testing.T) {
	if got := Split("", DefaultSplitOptions()); len(got) != 0 {
		t.Fatalf("expected no passages, got %d", len(got))
	}
	if got := Split("   \n  ", DefaultSplitOptions()); len(got) != 0 {
		t.Fatalf("whitespace-only text should yield no passages, got %d", len(got))
	}
}

func TestSplitBreaksOnSentenceBoundaries(t *testing.T) {
	text := strings.TrimSpace(strings.Repeat("Alpha beta gamma delta. ", 300))

	for _, p := range Split(text, DefaultSplitOptions()) {
		trimmed := strings.TrimSpace(p.Text)
		if trimmed == "" {
			t.Fatal("passage is blank")
		}
		// Every passage but the last should end at a sentence terminator.
		if p.CharEnd < len(text) && !strings.HasSuffix(trimmed, ".") {
			t.Fatalf("passage does not end on a sentence boundary: %q", trimmed)
		}
	}
}
```

- [x] **Step 3: Run to verify failure**

```bash
cd sidecar && go test -tags ORT ./internal/passage/ -v
```
Expected: FAIL — undefined symbols.

- [x] **Step 4: Implement extraction**

`sidecar/internal/passage/extract.go` walks the HTML with `golang.org/x/net/html`, skipping `script`, `style`, `noscript` and comment nodes, emitting text nodes, and inserting a single space between block-level elements. Then collapse all runs of whitespace to one space and trim.

Add the dependency:
```bash
cd sidecar && go get golang.org/x/net@v0.58.0
```

Do not pull in a Markdown or readability library — the input is already sanitised HTML from Miniflux, and the job here is text extraction, not article detection.

- [x] **Step 5: Implement splitting**

`sidecar/internal/passage/split.go`:

```go
// SplitOptions controls passage granularity. Sizes are in approximate tokens,
// estimated as words — close enough for chunking, and far cheaper than running
// the real tokenizer over the whole corpus twice.
type SplitOptions struct {
	TargetTokens int // aim for this many tokens per passage
	MaxTokens    int // never exceed this
	OverlapTokens int // repeat this much of the previous passage
}

func DefaultSplitOptions() SplitOptions {
	return SplitOptions{TargetTokens: 320, MaxTokens: 512, OverlapTokens: 64}
}
```

Algorithm: find sentence boundaries (`.`, `!`, `?` followed by whitespace, ignoring common abbreviations is *not* required — keep it simple), accumulate sentences until `TargetTokens` is reached, emit a passage, then step back `OverlapTokens` worth of sentences for the next one. A single sentence longer than `MaxTokens` is emitted alone rather than split mid-sentence.

**Offsets are the point:** track byte offsets into the original string throughout and never rebuild passage text by concatenation — slice it from the source, so `text[p.CharStart:p.CharEnd] == p.Text` holds by construction.

- [x] **Step 6: Run the tests**

```bash
cd sidecar && go test -tags ORT ./internal/passage/ -v
```
Expected: all PASS.

- [x] **Step 7: Commit**

```bash
cd sidecar && make lint
git add sidecar/
git commit -m "feat(sidecar): extract plaintext and split it into passages"
```

---

### Task 4: Index one entry end to end

**Files:**
- Create: `sidecar/internal/indexer/indexer.go`, `sidecar/internal/indexer/indexer_test.go`
- Create: `sidecar/internal/store/entries.go`, `sidecar/internal/store/entries_test.go`
- Create: `sidecar/internal/store/passages.go`, `sidecar/internal/store/passages_test.go`

**Interfaces:**
- Consumes: `store.Store` (Task 1), `embed.Embedder` (Task 2), `passage.ExtractText`/`Split` (Task 3)
- Produces: `store.PassageRow{Ordinal int; Text string; CharStart, CharEnd int; Embedding []float32}` — the
  store's own row type, so package `store` does not import package `passage`; the indexer converts between them
- Produces:
  - `(*store.Store).PendingEntryIDs(afterID int64, limit int) ([]int64, error)`
  - `(*store.Store).EntryForIndexing(entryID int64) (*store.Entry, error)`
  - `(*store.Store).ReplacePassages(entryID int64, contentHash string, passages []store.PassageRow) error`
  - `(*store.Store).MarkEntrySkipped(entryID int64, contentHash, reason string) error`
  - `indexer.New(s *store.Store, e embed.Embedder) *Indexer`; `(*Indexer).IndexEntry(ctx, entryID int64) error`

**Correctness requirements from spec §10:**

| Case | Required behaviour |
|---|---|
| Entry has no usable text | `status = 'skipped'` with a reason; not retried |
| Embedding fails | `status = 'failed'` with a reason; retried later with backoff |
| Entry content changed | `content_hash` mismatch triggers a full re-index |
| Re-indexing | Replaces passages atomically — never leaves a half-indexed entry visible |

- [x] **Step 1: Write the failing tests**

`sidecar/internal/indexer/indexer_test.go` must cover, each as its own test:

1. **A plain entry indexes**: passages are written, each with a non-null 384-dim embedding, and `entry_index_state.status = 'ok'`.
2. **An entry with no usable text is skipped**: no passages written, `status = 'skipped'`, `reason` non-empty, and a second `IndexEntry` call does not re-embed it (assert via a counting fake Embedder).
3. **Re-indexing is atomic**: index an entry, change its content, re-index, and assert the passage set matches the *new* content exactly — no orphans from the first pass.
4. **An unchanged entry is not re-embedded**: same `content_hash` means the fake Embedder sees zero calls on the second run.
5. **Embedding failure marks the entry failed and retryable**: with an Embedder that returns an error, assert `status = 'failed'`, `reason` non-empty, and that the entry still appears in the pending set.

Use a **fake `Embedder`** here — this task tests pipeline wiring, not inference. The real model is covered by Task 2. A fake that returns deterministic vectors and counts calls makes assertions 2, 4 and 5 possible at all.

```go
type fakeEmbedder struct {
	calls int
	err   error
}

func (f *fakeEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	out := make([][]float32, len(texts))
	for i := range texts {
		v := make([]float32, 384)
		v[0] = float32(len(texts[i])) // deterministic, and distinguishes passages
		out[i] = v
	}
	return out, nil
}

func (f *fakeEmbedder) Dimensions() int { return 384 }
func (f *fakeEmbedder) Close() error    { return nil }
```

Fixtures insert into `public.entries` with raw SQL, mirroring the P0 fork's test fixtures, and clean up with `t.Cleanup`.

- [x] **Step 2: Run to verify failure**

```bash
cd sidecar && go test -tags ORT ./internal/indexer/ -v
```
Expected: FAIL — undefined symbols.

- [x] **Step 3: Implement the store queries**

`PendingEntryIDs` selects entries needing indexing — either absent from `entry_index_state`, or present with a `content_hash` that no longer matches the entry's current content, or `status = 'failed'`. Ascending id, so the caller can checkpoint.

`EntryForIndexing` returns id, content and the computed content hash for one entry.

`ReplacePassages` does the whole write in **one transaction**: delete existing passages for the entry, insert the new ones, upsert `entry_index_state` with `status='ok'` and the new hash. Atomicity is what keeps a crash from leaving an entry half-indexed and silently under-searchable.

Use `pgvector`'s text format for the embedding parameter — `[0.1,0.2,...]` — or add `github.com/pgvector/pgvector-go`. Prefer the plain text format to avoid a dependency for one type.

- [x] **Step 4: Implement the indexer**

`IndexEntry` sequence: load entry → compute content hash → if unchanged and `status='ok'`, return without work → `ExtractText` → if empty, `MarkEntrySkipped` and return → `Split` → `Embed` in batches of `DefaultBatchSize = 16` (inside the measured 8–32 sweet spot, spec §6.7) → `ReplacePassages`.

On embedding error, mark `status='failed'` with the reason and return the error. Never mark `ok` on a partial result.

- [x] **Step 5: Run the tests**

```bash
cd sidecar
SIDECAR_DATABASE_URL="postgres://postgres:postgres@127.0.0.1:5434/miniflux2?sslmode=disable" \
  go test -tags ORT ./internal/indexer/ ./internal/store/ -v
```
Expected: all PASS.

- [x] **Step 6: Commit**

```bash
cd sidecar && make lint
git add sidecar/
git commit -m "feat(sidecar): index a single entry into passages and vectors"
```

---

### Task 4.5: Split the `Embedder` interface from its ONNX implementation

**Inserted after Task 4**, in response to a task-4 review finding. Not in the
original plan; recorded here because the plan is this phase's record.

**Files:**
- Create: `sidecar/internal/embed/onnx/` — move `onnx.go`, `onnx_test.go`,
  `backend_ort.go`, `backend_noort.go`, `resolve_model_root_test.go` there
- Keep: `sidecar/internal/embed/embed.go` (the interface, untouched)

**Why:** `internal/indexer` imports `internal/embed` only for the
`Embedder` interface, but the ONNX implementation sat in that same package,
so every importer transitively pulled in hugot and CGO. The consequence was
concrete and was reproduced before the change: `go test -tags ORT
./internal/indexer/` failed at the **link** step with
`ld: library 'tokenizers' not found` on any machine without
`libtokenizers.a`, even though nothing in `internal/indexer` needs
inference.

Moving the implementation to a leaf package makes the native dependency
reachable only from packages that actually want it. `cmd/sidecar` is the
only importer of `internal/embed/onnx` in the module, and that is now an
invariant worth keeping — it is what lets the rest of the suite run without
native artifacts.

- [x] **Step 1: Move the files, fix the package clauses and import comments**
- [x] **Step 2: Prove the before/after** — the same `go test -tags ORT` command
      failing to link before the move and passing after it
- [x] **Step 3: Commit**

---

### Task 5: The live lane

**Files:**
- Create: `sidecar/internal/indexer/live.go`, `sidecar/internal/indexer/live_test.go`
- Create: `sidecar/cmd/sidecar/main.go`

**Interfaces:**
- Consumes: `indexer.Indexer` (Task 4)
- Produces: `indexer.RunLive(ctx context.Context, ix *Indexer, interval time.Duration) error`; the `sidecar` binary

**Design (spec §9.1):** the live lane handles new entries — a few hundred a day, always on, one worker, effectively free. It is deliberately separate from the backfill lane so that pausing a multi-hour backfill never stops new articles becoming searchable.

- [x] **Step 1: Write the failing test**

Cover: that a newly inserted entry becomes indexed within a couple of poll intervals; that the lane survives a single entry failing (it logs, marks failed, and carries on to the next rather than aborting the loop); and that it exits cleanly on context cancellation.

Use a short interval (50ms) so the test finishes quickly.

- [x] **Step 2: Run to verify failure**

```bash
cd sidecar && go test -tags ORT ./internal/indexer/ -run TestRunLive -v
```

- [x] **Step 3: Implement the live lane**

A ticker loop: fetch a page of pending ids with a modest limit (say 50), index each, log a summary, sleep, repeat. Poll on `WHERE id > lastSeen` semantics as spec §4 prescribes — at a few hundred entries a day, `LISTEN/NOTIFY` is complexity without a payoff.

Per-entry failures are logged and skipped, never retried in a tight loop within one pass.

Respect `ctx.Done()` between every entry, not merely between polls, so shutdown during a long batch is prompt.

- [x] **Step 4: Write the binary**

`sidecar/cmd/sidecar/main.go`: parse config from environment variables (`SIDECAR_DATABASE_URL`, `SIDECAR_MODEL_PATH`, `SIDECAR_ONNX_LIB_DIR`), run migrations, construct the embedder, start the live lane, and handle SIGINT/SIGTERM with graceful shutdown.

Log `embed.BackendName()` at startup. In production, a line saying `GoMLX` is the only visible symptom of the silent build-tag mistake.

- [x] **Step 5: Run the tests and the binary**

```bash
cd sidecar && go test -tags ORT ./internal/indexer/ -v
make build && ./bin/sidecar --help
```

- [x] **Step 6: Commit**

```bash
cd sidecar && make lint
git add sidecar/
git commit -m "feat(sidecar): add the live indexing lane and the binary"
```

---

### Task 6: The backfill lane and its adaptive controller

**Files:**
- Create: `sidecar/internal/indexer/backfill.go`, `sidecar/internal/indexer/backfill_test.go`
- Create: `sidecar/internal/indexer/throttle.go`, `sidecar/internal/indexer/throttle_test.go`

**Interfaces:**
- Consumes: `indexer.Indexer` (Task 4)
- Produces: `indexer.Backfill` with `Start`, `Pause`, `Resume`, `Stats`; `indexer.Controller` with `Workers() int` and `Observe(batchLatency time.Duration)`

**The measured constraint that shapes this task (spec §6.7):** throughput comes from *multiple goroutines sharing one unconstrained ORT session*. Pinning threads per worker measured 11.7/sec against 33.9. So the controller's only lever is **how many goroutines are in flight**, never ORT's thread counts.

**Budget:** ~4.1 hours per 0.5M passages, ~41 hours per 5M. The lane must survive being interrupted at hour 30.

- [x] **Step 1: Write the failing controller tests**

`throttle_test.go` — pure unit tests, no database, no model:

1. Starts at the configured minimum worker count.
2. Increases workers, up to the ceiling, while observed latency stays near baseline.
3. Decreases workers when observed latency degrades past the threshold.
4. **Never exceeds `MaxWorkers` regardless of observations** — the hard ceiling always wins over the controller.
5. Never drops below `MinWorkers` (at least 1, so progress never stops entirely).
6. Is outside its window ⇒ reports zero workers.

Inject the load reading and the clock rather than sampling the real machine — a controller test that depends on the host's actual load average is not a test.

- [x] **Step 2: Write the failing backfill tests**

1. Processes every pending entry and terminates.
2. **Resumes from its checkpoint**: interrupt after N entries, restart, and assert it neither reprocesses the first N nor skips any.
3. Pause stops work; resume continues from where it stopped.
4. Respects the schedule window: outside it, no work happens.
5. `Stats()` reports progress, throughput, current worker count, and error/skip counts by cause — the admin page in Task 7 renders exactly this.

- [x] **Step 3: Run both to verify failure**

```bash
cd sidecar && go test -tags ORT ./internal/indexer/ -run 'TestController|TestBackfill' -v
```

- [x] **Step 4: Implement the controller**

```go
type ControllerConfig struct {
	MinWorkers     int           // never fewer; at least 1
	MaxWorkers     int           // hard ceiling; the controller never exceeds it
	Window         Window        // wall-clock hours the lane may run; the zero
	                             // value means "always", never "never".
	LatencyMargin  float64       // degradation factor that triggers backing off
	LoadThreshold  float64       // 1-minute load average above which to back off
}

func DefaultControllerConfig() ControllerConfig {
	// MaxWorkers 2 matches the measured optimum (spec §6.7); higher is
	// permitted but was not faster on the spike machine.
	return ControllerConfig{MinWorkers: 1, MaxWorkers: 2, LatencyMargin: 1.5, LoadThreshold: 0.8}
}
```

Sample load and per-batch latency every few seconds, establish a baseline from the first healthy batches, and move the worker count one step at a time within `[MinWorkers, MaxWorkers]`. One step at a time prevents oscillation.

Record *why* the current count was chosen — a short string — because Task 7 displays it and "why is it at 1?" is the first question an operator asks.

- [x] **Step 5: Implement the backfill lane**

A worker pool sized by the controller, pulling entry ids from a channel fed by `PendingEntryIDs` pagination. Checkpointing is implicit: `entry_index_state` already records what is done, so a restart naturally resumes. Do not add a separate checkpoint table.

Rechecking the worker count between batches (not mid-batch) keeps the adjustment simple and predictable.

- [x] **Step 6: Run the tests**

```bash
cd sidecar
SIDECAR_DATABASE_URL="postgres://postgres:postgres@127.0.0.1:5434/miniflux2?sslmode=disable" \
  go test -tags ORT ./internal/indexer/ -v
```

- [x] **Step 7: Commit**

```bash
cd sidecar && make lint
git add sidecar/
git commit -m "feat(sidecar): add the throttled backfill lane"
```

---

### Task 7: Status and admin page

**Files:**
- Create: `sidecar/internal/web/server.go`, `sidecar/internal/web/server_test.go`
- Create: `sidecar/internal/web/templates/status.html`
- Modify: `sidecar/cmd/sidecar/main.go`

**Interfaces:**
- Consumes: `indexer.Backfill.Stats()` (Task 6)
- Produces: an HTTP server exposing `GET /` (status page), `GET /api/status` (JSON), `POST /api/backfill/pause`, `POST /api/backfill/resume`

**Why it lives here, not in Miniflux (spec §9.4):** keeping it in the sidecar holds the fork's diff to reader-facing UI only, and indexing operations are a different concern from reading preferences even when the same person handles both.

- [x] **Step 1: Write the failing tests**

Using `httptest`: `/api/status` returns JSON with progress, throughput, worker count and the controller's reason; pause then status shows paused; resume then status shows running; the HTML page renders without error and contains the progress figure.

- [x] **Step 2: Run to verify failure**

```bash
cd sidecar && go test -tags ORT ./internal/web/ -v
```

- [x] **Step 3: Implement the server**

Plain `net/http` with `html/template`, matching Miniflux's own no-framework style. The page shows: entries indexed against total with an ETA derived from current throughput, current throughput, live worker count with the controller's stated reason, error and skip counts grouped by cause, and pause/resume controls.

No JavaScript framework. A meta refresh or a small inline fetch is sufficient.

Bind to localhost by default — this page has no authentication and exposes operational control.

- [x] **Step 4: Wire it into the binary and run it**

```bash
cd sidecar && make build && ./bin/sidecar
# then open http://127.0.0.1:8081/
```
Confirm the page renders and that pause and resume visibly change the reported state.

- [x] **Step 5: Commit**

```bash
cd sidecar && make lint
git add sidecar/
git commit -m "feat(sidecar): add the indexing status and admin page"
```

---

### Task 8: Production migration runbook

**Files:**
- Create: `sidecar/docs/production-migration.md`
- Create: `sidecar/scripts/verify-extensions.sh`

**Interfaces:**
- Consumes: everything above
- Produces: a rehearsed procedure for moving a live instance onto ParadeDB

**Why this is a task and not a footnote:** stock Postgres carries neither extension, and ParadeDB ships **PostgreSQL 18** while the existing instance runs 17. So P1 requires a major-version migration of live data with a dump and restore — scheduled downtime, and the one step in this plan that can lose data if botched.

- [x] **Step 1: Write the verification script**

`sidecar/scripts/verify-extensions.sh` takes a DSN and exits non-zero unless both `vector` and `pg_search` are available, printing their versions. It is the precondition check the runbook opens with and the smoke test it closes with.

- [x] **Step 2: Write the runbook**

`sidecar/docs/production-migration.md` covers, in order:

1. **Before you start** — record `SELECT count(*) FROM entries`, `SELECT min(published_at) FROM entries`, and `SELECT count(*) FROM entry_tombstones`. These are the numbers you compare against afterwards, and the tombstone count is how much history was already lost before P0 landed.
2. **Back up** — `pg_dump -Fc`, verified restorable into a scratch database *before* touching the original. An unverified backup is not a backup.
3. **Stop Miniflux** — the daemon must not be writing during the dump.
4. **Dump, start ParadeDB, restore** — with the exact commands, noting that a 17→18 restore may emit warnings that are safe and naming which ones.
5. **Verify** — row counts match the numbers from step 1, `verify-extensions.sh` passes, Miniflux starts and serves the UI, and a feed refresh works.
6. **Run the sidecar migrations** and confirm the `search` schema exists.
7. **Rollback** — how to return to the Postgres 17 volume if verification fails. Write this section as though you will need it at 2am.

- [x] **Step 3: Rehearse it**

Rehearse against a **copy**, never the live database: dump the existing dev Miniflux database (port 5432), restore it into the ParadeDB dev container (5434), and confirm Miniflux runs against the restored copy. Record in the runbook anything that differed from what you wrote.

A runbook that has never been executed is a guess. This step is what makes it a procedure.

- [x] **Step 4: Commit**

```bash
git add sidecar/docs sidecar/scripts
git commit -m "docs(sidecar): add the production migration runbook"
```

---

## Done criteria

- [x] `cd sidecar && make test` passes; database and model tests SKIP when their environment variables are unset
- [x] `TestBackendIsORT` fails when built without `-tags ORT` — verify by running it once without the tag
- [x] A real entry indexes end to end: passages present with 384-dim embeddings, `status='ok'`
- [x] The backfill lane resumes correctly after interruption
- [x] The controller never exceeds its worker ceiling
- [x] The status page renders and pause/resume work
- [x] The migration runbook has been rehearsed against a copy
- [x] No file outside `sidecar/` was modified by this plan — except this
      plan document itself, updated after the whole-branch review to match
      what was built (see below)

## As built: where the branch diverged from this plan

Recorded after the whole-branch review, because the plan is this phase's
record and two of that review's findings were the plan's own fault rather
than any implementer's.

- **Task 4.5 was inserted** after Task 4 (see above).
- **§9.2's three knobs were built but never wired.** Task 6's brief said
  build the setters and Task 7's said build the admin surface; neither said
  connect them, so `Controller.SetConfig`, `Controller.SetWindow`,
  `Indexer.SetBatchSize`, `Backfill.SetPageSize` and
  `Backfill.SetPollInterval` shipped with no non-test caller at all. The
  fix wave added `SIDECAR_BACKFILL_*` startup variables and
  `POST /api/backfill/config`, both going through one validator
  (`indexer.ApplyConfig`).
- **`LoadThreshold: 0.8` was written as a bare literal** with no statement
  of whether it meant an absolute or a per-core load average. Only per-core
  is meaningful as a default; as built it was absolute, which pinned the
  lane at one worker for an entire run. The sampler now normalises by
  `runtime.NumCPU()`. A future plan naming a threshold should name its
  units.
- **The backfill lane returned when it drained**, which the plan never
  said it should not. Nothing else re-examines entries below the live
  lane's cursor, so a content change there — exactly what P0's
  scrape-backfill CLI produces — went unnoticed until a restart. The lane
  now idles and re-sweeps.
- **A pipeline version was added** to the content hash
  (`store.PipelineVersion`). The plan stored `char_start`/`char_end`
  offsets into a plaintext it never persisted, with nothing to invalidate
  them when the derivation changed.

## Deferred to P1b

BM25 and vector retrieval, RRF fusion, the search HTTP API, the passage-mode presentation, similar articles (P2), and the fork's search-bar and entry-page integration. This plan produces the index those query; it deliberately ships no search.

**Before starting P1b**, build the evaluation set spec §11 calls for — query → expected-entry pairs scored as recall@10. Without it there is no way to tell whether a change to chunking, the model, or fusion weights helped or hurt, and every later tuning decision is guesswork.
