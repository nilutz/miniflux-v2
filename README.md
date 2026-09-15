Miniflux 2 + semantic search
============================

A fork of [Miniflux](https://miniflux.app) that turns your feed reader into a
searchable corpus of everything you have read — and lets an AI agent query it.

Upstream Miniflux is a minimalist, opinionated feed reader: single binary,
PostgreSQL only, no ORM, no web framework. All of that is unchanged and
still true here. See [upstream's README](https://github.com/miniflux/v2) for
the base feature list, and `CONTRIBUTING.md` for the philosophy this fork
tries not to violate.

What this fork adds is below.

---

Search over what you have actually read
---------------------------------------

Miniflux ships full-text search over Postgres. This fork keeps that and adds
a **search sidecar**: a separate Go service, sharing one database, that
indexes every article into overlapping passages and embeds them with a local
neural model.

Five modes, pickable from the search bar:

| mode | what it does |
|---|---|
| **keyword** | BM25 via ParadeDB's `pg_search` — real IDF ranking, not `ts_rank` |
| **semantic** | vector similarity over passage embeddings (pgvector/HNSW) |
| **hybrid** | both, fused by Reciprocal Rank Fusion (k=60) — the default |
| **passages** | the same retrieval, but returns the matching extracts |
| **fulltext** | Miniflux's own Postgres FTS, unchanged |

Fusion is by **rank position, not score**, because BM25 scores and cosine
distances are not comparable quantities and averaging them is meaningless.

**Articles are scraped in full.** Upstream only fetches the original page
when a feed's crawler flag is set, and that flag is off by default — so the
corpus is mostly RSS excerpts, which is very little to search.
`CRAWLER_ENABLED_BY_DEFAULT=1` turns it on for new feeds, and
`-backfill-full-text` fills in everything that predates it.

### More like this

Every article page offers its nearest neighbours by vector similarity, and
so do search results. Per-passage neighbours are merged by best distance, so
one strongly-matching paragraph surfaces the article.

Rate what you read
------------------

A four-state control on the article list: **star** (exceptional), **read**,
**unread**, **hide** (not interested).

`hidden` is a separate boolean, deliberately not a third `status` value —
every existing status path keeps working, and Fever and Google Reader never
see the flag. Hiding drops an article from the unread list and its counts
**and nothing else**: it stays searchable, stays in the corpus, stays
reachable in feed and category views. Hiding means "not now", not "never".

Bulk actions: mark a page hidden, mark a feed or category hidden, and **hide
the whole backlog when subscribing** — so you can follow a blog from today
without importing its archive as unread.

Bulk-hidden entries record `hidden_reason = 'bulk'`. The four states are a
preference signal, and an archive you never looked at is not a judgement —
without that distinction, a future recommender would learn from hundreds of
false negatives.

Save a page that has no feed
----------------------------

Paste a URL, get it fetched, parsed and indexed as a search source — not as
a subscription. It becomes an entry in a per-user, disabled synthetic feed,
so the scheduler never refreshes it, and the sidecar indexes it like any
other entry with no changes at all.

Query it from an AI agent
-------------------------

`cmd/mcp` is an [MCP](https://modelcontextprotocol.io) server over stdio.
Point Claude Code at it and your agent can search your own reading:

```sh
go build -o ~/bin/miniflux-mcp ./sidecar/cmd/mcp
claude mcp add miniflux-search -- ~/bin/miniflux-mcp
```

Three tools: `search`, `similar`, and `fetch_article` (full text, so the
agent can read what it found rather than only a snippet). It is a plain HTTP
client — no CGO, no model, no native libraries — so it installs anywhere.

Set `MINIFLUX_API_KEY` to a key from Miniflux's Settings → API Keys.

Operate it
----------

The sidecar serves an admin page with four sections:

- **Status** — indexing progress, skips and failures by cause, and whether
  either lane is paused (and *why*: an operator pause and an unreachable
  embedder are different states).
- **Database** — total size, `search` schema size, the HNSW index size on
  its own, dead tuples, and **passages per entry**. That ratio is what makes
  growth predictable; the original design estimate was wrong by about 5×.
- **Indexing** — workers, batch size, schedule window, pause/resume.
- **Model** — which model is running, its identity and dimensions, and
  whether the backend is the real ONNX runtime or the ~10× slower pure-Go
  fallback. Switch between a local model and a remote embedding server, with
  a test-before-apply probe.

### Embedding elsewhere

Embedding can run on another machine over a small JSON protocol — a GPU box
does in minutes what a laptop does in hours. If the remote is unreachable,
indexing **pauses**; it never silently falls back to a local model, because
a corpus embedded by two different models is quietly wrong rather than
loudly broken.

The model's identity is folded into each entry's content hash, so changing
models marks the corpus stale and re-indexes it instead of mixing
incompatible vectors in one index.

Authentication
--------------

The sidecar has no accounts of its own. It authenticates against Miniflux's
own tables in the shared database:

- **API keys** (`X-Auth-Token`) for programmatic callers, including the MCP
  server.
- **Miniflux's session cookie** for the browser — sign into Miniflux and the
  sidecar's page just works. No second login.

Search results are scoped to the key's owner, so a caller cannot ask for
someone else's corpus. Control endpoints and the admin page require an
**admin** user.

Two things worth knowing: there is **no TLS** in the compose files (put a
reverse proxy in front of anything reachable over a network), and Miniflux
stores API keys in plaintext — that is upstream's schema, and it means
database access is full access.

Install
-------

```sh
cp .env.example .env    # fill in every value; there are no defaults
docker compose up -d
```

Images are published on each `v*` tag:

```
ghcr.io/nilutz/miniflux
ghcr.io/nilutz/miniflux-sidecar
```

The database is **ParadeDB**, not stock PostgreSQL: it ships `pg_search`
(BM25) and `vector` (pgvector), and stock Postgres has neither.

For development, `docker-compose.dev.yaml` builds both images from source.
See `SEARCH.md` for a from-zero walkthrough and `sidecar/README.md` for the
sidecar's own documentation, including the native dependencies its build
needs.

Under the hood
--------------

- **`sidecar/`** is a separate Go module. The fork never imports it; they
  talk over HTTP, so the fork stays close to upstream and rebasable.
- **Fork migrations run on their own track** (`internal/database/fork_migrations.go`),
  separate from upstream's array — appending there would occupy an index
  upstream may later use, and after a rebase that upstream migration would
  silently never run.
- **Embeddings** come from `nomic-embed-text-v1.5` (768 dimensions, 8192-token
  context) via ONNX Runtime, or from a remote server speaking the same
  protocol.

Honest limitations
------------------

- **Retrieval quality on the current model is unmeasured.** An evaluation
  harness exists (`sidecar/internal/search/eval`) and earlier numbers were
  measured on a different, smaller model against a corpus that has since
  been deleted. Those numbers do not describe what runs today, so they are
  not quoted here.
- **The chunker is still tuned for a 512-token model.** It targets ~320
  words per passage, which uses a small fraction of the current model's
  8192-token window. A sweep tool exists to pick better values by
  measurement; it needs a corpus and re-labelled evaluation queries first.
- **Daily best-of and blog discovery are not built.** The rating control
  produces the signal they would need.
- Search asks the sidecar and falls back to Miniflux's own full-text search
  when it is unavailable, so the reader keeps working without it.

License
-------

Apache 2.0, as upstream. See `LICENSE`.
