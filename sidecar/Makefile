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
HERMETIC_PKGS := ./internal/search/... ./internal/web/ ./internal/store/ ./internal/passage/

dev-db:
	docker compose -f docker-compose.dev.yml up -d

build:
	go build -tags $(GO_TAGS) -o bin/sidecar ./cmd/sidecar

# test runs everything, including the database-backed suites. Those read
# SIDECAR_TEST_DATABASE_URL -- NOT SIDECAR_DATABASE_URL, which is what the
# sidecar binary itself reads and what an operator points at their live
# corpus. Set the test one to a throwaway database; internal/testdb fails
# the run outright if the two name the same place.
test:
	go test -tags $(GO_TAGS) -race ./...

# test-hermetic is the subset that needs neither native libraries nor a
# database. Safe to run anywhere.
test-hermetic:
	go test -race $(HERMETIC_PKGS)

lint:
	go vet -tags $(GO_TAGS) ./...
	test -z "$$(gofmt -l .)"
