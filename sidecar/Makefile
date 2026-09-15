export CGO_ENABLED := 1
GO_TAGS := ORT

# HERMETIC_PKGS pass with no build tags, no native ONNX/tokenizer
# libraries, and no database: their database-backed tests t.Skip when
# SIDECAR_TEST_DATABASE_URL is unset. This is what CI runs
# (.github/workflows/sidecar.yml).
#
# internal/indexer is deliberately excluded even though its controller
# tests are hermetic: `go test ./internal/indexer/` also runs a suite that
# sweeps the entire entries table and rewrites search.passages, which
# belongs on a throwaway database only, never in CI. See internal/testdb.
HERMETIC_PKGS := ./internal/search/... ./internal/web/ ./internal/store/ ./internal/passage/ ./internal/sidecarclient/... ./internal/mcpserver/...

dev-db:
	docker compose -f docker-compose.dev.yml up -d

build:
	go build -tags $(GO_TAGS) -o bin/sidecar ./cmd/sidecar

# build-mcp builds the MCP server with CGO explicitly disabled and no
# CGO_LDFLAGS, since it must never need the native ONNX Runtime library or
# libtokenizers.a that ./cmd/sidecar requires -- see cmd/mcp/main.go.
build-mcp:
	CGO_ENABLED=0 go build -o bin/miniflux-mcp ./cmd/mcp

# Reads SIDECAR_TEST_DATABASE_URL -- NOT SIDECAR_DATABASE_URL, which the
# sidecar binary itself reads for an operator's live corpus. Point the test
# var at a throwaway database; internal/testdb refuses to run if the two
# name the same place.
#
# -p 1 runs one package at a time: several packages' database-backed tests
# share the same database and assert on whole-table counts, so running
# them in parallel makes fixtures race across packages.
test:
	go test -tags $(GO_TAGS) -race -p 1 ./...

# The subset needing neither native libraries nor a database. Safe to run
# anywhere.
test-hermetic:
	go test -race $(HERMETIC_PKGS)

lint:
	go vet -tags $(GO_TAGS) ./...
	test -z "$$(gofmt -l .)"
