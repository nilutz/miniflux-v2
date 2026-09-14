#!/usr/bin/env bash
#
# verify-extensions.sh — confirm a Postgres server can supply the two
# extensions the sidecar's schema requires: pgvector ("vector") and
# ParadeDB's BM25 extension ("pg_search"). Stock PostgreSQL (including a
# plain postgres:17-alpine) ships neither; only an image such as
# paradedb/paradedb does.
#
# This is a precondition check, not a "did we already CREATE EXTENSION"
# check: it queries pg_available_extensions, so it passes as soon as the
# server *could* install these extensions, even in a freshly restored
# database where `CREATE SCHEMA search` / `CREATE EXTENSION` have not run
# yet. That is deliberate — production-migration.md opens with this script
# (before anything has been created) and closes with it again right after
# the restore (before the sidecar's own migrations run).
#
# Usage:
#   verify-extensions.sh <DSN>
#
# <DSN> is any libpq connection string/URI accepted by psql, e.g.:
#   postgres://postgres:postgres@127.0.0.1:5434/miniflux2?sslmode=disable
#
# Exit status:
#   0  both extensions are available; versions are printed to stdout
#   1  the DSN was missing, psql failed to connect, or run without an
#      argument (usage printed to stderr)
#   2  connected fine, but one or both extensions are not available
#
# Requires: psql on PATH, able to reach the target server.

set -euo pipefail

usage() {
  echo "Usage: $0 <DSN>" >&2
  echo "  DSN: a libpq connection string, e.g." >&2
  echo "       postgres://postgres:postgres@127.0.0.1:5434/miniflux2?sslmode=disable" >&2
}

if [ "$#" -ne 1 ]; then
  usage
  exit 1
fi

dsn="$1"

if ! command -v psql >/dev/null 2>&1; then
  echo "verify-extensions.sh: psql not found on PATH" >&2
  exit 1
fi

query="SELECT name, default_version FROM pg_available_extensions WHERE name IN ('vector', 'pg_search') ORDER BY name;"

result="$(psql "$dsn" -X -q -A -t -F'|' -c "$query" 2>&1)" || {
  echo "verify-extensions.sh: unable to query the server:" >&2
  echo "$result" >&2
  exit 1
}

vector_version=""
pg_search_version=""

while IFS='|' read -r name version; do
  [ -z "$name" ] && continue
  case "$name" in
    vector) vector_version="$version" ;;
    pg_search) pg_search_version="$version" ;;
  esac
done <<EOF
$result
EOF

missing=""
if [ -z "$vector_version" ]; then
  missing="${missing} vector"
fi
if [ -z "$pg_search_version" ]; then
  missing="${missing} pg_search"
fi

if [ -n "$missing" ]; then
  echo "verify-extensions.sh: missing required extension(s):${missing}" >&2
  echo "verify-extensions.sh: this server does not offer what the sidecar needs (e.g. stock postgres:17-alpine only has pg_trgm); use a paradedb/paradedb image instead." >&2
  exit 2
fi

echo "vector:    ${vector_version}"
echo "pg_search: ${pg_search_version}"
exit 0
