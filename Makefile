.PHONY: build test test-tools lint spectral check bootstrap deploy

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
	deploy/bootstrap_test.sh
	deploy/deploy_test.sh
	shellcheck -S warning deploy/*.sh

lint:
	go vet ./...
	test -z "$$(gofmt -l .)" || { gofmt -l .; exit 1; }
	staticcheck ./...

# The OpenAPI lint (CI runs it too); needs npx.
spectral:
	npx --yes @stoplight/spectral-cli@6.11.1 lint internal/httpapi/openapi.yaml

check: lint test test-tools

# The host (deploy/host) and its config (deploy/config.local.yaml) are
# gitignored; both scripts take --check first.
bootstrap:
	deploy/bootstrap.sh

deploy:
	deploy/deploy.sh
