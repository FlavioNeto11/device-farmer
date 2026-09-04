# device-farmer
#
#   make help     what you can do
#   make demo     the whole thing, on this machine, with no phones
#
# Everything here works from Git Bash on Windows as well as from Linux.

SHELL       := /bin/bash
BIN         := bin
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT      ?= $(shell git rev-parse --short HEAD 2>/dev/null)
LDFLAGS     := -X main.version=$(VERSION) -X main.commit=$(COMMIT)
EXE         := $(if $(findstring Windows,$(OS)),.exe,)

# The throwaway cluster the demo uses. Override to point at your own.
DATABASE_URL ?= postgres://farm@127.0.0.1:55432/device_farmer?sslmode=disable
export DATABASE_URL

.DEFAULT_GOAL := help

## help: list the targets
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/^## /  /' | column -t -s ':'

## build: compile farmd for this machine
build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN)/farmd$(EXE) ./cmd/farmd
	@echo "built $(BIN)/farmd$(EXE)  $(VERSION)"

## build-linux: cross-compile the static Linux binary the images ship
build-linux:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath \
		-ldflags "-s -w $(LDFLAGS)" -o $(BIN)/farmd-linux-amd64 ./cmd/farmd
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath \
		-ldflags "-s -w $(LDFLAGS)" -o $(BIN)/farmd-linux-arm64 ./cmd/farmd
	@echo "built static Linux binaries in $(BIN)/"

## test: vet, format check and the Go test suite
test:
	go vet ./...
	@test -z "$$(gofmt -l . | grep -v '^$$')" || { echo "gofmt:"; gofmt -l .; exit 1; }
	go test -count=1 ./...

## assertions: run every SQL assertion file against DATABASE_URL
#
# Every test/assertions*.sql, not just the first one. Each new migration
# ships its proof as another file here, and a proof that no target runs is
# a proof nobody checks.
#
# The exit status is the psql one, not grep's. The previous form piped into
# grep and ended in `|| true`, so a failed assertion — the whole point of
# ON_ERROR_STOP — still exited 0 and a gate built on this target would have
# been green through a broken lease protocol.
assertions:
	@set -e; for f in test/assertions*.sql; do \
		echo "== $$f"; \
		out=$$(psql "$(DATABASE_URL)" -v ON_ERROR_STOP=1 -f "$$f" 2>&1) || \
			{ echo "$$out" | grep -E 'ERROR|FATAL' || echo "$$out"; exit 1; }; \
		echo "$$out" | grep -E 'ok |PASSED' || true; \
	done

## migrate: apply the schema to DATABASE_URL
migrate: build
	$(BIN)/farmd$(EXE) migrate up

## demo: 56 simulated devices and the real control plane, no hardware
demo: build migrate
	FARM_API_ADDR=$${FARM_API_ADDR:-127.0.0.1:8420} \
		$(BIN)/farmd$(EXE) demo -hosts 2 -devices 56

## all: every control-plane role in one process, against real hosts
all: build migrate
	FARM_API_ADDR=$${FARM_API_ADDR:-127.0.0.1:8420} $(BIN)/farmd$(EXE) all

## docker: build the container image
docker:
	docker build --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) \
		-t device-farmer/farmd:$(VERSION) -t device-farmer/farmd:latest .

## up: docker compose, schema and demo, on http://localhost:8420
up:
	docker compose up -d --build
	@echo "dashboard: http://localhost:$${FARM_PORT:-8420}"

## down: stop the compose stack, keeping the database volume
down:
	docker compose down

## clean: remove build output
clean:
	rm -rf $(BIN)

.PHONY: help build build-linux test assertions migrate demo all docker up down clean
