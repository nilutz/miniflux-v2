#!/usr/bin/env bash
#
# SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# seed-eval-corpus.sh subscribes a local Miniflux instance to a fixed list of
# real, text-heavy feeds and refreshes them, so `entries` ends up holding a
# few hundred genuine articles to hand-label for
# sidecar/internal/search/eval/testdata/queries.json.
#
# It never starts Miniflux as a daemon: every invocation of the miniflux
# binary here runs one bounded CLI action (-refresh-feeds) and exits on its
# own. Feed subscription itself has no CLI flag in this fork, so this script
# inserts the `feeds` rows directly with psql — the fetch, parse and (crawler)
# scrape that follow are all real Miniflux code, exercised via -refresh-feeds.
#
# Safe to re-run: the admin user and every feed row are inserted
# idempotently (ON CONFLICT DO NOTHING), and -refresh-feeds only refetches
# what is due.
set -euo pipefail

usage() {
	cat <<'EOF'
Usage: DATABASE_URL=postgres://user:pass@host:port/db sidecar/scripts/seed-eval-corpus.sh [miniflux-binary]

Seeds a small, real corpus for the P1b retrieval evaluation set:
  1. creates an admin user (if one doesn't already exist)
  2. subscribes it to the feeds listed in FEEDS below (edit that list to
     change the corpus)
  3. runs -refresh-feeds so Miniflux fetches and (crawler on) scrapes each
     feed's entries for real

Arguments:
  miniflux-binary   Path to the miniflux binary (default: ./miniflux at the
                     repo root; build it with `make miniflux`)

Environment:
  DATABASE_URL      Postgres DSN Miniflux should use (required)
  ADMIN_USERNAME    admin username to create if missing (default: eval-admin)
  ADMIN_PASSWORD    admin password to create if missing (default: eval-admin-pw-2026)

This script only ever runs the miniflux binary with -refresh-feeds, a
one-shot CLI action that exits on its own; it never starts the daemon.
EOF
}

if [[ "${1:-}" == "-h" || "${1:-}" == "--help" ]]; then
	usage
	exit 0
fi

if [[ -z "${DATABASE_URL:-}" ]]; then
	echo "seed-eval-corpus.sh: DATABASE_URL is required" >&2
	usage >&2
	exit 1
fi

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
MINIFLUX_BIN="${1:-$REPO_ROOT/miniflux}"

if [[ ! -x "$MINIFLUX_BIN" ]]; then
	echo "seed-eval-corpus.sh: $MINIFLUX_BIN not found or not executable; build it with 'make miniflux' at the repo root" >&2
	exit 1
fi

if ! command -v psql >/dev/null 2>&1; then
	echo "seed-eval-corpus.sh: psql is required on PATH to insert feed rows" >&2
	exit 1
fi

ADMIN_USERNAME="${ADMIN_USERNAME:-eval-admin}"
ADMIN_PASSWORD="${ADMIN_PASSWORD:-eval-admin-pw-2026}"

# The feed list. Edit freely — these were picked for real, substantial
# article prose rather than link round-ups, and span a few topics (software
# engineering, security, distributed systems, AI) so that labelled queries
# in Step 4 of the task brief can cover both close-wording and
# rare-discriminating-term cases across genuinely different subject matter.
# Every feed below returned HTTP 200 and had double-digit item counts as of
# 2026-09-14; re-check before relying on the list again much later.
#
# The fork enables the crawler by default for new subscriptions, and this
# script does the same explicitly (crawler=true below) — full page text is
# scraped for every feed here, not just the ones with full-content RSS.
FEEDS=(
	"Simon Willison|https://simonwillison.net/atom/everything/|https://simonwillison.net/"
	"Julia Evans|https://jvns.ca/atom.xml|https://jvns.ca/"
	"Dan Luu|https://danluu.com/atom.xml|https://danluu.com/"
	"Overreacted (Dan Abramov)|https://overreacted.io/rss.xml|https://overreacted.io/"
	"Schneier on Security|https://www.schneier.com/feed/atom/|https://www.schneier.com/"
	"Cloudflare Blog|https://blog.cloudflare.com/rss/|https://blog.cloudflare.com/"
	"Stack Overflow Blog|https://stackoverflow.blog/feed/|https://stackoverflow.blog/"
	"Martin Fowler|https://martinfowler.com/feed.atom|https://martinfowler.com/"
	"Google Research Blog|https://research.google/blog/rss/|https://research.google/"
)

psql_db() {
	psql "$DATABASE_URL" -v ON_ERROR_STOP=1 "$@"
}

echo "seed-eval-corpus.sh: step 1/3 - ensuring admin user '$ADMIN_USERNAME' exists" >&2
DATABASE_URL="$DATABASE_URL" \
	RUN_MIGRATIONS=1 \
	CREATE_ADMIN=1 \
	ADMIN_USERNAME="$ADMIN_USERNAME" \
	ADMIN_PASSWORD="$ADMIN_PASSWORD" \
	"$MINIFLUX_BIN" -refresh-feeds

USER_ID="$(psql_db -t -A -c "SELECT id FROM users WHERE username = '$ADMIN_USERNAME';")"
if [[ -z "$USER_ID" ]]; then
	echo "seed-eval-corpus.sh: could not find user '$ADMIN_USERNAME' after creation" >&2
	exit 1
fi
CATEGORY_ID="$(psql_db -t -A -c "SELECT id FROM categories WHERE user_id = $USER_ID ORDER BY id LIMIT 1;")"
if [[ -z "$CATEGORY_ID" ]]; then
	echo "seed-eval-corpus.sh: could not find a default category for user_id=$USER_ID" >&2
	exit 1
fi

# sql_quote doubles single quotes so each feed field can be embedded
# straight into a literal ('...'). The feed list above is a fixed literal
# in this file, not untrusted input, so this is just correctness against
# the one apostrophe-shaped thing that could appear in a title, not a
# defense against adversarial input.
sql_quote() {
	printf "%s" "${1//\'/\'\'}"
}

echo "seed-eval-corpus.sh: step 2/3 - subscribing to ${#FEEDS[@]} feeds (user_id=$USER_ID, category_id=$CATEGORY_ID)" >&2
{
	echo "BEGIN;"
	for row in "${FEEDS[@]}"; do
		IFS='|' read -r title feed_url site_url <<<"$row"
		printf "INSERT INTO feeds (user_id, category_id, title, feed_url, site_url, crawler, next_check_at)\n"
		printf "VALUES (%s, %s, '%s', '%s', '%s', true, now())\n" \
			"$USER_ID" "$CATEGORY_ID" \
			"$(sql_quote "$title")" "$(sql_quote "$feed_url")" "$(sql_quote "$site_url")"
		printf "ON CONFLICT (user_id, feed_url) DO NOTHING;\n"
	done
	echo "COMMIT;"
} | psql_db

echo "seed-eval-corpus.sh: step 3/3 - refreshing feeds (fetch + crawler scrape; this hits the network and can take a few minutes)" >&2
DATABASE_URL="$DATABASE_URL" RUN_MIGRATIONS=1 "$MINIFLUX_BIN" -refresh-feeds

echo "seed-eval-corpus.sh: done. Summary:" >&2
psql_db -c "
SELECT f.title, f.parsing_error_count, count(e.id) AS entries
FROM feeds f
LEFT JOIN entries e ON e.feed_id = f.id
WHERE f.user_id = $USER_ID
GROUP BY f.id, f.title, f.parsing_error_count
ORDER BY f.id;
"
psql_db -c "SELECT count(*) AS total_entries FROM entries WHERE feed_id IN (SELECT id FROM feeds WHERE user_id = $USER_ID);"

cat >&2 <<EOF

Next: run the sidecar's backfill lane (StopWhenDrained) to index these
entries into search.passages, then hand-label queries into
sidecar/internal/search/eval/testdata/queries.json against the entry ids
above. See sidecar/internal/search/eval/README.md.
EOF
