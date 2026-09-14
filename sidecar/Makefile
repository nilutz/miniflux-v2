export CGO_ENABLED := 1
GO_TAGS := ORT

dev-db:
	docker compose -f docker-compose.dev.yml up -d

build:
	go build -tags $(GO_TAGS) -o bin/sidecar ./cmd/sidecar

test:
	# -p 1: internal/indexer's live-lane tests (Task 5) scan the whole
	# `entries` table via PendingEntryIDs, not just the ids they created.
	# Running package test binaries concurrently (the default) lets them
	# observe another package's (internal/store's) fixture rows created
	# and torn down against the same live database, which is a spurious,
	# non-deterministic error in the live lane's log even though it is
	# handled correctly (see internal/indexer/live_test.go). -p 1 keeps
	# the suite genuinely deterministic rather than depending on timing.
	go test -tags $(GO_TAGS) -race -p 1 ./...

lint:
	go vet -tags $(GO_TAGS) ./...
	test -z "$$(gofmt -l .)"
