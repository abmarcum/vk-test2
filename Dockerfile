# syntax=docker/dockerfile:1.6
#
# Simple HTTP Server (SSL / Proxy / Load Balancing)
# Multi-stage build -> static binary -> distroless runtime
# -----------------------------------------------------------

FROM golang:1.22-bookworm AS builder

WORKDIR /src

# Leverage build cache for module downloads
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

ARG VERSION=dev
ARG COMMIT=none
ARG BUILD_DATE=unknown

RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build \
      -trimpath \
      -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.buildDate=${BUILD_DATE}" \
      -o /out/http-server \
      ./cmd/server

# -----------------------------------------------------------
# Runtime: distroless, non-root, minimal attack surface
# -----------------------------------------------------------
FROM gcr.io/distroless/static-debian12:nonroot AS runtime

LABEL org.opencontainers.image.title="simple-http-server" \
      org.opencontainers.image.description="Simple HTTP server with SSL, reverse proxy, and load balancing support" \
      org.opencontainers.image.source="https://github.com/your-org/simple-http-server" \
      org.opencontainers.image.licenses="Apache-2.0"

WORKDIR /app

COPY --from=builder --chown=nonroot:nonroot /out/http-server /app/http-server
COPY --from=builder --chown=nonroot:nonroot /src/configs/config.yaml /app/config.yaml

USER nonroot:nonroot

EXPOSE 8080 8443 9090

# Requires the binary to implement a lightweight `healthcheck` subcommand
# (calls its own /healthz endpoint) since distroless has no shell/curl.
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
  CMD ["/app/http-server", "healthcheck"]

ENTRYPOINT ["/app/http-server"]
CMD ["--config", "/app/config.yaml"]
