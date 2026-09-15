# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project

Miniflux 2 — a minimalist feed reader written in Go, backed by PostgreSQL only. Single statically-linked binary: all templates, CSS, JS, icons and translations are embedded with `go:embed`. No ORM, no web framework (stdlib `net/http` + `http.ServeMux` pattern routing), and a deliberately small dependency set.

`CONTRIBUTING.md` is authoritative on philosophy: improving existing features beats adding new ones, simplicity beats cleverness, and new third-party dependencies are discouraged.

## Commands

```bash
make miniflux            # build binary for the current platform (PIE)
make run                 # run locally in debug mode (runs migrations, creates admin/test123)
make test                # go test -cover -race -count=1 ./...
make lint                # go vet + gofmt check + golangci-lint
make build               # cross-compile all release targets
make docker-image        # build packaging/docker/alpine image
```

Single test / package:

```bash
go test ./internal/reader/sanitizer/
go test -run TestParseAtom ./internal/reader/parser/
```

Integration tests need a local PostgreSQL (`psql` as `postgres`/`postgres`). They boot the real binary on :8080 against a throwaway `miniflux_test` database and run `./internal/api`:

```bash
make integration-test
make clean-integration-test   # always run this afterwards; it kills the daemon and drops the DB
```

Dev database:

```bash
docker run --rm --name miniflux2-db -p 5432:5432 \
  -e POSTGRES_DB=miniflux2 -e POSTGRES_USER=postgres -e POSTGRES_PASSWORD=postgres postgres
```

JavaScript is linted separately in CI (not in `make lint`): `jshint internal/ui/static/js/*.js` and `eslint internal/ui/static/js/*.js`.

## Conventions

- **Commit messages must follow conventional commits** — CI enforces `^(build|chore|ci|docs|feat|fix|perf|refactor|revert|security|style|test)(\(scope\))?!?: <subject>` via `.github/workflows/scripts/commit-checker.py`.
- Every Go file starts with the two-line SPDX header (`goheader` linter enforces it) and the package clause carries an import comment: `package foo // import "miniflux.app/v2/internal/foo"`.
- Logging is `log/slog` with structured attributes (`slog.Int64("user_id", ...)`); the `loggercheck` linter validates slog call sites.
- `errcheck` is disabled, but `gocritic`, `staticcheck`, `perfsprint`, `sqlclosecheck`, `misspell` and `whitespace` are on.
- Go version is pinned in both `go.mod` and `CONTRIBUTING.md` — update both together.

## Architecture

`main.go` → `internal/cli.Parse()`. The CLI layer owns everything: flag parsing, config loading, migrations, static-bundle generation, and one-shot commands (`-migrate`, `-create-admin`, `-refresh-feeds`, `-export-user-feeds`, `-run-cleanup-tasks`, `-health-check`, …). With no flags it calls `startDaemon`.

**Daemon** (`internal/cli/daemon.go`) wires three long-lived pieces and blocks on signals (SIGTERM shutdown, SIGHUP reloads TLS certs):
1. `worker.Pool` — fixed goroutine pool consuming `model.Job` from an unbuffered channel.
2. `runScheduler` — two tickers. The feed scheduler builds a batch via `store.NewBatchBuilder()` (filters on error limit, disabled feeds, expired `next_check_at`, per-host limit) and pushes jobs into the pool; the cleanup scheduler archives old entries and prunes sessions.
3. `server.StartWebServer` — HTTP(S) listeners, optional Let's Encrypt / custom certs.

**Routing** (`internal/http/server/routes.go`) is a two-level mux. `rootMux` always exposes `/liveness`, `/healthz`, `/readiness`, `/readyz` at the true root; everything else is mounted under `config.Opts.BasePath()` with `http.StripPrefix`. **Consequently every downstream handler — `ui`, `api`, `fever`, `googlereader` — sees paths with the base path already stripped, and must use `h.routePath(...)` / `basePath +` when generating outbound URLs.** Under the app mux, in order: `/fever/`, `/reader/api/0/` (Google Reader), `/v1/` (REST API), `/metrics`, and `ui.Serve` as the catch-all.

**Four independent HTTP surfaces** share one `*storage.Storage`:
- `internal/api` — REST API v1, `X-Auth-Token` API keys or HTTP Basic. Route table lives in `api.NewHandler`.
- `internal/ui` — server-rendered HTML. One file per route handler (`feed_edit.go`, `entry_read.go`, …), routes declared in `ui.go`, chained through the web-session, CSRF and auth-proxy middlewares. `internal/ui/form` unmarshals POST bodies; `internal/ui/view` carries template data.
- `internal/fever` and `internal/googlereader` — compatibility APIs for third-party mobile clients, each with its own middleware and response shapes (see their local `README.md`).

**Feed refresh pipeline** — the core of the app, entered from a worker or a manual refresh, all in `internal/reader/handler`:
`fetcher` (conditional GET with ETag/Last-Modified, optional proxy rotation, custom UA/cookies) → `parser.DetectFeedFormat` sniffs and dispatches to `atom`/`rss`/`rdf`/`json` → `processor.ProcessFeedEntries` applies per-feed logic: `filter` (block/keep rules), `rewrite` (content and URL rewrite rules), `scraper` + `readability` (full-content crawling), `sanitizer`, `urlcleaner` (tracking-param stripping), `readingtime`, and site-specific handlers (`youtube.go`, `bilibili.go`, `nebula.go`, `odysee.go`) → `storage` upsert → `icon` fetch.

**Storage** (`internal/storage`) is hand-written `database/sql` against `lib/pq`. No ORM. Complex reads go through fluent builders — `entry_query_builder.go`, `feed_query_builder.go`, `entry_pagination_builder.go`, `batch.go` — which assemble WHERE clauses and args incrementally. Add new query options as builder methods rather than new bespoke SQL functions.

**Migrations** (`internal/database/migrations.go`) are an ordered `[]func(tx *sql.Tx) error` array; `schemaVersion` is simply `len(migrations)`. **Append new migrations to the end, never reorder or edit existing entries.** They run only when `RUN_MIGRATIONS=1` or `-migrate` is passed.

**Configuration** (`internal/config`) is env-var driven (twelve-factor) into the global `config.Opts`. Options are declared in one large map in `options.go` with a type (`stringType`, `secondType`, `bytesType`, …), optional validator, and optional `targetKey` for `*_FILE` secret variants. Adding an option means: add the map entry, add an accessor method, and update `miniflux.1`.

**Static assets** (`internal/ui/static/static.go`) are embedded then bundled and minified *at startup*, not at build time: CSS bundles are theme combinations (`light_serif`, `dark_sans_serif`, …), the `app` JS bundle is concatenated from `touch_handler.js`, `keyboard_handler.js`, `app.js` (plus `webauthn_handler.js` when WebAuthn is on). Each bundle gets a content hash used for cache-busting URLs (`/stylesheets/{checksum}/{name}`). Adding a CSS or JS file requires adding it to the bundle map — it is not picked up automatically.

**Templates** (`internal/template`) are `html/template`, embedded from `templates/views` and `templates/common`. `engine.go` holds an explicit map of view → common dependencies; a new view must be registered there. The func map (`functions.go`) provides `t` for translation and route/URL helpers bound to the base path.

**i18n** (`internal/locale`) uses per-language JSON files in `internal/locale/translations`. Translation keys are flat strings; plurals are arrays resolved by `plural.go`. Use `make add-string KEY=... VAL=...` to insert a key into *every* language file at once (requires `jq`). Errors surfaced to users are wrapped in `locale.LocalizedErrorWrapper` / `locale.LocalizedError` so they can be translated per-user at render time rather than at creation time.

**Integrations** (`internal/integration`) — each third-party service is its own subpackage with a `NewClient(...)` and a single action method. `integration.go` is a flat dispatcher that checks `userIntegrations.XEnabled` and logs failures without aborting. Adding one means: new subpackage, a branch in the dispatcher, fields on `model.Integration` + a migration, form fields in `internal/ui/form/integration.go`, and strings in the translation files.

`client/` is the standalone public Go client library for the REST API (module path `miniflux.app/v2/client`), versioned alongside the server.
