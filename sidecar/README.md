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

## The embedder (`internal/embed`, `internal/embed/onnx`)

`internal/embed` holds the small, pure-Go `Embedder` interface
(`Embed`/`Dimensions`/`Close`) with no CGO and no native dependencies.
`internal/embed/onnx` implements it: it wraps
[hugot](https://github.com/knights-analytics/hugot) (v0.7.8, build tag
`ORT`) around [ONNX Runtime](https://onnxruntime.ai/) (native v1.30.0) to
embed text with a pinned model,
[`Xenova/bge-small-en-v1.5`](https://huggingface.co/Xenova/bge-small-en-v1.5)
(int8-quantized ONNX export, 384 dimensions). This is the exact stack a spike
measured end to end — see `NewONNX` in `internal/embed/onnx/onnx.go` for the
call sequence and the reasoning behind it. The split keeps consumers that
only need the interface (such as `internal/indexer`'s tests) from linking
the native `libtokenizers.a`.

Building or running the sidecar with this package requires three native
dependencies that are **not** Go modules and are **not** vendored into this
repository:

1. **ONNX Runtime**, the native shared library that actually runs the model.
   On macOS: `brew install onnxruntime` (pins v1.30.0), which installs
   `libonnxruntime.dylib` under `/opt/homebrew/opt/onnxruntime/lib`. hugot
   defaults its library search path to `/usr/lib` (a Linux path), so you must
   also pass that directory explicitly — see `ONNXConfig.ONNXLibraryDir`
   below — and set `DYLD_LIBRARY_PATH` at runtime so the dynamic linker can
   find it. On Linux, download the matching `libonnxruntime.so` from the
   [ONNX Runtime releases](https://github.com/microsoft/onnxruntime/releases)
   instead.

2. **`libtokenizers.a`**, a static library (prebuilt Rust) that
   [`daulet/tokenizers`](https://github.com/daulet/tokenizers) v1.27.0 needs
   to tokenize text before it reaches the model. `go get` does not fetch
   this — download the release archive matching that exact module version
   (`libtokenizers.darwin-arm64.tar.gz` on macOS/arm64, with Linux
   equivalents on the same releases page), extract `libtokenizers.a`
   somewhere, and point `CGO_LDFLAGS` at its directory:
   `CGO_LDFLAGS="-L<dir-with-libtokenizers.a>"`.

3. **The model itself**, `onnx/model_quantized.onnx` from
   `Xenova/bge-small-en-v1.5` on Hugging Face (~32MB, int8-quantized — do
   *not* use the ~127MB fp32 `model.onnx`, which the spike measured at
   roughly half the throughput for no accuracy benefit evaluated here),
   alongside the rest of that repo's files (`tokenizer.json`, `vocab.txt`,
   `config.json`, `tokenizer_config.json`, `special_tokens_map.json`), which
   hugot expects to find next to (one directory up from) the `.onnx` file.

`ONNXConfig` (in `internal/embed/onnx/onnx.go`) takes two fields:

- `ModelPath` — the path to `model_quantized.onnx` itself; the surrounding
  model directory is located automatically by walking up from it looking for
  `tokenizer.json`.
- `ONNXLibraryDir` — the directory containing `libonnxruntime.dylib`/`.so`
  (e.g. `/opt/homebrew/opt/onnxruntime/lib` on macOS via Homebrew). Required
  on macOS; hugot's own default (`/usr/lib`) only exists on Linux.

### Full build/test invocation (macOS example)

```sh
CGO_LDFLAGS="-L<dir-with-libtokenizers.a>" \
DYLD_LIBRARY_PATH=/opt/homebrew/opt/onnxruntime/lib \
SIDECAR_MODEL_PATH=<path-to-model_quantized.onnx> \
SIDECAR_ONNX_LIB_DIR=/opt/homebrew/opt/onnxruntime/lib \
  make test
```

`CGO_LDFLAGS` and `DYLD_LIBRARY_PATH` are needed to build and link at all
once `internal/embed/onnx` is in the build (with `-tags ORT`, which `make`
already passes). `SIDECAR_MODEL_PATH` and `SIDECAR_ONNX_LIB_DIR` are only
needed to run the embedder tests against the real model instead of skipping
them — see "Environment variables used by tests" below.

### One shared session, safe for concurrent use

`NewONNX` creates one hugot session and one pipeline, both reused for every
`Embed` call. This is deliberate: the spike's best-measured throughput (33.9
passages/sec) came from **two goroutines sharing one session with ORT's
default threading left unconstrained**, not from one session per goroutine
or from pinning `WithIntraOpNumThreads`/`WithInterOpNumThreads` to 1 per
hugot's own README advice — that measured **11.7** passages/sec, three times
slower. `Embed` does no additional locking of its own; a later task's
controller calls it concurrently from multiple goroutines on purpose.

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
