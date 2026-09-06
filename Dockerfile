# syntax=docker/dockerfile:1

# ---------------------------------------------------------------------------
# Build. CGO is off so the result is a static binary that runs on distroless,
# on Alpine, and on a scratch image without a libc to match.
# ---------------------------------------------------------------------------
FROM golang:1.26-bookworm AS build

WORKDIR /src

# Dependencies first, so a source-only change does not re-download the module
# cache on every build.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

# .dockerignore is an allowlist and names, line by line, why each path is here.
# Read it before adding an import that reaches outside cmd/, internal/,
# migrations/ and test/: the failure mode it protects against is an image that
# builds fine and migrates a production database to version 0.
COPY . .

ARG VERSION=dev
ARG COMMIT=
ARG BUILD_DATE=

# The migrations and the dashboard are embedded, so this binary carries its own
# schema and its own UI. An image that shipped the binary without the SQL would
# migrate to version 0 and report success against an empty database.
#
# -buildvcs=false because .dockerignore keeps .git out of the context. It was in
# the context before there was a .dockerignore, and Go's VCS stamping filled
# vcs.revision from it, so `farmd version` printed a full SHA that no build
# argument had supplied — the image's identity was a property of the directory
# the build happened to run in. Saying `false` here makes the absence a decision
# instead of a side effect: whoever re-includes .git does not silently change
# where this image's commit comes from. It comes from COMMIT, and a build that
# passes none produces a binary that prints no commit at all, which is the
# truthful answer to "which commit is this".
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -buildvcs=false \
      -ldflags "-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.buildDate=${BUILD_DATE}" \
      -o /out/farmd ./cmd/farmd

# Vet and test the whole tree in the configuration the binary above ships in.
# On by default, and not the duplicate of CI's `go` job it looks like — but it
# was, until the env stopped stopping at the first command.
#
# The line read `CGO_ENABLED=0 GOOS=linux go vet ./... && go test ./...`. In
# POSIX sh an assignment prefix binds to one simple command, so `go test` ran at
# the golang image's default CGO_ENABLED=1: the tests linked the cgo resolvers
# while the artifact standing next to them did not, and they could not even
# reuse the CGO-off stdlib the build above had just compiled. The one thing this
# step can prove that CI cannot was the one thing it did not.
#
# It can prove it because nothing else compiles the tree this way.
# .github/workflows/ci.yml runs `go build ./...`, `go vet ./...` and
# `go test -count=1 ./...` at the runner's default CGO_ENABLED=1, and
# `make build-linux` is CGO-off but compiles only ./cmd/farmd. Here the _test.go
# files, test/e2e and test/testpki — the two packages outside `go build
# ./cmd/farmd`'s import graph — are compiled static, for linux, exactly once.
#
# What it does not prove, so nobody reads more into a green build than is there:
# test/e2e skips every scenario without DATABASE_URL and this stage sets none,
# so the database scenarios stay CI's `sql` job, and gofmt stays CI's `go` job.
# If a CI image build lands, this step is what keeps that build from being a
# third copy of the same signal; it is the static compile either way.
#
# Cost, measured on this machine: 26 s of a 49 s `docker build --no-cache`, and
# nothing at all on a build where COPY . . still hits, because both RUN layers
# mount the module and build caches and `go test` is left without -count=1 so
# the test cache inside that mount counts. CI runs -count=1 and owns the
# from-scratch verdict. The same --no-cache build cost 38 s of 61 s with the
# broken prefix, because a CGO_ENABLED=1 test compile shares nothing with the
# CGO-off build above and builds a second copy: the mistake was not only weaker
# than it looked, it was slower.
#
# RUN_TESTS=0 turns it off for the one caller that cannot run it: a cross
# compile, where the test binary is for an architecture the builder cannot
# execute. Do not flip the default. No CI job builds this image today, so with
# tests off a broken tree produces a green image and the first thing to notice
# is a farm.
ARG RUN_TESTS=1
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    if [ "${RUN_TESTS}" = "0" ]; then \
      echo "RUN_TESTS=0: skipping vet and test"; \
    else \
      export CGO_ENABLED=0 GOOS=linux; \
      go vet ./... && go test ./...; \
    fi

# ---------------------------------------------------------------------------
# Runtime. No shell and no package manager: there is nothing to exec into, and
# nothing to install at 3am instead of fixing the image.
# ---------------------------------------------------------------------------
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/farmd /usr/local/bin/farmd

# What this image is, in the one place a registry, a scanner and an admission
# policy all know how to read it. `docker inspect` reported `Labels: null`:
# nothing tied a running digest back to a commit, and GHCR will not link a
# package to its repository without org.opencontainers.image.source — no Source
# link on the package page, no inherited visibility, no README.
#
# The args are re-declared because an ARG is scoped to its stage; --build-arg
# still reaches both. Declaring them here rather than before the first FROM
# means restamping a version rewrites this metadata layer and leaves the compile
# above untouched.
#
# Every value comes from the caller, because -buildvcs=false above left no other
# source. `make docker` passes all three and then runs the image to check the
# commit arrived; an unstamped build labels itself empty rather than guessing.
# No licenses label: this repository ships no LICENSE, and a label asserting one
# would be the same kind of defect as a doc that claims something false.
ARG VERSION=dev
ARG COMMIT=
ARG BUILD_DATE=
LABEL org.opencontainers.image.title="farmd" \
      org.opencontainers.image.description="device-farmer control plane: one static binary, every role a subcommand" \
      org.opencontainers.image.source="https://github.com/FlavioNeto11/device-farmer" \
      org.opencontainers.image.url="https://github.com/FlavioNeto11/device-farmer" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${COMMIT}" \
      org.opencontainers.image.created="${BUILD_DATE}" \
      org.opencontainers.image.base.name="gcr.io/distroless/static-debian12:nonroot"

USER nonroot:nonroot
EXPOSE 8080 9090

# No default role. Every role is a subcommand and the deployment says which
# one it wants; a default would let a manifest with a typo start something
# plausible instead of failing.
ENTRYPOINT ["/usr/local/bin/farmd"]
