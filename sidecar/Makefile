export CGO_ENABLED := 1
GO_TAGS := ORT

dev-db:
	docker compose -f docker-compose.dev.yml up -d

build:
	go build -tags $(GO_TAGS) -o bin/sidecar ./cmd/sidecar

test:
	go test -tags $(GO_TAGS) -race ./...

lint:
	go vet -tags $(GO_TAGS) ./...
	test -z "$$(gofmt -l .)"
