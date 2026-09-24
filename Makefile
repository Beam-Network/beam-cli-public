.PHONY: build test vet fmt-check check

CLI_BINARY ?= beam

build:
	go build -o $(CLI_BINARY) ./cmd/beam

test:
	go test ./...

vet:
	go vet ./...

fmt-check:
	@test -z "$$(gofmt -l .)" || (gofmt -l . && exit 1)

check: fmt-check test vet
