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
ARG VERSION=docker
RUN CGO_ENABLED=0 go build \
    -ldflags "-s -w -X github.com/Einlanzerous/purser/internal/version.Version=${VERSION}" \
    -o /out/purser ./cmd/purser

# 2) Runtime: slim Alpine with ca-certificates (HTTPS to the Cloudflare API +
#    SMTP TLS) and wget (busybox) for the compose healthcheck.
FROM alpine:3.20 AS runtime
RUN apk add --no-cache ca-certificates
COPY --from=build /out/purser /usr/local/bin/purser
ENV PURSER_ADDR=":4006"
EXPOSE 4006
ENTRYPOINT ["purser"]
