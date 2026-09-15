# nomic-embed-text-v1.5 Migration Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task.

**Goal:** Replace `bge-small-en-v1.5` (384-d, 512-token) with
`nomic-ai/nomic-embed-text-v1.5` (768-d, 8192-token) as the sidecar's
embedding model, including the asymmetric query/document prefixes it
requires.

**Architecture:** The sidecar's `embed.Embedder` gains a document/query
distinction; the `search.passages.embedding` column widens to
`vector(768)`; the chunker is re-tuned for a context 16× larger. Task 1 of
the P2 plan already folds the model's identity into the content hash, so the
switch marks every entry pending and re-indexes rather than mixing two
models' vectors in one HNSW graph (spec §13.1).

**Tech Stack:** Go, hugot v0.7.8 (`-tags ORT`), ONNX Runtime v1.30.0,
daulet/tokenizers v1.27.0, pgvector, ParadeDB.

**Spec:** `docs/superpowers/specs/2026-09-12-miniflux-search-design.md`,
§6.7 (inference), §13.1 (model identity and remote embedding).

## Global Constraints

- **Feasibility is established, not assumed.** A throwaway spike loaded the
  model through the identical hugot call sequence and measured: 768 dims,
  L2 norm 1.000000, `cosine(cat,kitten)=0.9197 > cosine(cat,transformer)=0.6359`,
  8,120 tokens embedded without truncation, ~13 passages/sec apples-to-apples
  against the 33.9 baseline. Findings:
  `/private/tmp/claude-501/-Users-nico-Dev-miniflux-v2/15940a9c-8c0f-45a5-b91c-1f83d1d2a02f/scratchpad/spike-nomic/FINDINGS.md`
- **`config.json` lies about the context.** It declares
  `max_position_embeddings: 2048`; the tokenizer declares
  `model_max_length: 8192` and the rotary export empirically honours 8192.
  **Never cap on `config.json`'s value** — doing so silently discards three
  quarters of the context this migration exists to obtain.
- **The prefixes are mandatory and their absence is silent.** Documents must
  be embedded as `search_document: <text>`, queries as `search_query: <text>`.
  Omitting them produces no error and degrades retrieval.
- Model file: `onnx/model_quantized.onnx` from
  `nomic-ai/nomic-embed-text-v1.5`, int8, ~131 MiB.
- `-tags ORT` is mandatory; without it the build silently selects a ~10×
  slower pure-Go backend.
- Do not touch the fork (`internal/...` outside `sidecar/`). This is entirely
  a sidecar change.
- Every test must be proven to discriminate: break the code it covers, run
  it, confirm it fails for the right reason, restore. Confirm the test
  actually ran — `go test -run <pattern>` prints `ok` when the pattern
  matches nothing.

---

### Task 1: Asymmetric embedding — documents versus queries

**Files:**
- Modify: `sidecar/internal/embed/embed.go`, `internal/embed/onnx/onnx.go`,
  `internal/embed/remote/remote.go`, `internal/search/querycache.go`,
  `internal/indexer/indexer.go`, and their tests.

**Interfaces:**
- Produces: a document/query distinction on `embed.Embedder` that every
  implementation and both call sites honour.

Today `Embed(ctx, texts)` is called identically for passages
(`internal/indexer`) and for the search query
(`internal/search/querycache.go:113`). nomic requires different prefixes for
each, so one method cannot serve both.

**The design decision is yours to make and to argue in the report.** Two
shapes are plausible: separate `EmbedDocuments`/`EmbedQuery` methods, or a
single method taking a task parameter. Consider which makes the *remote*
protocol cleaner — Task 2 of the P2 plan defined a wire format carrying only
`{"texts": [...]}`, and it must now carry the distinction too, without
breaking the dimension and identity checks already built around it.

Whatever you choose, an implementation that forgets to apply a prefix must
fail loudly rather than silently, because a missing prefix has no visible
symptom. Say in the report how you achieved that.

**bge-small does not use prefixes.** Keep it working — the local ONNX
embedder must apply nomic's prefixes only for nomic. Do not hardcode a
prefix into the generic path.

- [ ] **Step 1:** Write failing tests for both call paths and both embedder
      implementations, including a test that a remote embedder receives the
      task distinction over the wire.
- [ ] **Step 2:** Implement the interface change and both implementations.
- [ ] **Step 3:** Update the two call sites.
- [ ] **Step 4:** Discrimination proof for each new test; commit.

---

### Task 2: The model swap and the 768-dimension migration

**Files:**
- Modify: `sidecar/internal/embed/onnx/onnx.go`, `internal/store/migrations.go`,
  `sidecar/Dockerfile`, `sidecar/README.md`.

**Interfaces:**
- Consumes: Task 1's asymmetric embedding.

The `dimensions` constant is `384` (`onnx.go:33`) and the column is
`public.vector(384)` (`migrations.go:36`). Both change to 768, and the HNSW
index must be rebuilt.

**Read Task 12's brief in `.superpowers/sdd/2026-09-14-p2-amendments/` before
writing the index rebuild** — it covers `maintenance_work_mem` and the
autovacuum settings for exactly this table, and a 768-d index is roughly
twice the size that analysis assumed.

`remote.go`'s `wantDimensions` constant (384) must move in lockstep, or a
conforming remote server will be rejected at startup for reporting the
correct width.

The Dockerfile's `MODEL_REPO` arg becomes `nomic-ai/nomic-embed-text-v1.5`.
Note the model directory basename feeds the embedder's identity, which feeds
the content hash — so the directory must be named after the model, as the
existing Dockerfile comment explains.

- [ ] **Step 1:** Write the failing migration test — an existing `vector(384)`
      column migrates to `vector(768)` with the index rebuilt, and the
      migration is idempotent.
- [ ] **Step 2:** Implement the migration and the constant changes.
- [ ] **Step 3:** Update the Dockerfile and README; verify the image builds
      and the sidecar reports `backend=ORT`, 768 dimensions, and an identity
      naming nomic.
- [ ] **Step 4:** Discrimination proof; commit.

---

### Task 3: Re-tune the chunker for an 8192-token context

**Files:**
- Modify: `sidecar/internal/passage/split.go` and its tests.

**Supersedes Task 13 of the P2 amendments** — fold in its word-versus-token
fix, whose brief is at `.superpowers/sdd/2026-09-14-p2-amendments/task-13-brief.md`.

`wordCount` is `len(strings.Fields(s))`, so every "token" figure in
`SplitOptions` is a **word** count. Against bge-small that was a silent
truncation risk (512 words ≈ 660–700 real tokens, past its 512 limit).
Against nomic the cap stops being a truncation risk and becomes an
arbitrarily small fraction of an 8192-token window — the defaults would use
about 5% of the context this migration is buying.

Deliver: honest units (see the Task 13 brief for the two acceptable
approaches and the package-split constraint that rules out importing the
tokenizer into `internal/passage`), and defaults that actually use the new
context.

**Do not guess the new defaults.** Task 13's brief also specifies a
repeatable sweep tool reporting per-mode recall@10 alongside passages-per-entry.
Build that, and state clearly in the report that the numbers must come from
running it rather than from reasoning. Baseline to beat: keyword 0.828,
semantic 0.955, hybrid 0.932, passages 0.919, at 13 passages per entry.

- [ ] **Step 1:** Failing test that a passage at the cap does not exceed the
      model's real token limit.
- [ ] **Step 2:** Fix the units; implement the sweep tool.
- [ ] **Step 3:** Discrimination proof; commit.

---

### Task 4: Re-index and measure

**Not code — an operator procedure, run deliberately.**

The corpus lives in `sidecar-db-1` (port 5434), currently stopped. At the
measured ~13 passages/sec the existing 5,681 passages re-embed in roughly
seven minutes of pure inference, longer in wall-clock under the backfill
throttle — and the passage count itself changes once Task 3 lands.

Deliver a documented procedure covering: bring the corpus up, apply the
migration, let the backfill re-index (it starts automatically — every hash
changed), verify the integrity guard passes, then run the eval and compare
per-mode recall@10 against the baseline above.

**State plainly that a recall regression is a real possible outcome** and
that the decision to keep or revert belongs to the operator with the numbers
in hand. This plan makes the switch possible and safe; it does not assert it
is an improvement.

## Done criteria

- [ ] Both embedder implementations apply the correct prefix; a missing
      prefix fails loudly.
- [ ] `search.passages.embedding` is `vector(768)` with a rebuilt HNSW index.
- [ ] The sidecar starts reporting `backend=ORT`, 768 dimensions, and an
      identity naming nomic.
- [ ] The chunker's units are honest and its defaults use the 8192 context.
- [ ] The sweep tool reports per-mode recall@10 and passages-per-entry.
- [ ] `go test -tags ORT ./...` green; `make lint` clean.
