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
# The commit's own date, never now(). Two builds of the same tree then stamp the
# same thing, so `farmd version` and org.opencontainers.image.created answer
# "what is in this" rather than "when did somebody type make" — a wall clock
# makes every rebuild a different artifact that differs in nothing that matters.
# VERSION already carries `-dirty` when the tree does, which is where an
# uncommitted change shows up.
BUILD_DATE  ?= $(shell git log -1 --format=%cI 2>/dev/null)
LDFLAGS     := -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.buildDate=$(BUILD_DATE)
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
	helm template ci deploy/helm/device-farmer -n device-farmer \
		--set database.dsn=postgres://farm@pg:5432/device_farmer \
		--set auth.tokens=ci-token:operator:ci > /dev/null
	helm template ci deploy/helm/device-farmer -n device-farmer -f deploy/helm/ci-values.yaml \
		--set database.dsn="" --set database.existingSecret=farm-postgres \
		--set auth.tokens="" --set auth.existingSecret=farm-api-tokens > /dev/null
	@roles="$$(sed -n 's/^[[:space:]]*"all":[[:space:]]*{\(.*\)},$$/\1/p' internal/config/config.go | tr -d '"' | tr ',' ' ')"; \
	test "$$(echo $$roles | wc -w)" -ge 8 || { echo "could not read roleComponents[all]"; exit 1; }; \
	for role in $$roles; do \
		helm template ci deploy/helm/device-farmer -n device-farmer -f deploy/helm/ci-values.yaml -s "templates/$$role.yaml" \
			| grep -q '^kind: Deployment$$' || { echo "templates/$$role.yaml renders no Deployment"; exit 1; }; \
	done
	@out=$$(helm template ci deploy/helm/device-farmer -n device-farmer \
		--set database.dsn=postgres://farm@pg:5432/device_farmer --set auth.tokens=ci-token:operator:ci \
		--set fenceProxy.enabled=true --set fenceProxy.ca=CA --set fenceProxy.cert=CERT --set fenceProxy.key=KEY \
		--set fenceProxy.controlCert=CTL --set fenceProxy.controlKey=CTLK \
		--set extraVolumes[0].name=fence --set extraVolumes[0].secret.secretName=ci-device-farmer-fence \
		--set extraVolumeMounts[0].name=fence --set extraVolumeMounts[0].mountPath=/etc/device-farmer/fence); \
	echo "$$out" | grep -q FARM_FENCE_CLIENT_CERT || { echo "a fenced farm renders no FARM_FENCE_CLIENT_CERT"; exit 1; }; \
	test "$$(echo "$$out" | awk '/^# Source:/{src=$$0} /FARM_FENCE_CONTROL_CERT/{print src}' | sort -u)" \
		= "# Source: device-farmer/templates/api.yaml" || { echo "the control certificate reaches more than the api"; exit 1; }
	@! helm template ci deploy/helm/device-farmer -n device-farmer > /dev/null 2>&1 || \
		{ echo "the chart rendered with no database configured"; exit 1; }
	@! helm template ci deploy/helm/device-farmer -n device-farmer \
		--set database.dsn=postgres://farm@pg:5432/device_farmer > /dev/null 2>&1 || \
		{ echo "the chart rendered a release whose api cannot start"; exit 1; }
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

## docker: build the container image, then run it to prove the stamps arrived
#
# All three build args are passed, and COMMIT is no longer optional.
# .dockerignore keeps .git out of the build context and the Dockerfile builds
# with -buildvcs=false, so Go's VCS stamping has nothing to read: the commit in
# `farmd version` and in org.opencontainers.image.revision is what this line
# hands over and nothing else. That is the fix — it used to be whatever .git
# happened to be lying in the context — but it also means an image built with an
# empty COMMIT cannot say what it is, so this refuses rather than shipping one.
#
# Then it runs what came out. Nothing else in this repository executes the
# image, and the image is the only artifact where the binary can be right and
# the packaging wrong: an allowlist entry lost in a merge, a build arg that
# never reached the linker. `version` answers before config.Load, so it needs no
# database, no network and no shell — there is no shell in distroless — which
# makes it the cheapest proof that the thing starts at all.
docker:
	@test -n "$(COMMIT)" || { echo "COMMIT is empty: pass COMMIT=<sha>. An image that cannot name its commit is not one to deploy"; exit 1; }
	docker build --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) \
		--build-arg BUILD_DATE=$(BUILD_DATE) \
		-t device-farmer/farmd:$(VERSION) -t device-farmer/farmd:latest .
	@docker run --rm device-farmer/farmd:$(VERSION) version | grep -q '$(COMMIT)' || \
		{ echo "the image does not report commit $(COMMIT): the build args never reached the binary"; exit 1; }
	@echo "built device-farmer/farmd:$(VERSION)  commit $(COMMIT)"

## up: docker compose, schema and demo, on http://localhost:8420
up:
	docker compose up -d --build
	@echo "dashboard: http://localhost:$${FARM_PORT:-8420}"

## down: stop the compose stack, keeping the database volume
down:
	docker compose down

## k8s-up: the same evaluation farm on Kubernetes, on http://127.0.0.1:8420
#
# Not a variant of `up`. It builds the image, side-loads it into the local
# cluster's node — a local Kubernetes does NOT share the Docker daemon's image
# store, which is the single most common way a correct chart produces a broken
# cluster — brings up an evaluation Postgres, installs the chart and holds the
# port-forward open. `--farm` leaves the simulated hardware out.
# deploy/local/README.md is the long form.
k8s-up:
	@bash scripts/k8s-up.sh

## k8s-down: remove the namespace, the release and the side-loaded image
k8s-down:
	@bash scripts/k8s-down.sh

## clean: remove build output
clean:
	rm -rf $(BIN)

.PHONY: help build build-linux test assertions migrate ci ci-go ci-sql ci-helm demo all docker up down k8s-up k8s-down clean
