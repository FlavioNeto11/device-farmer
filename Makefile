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

## assertions: run every SQL assertion suite against DATABASE_URL
#
# Globbed, not listed. Each migration ships its proof as another
# test/assertions*.sql, and a list has to be edited to pick one up — which is
# how test/assertions_v5.sql sat here unrun. A proof no target invokes is a
# proof nobody checks.
#
# The exit status is psql's, not grep's. The previous form piped into grep and
# ended in `|| true`, so a failed assertion — the entire point of
# ON_ERROR_STOP — still exited 0, and this target was green through a broken
# lease protocol.
assertions:
	@set -e; for f in test/assertions*.sql; do \
		echo "== $$f"; \
		out=$$(psql "$(DATABASE_URL)" -v ON_ERROR_STOP=1 -f "$$f" 2>&1) || \
			{ echo "$$out" | grep -E 'ERROR|FATAL' || echo "$$out"; exit 1; }; \
		echo "$$out" | grep -E 'ok |PASSED' || true; \
		echo "$$out" | grep -q 'PASSED' || \
			{ echo "$$f ran without error but never reported PASSED"; exit 1; }; \
	done

## linux-acceptance: run the whole control plane on Linux, where it ships
#
# Not part of `test` or `ci`, because it needs a Linux host with a PostgreSQL of
# its own and it takes two minutes. It is the only check that touches three
# things a `go test` on a developer's machine cannot:
#
#   the binary STARTS       — every role builds a metrics registry, and nothing
#                             in the Go suite calls that function. A duplicate
#                             registration once made every role panic at startup
#                             with the whole suite green, and only on Linux,
#                             because prometheus's process collector describes
#                             nothing on other platforms.
#   topo.Sysfs READS        — all eighteen topology tests hand FromFS an
#                             fstest.MapFS. The binary calls Sysfs, which
#                             refuses off Linux and reads through os.DirFS, and
#                             takes a hub's VBUS switchability from the file
#                             MODE on each port's `disable` — a fact a MapFS can
#                             only assert into being.
#   the SCHEMA runs there   — the assertion suites against the PostgreSQL major
#                             you deploy, not the one on the laptop.
#
# From Windows:
#   wsl -d Ubuntu -- bash -c 'cd /mnt/c/git/device-farmer && make linux-acceptance'
# On a host with no Go toolchain, hand it the binary:
#   FARMD=/usr/local/bin/farmd scripts/linux-acceptance.sh
linux-acceptance:
	@bash scripts/linux-acceptance.sh

## migrate: apply the schema to DATABASE_URL
migrate: build
	$(BIN)/farmd$(EXE) migrate up

## ci: the three jobs in .github/workflows/ci.yml, locally — go, sql, helm
ci: ci-go ci-sql ci-helm

## ci-go: build, vet, gofmt, and the suite WITHOUT a database
#
# DATABASE_URL is exported above for every other target, so it is removed
# here on purpose: the SQL-backed tests must keep skipping cleanly when there
# is no database, or the suite stops being run on laptops at all.
ci-go:
	go build ./...
	go vet ./...
	@test -z "$$(gofmt -l . | grep -v '^$$')" || { echo "gofmt:"; gofmt -l .; exit 1; }
	env -u DATABASE_URL go test -count=1 ./...

## ci-sql: schema, every assertion suite, and the suite WITH DATABASE_URL
ci-sql: migrate assertions
	go test -count=1 ./...

## ci-helm: lint and render the chart, and prove its guards still refuse
ci-helm:
	helm lint deploy/helm/device-farmer -f deploy/helm/ci-values.yaml
	helm template ci deploy/helm/device-farmer -n device-farmer -f deploy/helm/ci-values.yaml > /dev/null
	helm template ci deploy/helm/device-farmer -n device-farmer --set database.dsn=postgres://farm@pg:5432/device_farmer > /dev/null
	helm template ci deploy/helm/device-farmer -n device-farmer -f deploy/helm/ci-values.yaml \
		--set database.dsn="" --set database.existingSecret=farm-postgres \
		--set auth.tokens="" --set auth.existingSecret=farm-api-tokens > /dev/null
	@for role in api scheduler reaper recovery jobrunner janitor watchdog; do \
		helm template ci deploy/helm/device-farmer -n device-farmer -f deploy/helm/ci-values.yaml -s "templates/$$role.yaml" \
			| grep -q '^kind: Deployment$$' || { echo "templates/$$role.yaml renders no Deployment"; exit 1; }; \
	done
	@! helm template ci deploy/helm/device-farmer -n device-farmer > /dev/null 2>&1 || \
		{ echo "the chart rendered with no database configured"; exit 1; }
	@! helm template ci deploy/helm/device-farmer -n device-farmer -f deploy/helm/ci-values.yaml \
		--set database.existingSecret=farm-postgres > /dev/null 2>&1 || \
		{ echo "the chart rendered with database.dsn AND database.existingSecret"; exit 1; }
	@! helm template ci deploy/helm/device-farmer -n device-farmer -f deploy/helm/ci-values.yaml \
		--set config.db.maxConns=1 > /dev/null 2>&1 || \
		{ echo "the chart rendered with config.db.maxConns=1"; exit 1; }

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

.PHONY: help build build-linux test assertions migrate ci ci-go ci-sql ci-helm demo all docker up down clean
