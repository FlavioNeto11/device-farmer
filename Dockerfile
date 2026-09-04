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

COPY . .

ARG VERSION=dev
ARG COMMIT=
ARG BUILD_DATE=

# The migrations and the dashboard are embedded, so this binary carries its own
# schema and its own UI. An image that shipped the binary without the SQL would
# migrate to version 0 and report success against an empty database.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build -trimpath \
      -ldflags "-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.buildDate=${BUILD_DATE}" \
      -o /out/farmd ./cmd/farmd

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go vet ./... && go test ./...

# ---------------------------------------------------------------------------
# Runtime. No shell and no package manager: there is nothing to exec into, and
# nothing to install at 3am instead of fixing the image.
# ---------------------------------------------------------------------------
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/farmd /usr/local/bin/farmd

USER nonroot:nonroot
EXPOSE 8080 9090

# No default role. Every role is a subcommand and the deployment says which
# one it wants; a default would let a manifest with a typo start something
# plausible instead of failing.
ENTRYPOINT ["/usr/local/bin/farmd"]
