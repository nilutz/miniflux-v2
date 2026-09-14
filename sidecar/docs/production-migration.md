# Production migration runbook: PostgreSQL 17 → ParadeDB (PostgreSQL 18)

This is the procedure for moving a **live** Miniflux instance onto the
database the search sidecar requires. It is scheduled-downtime work, and the
one step in the whole P1 plan that can destroy data if botched. Read it
fully before starting, especially §7 (rollback).

## Why this migration exists

Stock PostgreSQL ships neither extension the sidecar's schema needs —
verified directly: a `postgres:17-alpine` lists 59 available extensions,
and neither `vector` nor `pg_search` is among them. The `paradedb/paradedb`
image ships `vector` 0.8.4 and `pg_search` 0.25.9 — but on **PostgreSQL
18**, while a typical existing Miniflux instance runs PostgreSQL 17. There
is no in-place, same-major-version path to get both extensions: the
database itself has to move to a new major version.

**This is a known-good path, not a guess.** During the design of this plan,
Miniflux's own migrator (`./miniflux -migrate`) was run directly against a
`paradedb/paradedb` (PostgreSQL 18) container and applied all **134
upstream migrations plus both fork migrations** cleanly:
`schema_version = 134`, `fork_schema_version = 2`, and the fork's
`entries.full_text_fetched_at` column present and correct. Miniflux's
schema is confirmed compatible with PostgreSQL 18 / ParadeDB. What was not
previously exercised — and what this runbook's rehearsal (§8) exists to
close — is the *dump-and-restore path* by which a live PostgreSQL 17
database with real data gets onto that PostgreSQL 18 server intact.

## Before you start

Decide on a maintenance window. Miniflux must be stopped for the whole
dump (§3–4); expect it to be unavailable for that entire window, not just
the instant of cutover.

Get `psql`/`pg_dump`/`pg_restore` binaries whose version is **at least the
newer of the two servers involved** (i.e. at least PostgreSQL 18) — this is
the standard PostgreSQL cross-version upgrade guidance: an older client
dumping a newer, or restoring a mismatch, is unsupported and can silently
skip newer syntax. The ParadeDB image itself ships matching PostgreSQL 18
client tools, so if both databases are reachable from a host that can
`docker exec` into the ParadeDB container, that container's own `pg_dump` /
`pg_restore` is a convenient way to satisfy this without installing
anything extra (see the exact commands in §4 and §8 — the rehearsal used
exactly this). This assumes a Docker-based deployment where that container
can reach the source server (e.g. via `host.docker.internal` or a shared
Docker network) — in a non-Docker deployment, install matching-version
`postgresql-client` packages on whichever host runs the dump/restore
instead.

### 1. Record what you have

Connect to the **live PostgreSQL 17** database and record:

```sql
SELECT count(*) FROM entries;
SELECT min(published_at) FROM entries;
SELECT count(*) FROM entry_tombstones;
```

Write all three numbers down somewhere outside the database (a chat
message, a ticket, a sticky note — anywhere that survives the migration
regardless of what happens to it). They are:

- **`count(*) FROM entries`** — the number every later row-count check
  (§5) must match exactly. Any difference after restore means something
  was dropped or duplicated and the migration must not be trusted.
- **`min(published_at)`** — the oldest entry surviving in the database.
  Sanity-checks that the restore didn't silently truncate history.
- **`count(*) FROM entry_tombstones`** — not a migration safety check, but
  worth reading before investing further: this is how much history the
  retention job (spec §5.2, §5.4) already destroyed *before* the corpus
  fix landed, permanently and unrecoverably from the feed. A very large
  number here is a sign the corpus this migration is protecting starts
  thinner than expected — worth knowing before scheduling the downtime for
  the migration, not after.

### 2. Back up — and verify the backup, before touching the original

```sh
pg_dump -h <SRC_HOST> -p <SRC_PORT> -U <user> -d <dbname> -Fc -f miniflux.dump
```

`-Fc` (custom format) is required — it's what `pg_restore` needs and what
allows restoring into a differently-shaped scratch database first.

**An unverified backup is not a backup.** Before this dump file is trusted
with the real cutover, prove it actually restores, into a **scratch**
database that is *not* the eventual production target:

```sh
createdb -h <SCRATCH_HOST> -p <SCRATCH_PORT> -U <user> migration_verify
pg_restore -h <SCRATCH_HOST> -p <SCRATCH_PORT> -U <user> \
  -d migration_verify --no-owner --no-acl miniflux.dump
```

Then check the same three numbers from step 1 against this scratch
restore:

```sh
psql -h <SCRATCH_HOST> -p <SCRATCH_PORT> -U <user> -d migration_verify \
  -c "SELECT count(*) FROM entries;" \
  -c "SELECT min(published_at) FROM entries;" \
  -c "SELECT count(*) FROM entry_tombstones;"
```

If they don't match, **stop** — do not proceed to step 3. Something is
wrong with the dump or the connection details, and it needs to be found
now, with the original database still live and untouched, not after
Miniflux has already been stopped. Once satisfied, drop the verification
database:

```sh
dropdb -h <SCRATCH_HOST> -p <SCRATCH_PORT> -U <user> migration_verify
```

### 3. Stop Miniflux

The daemon must not be writing to the database while the real dump (§4)
runs, or the dump can capture a torn, inconsistent snapshot.

Stop however this deployment normally stops the process — `systemctl stop
miniflux`, `docker compose stop miniflux`, killing the supervised process,
etc. Confirm it is actually down (no listener on its port, no PID) before
continuing. Nothing else should write to this database until §5 either
confirms success or §7 rolls back.

### 4. Dump, start ParadeDB, restore

With Miniflux stopped, take the real dump:

```sh
pg_dump -h <SRC_HOST> -p <SRC_PORT> -U <user> -d <dbname> -Fc -f miniflux.dump
```

Bring up the ParadeDB (PostgreSQL 18) server if it isn't already running —
in this repo's dev setup that's `sidecar/docker-compose.dev.yml`
(`make -C sidecar dev-db`); in production, whatever provisions the new
database host or container. Do **not** restore over an existing database
that anything else depends on — restore into a fresh database:

```sh
createdb -h <DST_HOST> -p <DST_PORT> -U <user> <dbname>
pg_restore -h <DST_HOST> -p <DST_PORT> -U <user> \
  -d <dbname> --no-owner --no-acl miniflux.dump
```

`--no-owner --no-acl` avoid `pg_restore` trying to `ALTER OWNER` or grant
privileges to roles that may not exist by the same name on the new server —
without these flags, expect (and it is safe to ignore) messages like:

```
pg_restore: warning: errors ignored on restore: N
... role "miniflux" does not exist
```

Two more classes of message are normal on a 17→18 restore and are **not**
reasons to abort:

- **Extension/comment already-exists notices** for built-in extensions
  such as `plpgsql` (`CREATE EXTENSION IF NOT EXISTS plpgsql` /
  `COMMENT ON EXTENSION plpgsql ...` against an extension the fresh
  PostgreSQL 18 database already created at `initdb` time). These are
  informational `NOTICE`s, not errors.
- **Harmless server-version banner differences** — `pg_restore`'s own
  preamble may note the archive was produced by a different `pg_dump`
  server version than the target; this is expected for a cross-major-
  version migration and is not itself an error.

What is **not** safe to ignore: any message naming a *user* table (from
`public`, not from an extension) as skipped, failed, or already
containing conflicting data, or a nonzero `pg_restore` exit status paired
with a table row count that doesn't match §1. Stop and go to §7 if you see
either.

### 5. Verify

In order, all of these must pass before Miniflux is pointed at the new
database for real:

1. **Row counts match step 1 exactly** — re-run the three queries from
   §1 against the restored database and diff them against the numbers you
   wrote down.
2. **`verify-extensions.sh` passes**:
   ```sh
   sidecar/scripts/verify-extensions.sh "postgres://<user>:<pass>@<DST_HOST>:<DST_PORT>/<dbname>?sslmode=disable"
   ```
   This must print both `vector` and `pg_search` with version numbers and
   exit `0`. If it exits non-zero, the ParadeDB image/server is missing
   something and nothing past this point should proceed.
3. **Miniflux starts and serves** against the new database — point
   `DATABASE_URL` at the restored ParadeDB instance and start the daemon
   normally; confirm it logs a listening address and that its
   `/healthcheck` endpoint (or the login page) responds.
4. **A feed refresh works** — trigger a refresh (the UI's per-feed
   refresh button, or `./miniflux -refresh-feeds`) and confirm it
   completes without a database error. A feed-fetch error for an
   unreachable *feed URL* is not a failure of this step; a database
   connection or schema error is.

Only once all four pass should Miniflux be pointed at the new database
for production traffic (DNS/config/env change, whatever mechanism this
deployment uses) and restarted for real.

### 6. Run the sidecar migrations

Start the sidecar (`SIDECAR_DATABASE_URL` pointed at the same ParadeDB
database) the normal way — its `main` runs `store.Migrate()`
unconditionally before anything else. Confirm the `search` schema landed:

```sh
psql -h <DST_HOST> -p <DST_PORT> -U <user> -d <dbname> \
  -c "\dt search.*" \
  -c "SELECT * FROM search.schema_version;"
```

Expect `search.passages`, `search.entry_index_state`, and
`search.schema_version`, with `schema_version.version` equal to the number
of migrations in `sidecar/internal/store/migrations.go` (2 as of this
writing).

## 7. Rollback

Follow this if **any** check in §5 fails, or anything else about the new
database looks wrong. Do not try to "fix forward" against the new
database under time pressure — roll back, then investigate calmly with
Miniflux back up on the old side.

1. **Stop Miniflux again** if it was started against the new database in
   §5 step 3. Confirm it's down.
2. **Do not touch, drop, or modify the original PostgreSQL 17
   database/volume.** It was never modified by this procedure — the dump
   in §4 is read-only against it. This is exactly why: it is still there,
   complete, as of the moment Miniflux was stopped in §3.
3. **Point Miniflux's `DATABASE_URL` back at the PostgreSQL 17
   database** — undo whatever config/env change §5 made, or simply don't
   make it permanent until §5 fully passes.
4. **Start Miniflux against the PostgreSQL 17 database.**
5. **Verify it's the original again**: run the three queries from §1 and
   confirm they match what you wrote down before this procedure started.
   If they match, the rollback is complete and the instance is exactly
   where it was before you began.
6. **Leave the failed ParadeDB restore in place, untouched**, for
   post-mortem — do not drop that database yet. Investigate what went
   wrong (re-read the `pg_restore` output in full, check for a version
   mismatch per "Before you start," check disk space) before attempting
   the migration again from a fresh dump.
7. Only after the cause is understood and fixed, restart from §2 (a fresh
   backup — do not reuse a dump that was involved in a failed run without
   re-verifying it per §2). Before re-running §4's `createdb`, rename or
   drop the failed-restore database from step 6 — `createdb` fails loudly
   on a name collision rather than overwriting it, but it will still block
   the retry until you do.

There is no scenario in this procedure where the original PostgreSQL 17
database is dropped, altered, or overwritten. The only way data is lost is
by skipping §2's verification and then also skipping this rollback.

## 8. Rehearsal record

This runbook was rehearsed end-to-end before being trusted, against the
local dev databases only — **never against a live or production
database**:

- **Source**: `miniflux-sdd-db` (PostgreSQL 17, port 5432) — the local dev
  Miniflux database.
- **Target**: a scratch database created inside `sidecar-db-1` (ParadeDB /
  PostgreSQL 18, port 5434), named `migration_rehearsal` (and a second,
  short-lived scratch database `migration_rehearsal_verify` used only to
  rehearse §2's "verify the backup restores before touching anything
  else" step). Neither scratch database touched `sidecar-db-1`'s own
  `miniflux2` database, which the sidecar's own test suite depends on.
  Both scratch databases were dropped after the rehearsal; both
  containers were left running throughout and are still running.

What the rehearsal did, in order, and what changed from the first draft
of this document as a result:

1. The dev source database (`miniflux-sdd-db`) started out **completely
   empty** (no schema at all) — the "dev Miniflux database" this plan
   refers to had never actually been migrated. `./miniflux -migrate` was
   run against it first to create the real 134 + 2 fork migrations'
   worth of schema, then a handful of rows (one user, one category, one
   feed, two entries, two tombstones) were inserted so the row-count
   checks in §1/§5 would be exercising something other than zero. This
   isn't part of the runbook itself (a real production database already
   has data); it only reflects that this particular dev database needed
   seeding before it could stand in for one.
2. **`pg_dump`/`pg_restore` version choice.** The host's own `psql`/
   `pg_dump` is version 16 — *older* than both servers involved (17 and
   18), which is the wrong direction per standard PostgreSQL upgrade
   guidance. `sidecar-db-1` (the ParadeDB container) conveniently ships
   matching PostgreSQL 18 client tools, and `host.docker.internal` from
   inside that container reaches the source database's published port on
   the host. So the rehearsal ran `pg_dump`/`pg_restore` from *inside*
   `sidecar-db-1` via `docker exec`, targeting `host.docker.internal:5432`
   for the dump and `localhost` for the restore. §4 and "Before you
   start" above were written to recommend this same approach rather than
   assuming a locally installed matching-version client — this was not
   in the original plan and was discovered only by hitting the version
   mismatch.
3. **The backup-verification restore (§2)** was rehearsed into
   `migration_rehearsal_verify`, dropped, and then the "real" restore
   was rehearsed separately into `migration_rehearsal` — exercising both
   restores the runbook actually calls for, not just one.
4. **Row counts matched exactly** after both restores: 2 entries (oldest
   `published_at` = `2025-08-10 12:02:06.163713+00`), 2 tombstones, both
   times.
5. **`verify-extensions.sh` passed** against the restored
   `migration_rehearsal` database (`vector: 0.8.4`, `pg_search: 0.25.9`)
   and correctly **failed** (exit 2) when pointed at the plain PostgreSQL
   17 source, confirming the script actually discriminates the two.
6. **Miniflux's migrator** (`./miniflux -migrate`) ran against the
   restored copy and reported `current_version=134 latest_version=134` /
   `current_version=2 latest_version=2` — i.e. correctly recognized the
   restored schema as already fully migrated, a no-op. This is the same
   evidence class as the "why this migration exists" claim above, now
   demonstrated via dump-and-restore rather than a from-scratch migrate.
7. **Miniflux served traffic** against the restored copy: started as a
   background process (bounded, immediately killed after one check — not
   left running), `GET /healthcheck` returned HTTP 200, and the process
   was confirmed gone afterward.
8. **A feed refresh was triggered** (`./miniflux -refresh-feeds`) against
   the restored copy. It ran the real refresh pipeline end-to-end — batch
   creation, worker pool, HTTP fetch attempt — and logged a fetch failure
   for the single seeded feed, because that feed's URL
   (`https://example.com/feed.xml`) is a placeholder that doesn't serve a
   real feed, not because of any database or migration problem. This
   confirms the refresh *pipeline* runs correctly against the migrated
   database; it does not confirm refreshing a real, reachable feed,
   which the rehearsal had no real feed available to test.
9. **The sidecar's own migrations** were run against the restored copy
   (via `store.Migrate()`, not the full `sidecar` binary — see the
   limitation below) and created `search.passages`,
   `search.entry_index_state`, and `search.schema_version` (version 2),
   exactly as §6 describes.
10. **Rollback** was rehearsed only in the sense that the original source
    database was re-queried at the end and confirmed unchanged (still 2
    entries, still 2 tombstones) — proving the dump-and-restore steps
    never wrote to it. The rollback steps that involve actually stopping
    and restarting a live Miniflux daemon pointed at each database in
    turn were not exercised as a cutover-and-back sequence, because no
    daemon was ever actually cut over to the new database as production
    traffic in this rehearsal (see limitations below).

### What this did and did not prove

**Proved:** the exact command sequence in §1–§6 works, end to end, against
a real (if tiny) Miniflux schema and dataset, using the same PostgreSQL
17→18 dump/restore path production will use; the version-mismatch pitfall
in "Before you start" is real and the documented workaround resolves it;
`verify-extensions.sh` correctly distinguishes a ParadeDB target from a
stock Postgres source; the original database is never written to by this
procedure, which is what makes §7's rollback trivial.

**Did not prove:**

- **Scale.** The dev database held two entries. Nothing here says how long
  `pg_dump`/`pg_restore` take, how much disk headroom is needed, or how
  the restore behaves at spec §5.5's estimated 20–40 GB / 1M-entry scale.
  Time and disk sizing for the real migration should be estimated
  separately (e.g. by checking the real source database's on-disk size
  before scheduling the maintenance window) rather than assumed from this
  rehearsal.
- **A production stop/start cutover.** Miniflux was never actually
  stopped-as-production and restarted-pointed-at-the-new-database in this
  rehearsal; it was started standalone against the restored copy for a
  few seconds to confirm it serves, then killed. The mechanics of
  whatever this deployment uses to stop/start the real daemon (systemd
  unit, docker compose, process supervisor) are not exercised here.
- **A real feed refresh.** As noted above, the seeded feed's URL is a
  placeholder; the pipeline ran, but no real network content was
  actually re-ingested.
- **Migrating via the actual `sidecar` binary.** This machine has no ONNX
  Runtime model file and no `libtokenizers.a` available, so the compiled
  `sidecar` binary (which requires both to build with `-tags ORT`) could
  not be built or started here. §6 was rehearsed by calling
  `store.Migrate()` directly through a small throwaway program using the
  same `internal/store` package the real binary calls — this exercises
  the identical SQL and the identical `Store.Migrate` code path, but it is
  not a rehearsal of starting the real `sidecar` process end to end.
- **The specific harmless-`pg_restore`-warning text quoted in §4.** The
  rehearsal's own restores (run with `--no-owner --no-acl` against
  extension-free scratch databases, and small) produced **no** warnings
  at all — clean exit, no stderr output, both times. The warning text
  quoted in §4 is standard, documented `pg_restore` behavior for
  ownership/ACL mismatches and pre-existing built-in extensions on a
  fresh PostgreSQL cluster, not something this rehearsal personally
  observed. Treat that part of §4 as well-founded but not
  rehearsal-verified.

A runbook that has never been executed is a guess; this one has been run
once, on a copy, at a scale far below production. Re-rehearse against a
size- and shape-representative copy (or at minimum, check real disk usage
and timing) before relying on this for the actual cutover.
