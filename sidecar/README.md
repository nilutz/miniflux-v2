# Miniflux search sidecar

A separate Go service that reads Miniflux's entries and maintains a hybrid
search index — BM25 plus vector embeddings — in the same PostgreSQL database,
under its own `search` schema. It is a standalone Go module (`miniflux.app/v2/sidecar`)
and does not depend on, and is not depended on by, the root Miniflux module.

## Database requirement: ParadeDB

Stock PostgreSQL ships neither extension this design needs — a plain
`postgres:17-alpine` was checked and offers only `pg_trgm`. The sidecar
requires [ParadeDB](https://github.com/paradedb/paradedb), which bundles:

- `vector` (pgvector) — for the embedding column and HNSW similarity search.
- `pg_search` — for BM25 full-text ranking.

`docker-compose.dev.yml` runs `paradedb/paradedb:latest` on **port 5434**
(deliberately not 5432 or 5433, which are used by other databases on this
machine) so it never collides with an existing Miniflux dev database. Start
it with:

```sh
make dev-db
```

or directly:

```sh
docker compose -f docker-compose.dev.yml up -d
```

Note: PostgreSQL 18+ images expect a single volume mounted at
`/var/lib/postgresql` (not `/var/lib/postgresql/data`) so that
version-specific subdirectories work correctly; the compose file here
reflects that.

Migrations (`internal/store/migrations.go`) create the `search` schema and
run `CREATE EXTENSION IF NOT EXISTS vector` / `pg_search`. Both extensions
install unqualified, which lands them in whatever schema the connection's
`search_path` names first — on ParadeDB that default is `public, paradedb`,
so the unqualified `vector` type used inside `search.passages` resolves via
`public.vector` without any extra qualification. If a future environment's
default `search_path` ever excludes `public`, the fix is to either add
`public` to the search path for the migration session or schema-qualify the
column as `public.vector(384)` — not to restructure the `search` schema.

## Native dependencies (added in a later task)

Once the embedding backend is wired in (spec §6.7), building and running the
sidecar also requires two native dependencies that are **not** Go modules:

- The **ONNX Runtime** shared library (`libonnxruntime.dylib` /
  `.so`), used to run the embedding model.
- **`libtokenizers.a`**, a static library used to tokenize text before it
  reaches the model.

Both must be present on the machine building or running the sidecar; neither
is vendored into this repository.

## The `ORT` build tag

Every Go target in the `Makefile` passes `-tags ORT`. This is required from
this task onward even though nothing here yet uses it, so the habit is
already in place before the ONNX backend lands: the tokenizer/embedding
library this sidecar will use falls back to a pure-Go implementation when
built *without* `ORT`, and that fallback is roughly **10x slower** with no
build error or runtime warning to say so. Always build and test through
`make build` / `make test` (or pass `-tags ORT` yourself) rather than plain
`go build` / `go test`.

## Environment variables used by tests

Tests that need a real database or model are skipped, not failed, when the
corresponding variable is unset — `go test ./...` stays hermetic without any
external services.

- `SIDECAR_DATABASE_URL` — a Postgres DSN (e.g.
  `postgres://postgres:postgres@127.0.0.1:5434/miniflux2?sslmode=disable`)
  pointing at a running ParadeDB instance. Used by `internal/store` tests
  that exercise real migrations. Unset: those tests `t.Skip`.
- `SIDECAR_MODEL_PATH` — filesystem path to the quantized embedding model
  (`model_quantized.onnx`), used by the embedder tests added in a later
  task. Unset: those tests `t.Skip`.

## Running the tests

```sh
make dev-db
SIDECAR_DATABASE_URL="postgres://postgres:postgres@127.0.0.1:5434/miniflux2?sslmode=disable" \
  make test
```

Without `SIDECAR_DATABASE_URL` set, `make test` still passes — the
database-backed tests just skip.
