# Semantic search and recommendations over a personal Miniflux corpus

**Date:** 2026-09-12
**Status:** Design approved, pending implementation plan
**Scope:** P0 (corpus), P1 (hybrid search), P2 (similar articles)

## 1. Context

Miniflux stores feed entries in Postgres and already has entry-level full-text
search. The goal is a genuine search engine and recommendation layer over the
*content* of subscribed blogs: hybrid lexical + semantic search, similar-article
suggestions, a personalised daily digest, and discovery of new blogs.

Three facts about the existing system shape everything below.

**The content largely is not there.** Fetching the underlying web page is opt-in
per feed (`crawler`, default false, `internal/ui/form/feed.go:125`). Without it
an entry holds only whatever the publisher put in the feed, usually an excerpt.

**The corpus deletes itself.** `runCleanupTasks` hard-deletes read entries after
60 days and unread after 180 (`internal/cli/cleanup_tasks.go:26-49`), writing
`entry_tombstones (feed_id, hash)` rows that permanently block re-ingestion
(`internal/storage/entry.go:120`, `:292`). Feeds only serve a recent window, so
deleted entries cannot be recovered from the source either.

**Scraped content is written once.** `updateExistingEntries` is false whenever
the crawler is enabled (`internal/reader/handler/handler.go:332`), so enabling
the crawler affects new entries only.

## 2. Decisions

| Question | Decision |
|---|---|
| Split of work | Thin fork of Miniflux (UI touchpoints only) plus a sidecar service |
| Sidecar language | Go, matching the fork |
| Inference runtime | ONNX Runtime via CGO, behind an `Embedder` interface |
| Corpus scale | Single user, 100k–1M entries, long retained history |
| Index location | pgvector in Miniflux's own Postgres, in a sidecar-owned schema |
| Embeddings | Local model on CPU, same host, 384 dimensions |
| Backfill pacing | Load-adaptive within a concurrency ceiling; no fixed nightly count |
| "RAG" mode | Extractive passage retrieval, no language model |
| Lexical ranking | True BM25 via the ParadeDB `pg_search` extension |
| Fusion | Reciprocal Rank Fusion over rank positions |

## 3. Scope

**In scope.** P0 corpus capture and retention; P1 hybrid search with a mode
picker; P2 similar-article suggestions.

**Deliberately deferred.** P3 (read/hide signals and a daily digest) and P4
(discovering new blogs) get their own specs. P3 is independent of this work and
can proceed in parallel; it needs a new user-facing "hide" action, which is
schema and UI work inside the fork rather than sidecar work. P4 depends on a
source of *candidate* feeds not yet in the instance, which is an unresolved
external dependency.

P2 is included here rather than specced separately because it is a second query
against the index P1 builds, not a new subsystem.

## 4. Architecture

```
┌────────────────────┐         ┌─────────────────────┐
│  Miniflux (fork)   │  HTTP   │      Sidecar        │
│  · reader UI       │────────▶│  · indexer          │
│  · search bar      │◀────────│  · search API       │
│  · similar block   │         │  · admin/status UI  │
└─────────┬──────────┘         └──────────┬──────────┘
          │                               │
          │        ┌──────────────┐       │
          └───────▶│  PostgreSQL  │◀──────┘
                   │  public.*    │ ← Miniflux owns
                   │  search.*    │ ← sidecar owns
                   └──────────────┘
```

Both processes talk to one database. Miniflux owns the `public` schema and never
reads `search`. The sidecar owns the `search` schema, reads `public.entries`
read-only, and never writes to it.

**Both are Go.** One language across fork and sidecar means one toolchain, one
CI, and no second runtime on the host. Everything except the forward pass —
Postgres access, the HTTP API, the admin page, the two-lane scheduler and its
adaptive throttle — is straightforward Go and benefits from it.

**Why one database.** Every useful query filters on entry metadata: "similar to
this but unread", "best of this week", "within these feeds", "excluding what I
hid". When lexical index, vectors and entries share a database those are `WHERE`
clauses. Split across stores, each becomes a hand-rolled distributed join. At
1M entries × 384 dimensions an HNSW index is roughly 1.5 GB, comfortably within
pgvector's range.

**Synchronisation** is a poll on `WHERE id > last_seen_id` at a fixed interval.
At a few hundred new entries a day, `LISTEN/NOTIFY` would be complexity without
a payoff.

## 5. P0 — Corpus (fork)

The fork's only job is to *capture* full text and *stop deleting* it. Every
derived representation — plaintext, passages, vectors — belongs to the sidecar.
This keeps the fork's diff small and rebasable.

### 5.1 Capture

New feeds default to `crawler = true`, exposed as a config option rather than a
hardcoded form default, plus a one-time `UPDATE feeds SET crawler = true`.

Cost: one additional HTTP request per new entry. The scraper has no per-host
throttle equivalent to `POLLING_LIMIT_PER_HOST`; acceptable at personal scale,
noted as a limitation.

### 5.2 Retention

Archiving is disabled **structurally**, not by configuration.

`ArchiveEntries` disables only on a negative interval
(`internal/storage/entry.go:369`). Setting `CLEANUP_ARCHIVE_READ_DAYS=0` does
not disable it: zero is not less than zero, so execution falls through to
`days := max(int(interval/24h), 1)` = 1, deleting everything older than a single
day. Because the correct value is counter-intuitive and the failure is
irreversible, the fork removes the call rather than relying on configuration.

`FlushHistory` (`internal/storage/entry.go:486`), the "flush history" UI button,
deletes every read non-starred entry and tombstones it. It is removed from the
UI in the fork.

### 5.3 Backfill

Enabling the crawler does not retroactively scrape. A new CLI command walks
entries and calls `processor.ProcessEntryWebPage`, the same path as the existing
per-entry "fetch content" button.

One new column, `entries.full_text_fetched_at timestamptz NULL`, is set on a
successful scrape. It makes the backfill resumable and tells the sidecar whether
an entry holds a real article or an RSS excerpt.

### 5.4 What is already lost

Tombstoned entries cannot be recovered from the database or re-fetched from the
feed. The corpus begins with whatever remains in the database on the day
retention is disabled, plus everything after. **Before implementation, measure
the surviving history** — `SELECT min(published_at), count(*) FROM entries` and
the `entry_tombstones` count — because the result may change how much the
backfill is worth.

### 5.5 Sizing

1M entries of full article HTML is roughly 20–40 GB before indexes, plus ~1.5 GB
for HNSW and the BM25 index. Confirm the disk exists before P1 assumes it.

## 6. P1 — Hybrid search

### 6.1 Passage-level index

Both halves of the index live at passage granularity so fusion compares like
with like. Embedding a 3000-word post as a single vector washes out its
specifics; passages also make the extractive mode possible.

Articles split into 256–512 token passages on sentence boundaries with ~64
tokens of overlap, retaining character offsets into the extracted plaintext so
highlighting is exact.

```sql
CREATE SCHEMA search;

CREATE TABLE search.passages (
    id          bigserial PRIMARY KEY,
    entry_id    bigint NOT NULL,
    ordinal     int NOT NULL,
    text        text NOT NULL,
    char_start  int NOT NULL,
    char_end    int NOT NULL,
    embedding   vector(384),
    UNIQUE (entry_id, ordinal)
);

CREATE TABLE search.entry_index_state (
    entry_id     bigint PRIMARY KEY,
    content_hash text NOT NULL,
    indexed_at   timestamptz,
    status       text NOT NULL,   -- ok | skipped | failed
    reason       text
);
```

HNSW index on `embedding` using cosine ops; a `pg_search` BM25 index on `text`.

`content_hash` lets a re-scraped entry be detected and re-indexed rather than
going stale. `entry_index_state` is also what the admin page counts.

Plaintext extraction from the sanitised HTML in `entries.content` happens in the
sidecar, not the fork.

### 6.2 Lexical half

Postgres native full-text search is not BM25: `ts_rank`/`ts_rank_cd` score on
term frequency and position weights with no IDF term, so a rare discriminating
word gets no boost over a common one. The ParadeDB `pg_search` extension
provides genuine BM25 with IDF and tunable *k1*/*b*, and being a Postgres
extension it stays inside the single-database architecture.

Cost: a second non-trivial extension to install and keep upgraded alongside
pgvector. This is accepted.

### 6.3 Fusion

Lexical scores and vector distances are not on comparable scales, and
normalising them is where hybrid search usually goes wrong. Reciprocal Rank
Fusion ignores magnitudes and fuses on rank position, requiring no per-corpus
tuning.

The search bar's mode picker is a weighting over the same machinery:

| Mode | Behaviour |
|---|---|
| Keyword | BM25 only |
| Semantic | vector only |
| **Hybrid** (default) | RRF over both |
| Passages | same retrieval, passage-first presentation |

### 6.4 Results and presentation

A normal search returns **entries** ranked by their best-scoring passage, each
carrying that passage as a highlighted snippet. Passage mode inverts the
aggregation: a flat ranked list of passages with their source entry as context.
That is the extractive answer to "RAG" — the same query, different aggregation,
no language model and no per-query cost.

Filters — feed, category, date range, unread-only, starred, and later
"not hidden" — are plain SQL predicates.

### 6.5 Query path

The query is embedded at search time: one short forward pass, single-digit
milliseconds on CPU, with a small LRU cache for repeated queries.

### 6.6 Inference

Embedding runs in-process through ONNX Runtime via CGO — `hugot` over
`onnxruntime_go`, which supplies WordPiece tokenization, the forward pass, and
mean pooling. An int8-quantised bge-small-class model keeps CPU inference fast
enough that the backfill is measured in nights rather than weeks.

The cost is the static-binary property: ONNX Runtime is a shared library that
must be installed on the host. On a single self-hosted machine that is an
acceptable trade, and it is confined to the sidecar — the Miniflux fork still
builds as before.

Inference sits behind a narrow interface so the runtime can be replaced without
touching the pipeline:

```go
type Embedder interface {
    // Embed returns one vector per input text, each of Dimensions() length.
    Embed(ctx context.Context, texts []string) ([][]float32, error)
    Dimensions() int
}
```

A pure-Go implementation (`cybertron`/`spago`, no CGO, roughly 3–5× slower) and
a local-HTTP implementation are both viable behind this interface if the native
dependency becomes inconvenient.

## 7. P2 — Similar articles

Given an entry, retrieve its passages' nearest neighbours excluding passages
from the same entry, aggregate to entry level, and return the top *n*. Same
index, same aggregation logic as P1, different seed vector.

Surfaced as a block on the entry page, and as a "more like this" affordance on
search results — the case from the original goal where the search succeeded and
the next question is what else is like it.

## 8. Fork surface and rebase strategy

### 8.1 Migration ownership

Miniflux migrations are an ordered array executed **by index**, with a single
integer in `schema_version` (`internal/database/database.go:22`,
`internal/database/migrations.go:13`). If the fork appends migration #136 and
upstream later adds its own #136, the database already records v136 and
**upstream's migration silently never runs**. This is a data-corruption class
failure, not a merge conflict.

The fork therefore never touches the `migrations` array. It carries a separate
track — its own array, its own `fork_schema_version` table, executed after
`database.Migrate(db)` returns. Upstream migrations stay pristine and rebasable.

### 8.2 Fork diff inventory

| Area | Change |
|---|---|
| P0 capture | `crawler` default config option; one-time UPDATE |
| P0 retention | `ArchiveEntries` call removed; flush-history UI removed |
| P0 backfill | `full_text_fetched_at` column (fork migration); backfill CLI command |
| P1 search | `showSearchPage` routes to the sidecar instead of `WithSearchQuery` |
| P1 UI | mode picker in the search bar; snippet rendering with highlights |
| P2 UI | similar-articles block on the entry page |
| Shared | HTTP client for the sidecar, with config for its address |

The existing `EntryQueryBuilder.WithSearchQuery`
(`internal/storage/entry_query_builder.go:46`) is left in place and unused by
the UI path, so Fever and Google Reader API search continue to work unchanged
against entry-level FTS.

### 8.3 Degradation

If the sidecar is unreachable the fork falls back to the existing
`WithSearchQuery` path and shows a notice. Search gets worse; the reader keeps
working. No reader page ever blocks on the sidecar.

## 9. Indexing pipeline and admin

### 9.1 Two lanes

The **live lane** handles new entries: a few hundred a day, always on, one
worker, effectively free.

The **backfill lane** handles the historical backlog. It is the only thing that
can overload the machine, and is bounded by a schedule window, a concurrency
ceiling, and an adaptive throttle. Separating the lanes means the backfill can
be paused for a week without new articles falling out of search.

### 9.2 Throttle

Three knobs, all live-editable without a restart:

- **window** — wall-clock hours the backfill may run (e.g. 02:00–07:00), or always
- **concurrency** — worker ceiling, default 2
- **batch size** — passages per forward pass; the main lever on CPU efficiency

An adaptive controller samples system load and per-batch latency every few
seconds and moves concurrency within a `[min, max]` band: backing off when the
1-minute load average crosses a threshold or batch latency degrades past its
baseline, recovering when it settles. "Do not overload the machine" becomes a
property of the system rather than a number to guess. The hard ceiling always
wins over the controller.

### 9.3 Expected duration

At 100k–1M entries and 4–6 passages each, the backlog is roughly 0.5M–5M
embeddings. A quantised bge-small-class model on CPU throttled to two workers
runs on the order of 30–80 passages/sec, putting the full backfill between a few
nights and a couple of weeks.

Search degrades gracefully meanwhile: BM25 is at full coverage from day one and
semantic recall improves as the backfill advances.

### 9.4 Admin surface

The sidecar serves its own status and admin page. This keeps the fork's diff to
reader-facing UI, and indexing operations are a different concern from reading
preferences even though the same person handles both.

It shows progress and ETA, current throughput, live concurrency with the
controller's reason for it, error and skip counts by cause, and pause/resume.

## 10. Error handling

| Failure | Behaviour |
|---|---|
| Scrape fails (paywall, JS-only, non-HTML) | `full_text_fetched_at` stays null; entry indexed from its excerpt |
| Entry has no usable text | `entry_index_state.status = 'skipped'` with a reason; not retried |
| Embedding model error | `status = 'failed'` with a reason; retried on the next pass with backoff |
| Sidecar down | Fork falls back to entry-level FTS with a notice |
| Backfill crash or window close | Resumes from the `entry_index_state` checkpoint |
| Entry content changed | `content_hash` mismatch triggers re-index |

Per-entry failures are recorded and skipped, never retried in a tight loop.

## 11. Testing

**Fork.** Miniflux's existing suites must stay green (`make test`,
`make integration-test`). New coverage: the fork migration track runs
independently of upstream's and neither clobbers the other's version row; the
backfill command is resumable and idempotent; search falls back correctly when
the sidecar is unreachable.

**Sidecar.** Passage splitting is deterministic and offsets round-trip to the
source text exactly. RRF fusion produces the expected order for constructed
rank inputs. The adaptive controller respects its hard ceiling under synthetic
load. Index state transitions are correct across crash and resume.

**Retrieval quality.** A small hand-labelled set of query → expected-entry pairs,
scored as recall@10, run against each mode. Without it there is no way to tell
whether a change to chunking, the model, or fusion weights helped or hurt. This
set should be built during P1, not after.

## 12. Risks

**Surviving history may be thin.** If most of the corpus has already been
tombstoned, the search engine is over recent entries only, and the backfill is
cheap but the product is weaker. Measure first (§5.4).

**Two Postgres extensions.** pgvector and `pg_search` both need to be installed
and upgraded in step with the Postgres major version. This constrains hosting.

**Scrape quality is the ceiling on search quality.** Readability extraction on a
paywalled or JS-rendered site yields navigation chrome. Garbage passages are
indexed as confidently as good ones. The `full_text_fetched_at` signal helps but
does not measure extraction *quality*.

**CPU embedding constrains model choice.** Moving to a larger or
higher-dimension model later means re-embedding the whole corpus — days of
compute. The 384-dimension choice is effectively load-bearing.

**Go's ML libraries are less trodden than Python's.** `hugot`,
`onnxruntime_go` and the pure-Go alternatives are usable but far less
battle-tested than `sentence-transformers`. The P1 plan therefore opens with a
spike that embeds several hundred real passages and measures throughput and
output sanity *before* anything is built on top. If that spike fails, the
`Embedder` interface is the seam at which a local inference service or a Python
component gets substituted.

## 13. Open questions

None blocking. Two to resolve during implementation:

1. Exact embedding model within the bge-small class, chosen by measuring
   recall@10 on the evaluation set rather than by reputation, and confirmed to
   load and run under ONNX Runtime in Go before the pipeline depends on it.
2. Whether passage mode is a separate picker option or a toggle on results,
   which is a UI question best answered by using the search first.
