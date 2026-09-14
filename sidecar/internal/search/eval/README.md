# Retrieval evaluation harness

Implements spec §11's requirement for P1: a recall@k metric (`RecallAtK`,
`Report`) plus a loader for a hand-labelled query set (`LoadQueries`), so
that later decisions in the P1b plan — chunking, model choice, fusion
weights, the RRF *k* — can be checked against something rather than tuned
by vibes.

## What this measures, honestly

**This is a regression harness, not a benchmark.** `testdata/queries.json`
holds 32 queries hand-labelled against a corpus of 436 real articles. That
is enough to notice "this change made keyword search worse" or "hybrid
still isn't beating semantic-only on this set" — the two things spec §11
and Task 4 actually need. It is not enough to claim an absolute recall
number generalizes beyond this corpus, and it is far too small and too
English-tech-blog-shaped to support any claim about retrieval quality on a
real, multi-hundred-thousand-entry personal feed corpus. Treat every
number this produces as "relative to the last time we ran it," not as
"the system is N% good."

A few specific limits worth naming:

- **Multi-relevant queries cap recall@10 below 1.0 structurally.** The
  "React Server Components" query has 14 labelled relevant entries;
  recall@10 tops out at 10/14 ≈ 0.71 even for perfect retrieval. That is
  not a retrieval failure, it's arithmetic — don't read a mean recall
  below 1.0 as "the system missed something" without checking which
  queries have more than 10 relevant documents.
- **The labelled relevant sets for multi-document topic queries are
  best-effort, not exhaustive.** For "how CPU caches affect performance
  and cause reliability incidents" and the AI-coding-agents query, each
  listed entry id was individually read and confirmed on-topic, but the
  corpus was not exhaustively searched for every conceivably-relevant
  entry — a handful of borderline entries that mention the topic in
  passing were deliberately left unlabelled rather than guessed at. This
  means real recall against "every entry a generous judge might accept"
  is very likely *higher* than what gets scored, not lower.
- **The corpus is nine tech/security blogs, one topic shape.** No
  recipes, no local news, no non-English content, no podcast-only feeds
  without transcripts — whatever this set says about BM25 vs. vector vs.
  hybrid on prose-heavy English technical writing, it says nothing about
  other content shapes a real personal feed corpus will contain.
- **Small N means single-query noise moves the mean a lot.** With 32
  queries, one query flipping from recall 1.0 to 0.0 moves the overall
  mean by about 3 points. Look at `Report.PerQuery` before concluding
  anything from `Report.MeanRecall` alone.

## The corpus

Seeded 2026-09-14 via `sidecar/scripts/seed-eval-corpus.sh` from nine real
RSS/Atom feeds (see that script for the exact list and URLs), subscribed
with the crawler on so full page text is scraped for every entry, not just
whichever feeds happen to publish full content:

| Feed | Entries |
|---|---|
| Cloudflare Blog | 20 |
| Dan Luu | 128 |
| Google Research Blog | 100 |
| Julia Evans | 20 |
| Martin Fowler | 30 |
| Overreacted (Dan Abramov) | 58 |
| Schneier on Security | 10 |
| Simon Willison | 30 |
| Stack Overflow Blog | 40 |
| **Total** | **436** |

All 436 entries indexed cleanly into `search.passages` on the first
backfill run (436 indexed, 0 skipped, 0 failed; 5,245 passages) — no
network or parsing failures to report. Some Cloudflare Blog pages carry a
large scraped tag-cloud alongside the real article text (a property of
crawling the live page rather than the feed's own content), which is
realistic noise this evaluation set was deliberately labelled against
rather than around.

Re-running the seed script is safe (it's idempotent: existing admin user,
feeds and already-fetched entries are left alone) but will not reproduce
this exact corpus indefinitely — these are live blogs and their feeds will
have moved on. If the corpus needs rebuilding from scratch, re-run the
script, re-run the sidecar's backfill to drain `search.entry_index_state`,
and re-derive `testdata/queries.json`'s entry ids against whatever lands.

## The labels

32 queries in `testdata/queries.json`, each with a `note` field recording
which category it was chosen for and, for anything non-obvious, why the
labelled entry ids are the right ones. Every relevant entry id was read in
the database (`entries.content`, plus for a couple of judgment calls the
actual passage-level term frequency in `search.passages`) before being
recorded — not inferred from a title alone. Categories, per the task
brief:

- **Close wording** (6 queries) — the query is at or near the article's
  own title; BM25 should have the advantage.
- **Paraphrase** (9 queries) — the query deliberately avoids the article's
  own vocabulary; vector search should have the advantage. Two of these
  share a target article with a close-wording query on the same topic
  (`monorepos`/"arguments against splitting...", `keyboard latency`/
  "measuring how slow keyboards feel...") specifically so the two
  retrieval styles can be compared head-to-head on the same document.
- **Rare discriminating term** (6 queries) — a query where one rare,
  specific token (`wrapture`, `su3su2u1`, an RFC number, a byte count, a
  compression library name, `TF-IDF`) is surrounded by common words. Each
  rare term's corpus-wide passage frequency was checked directly against
  `search.passages` (2–23 hits out of 5,245 passages) before being used —
  this is exactly the case spec §6.2 argues BM25's IDF handles better than
  Postgres' plain `ts_rank`. If a later task finds BM25 doesn't actually
  win these, that is a real finding against §6.2, not a labelling bug.
- **Short queries** (5 queries, 1–2 words) and **sentence-length queries**
  (3, plus the three multi-relevant queries below) — covers the length
  extremes real search-bar input takes.
- **Multi-relevant topic queries** (3 queries, 4–14 relevant entries each)
  — added beyond the brief's minimum because a single-relevant-document
  query makes recall@10 nearly binary (1.0 or 0.0) and says little about
  ranking quality; a query with several genuinely relevant documents
  gives `RecallAtK` room to produce a meaningful fraction.

## Using this package

```go
queries, err := eval.LoadQueries("testdata/queries.json")
// for each query, run it through a retrieval mode to get a ranked
// []int64 of entry ids, then:
recall := eval.RecallAtK(results, query.RelevantEntryIDs, 10)
// collect eval.PerQueryResult{Query: query, Recall: recall} across all
// queries, then:
report := eval.NewReport(perQuery)
```

This package does no retrieval itself (`go.mod`'s dependency graph has
nothing pointing the other way) — Task 4 is where `LoadQueries` output
actually gets driven through keyword, semantic, hybrid and passage
retrieval and turned into a `Report` per mode.

## Hermeticity

`eval_test.go` uses no database, no embedder, and no network — it is safe
to run with a bare `go test ./internal/search/eval/...`, no `-tags ORT`
and no `SIDECAR_DATABASE_URL` required. `TestLoadQueriesLoadsTheRealEvalSet`
reads `testdata/queries.json` from disk, which is a static committed file,
not a live corpus — that stays within the hermetic contract.
