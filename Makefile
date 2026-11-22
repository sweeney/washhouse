.PHONY: build test test-tools lint check

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

check: lint test test-tools
