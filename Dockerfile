# syntax=docker/dockerfile:1
#
# Arkwen — factory runtime for autonomous software work.
# Multi-stage build: static CGO-free binary -> minimal non-root distroless runtime.
#
# go.mod pins `go 1.26.4`. Verified present on Docker Hub (2026-07-05):
#   docker manifest inspect golang:1.26.4-bookworm  -> OCI index digest
#   sha256:b305420a68d0f229d91eb3b3ed9e519fcf2cf5461da4bef997bf927e8c0bfd2b
#   (golang:1.26 / -bookworm / -alpine also resolve.)

##############################  builder  ##############################
FROM golang:1.26.4-bookworm AS builder

WORKDIR /src

# Dependency layer first so `go mod download` is cached until go.mod/go.sum change.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

# TARGETOS/TARGETARCH are provided by BuildKit; defaulted for plain `docker build`.
ARG TARGETOS=linux
ARG TARGETARCH=amd64

# CGO_ENABLED=0  -> fully static (pure-Go resolver: pgx + grpc are pure Go), so it
#                  runs unchanged on distroless/scratch and resolves IPv6-only
#                  Railway hosts (*.railway.internal) without libc.
# -trimpath      -> reproducible paths.  -ldflags -s -w -> strip symbols/DWARF.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/arkwen ./cmd/arkwen

##############################  runtime  ##############################
# distroless/static: no shell, no package manager, minimal attack surface, and
# the :nonroot tag runs as uid 65532 by default (least privilege). It ships CA
# roots and /etc/passwd, so outbound TLS (Railway Postgres sslmode=require) and
# the numeric user both work. The binary reads $PORT itself, so no shell is
# needed to expand it — the CMD is pure exec form (PID 1 gets SIGTERM directly).
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /out/arkwen /usr/local/bin/arkwen

# $PORT is injected by Railway at runtime; 7777 is the standalone default. The
# binary binds "[::]:$PORT" (dual-stack) automatically when PORT is set.
EXPOSE 7777

ENTRYPOINT ["/usr/local/bin/arkwen"]
CMD ["serve"]
