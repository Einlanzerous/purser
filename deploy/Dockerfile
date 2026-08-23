# syntax=docker/dockerfile:1

# Production image: one self-contained static Go binary (CLI + thin HTTP API).
# construct-server pulls ghcr.io/einlanzerous/purser:latest. Build from the repo
# root:
#   docker build -f deploy/Dockerfile -t purser:latest .

# 1) Build the static binary. Pure Go (pgx, net/http, net/smtp) => CGO disabled.
FROM golang:1.26-bookworm AS build
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# Build identity (PRSR-32). Stamped into the binary and reported by /healthz,
# which Switchyard's delivery reconciler polls to record what is actually
# running (the SWY-192 contract, rolled out by SERV-128).
#
# Both default to EMPTY, not to a placeholder like "docker". The old "docker"
# default put that literal string where a version belongs for every hand-built
# image; `internal/version` maps empty back to "dev", which is the same rule
# that already had to cover an ARG declared but never passed — one that expands
# to an empty string regardless of what default is written here.
#
# There is deliberately no default of the release version: an image built
# outside the publish workflow must not be able to claim it is a release.
#
# VERSION is bare semver, no "v" prefix — it is compared with strict equality
# against org.opencontainers.image.version, which metadata-action stamps bare.
# GIT_SHA is the full 40-char commit; /healthz reports it verbatim rather than
# abbreviating.
ARG VERSION=
ARG GIT_SHA=
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w \
      -X github.com/Einlanzerous/purser/internal/version.Version=${VERSION} \
      -X github.com/Einlanzerous/purser/internal/version.Commit=${GIT_SHA}" \
    -o /out/purser ./cmd/purser

# 2) Runtime: slim Alpine with ca-certificates (HTTPS to the Cloudflare API +
#    SMTP TLS) and wget (busybox) for the compose healthcheck.
FROM alpine:3.20 AS runtime
RUN apk add --no-cache ca-certificates
COPY --from=build /out/purser /usr/local/bin/purser
ENV PURSER_ADDR=":4006"
EXPOSE 4006
ENTRYPOINT ["purser"]
