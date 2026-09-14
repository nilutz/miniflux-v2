# P2 Amendments Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the embedding model swappable and runnable on another machine, surface database growth where it will be seen, and let a reader rate articles — which is the prerequisite for any recommendation work.

**Architecture:** Three independent strands. The model work is sidecar-only and gated behind a safety fix. The admin metrics are sidecar-only and trivial. The rating control is fork-only and touches Miniflux's entry model.

**Tech Stack:** Go 1.27, PostgreSQL 18 via ParadeDB, the pinned hugot/ONNX embedder, Miniflux's own `database/sql` patterns.

**Spec:** `docs/superpowers/specs/2026-09-12-miniflux-search-design.md` §13. Read it — it records *why* each decision went the way it did, and two of them rejected the obvious approach.

## Global Constraints

- **Tasks 1–4 are sidecar-only (`sidecar/`). Tasks 5–6 are fork-only (repo root).** No task touches both.
- The fork must not import `miniflux.app/v2/sidecar/...`. The contract is HTTP and JSON.
- **Only `cmd/sidecar` may import `internal/embed/onnx`** — packages that never do inference must keep linking without the 37MB native library. Verify every task.
- **The fork's diff against upstream stays minimal.** It rebases onto upstream indefinitely; new fork-owned files are cheap, edited upstream files are permanent cost.
- SPDX header and package import comment on every new `.go` file; conventional commits; `log/slog`.
- `golangci-lint` is not installed locally.
- **Never run `sidecar/internal/indexer`'s tests against a database holding a real corpus.** That suite sweeps the entire entries table with a fake embedder and has destroyed this project's corpus once.

## The corpus you are working against

436 entries, 5,681 passages (436 title + 5,245 content), every embedding unit-normalised, on ParadeDB port 5434. It took a network seed and a full re-index to build, and the evaluation harness's 32 labelled queries are meaningless without it. **Treat it as production data.**

---

### Task 1: Model identity in the content hash

**Files:**
- Modify: `sidecar/internal/store/entries.go`, `entries_test.go`
- Modify: `sidecar/internal/indexer/indexer.go` and its callers as needed

**Interfaces:**
- Consumes: `embed.Embedder`
- Produces: `embed.Embedder.Identity() string` (or equivalent); `store.contentHash` covering it

**This gates everything else in the model strand.** Spec §13.1: `contentHash` is `md5(pipelineVersion + title + content)` and does not include the model, so switching models changes no hashes, marks nothing pending, and leaves one HNSW graph holding vectors from two models. Cosine similarity across models is meaningless — search returns plausible wrong results, silently.

- [ ] **Step 1: Write the failing tests**

- an entry indexed under model A becomes pending when the configured model changes to B
- it does **not** become pending when the model is unchanged
- the identity includes dimensions, so two models that differ only in width are distinguished
- `PendingEntryIDs` and `PendingEntryCount` agree with the new hash (they each carry a copy of the predicate — keep them in step, as an earlier task already had to)

- [ ] **Step 2: Extend the `Embedder` interface**

Add an identity method returning a stable string — model name, revision, and dimensions. It must change when any of those change and must not change otherwise. The ONNX implementation derives it from its configured model; a future HTTP implementation reports what the remote says it is running.

- [ ] **Step 3: Fold it into the hash**

`contentHash(title, content)` becomes model-aware. Update **both** SQL predicates — `PendingEntryIDs` and `PendingEntryCount` — and the test-side copy, so they cannot drift.

- [ ] **Step 4: Verify and commit**

```bash
cd sidecar && go test -count=1 -tags ORT ./internal/store/ ./internal/indexer/ -v
make lint
git commit -m "feat(sidecar): fold the embedding model's identity into the content hash"
```

---

### Task 2: Remote embedding over HTTP

**Files:**
- Create: `sidecar/internal/embed/remote/remote.go`, `remote_test.go`
- Modify: `sidecar/cmd/sidecar/main.go`

**Interfaces:**
- Consumes: `embed.Embedder`, Task 1's identity
- Produces: `remote.New(cfg) (embed.Embedder, error)`

**Package placement matters.** The interface lives in `internal/embed`; ONNX lives in `internal/embed/onnx` specifically so packages that never do inference link without native libraries. Put the HTTP client in `internal/embed/remote` for the same reason — it has no CGO, so it must not drag any in.

- [ ] **Step 1: Write the failing tests**

Use `httptest`:

- a batch round-trips and returns vectors of the declared dimension
- **the remote's reported identity is surfaced**, so Task 1's hash sees the real model rather than a configured guess
- a dimension mismatch between what the remote reports and what the schema holds is a **loud error at startup**, not a runtime surprise
- connection failure, timeout, non-200 and malformed body each return an error
- the client honours an explicit timeout

- [ ] **Step 2: Implement**

A small JSON protocol: send texts, receive vectors plus the model identity. Define it in this package — do not invent a dependency on a specific serving framework. Document the contract in `sidecar/README.md` so someone can stand up a conforming server.

- [ ] **Step 3: Wire it into `main.go`**

`SIDECAR_EMBEDDER=local|remote` with `SIDECAR_REMOTE_EMBEDDER_URL`. Default `local`, so nothing changes for an existing deployment.

- [ ] **Step 4: Verify and commit**

Confirm `go test -count=1 -tags ORT ./internal/...` still links without `libtokenizers.a`.

---

### Task 3: Pause indexing when the remote is unreachable

**Files:**
- Modify: `sidecar/internal/indexer/backfill.go`, `live.go`, and their tests
- Modify: `sidecar/internal/web/` status page and JSON

**Spec §13.1 decision, and it rejected the alternative:** when the remote embedder is unreachable, **indexing pauses — it does not fall back to local.** A silent fallback produces a corpus embedded by two different paths with no record of which is which, and if the remote runs a different model that is exactly the corruption Task 1 exists to prevent.

- [ ] **Step 1: Write the failing tests**

- an embedder that reports unreachable pauses the backfill rather than marking entries failed
- **the pause is distinguishable from an ordinary pause** — the admin page must say *why*, and "paused by operator" and "paused: embedder unreachable" are different states
- indexing resumes automatically when the embedder returns
- **entries are not marked `failed` during the outage.** They must stay pending. Marking thousands of entries failed during a five-minute network blip, then relying on retry backoff to unwind it, is the failure mode to avoid.
- the live lane behaves the same way

- [ ] **Step 2: Implement**

Distinguish "this entry failed" from "the embedder is unavailable". The existing retry machinery handles the former; the latter is a lane-level state.

- [ ] **Step 3: Surface it**

The status page shows the pause and its reason. An operator whose backfill stopped needs to know whether to look at the network or the data.

- [ ] **Step 4: Verify and commit**

---

### Task 4: Database metrics in the admin page

**Files:**
- Create: `sidecar/internal/store/metrics.go`, `metrics_test.go`
- Modify: `sidecar/internal/web/server.go`, `templates/status.html`

**Spec §13.2.** The measured corpus is 13 passages per entry, not the 4–6 §5.5 assumed, and §5.5's disk estimate is wrong by roughly 5×. This exists so the next such surprise is visible early.

- [ ] **Step 1: Write the failing tests**

- total database size, `search` schema size, and the HNSW index size specifically
- passage and entry counts, and passages-per-entry
- **dead tuple count for `search.passages`** — a stale `VACUUM` silently truncates HNSW scans, which cost real debugging time in this project already
- the query is cheap enough to run on a page that refreshes every 10 seconds, or it is cached — an earlier task had to fix exactly this mistake with `PendingEntryCount`

- [ ] **Step 2: Implement**

`pg_database_size`, `pg_total_relation_size`, `pg_relation_size` on the index, and `pg_stat_user_tables.n_dead_tup`. None of these need to touch `entries.content`, so none should be expensive — verify with `EXPLAIN` rather than assuming.

- [ ] **Step 3: Render it**

A section on the status page. Include passages-per-entry, because that ratio is what makes growth predictable and it is what the spec got wrong.

- [ ] **Step 4: Verify and commit**

---

### Task 5: The `hidden` flag

**Files:**
- Modify: `internal/database/fork_migrations.go` (append), `internal/model/entry.go`
- Modify: `internal/storage/entry_query_builder.go`, and the storage/API paths that set it
- Create: fork-owned test files

**Spec §13.3, and it rejected the obvious approach.** `hidden` is a **separate boolean, exactly as `starred` already is** — not a third `status` value. Every Go path assumes two statuses, and Fever and Google Reader expose read/unread with no concept of hidden, so a third value forces a mapping decision in two legacy APIs and touches every status call site, permanently enlarging the fork's rebase surface.

**What hiding does:** drops the entry from the unread list and its counts. Nothing else. It stays searchable, stays in the corpus, stays reachable through feed and category views.

- [ ] **Step 1: Write the failing tests**

- a hidden entry does not appear in the unread list
- it does not count toward unread badges
- **it is still returned by search, and still appears in feed and category views**
- unhiding restores it
- **Fever and Google Reader responses are byte-identical whether or not an entry is hidden** — this is the property that makes the decision worth it, and it is the one most likely to be broken later

- [ ] **Step 2: Append the fork migration**

`entries.hidden boolean not null default false`, plus a partial index if the unread query needs one. **Append only** — never reorder or edit an existing migration; upstream runs by array index and a reordering silently shadows an upstream migration after a rebase.

- [ ] **Step 3: Implement**

A builder method mirroring the existing starred handling, and the unread paths filtering on it. Follow `starred`'s shape — the precedent exists, and matching it keeps the diff legible to someone rebasing.

- [ ] **Step 4: Verify and commit**

```bash
make test && make lint
```

---

### Task 6: The rating control

**Files:**
- Modify: `internal/ui/` handlers for the new action, `internal/ui/ui.go` for the route
- Modify: `internal/template/templates/views/` unread and entry templates
- Modify: `internal/locale/translations/*.json`

**Interfaces:**
- Consumes: Task 5's `hidden` flag and the existing `starred`

- [ ] **Step 1: Add the hide action**

A control beside the existing read/unread toggle. Match the surrounding markup and behaviour; the templates have an established shape for entry actions.

- [ ] **Step 2: Surface star where it is useful**

`starred` already exists and already has a control. Make sure the four states read as one coherent set to the user rather than two unrelated features — the point is rating an article, not toggling two independent flags.

- [ ] **Step 3: Translations**

Every new user-visible string needs a key in **all** translation files. `make add-string KEY=... VAL=...` inserts into every language at once; `TestMissingTranslations` fails otherwise, and it is easy to add English and forget the other 22.

- [ ] **Step 4: Verify**

```bash
make test && make lint
```

Exercise it in a browser against the seeded corpus: hide an article, confirm it leaves the unread list, confirm it is still findable by search, unhide it, confirm it returns.

- [ ] **Step 5: Commit**

---

## Done criteria

- [ ] `make test` and `make lint` pass at the repo root; the sidecar's hermetic packages pass
- [ ] `go test -count=1 -tags ORT ./internal/...` links without `libtokenizers.a`
- [ ] Changing the configured model marks every entry pending; leaving it unchanged does not
- [ ] An unreachable remote embedder pauses indexing with a distinguishable reason, and does not mark entries failed
- [ ] The admin page shows database size, passages-per-entry, and dead tuples
- [ ] A hidden entry leaves the unread list, stays searchable, and is invisible to Fever and Google Reader
- [ ] The corpus is intact: 436 entries, 5,681 passages, embeddings unit-normalised

## What this deliberately does not do

**It does not switch models.** It makes switching *safe* and *possible*. Choosing a different model is a decision to make with the evaluation harness — change the chunking or the model, re-index, re-run the 32 labelled queries, and compare. That is a measurement, not an implementation, and it should be driven by a number rather than by a plan.

**It does not build recommendations.** Task 6 produces the signal — star, read, unread, hide — that the daily best-of needs. What to do with that signal is its own design round, and it should not start until there is real rating data to look at.
