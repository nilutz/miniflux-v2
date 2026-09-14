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
run `CREATE EXTENSION IF NOT EXISTS vector SCHEMA public` / `pg_search`.

pgvector is installed into `public` explicitly, and every reference to it is
schema-qualified — the column type is `public.vector(384)` and the HNSW
operator class is `public.vector_cosine_ops` — so the migrations do not
depend on the connection's `search_path` at all. That matters on hardened
installs whose default `search_path` excludes `public`. `pg_search` needs no
such qualification: its control file always installs it into the `paradedb`
schema regardless of `search_path`.

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

Every Go target in the `Makefile` passes `-tags ORT`, and
`internal/embed/onnx` genuinely needs it: built *without* the tag, hugot
silently falls back to a pure-Go GoMLX backend roughly **10x slower**, with
no build error and no runtime warning. `internal/embed/onnx`'s
`TestBackendIsORT` fails loudly when the tag is missing, and the binary logs
the backend name it actually loaded on every startup
(`sidecar: embedder ready backend=ORT`) — a line reading `GoMLX` there is
the only symptom you will get in production.

`cmd/sidecar` is the **only** package in the module that imports
`internal/embed/onnx`. Everything else depends solely on the pure-Go
`embed.Embedder` interface, which is why the packages below link and test
without `libtokenizers.a` even with `-tags ORT` passed:

```sh
go test -tags ORT ./internal/indexer/ ./internal/store/ ./internal/web/ ./internal/passage/ ./internal/embed
```

Only `./internal/embed/onnx` and `./cmd/sidecar` need the native libraries.

## Environment variables used by tests

Tests that need a real database or a real model are skipped, not failed,
when the corresponding variable is unset. Note that this is about *runtime*
services only — it says nothing about the native libraries the ONNX packages
need in order to link at all (see "Running the tests" below).

- `SIDECAR_DATABASE_URL` — a Postgres DSN (e.g.
  `postgres://postgres:postgres@127.0.0.1:5434/miniflux2?sslmode=disable`)
  pointing at a running ParadeDB instance. Used by `internal/store` and
  `internal/indexer`. Unset: those tests `t.Skip`.
- `SIDECAR_MODEL_PATH` — filesystem path to the quantized embedding model
  (`model_quantized.onnx`), used by `internal/embed/onnx`. Unset: those
  tests `t.Skip`.

`internal/passage`, `internal/web` and the controller tests in
`internal/indexer` are hermetic and need neither.

## Running the tests

```sh
make dev-db
CGO_LDFLAGS="-L<dir-with-libtokenizers.a>" \
SIDECAR_DATABASE_URL="postgres://postgres:postgres@127.0.0.1:5434/miniflux2?sslmode=disable" \
  make test
```

Two separate things are optional here, and they fail in different ways:

- **`SIDECAR_DATABASE_URL` is genuinely optional.** Without it the
  database-backed tests `t.Skip` and everything else still passes.
- **`libtokenizers.a` is not.** `make test` passes `-tags ORT` and covers
  `./...`, which includes `internal/embed/onnx` and `cmd/sidecar`; both link
  the native tokenizer, so on a machine without it `make test` fails at the
  **link** step (`ld: library 'tokenizers' not found`), not at a test
  assertion. To run the suite without the native dependencies, run the
  packages that do not need them directly:

  ```sh
  go test ./internal/indexer/ ./internal/store/ ./internal/web/ ./internal/passage/ ./internal/embed
  ```

## Running the sidecar

```sh
make build                       # -> bin/sidecar, built with -tags ORT

DYLD_LIBRARY_PATH=/opt/homebrew/opt/onnxruntime/lib \
SIDECAR_DATABASE_URL="postgres://postgres:postgres@127.0.0.1:5434/miniflux2?sslmode=disable" \
SIDECAR_MODEL_PATH=<path-to-model_quantized.onnx> \
SIDECAR_ONNX_LIB_DIR=/opt/homebrew/opt/onnxruntime/lib \
  ./bin/sidecar
```

The process runs three things concurrently until SIGINT/SIGTERM: the live
indexing lane (new entries, one worker, always on), the throttled backfill
lane (the historical backlog), and the status/admin HTTP server. If any one
of them fails, the process cancels the others and exits non-zero.

`./bin/sidecar -h` lists every environment variable with its default.

### The admin page

The status and admin page is at **<http://127.0.0.1:8081/>** by default
(`SIDECAR_ADMIN_ADDR` to change it). It has **no authentication** — binding
to loopback is its only protection — so point it anywhere else only
deliberately.

It shows progress and ETA, current throughput, the live worker count with
the controller's reason for it, skip and failure counts by cause, the
throttle settings in force, and controls for pause/resume and for editing
the throttle. JSON equivalents:

| Endpoint | Purpose |
|---|---|
| `GET /api/status` | the whole status view |
| `GET /api/backfill/config` | the live-editable settings |
| `POST /api/backfill/pause` | stop starting new batches |
| `POST /api/backfill/resume` | release a paused lane |
| `POST /api/backfill/config` | change the throttle |

The three `POST` endpoints check the `Origin` header and reject
cross-origin requests, so a page open in your browser on another origin
cannot drive them.

### The backfill lane and its throttle

The backfill lane is the only part of this that can load the machine: at
100k–1M entries it is 4–41 hours of CPU-bound work. It is bounded by a
schedule window, a worker ceiling, and an adaptive controller that samples
the **per-core** load average and per-worker batch service time and moves
the worker count within its band between batches.

All of it is configurable at startup and live-editable afterwards without a
restart:

| Variable | Default | Meaning |
|---|---|---|
| `SIDECAR_BACKFILL_WINDOW` | `always` | hours the lane may run, e.g. `02:00-07:00`; wraps past midnight |
| `SIDECAR_BACKFILL_MIN_WORKERS` | `1` | worker floor |
| `SIDECAR_BACKFILL_MAX_WORKERS` | `2` | worker ceiling, capped at the host's core count |
| `SIDECAR_BACKFILL_LOAD_THRESHOLD` | `0.8` | per-core 1-minute load average to back off at |
| `SIDECAR_BACKFILL_BATCH_SIZE` | `16` | passages per forward pass, clamped to 8–32 |
| `SIDECAR_BACKFILL_PAGE_SIZE` | `20` | entries fetched per batch |
| `SIDECAR_BACKFILL_POLL_INTERVAL` | `5s` | wait between checks while it cannot work |
| `SIDECAR_BACKFILL_IDLE_RESWEEP` | `15m` | wait before re-sweeping an already-drained corpus |

A malformed value is a startup error rather than a silently ignored line.
Out-of-range numbers are clamped, and the effective configuration is logged
at startup and shown on the admin page — check it there rather than assuming
a value was taken.

The same settings can be changed at runtime. Fields you leave out are
unchanged:

```sh
curl -X POST http://127.0.0.1:8081/api/backfill/config \
  -H 'Content-Type: application/json' \
  -d '{"window":"02:00-07:00","max_workers":2,"batch_size":8}'
```

The lane does **not** stop when the backlog is empty: it reports `done`,
waits `SIDECAR_BACKFILL_IDLE_RESWEEP`, and sweeps again. That is what
notices an entry whose content changed underneath it — a re-scrape, say —
since nothing else re-examines entries below the live lane's cursor.

### Forcing a full re-index

`search.entry_index_state.content_hash` covers `store.PipelineVersion` as
well as the entry's content, so bumping that constant
(`internal/store/entries.go`) makes every indexed entry pending again on the
next sweep, with no `TRUNCATE` and no manual intervention. **Bump it in the
same change as any modification to `internal/passage`'s `ExtractText` or
`Split`**: `search.passages.char_start`/`char_end` index into a plaintext
that is never stored, so changing how it is derived invalidates every stored
offset with nothing else to detect it.
`internal/passage`'s `TestPipelineOutputDigestIsStable` fails when that
output changes, as a reminder.
