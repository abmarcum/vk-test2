# Multi-stage build producing a minimal, non-root, distroless runtime image
# ============================================================================

# ---- Build arguments ----
ARG GO_VERSION=1.22
ARG APP_NAME=simple-http-server

# ---------------------------------------------------------------------------
# Stage 1: Builder
# ---------------------------------------------------------------------------
FROM golang:${GO_VERSION}-alpine AS builder

ARG APP_NAME
ARG BUILD_VERSION=dev
ARG BUILD_COMMIT=unknown
ARG BUILD_DATE=unknown

RUN apk add --no-cache ca-certificates git tzdata upx && update-ca-certificates

WORKDIR /src

# Leverage build cache for dependencies
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Static, stripped, reproducible build
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
    -trimpath \
    -ldflags="-s -w \
      -X main.version=${BUILD_VERSION} \
      -X main.commit=${BUILD_COMMIT} \
      -X main.buildDate=${BUILD_DATE}" \
    -o /out/${APP_NAME} ./cmd/server \
    && upx --best --lzma /out/${APP_NAME} || true

# ---------------------------------------------------------------------------
# Stage 2: Runtime (distroless, non-root)
# ---------------------------------------------------------------------------
FROM gcr.io/distroless/base-debian12:nonroot AS runtime

ARG APP_NAME
LABEL org.opencontainers.image.title="Simple HTTP Server" \
      org.opencontainers.image.description="Go HTTP server with SSL, reverse proxy and load balancing support" \
      org.opencontainers.image.source="https://github.com/your-org/simple-http-server" \
      org.opencontainers.image.licenses="Apache-2.0"

ENV APP_NAME=${APP_NAME} \
    HTTP_ADDR=":8080" \
    HTTPS_ADDR=":8443" \
    METRICS_ADDR=":9090" \
    CONFIG_PATH="/etc/simple-http-server/config.yaml" \
    TLS_CERT_PATH="/etc/certs/tls.crt" \
    TLS_KEY_PATH="/etc/certs/tls.key"

WORKDIR /app

COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=builder /out/${APP_NAME} /app/simple-http-server

# nonroot user (65532) is baked into the distroless:nonroot image
USER nonroot:nonroot

EXPOSE 8080 8443 9090

# Binary is expected to implement a lightweight `healthcheck` subcommand
# that performs a local TCP/HTTP self-check and exits 0/1 (no shell needed).
HEALTHCHECK --interval=15s --timeout=3s --start-period=10s --retries=3 \
  CMD ["/app/simple-http-server", "healthcheck"]

ENTRYPOINT ["/app/simple-http-server"]
CMD ["serve", "--config", "/etc/simple-http-server/config.yaml"]
