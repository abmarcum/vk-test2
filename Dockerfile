# syntax=docker/dockerfile:1.6

# -----------------------------------------------------------------------------
# Stage 1: Build
# -----------------------------------------------------------------------------
FROM golang:1.22-bookworm AS builder

ARG APP_NAME=simple-http-server
ARG VERSION=dev
ARG COMMIT_SHA=unknown

WORKDIR /src

# Cache module downloads separately from source changes
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

# Static, stripped, reproducible binary
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath \
      -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT_SHA}" \
      -o /out/${APP_NAME} ./cmd/server

# -----------------------------------------------------------------------------
# Stage 2: Runtime (distroless, non-root)
# -----------------------------------------------------------------------------
FROM gcr.io/distroless/static-debian12:nonroot AS runtime

ARG APP_NAME=simple-http-server
LABEL org.opencontainers.image.title="Simple HTTP Server" \
      org.opencontainers.image.description="Go HTTP server with SSL termination, reverse proxy and load balancing" \
      org.opencontainers.image.source="https://github.com/your-org/simple-http-server" \
      org.opencontainers.image.licenses="Apache-2.0"

WORKDIR /app

COPY --from=builder /out/${APP_NAME} /app/server
COPY config/config.yaml /app/config/config.yaml

USER nonroot:nonroot

EXPOSE 8080 8443

ENTRYPOINT ["/app/server"]
CMD ["--config", "/app/config/config.yaml"]
