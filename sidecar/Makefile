export CGO_ENABLED := 1
GO_TAGS := ORT

# HERMETIC_PKGS are the packages that pass with no build tags, no native
# ONNX/tokenizer libraries, and no database: their database-backed tests
# t.Skip when SIDECAR_TEST_DATABASE_URL is unset. This is what CI runs
# (.github/workflows/sidecar.yml).
#
# internal/indexer is deliberately NOT in this list even though its
# controller tests are hermetic: `go test ./internal/indexer/` also runs
# the suite that sweeps the entire entries table and rewrites
# search.passages. That suite belongs on a throwaway database chosen
# deliberately, never in CI and never by accident. See internal/testdb.
#
# internal/sidecarclient and internal/mcpserver (task 15, cmd/mcp's own
# HTTP client and tool implementations) belong here too: both are tested
# entirely against httptest, need no database and no native library, and
# in fact must never gain either -- cmd/mcp is the one binary in this
# module required to build with CGO_ENABLED=0 and no libtokenizers.a (see
# build-mcp below).
HERMETIC_PKGS := ./internal/search/... ./internal/web/ ./internal/store/ ./internal/passage/ ./internal/sidecarclient/... ./internal/mcpserver/...

dev-db:
	docker compose -f docker-compose.dev.yml up -d

build:
	go build -tags $(GO_TAGS) -o bin/sidecar ./cmd/sidecar

# build-mcp builds the MCP server (task 15) with CGO explicitly disabled
# and no CGO_LDFLAGS, proving it needs neither the native ONNX Runtime
# library nor libtokenizers.a that ./cmd/sidecar requires -- see
# cmd/mcp/main.go's own package doc comment for why that matters: this is
# the binary meant to run on an operator's own laptop next to Claude Code.
build-mcp:
	CGO_ENABLED=0 go build -o bin/miniflux-mcp ./cmd/mcp

# test runs everything, including the database-backed suites. Those read
# SIDECAR_TEST_DATABASE_URL -- NOT SIDECAR_DATABASE_URL, which is what the
# sidecar binary itself reads and what an operator points at their live
# corpus. Set the test one to a throwaway database; internal/testdb fails
# the run outright if the two name the same place.
#
# -p 1 runs one package at a time. Several packages' database-backed tests
# create and delete fixture entries in the SAME database, and some of them
# assert on whole-table counts (store's PendingEntryCount/…Approx pair):
# run in parallel, one package's fixtures appear and disappear underneath
# another package's count and the assertion fails at random. Serialising
# packages is the fix; the database is the shared resource, and nothing in
# the test suite can isolate itself from it.
test:
	go test -tags $(GO_TAGS) -race -p 1 ./...

# test-hermetic is the subset that needs neither native libraries nor a
# database. Safe to run anywhere.
test-hermetic:
	go test -race $(HERMETIC_PKGS)

lint:
	go vet -tags $(GO_TAGS) ./...
	test -z "$$(gofmt -l .)"
