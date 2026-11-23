.PHONY: build test test-tools lint spectral check

BINARY  := washhouse
VERSION := $(shell git rev-parse --short HEAD 2>/dev/null || echo dev)

build:
	go build -trimpath -ldflags "-X main.version=$(VERSION)" -o bin/$(BINARY) ./cmd/washhouse

test:
	go test -race -count=1 ./...

test-tools:
	tools/test.sh
	cd scripts && python3 -m unittest -q fixture_test
	scripts/make_fixture_test.sh
	shellcheck -S warning scripts/make-fixture.sh scripts/make_fixture_test.sh

lint:
	go vet ./...
	test -z "$$(gofmt -l .)" || { gofmt -l .; exit 1; }
	staticcheck ./...

# The OpenAPI lint (CI runs it too); needs npx.
spectral:
	npx --yes @stoplight/spectral-cli@6.11.1 lint internal/httpapi/openapi.yaml

check: lint test test-tools
