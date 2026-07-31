# Multi-stage build producing a minimal, non-root, distroless runtime image
# =============================================================================

# ---- Stage 1: Build ---------------------------------------------------------
FROM golang:1.22-alpine AS builder

# Build metadata (overridable via --build-arg)
ARG VERSION=dev
ARG COMMIT_SHA=unknown
ARG BUILD_DATE=unknown

# Required for CGO-free static binaries and correct SSL cert handling
RUN apk add --no-cache ca-certificates git tzdata && update-ca-certificates

WORKDIR /src

# Cache go modules
COPY go.mod go.sum ./
RUN go mod download

# Copy source
COPY . .

# Build a fully static binary
ENV CGO_ENABLED=0 GOOS=linux GOARCH=amd64
RUN go build \
    -ldflags="-s -w \
      -X main.version=${VERSION} \
      -X main.commit=${COMMIT_SHA} \
      -X main.buildDate=${BUILD_DATE}" \
    -o /app/server ./cmd/server

# ---- Stage 2: Runtime ---------------------------------------------------------
FROM gcr.io/distroless/static-debian12:nonroot AS runtime

LABEL org.opencontainers.image.title="simple-http-server" \
      org.opencontainers.image.description="Go HTTP server with SSL, reverse proxy and load balancing" \
      org.opencontainers.image.vendor="SRE Platform Team"

WORKDIR /app

# CA certs for outbound TLS to upstream backends
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=builder /app/server /app/server

# Default config / TLS cert mount points
COPY --from=builder /src/config/server.yaml /app/config/server.yaml

USER nonroot:nonroot

EXPOSE 8080 8443

HEALTHCHECK --interval=15s --timeout=3s --start-period=5s --retries=3 \
  CMD ["/app/server", "-healthcheck"]

ENTRYPOINT ["/app/server"]
CMD ["-config", "/app/config/server.yaml"]
