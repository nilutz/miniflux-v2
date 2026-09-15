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
schema-qualified — the column type is `public.vector(768)` and the HNSW
operator class is `public.vector_cosine_ops` — so the migrations do not
depend on the connection's `search_path` at all. That matters on hardened
installs whose default `search_path` excludes `public`. `pg_search` needs no
such qualification: its control file always installs it into the `paradedb`
schema regardless of `search_path`.

### Keep `search.passages` vacuumed, or the HNSW index quietly under-returns

`passages_embedding_idx` is an HNSW index, and an HNSW scan returns *at
most* `hnsw.ef_search` tuples — dead ones included. Every dead tuple the
scan walks past is one row the query does not get back, with no error and
no warning: the result is simply short.

Re-indexing churns this table hard (a full re-index deletes and reinserts
every passage), so the deficit accumulates fast if autovacuum falls behind.
Measured directly against this corpus after several re-index cycles — 5,681
live passages, 1,066 dead tuples, autovacuum two hours stale — a forced
index scan returned a constant **31 rows fewer than asked for at every
`ef_search`**:

| `ef_search` | rows returned, before `VACUUM` | after `VACUUM` |
|---|---|---|
| 40 | 9 | 40 |
| 250 | 219 | 250 |
| 1000 | 969 | 1000 |

A plain `VACUUM search.passages` (not `VACUUM FULL`, no `REINDEX`) restored
exact counts at every width. The index itself was healthy the whole time —
`indisvalid`, `indisready`, correctly ordered results — it was only ever
returning a truncated prefix of them.

So: after any bulk re-index, vacuum the table.

```sql
VACUUM (VERBOSE) search.passages;
```

Two related knobs, and why neither is the answer here:

- **`REINDEX`** is not needed. The graph was never corrupt; rebuilding it
  costs minutes and fixes nothing a vacuum does not.
- **`hnsw.iterative_scan = 'relaxed_order'`** *does* mask the symptom (it
  makes the scan keep going until the `LIMIT` is satisfied, so the same
  query returned a full 40 rows at `ef_search = 40` even un-vacuumed). It is
  the wrong instrument for this: it hides an under-returning index behind
  extra scan work rather than fixing the cause, and it trades away the
  strict distance ordering the `relaxed_order` name is warning about. Vacuum
  first; reach for iterative scan only if a genuinely narrow `Filters`
  predicate — not dead tuples — is starving the candidate set.

At the current corpus size the index is not actually load-bearing: with
5,681 passages (~9 MB of heap) the planner prefers a sequential scan plus a
top-N sort for anything past a very small `LIMIT`, and measures about the
same as the index either way (~7-11 ms warm, both paths). That is a
reasonable planner choice at this scale, not a misconfiguration — the 22 MB
index earns its keep as the corpus grows, and the vacuum discipline above is
what keeps it correct when it does.

### Autovacuum is tuned on the table itself, and travels with a migration

`search.passages` carries its own storage parameters (`internal/store/migrations.go`),
tighter than the cluster defaults:

```sql
ALTER TABLE search.passages SET (
  autovacuum_vacuum_scale_factor = 0.05,
  autovacuum_vacuum_threshold = 500
);
```

Vacuum now fires at `500 + 0.05 × rows` instead of the cluster default's
`50 + 0.2 × rows`. At the 5,681-passage corpus in the incident above, that
is 784 dead tuples instead of 1,186 — comfortably below the 1,066 that
actually caused it, where the default's 1,186 trigger was not. 500 rather
than 1000 is deliberate: 1000's crossover against the cluster default is
~6,300 rows, above the corpus size that already broke, so it would have
been *looser* than the status quo, not tighter.

### Rebuilding `passages_embedding_idx`: what `maintenance_work_mem` actually buys, and its own footgun

There is **no migration that rebuilds `passages_embedding_idx` merely to
gain from a raised `maintenance_work_mem`**, and that is deliberate, not an
oversight — one was tried and reverted. (The nomic migration plan's task 2
*does* rebuild this index in a migration, but for an unrelated, unavoidable
reason — widening `embedding` from `vector(384)` to `vector(768)` requires
a new HNSW index regardless of `maintenance_work_mem`, since an HNSW
index's operator class is bound to its column's width; see that migration's
own comment in `internal/store/migrations.go` for how it avoids the
`/dev/shm` crash below.) Two things were measured directly, not assumed:

1. **`maintenance_work_mem` controls HNSW build *speed* only, not the
   resulting graph.** pgvector's on-disk build path (used when the graph
   doesn't fit under `maintenance_work_mem`) inserts each vector with the
   *same* neighbour-selection logic as the in-memory path — it just skips
   WAL-logging. Measured directly: 60,000 clustered 384-dimension vectors,
   built once under `maintenance_work_mem = '1MB'` (forcing the two-pass
   disk build) and once under `'1GB'` (in-memory). The two indexes were
   **byte-identical** (122,888,192 bytes) and recall@10 against a
   brute-force baseline was statistically indistinguishable (41.45% vs.
   41.65%). Build time differed 5.3× (213s vs. 40s). So an index that
   built slowly under a low `maintenance_work_mem` is not a worse index —
   it only took longer to build. There is nothing to "fix" about an
   already-built index on that basis alone.
2. **A migration that rebuilt this index anyway** — `DROP INDEX` +
   `CREATE INDEX` inside the migration's own transaction — held an
   `AccessExclusiveLock` on `search.passages` for the *entire* rebuild,
   confirmed via `pg_locks` and a concurrent `SELECT` that blocked
   completely (not merely degraded) until it finished. At the spec's own
   148k-passage reference point that is minutes of full outage on every
   deploy that crosses the migration, for zero index-quality benefit per
   point 1. It also crash-looped the sidecar outright on this host — see
   the `/dev/shm` warning below — which is a second, independent reason it
   doesn't belong in a migration.

So: `maintenance_work_mem` only matters for the **initial** build (still
handled correctly today — the table is empty or near-empty the first time
migration 2 runs, so the default build is fast regardless) and for an
**operator-driven** rebuild, where build time (not lock duration — see
above) is the only thing at stake. Measure the live value rather than
assume it:

```sql
SHOW maintenance_work_mem;
```

**Rule of thumb: give it comfortably more than the index's own on-disk
size** to stay on the faster in-memory path — a 292 MB index wants
something above roughly 350 MB. On this project's Docker dev stack it
reports **841 MB** (ParadeDB sizes it from container memory, not
PostgreSQL's 64 MB built-in default). A smaller container or a managed
instance that caps this setting lower is a real risk — if the cap sits
below ~400 MB, that is the constraint to raise *before* a rebuild, not
something to compensate for afterwards with a slower build.

A later, operator-driven rebuild should use `REINDEX INDEX CONCURRENTLY`,
so it doesn't hold the exclusive lock a plain `REINDEX` (or a `DROP INDEX`
+ `CREATE INDEX`) would — reads and writes against `search.passages`
continue throughout:

```sql
SET maintenance_work_mem = '1GB';

REINDEX INDEX CONCURRENTLY search.passages_embedding_idx;
```

This is deliberately a **plain `SET`, in its own dedicated `psql` (or
equivalent) session** — `REINDEX ... CONCURRENTLY` cannot run inside a
transaction block at all, so `SET LOCAL` is not an option here. A plain
`SET` is safe in this shape precisely because the session is disposable:
close it after the reindex and the setting goes away with the connection,
rather than sitting on a connection a pool hands back out to unrelated
queries. Follow this with a `VACUUM (VERBOSE) search.passages` per the
dead-tuple section above — a reindex churns the table hard enough to
matter.

**Do not also raise `max_parallel_maintenance_workers` without checking
`/dev/shm` first.** A parallel HNSW build requests a POSIX shared-memory
segment sized against `maintenance_work_mem`, backed by the container's
`/dev/shm`. This is exactly what crash-looped the reverted migration on
this host: Docker's default `/dev/shm` is 64 MiB, the build requested
~1.02 GB (matching `maintenance_work_mem = '1GB'`), and PostgreSQL failed
with `could not resize shared memory segment ... No space left on device`
— not a transient condition, a fixed property of the container that
recurs identically on every retry. Check the container's real `/dev/shm`
size (`df -h /dev/shm` inside it) before setting
`max_parallel_maintenance_workers` above its default; if it's small,
either raise the container's `shm_size` first or leave
`max_parallel_maintenance_workers` alone (serial builds don't touch
`/dev/shm` at all) rather than discovering the ceiling the way this task
did. The 768-dimension migration
(`docs/superpowers/plans/2026-09-15-nomic-migration.md`, task 2) rebuilds
this same index at roughly twice its previous size and explicitly pins
`SET LOCAL max_parallel_maintenance_workers = 0` for exactly this reason,
rather than leaving it at the cluster's default (2 on this project's dev
stack — already enough to trigger a parallel build and hit this ceiling).
Follow the same rule for any later operator-driven `REINDEX INDEX
CONCURRENTLY` against the widened index: check `/dev/shm` before raising
`max_parallel_maintenance_workers` above its default.

## The embedder (`internal/embed`, `internal/embed/onnx`)

`internal/embed` holds the small, pure-Go `Embedder` interface
(`EmbedDocuments`/`EmbedQuery`/`Dimensions`/`Identity`/`Close`) with no CGO
and no native dependencies. Embedding a document (to index) and embedding a
query (to search with) are separate methods, not one method with a
document/query flag: the configured model —
[`nomic-ai/nomic-embed-text-v1.5`](https://huggingface.co/nomic-ai/nomic-embed-text-v1.5)
(nomic migration plan, task 2; previously `bge-small-en-v1.5`, which used
no prefixes) — is asymmetric and requires a different, incompatible text
prefix (`"search_document: "`/`"search_query: "`) for each, and a missing
prefix degrades retrieval with no error and no other symptom. Two methods
make each call site's intent (internal/indexer only ever indexes;
internal/search/querycache.go only ever queries) visible in a diff rather
than depending on a runtime flag threaded correctly through every call.
`internal/embed/onnx` implements it: it wraps
[hugot](https://github.com/knights-analytics/hugot) (v0.7.8, build tag
`ORT`) around [ONNX Runtime](https://onnxruntime.ai/) (native v1.30.0) to
embed text with a pinned model,
[`nomic-ai/nomic-embed-text-v1.5`](https://huggingface.co/nomic-ai/nomic-embed-text-v1.5)
(int8-quantized ONNX export, 768 dimensions, 8192-token context). This is
the exact stack a spike measured end to end — see `NewONNX` in
`internal/embed/onnx/onnx.go` for the call sequence and the reasoning
behind it. The split keeps consumers that only need the interface (such as
`internal/indexer`'s tests) from linking the native `libtokenizers.a`.

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
   `nomic-ai/nomic-embed-text-v1.5` on Hugging Face (~131MB, int8-quantized
   — do *not* use the fp32 `model.onnx`, which the spike measured at
   roughly half the throughput for no accuracy benefit evaluated here),
   alongside the rest of that repo's files (`tokenizer.json`, `vocab.txt`,
   `config.json`, `tokenizer_config.json`, `special_tokens_map.json`), which
   hugot expects to find next to (one directory up from) the `.onnx` file.
   **Do not cap input length on `config.json`'s `max_position_embeddings`
   (2048)** — it disagrees with the tokenizer's own `model_max_length`
   (8192) and the model card, and the spike empirically embedded an
   8,120-token passage successfully; capping at 2048 would silently discard
   three quarters of the context this model exists to provide.

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
SIDECAR_TEST_DATABASE_URL="postgres://postgres:postgres@127.0.0.1:5434/miniflux2_test?sslmode=disable" \
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
`EmbedDocuments`/`EmbedQuery` call. This is deliberate: the spike's
best-measured throughput (33.9 passages/sec) came from **two goroutines
sharing one session with ORT's default threading left unconstrained**, not
from one session per goroutine or from pinning
`WithIntraOpNumThreads`/`WithInterOpNumThreads` to 1 per hugot's own README
advice — that measured **11.7** passages/sec, three times slower. Neither
method does any additional locking of its own; a later task's controller
calls them concurrently from multiple goroutines on purpose.

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

## Remote embedding (`internal/embed/remote`)

Embedding can run on a different machine — a GPU host, typically — instead
of in-process ONNX. `internal/embed/remote` implements `embed.Embedder` by
speaking HTTP to a service you run there. It has no CGO and does not import
`internal/embed/onnx`, so it links and tests (`go test ./internal/embed/remote/`)
with no build tag and no native libraries, the same as `internal/embed` itself.

Set `SIDECAR_EMBEDDER=remote` (default is `local`) plus:

| Variable | Default | Meaning |
|---|---|---|
| `SIDECAR_REMOTE_EMBEDDER_URL` | — (required) | base URL of the remote service, e.g. `http://gpu-host:9000` |
| `SIDECAR_REMOTE_EMBEDDER_TIMEOUT` | `30s` | per-request timeout, a Go duration |

**The remote's identity, not local config, becomes part of `contentHash`.**
At startup the sidecar sends the remote an empty batch to learn what model
it is actually running before indexing anything — this is what spec §13.1's
model-identity hash means to protect: a locally-configured guess at the
remote's model name would let an operator repoint the URL at a differently
configured box without changing a single hash, silently mixing two models'
vectors in one HNSW graph. A dimension mismatch against the fixed
`vector(768)` schema column is a startup error here, not a runtime insert
failure, and the sidecar refuses to start rather than guess.

### Wire protocol

One endpoint, `POST {SIDECAR_REMOTE_EMBEDDER_URL}/embed`, no authentication
(run it on a private network, the same trust boundary Postgres itself is
given). This is a small JSON protocol defined by `internal/embed/remote`
itself, not tied to any particular serving framework — implement it however
is convenient (a FastAPI/Flask wrapper around `sentence-transformers`, for
instance).

Request:

```json
{"texts": ["first passage", "second passage"], "task": "document"}
```

`task` is `"document"` or `"query"`, set by which of `EmbedDocuments` /
`EmbedQuery` the caller invoked. It exists because an asymmetric model
(`nomic-ai/nomic-embed-text-v1.5`) must embed a document differently than
it embeds a query for the identical string — it needs
`"search_document: "`/`"search_query: "` prepended respectively — and only
the remote knows what, if anything, the model it is actually running
needs done with that distinction: the client deliberately does not
hardcode either prefix itself. A server whose configured model needs no
such distinction (`bge-small-en-v1.5`, the model configured prior to the
nomic migration plan's task 2, is one example) is free to ignore this
field entirely.

`texts` may be empty — the sidecar sends an empty batch once, at startup,
purely to learn `model` below without embedding anything real; `task` is
still sent on that request too (arbitrarily, as `"document"` — it has no
effect on an empty batch), so every request this client ever sends has the
same shape. A conforming server must still populate `model` in that case.

Response, `200` only:

```json
{
  "vectors": [[0.01, -0.02, "... 768 floats ..."], [0.03, 0.04, "..."]],
  "model": {"name": "nomic-embed-text-v1.5", "revision": "abc123", "dimensions": 768}
}
```

- `vectors` has exactly one entry per input text, in the same order, each
  of `model.dimensions` length.
- `model` is returned on **every** response, not only the first, and must
  describe whatever model actually produced that batch's vectors. The
  client compares it against what it learned at startup on every call, so
  a remote that starts serving a different model mid-run (a redeploy
  behind the same URL) is caught on the next batch — refused with an
  error — rather than silently mixing two models' vectors into one index.
- `model.name` and `model.revision` must not contain `@` or `#`. Those are
  `embed.Identity`'s own separators (`"%s@%s#%d"`), and a name/revision
  pair containing one could format identically to a different, genuinely
  distinct pair. The client rejects such a name at startup rather than
  silently stripping or escaping it — fix the name at the source.

Anything other than a `200` status is treated as an error; on non-200 or a
body that fails to decode as the JSON above, the client fails loudly rather
than falling back to local CPU embedding (spec §13.1's explicit decision —
a silent fallback would produce a corpus embedded by two different paths
with no record of which is which).

## Environment variables used by tests

Tests that need a real database or a real model are skipped, not failed,
when the corresponding variable is unset. Note that this is about *runtime*
services only — it says nothing about the native libraries the ONNX packages
need in order to link at all (see "Running the tests" below).

- `SIDECAR_TEST_DATABASE_URL` — a Postgres DSN (e.g.
  `postgres://postgres:postgres@127.0.0.1:5434/miniflux2_test?sslmode=disable`)
  pointing at a running ParadeDB instance. Used by `internal/store`,
  `internal/search`, `internal/search/eval`, `internal/indexer` and
  `cmd/sidecar`. Unset: those tests `t.Skip`.

  **This is deliberately not `SIDECAR_DATABASE_URL`.** That one is what the
  sidecar *binary* reads, and what the operator guide tells you to point at
  your live Miniflux database. `make test` runs `internal/indexer`'s suite,
  which sweeps the entire `entries` table and rewrites `search.passages`
  with a fake embedder; sharing one variable between "run the service" and
  "run the tests" put a destroyed corpus one `export` away, and did destroy
  one in this project. Point `SIDECAR_TEST_DATABASE_URL` at a throwaway
  database — `internal/testdb` fails the run outright, rather than skipping,
  if the two variables name the same place.
- `SIDECAR_MODEL_PATH` — filesystem path to the quantized embedding model
  (`model_quantized.onnx`), used by `internal/embed/onnx`. Unset: those
  tests `t.Skip`. In production and in this package's own gated tests this
  points at the configured model — `nomic-embed-text-v1.5` as of the nomic
  migration plan's task 2.
- `SIDECAR_NOMIC_MODEL_PATH` — a second, separate model path, also used by
  `internal/embed/onnx`, specifically for
  `TestEmbedAppliesDistinctPromptPrefixesForNomic`: that test needs an
  actual `nomic-embed-text-v1.5` model file to prove its prompt prefixes
  reach the real tokenizer. Kept distinct from `SIDECAR_MODEL_PATH` so the
  two can be pointed at different models when needed.
- `SIDECAR_NOPREFIX_MODEL_PATH` — a third model path, used only by
  `TestEmbedDocumentsAndEmbedQueryAgreeWithoutPrefixes`, which needs a real
  model genuinely absent from `modelPromptPrefixes` (`bge-small-en-v1.5`,
  say) to prove the no-prefix code path is a true no-op at the tokenizer
  boundary — a proof `SIDECAR_MODEL_PATH` can no longer supply on its own
  now that it points at an asymmetric model.

`internal/passage`, `internal/web` and the controller tests in
`internal/indexer` are hermetic and need neither.

## Running the tests

```sh
make dev-db
CGO_LDFLAGS="-L<dir-with-libtokenizers.a>" \
SIDECAR_TEST_DATABASE_URL="postgres://postgres:postgres@127.0.0.1:5434/miniflux2_test?sslmode=disable" \
  make test
```

For the packages that need neither a database nor the native libraries —
the set CI runs — there is a target that needs no environment at all:

```sh
make test-hermetic
```

Two separate things are optional here, and they fail in different ways:

- **`SIDECAR_TEST_DATABASE_URL` is genuinely optional.** Without it the
  database-backed tests `t.Skip` and everything else still passes. What is
  *not* optional is that it must never be your live database: see the
  variable's own entry above.

  With it set, run packages one at a time (`make test` already passes
  `-p 1`). Several packages create and delete fixture entries in the same
  database, and `internal/store`'s exact-vs-approximate pending counts
  assert on whole-table numbers; run in parallel they fail at random on
  another package's fixtures.
- **`libtokenizers.a` is not.** `make test` passes `-tags ORT` and covers
  `./...`, which includes `internal/embed/onnx` and `cmd/sidecar`; both link
  the native tokenizer, so on a machine without it `make test` fails at the
  **link** step (`ld: library 'tokenizers' not found`), not at a test
  assertion. To run the suite without the native dependencies, run the
  packages that do not need them directly:

  ```sh
  make test-hermetic
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
(`SIDECAR_ADMIN_ADDR` to change it). The status page and every control
endpoint below have **no authentication** — binding to loopback is their
only protection — so point this server anywhere else only deliberately.
This is a deliberate decision, not an oversight left over from before the
[search API's own API-key requirement](#the-search-http-api) below: see
"Why the status page and control endpoints are not gated by an API key"
at the end of this section for the reasoning.

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
| `GET /api/embedder` | the Model section's current state (Task 9) |
| `POST /api/embedder/preview` | test a candidate embedder without switching |
| `POST /api/embedder/switch` | apply an embedder switch (can trigger a full re-index) |

The three `POST /api/backfill/*` and `/api/embedder/*` endpoints check the
`Origin` header and reject cross-origin requests, so a page open in your
browser on another origin cannot drive them.

#### Why the status page and control endpoints are not gated by an API key

None of the endpoints in this section require a Miniflux API key, even
though [the search API](#the-search-http-api) below does as of the change
that added authentication. That split was a deliberate decision, argued
here rather than made by accident:

- **There is no per-user data here to protect.** The confidentiality
  problem the search API's own authentication requirement exists to fix
  is one Miniflux user's articles leaking into another's search results.
  The status page and every control endpoint above are global —
  throughput, ETA, database size, the embedder in use, pause/resume — with
  no per-user dimension at all. A Miniflux API key has nothing to scope
  here.
- **Network isolation is the existing, documented protection**, unchanged
  by adding an API key requirement elsewhere: this server binds to
  loopback by default, and the production compose stack deliberately
  publishes no port for it at all. Anyone who can already reach this admin
  surface is already inside that trust boundary.
- **A Miniflux API key does not actually mean "operator".** Any Miniflux
  user — including a low-privilege reader — can mint their own API key
  from their account settings; `public.api_keys` carries no admin
  distinction. Gating the embedder switch (the most dangerous control
  endpoint here — it can trigger a multi-hour full corpus re-index) behind
  "any valid Miniflux key" would look like it raises the bar against a
  malicious actor on the network, while actually only requiring that actor
  to hold any one reader's key — which the search API's own confidentiality
  fix does nothing to make harder to obtain. That would be a worse outcome
  than today's plain network-isolation boundary: it would look like
  protection without providing much.
- **Locking an operator out of their own pause button has a real cost.**
  An operator recovering a stuck backfill from the box in front of them
  may not have a Miniflux API key to hand at all.

If your threat model requires more than network isolation for this admin
surface specifically, the answer is to isolate it further at the network
layer (a firewall rule, a reverse-proxy auth layer, an SSH tunnel) — not a
Miniflux API key, which was not designed to express an admin/operator
privilege level in the first place.

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

## The search HTTP API

`internal/web/search_handlers.go` and `article_handler.go` serve three
read-only GET endpoints on the same admin server above:

| Endpoint | Purpose |
|---|---|
| `GET /api/search?q=&mode=&limit=` | ranked results: `keyword` (BM25), `semantic` (vector), `hybrid` (both, default), or `passages` (paragraph-sized extracts) |
| `GET /api/similar?entry_id=&limit=` | nearest neighbours for one entry, by vector similarity; no snippet |
| `GET /api/article?entry_id=` | one entry's title, url, published date and full content (task 15) |

All three also accept the filter parameters `parseFilters` documents in
`search_handlers.go` (`feed`, `category`, `unread`, `starred`, `since`,
`until`, and `user` — see "Authentication" just below for what `user` does
now).

### Authentication

**These three endpoints require a Miniflux API key.** Send it as the
`X-Auth-Token` header — the exact header Miniflux's own REST API reads
(`internal/api/middleware.go`) — and the sidecar validates it with a
single read-only lookup against `public.api_keys`, the same table
Miniflux itself uses. **One Miniflux API key works against both
services**; there is no separate sidecar credential to create or manage.

Create one from Miniflux's own web UI: **Settings → API Keys → Create a
new API key**. Give it a description and save it; the token shown there
is what you send as `X-Auth-Token`.

```sh
curl -H 'X-Auth-Token: <your-miniflux-api-key>' \
  'http://127.0.0.1:8081/api/search?q=widgets'
```

- **Missing or unrecognised token → `401`**, with a JSON body naming the
  expected header (never anything that would let a caller tell "wrong
  token" apart from "this token doesn't exist" — see
  `internal/web/auth.go`).
- **A revoked key stops working immediately.** Validation is a live,
  read-only lookup on every request — nothing is cached — so deleting an
  `api_keys` row in Miniflux takes effect on the very next sidecar
  request. The sidecar never writes to `api_keys` itself (in particular,
  it never updates `last_used_at` — that column belongs to Miniflux).
- **Results are scoped to your key's user, not to a `user=` parameter you
  send.** Before this authentication requirement existed, `user=<id>` in
  the query string was the caller's own unchecked assertion of identity.
  It still exists for backward compatibility, but it can no longer be
  used to claim a different identity: the user id is now derived from your
  API key, an omitted `user` parameter is filled in from your key
  automatically, and a `user` parameter that disagrees with your key is
  rejected with `400` rather than honoured or silently overridden.

The admin/status page, `GET /api/status`, and the backfill/embedder
control endpoints documented under "The admin page" above are **not**
gated by an API key — see that section's own "Why the status page and
control endpoints are not gated by an API key" for the reasoning. Binding
this server to loopback (or a trusted LAN) remains that group's only
protection, and is why the MCP server below talks to it over a local
child process rather than opening its own network port.

## The MCP server (`cmd/mcp`)

`cmd/mcp` is a small, separate binary: an [MCP](https://modelcontextprotocol.io)
server that Claude Code spawns locally over **stdio** and that calls the
search HTTP API above. It is not another HTTP service, and it is not a
CLI flag on `cmd/sidecar` — see `cmd/mcp/main.go`'s own package doc
comment for why stdio, specifically, is the right transport here: a stdio
child process Claude Code spawns and owns the pipes of opens no network
surface at all, where serving MCP directly from the admin server would.

It exposes three tools, each wrapping one endpoint above:

| Tool | Wraps | Use it to |
|---|---|---|
| `search` | `GET /api/search` | find articles or passages relevant to a query; `mode` picks keyword/semantic/hybrid/passages retrieval — see the tool's own description (`internal/mcpserver/tools.go`) for what each is for |
| `similar` | `GET /api/similar` | browse "more like this" from an article's entry_id |
| `fetch_article` | `GET /api/article` | read one article's full text by entry_id, once search/similar have found it |

`search` and `similar` results carry `entry_id`, `title`, `url`, a score,
and (for `search`) a snippet — even though the two endpoints they wrap do
not return title/url themselves (`entryResultView`/`similarEntryResultView`
carry neither). `internal/mcpserver` fills those in itself, with one
`GET /api/article` call per distinct entry id in a result set, so that a
result is something an agent can act on without a second round trip just
to find out what it is.

### Building it

```sh
make build-mcp    # -> bin/miniflux-mcp, built with CGO_ENABLED=0 and no CGO_LDFLAGS
```

Unlike `make build` (`cmd/sidecar`, which requires `-tags ORT` and a linked
`libtokenizers.a`), this binary needs no native library at all — it is a
plain HTTP client. That is deliberate: it is the piece meant to run on an
operator's own laptop next to Claude Code, not inside the container that
runs the sidecar itself.

### Installing it into Claude Code

```sh
claude mcp add miniflux-search \
  --env SIDECAR_URL=http://localhost:8081 \
  --env MINIFLUX_API_KEY=your-miniflux-api-key \
  -- /path/to/bin/miniflux-mcp
```

Environment variables:

| Variable | Default | Meaning |
|---|---|---|
| `SIDECAR_URL` | `http://localhost:8081` | base URL of a running sidecar's admin server (`SIDECAR_ADMIN_ADDR` above, spelled out as a URL) |
| `MINIFLUX_API_KEY` | *(none)* | a Miniflux API key, sent as the `X-Auth-Token` header on every request — required; see "Authentication" under "The search HTTP API" above |

**`MINIFLUX_API_KEY` is required.** As of the sidecar's own API-key
requirement (see "Authentication" under "The search HTTP API" above),
`GET /api/search`, `/api/similar` and `/api/article` all reject a request
with no valid token. Leaving this unset still starts `cmd/mcp` — it is a
warning to stderr, not a fatal error, since the tool can still report a
clear failure per call rather than refusing to start — but every
`search`/`similar`/`fetch_article` tool call will fail with "sidecar
rejected the API key" until it is set to a real Miniflux key. Create one
from Miniflux's own **Settings → API Keys → Create a new API key**.

### Failure modes

Every tool call distinguishes three ways a request can fail, each with a
different fix (`internal/sidecarclient`'s `UnreachableError`/`AuthError`/
`APIError`):

- **The sidecar is not running** (the common case — the stack gets torn
  down routinely): `"sidecar unreachable at http://localhost:8081: ..."`.
- **The sidecar rejected the API key** (401 for a missing/unknown token,
  403 for a valid-but-forbidden one — see "Authentication" above):
  `"sidecar rejected the API key (HTTP 401): ... -- set MINIFLUX_API_KEY
  to a valid Miniflux API key"`.
- **The request itself was rejected** (400/404/500 — an unknown `mode`, an
  unknown `entry_id`, ...): the sidecar's own error message.

Zero results is none of the above: `search` and `similar` report it as a
normal, successful result (`result_count: 0` plus an explanatory
`message`), never as a tool error — the corpus can legitimately have
nothing that matches a query, or be empty altogether.
