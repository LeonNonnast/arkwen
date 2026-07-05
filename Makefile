# Arkwen — factory runtime for autonomous software work.
#
# Go is installed at ~/go-toolchain (self-contained; not on the global PATH).
# This Makefile prefers a `go` already on PATH, else falls back to that toolchain.

GO ?= $(shell command -v go 2>/dev/null || echo $(HOME)/go-toolchain/go/bin/go)
export GOROOT := $(shell $(GO) env GOROOT 2>/dev/null)
export GOPATH ?= $(HOME)/go
export PATH := $(GOROOT)/bin:$(GOPATH)/bin:$(PATH)

PKGS := ./...

PG_IMAGE ?= postgres:16-alpine
PG_CONTAINER ?= arkwen-pg-test
PG_DSN ?= postgres://postgres:arkwen@127.0.0.1:5433/arkwen?sslmode=disable
IMAGE ?= arkwen:local

.PHONY: all build test conformance test-pg vet lint demo serve proto tidy clean docker-build docker-run

all: build test

## build: compile everything
build:
	$(GO) build $(PKGS)

## test: run the full test suite (unit + conformance)
test:
	$(GO) test $(PKGS)

## conformance: run only the adversarial golden-vector conformance suite
conformance:
	$(GO) test ./test/conformance/... -v

## test-pg: boot a throwaway Postgres and run the durable event-store tests (-race)
## against it, then tear it down. `make test` stays Docker-free (these tests skip
## unless ARKWEN_TEST_DATABASE_URL is set).
test-pg:
	@docker rm -f $(PG_CONTAINER) >/dev/null 2>&1 || true
	docker run -d --name $(PG_CONTAINER) -e POSTGRES_PASSWORD=arkwen -e POSTGRES_DB=arkwen -p 5433:5432 $(PG_IMAGE) >/dev/null
	@echo "waiting for postgres…"; \
	for i in $$(seq 1 30); do docker exec $(PG_CONTAINER) pg_isready -U postgres >/dev/null 2>&1 && break; sleep 1; done
	ARKWEN_TEST_DATABASE_URL='$(PG_DSN)' $(GO) test -count=1 -race ./internal/eventlog/... -run 'PG_|Replay|Create|Append|Subscribe' -v; \
	status=$$?; docker rm -f $(PG_CONTAINER) >/dev/null 2>&1; exit $$status

## vet: static checks
vet:
	$(GO) vet $(PKGS)

## lint: gofmt check + vet
lint: vet
	@test -z "$$(gofmt -l internal cmd test)" || (echo "gofmt needed:"; gofmt -l internal cmd test; exit 1)

## demo: run the in-process walking-skeleton demo (one Mission -> completed)
demo:
	$(GO) run ./cmd/arkwen demo

## serve: start the gRPC contract plane on 127.0.0.1:7777
serve:
	$(GO) run ./cmd/arkwen serve

## docker-build: build the deployable container image (same Dockerfile Railway uses)
docker-build:
	docker build -t $(IMAGE) .

## docker-run: run the image locally on :8080 (in-memory store, sealed command plane)
## Add -e DATABASE_URL=… and -e ARKWEN_OPERATOR_TOKEN=… to mirror a real deploy.
docker-run: docker-build
	docker run --rm -e PORT=8080 -p 8080:8080 $(IMAGE)

## proto: regenerate Go from proto/arkwen/v1 (needs buf on PATH)
proto:
	buf generate

## tidy: sync go.mod/go.sum
tidy:
	$(GO) mod tidy

clean:
	$(GO) clean -cache
