# syntax=docker/dockerfile:1.7

# ============================================================================
# Build Stage
# ============================================================================
FROM golang:1.22-alpine AS builder

RUN apk add --no-cache git ca-certificates tzdata && update-ca-certificates

WORKDIR /src

# Leverage layer caching for dependencies
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
ARG COMMIT_SHA=unknown

RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
    -trimpath \
    -ldflags="-s -w -X main.Version=${VERSION} -X main.CommitSHA=${COMMIT_SHA}" \
    -o /out/httpserver ./cmd/server

# ============================================================================
# Runtime Stage (distroless, non-root, no shell)
# ============================================================================
FROM gcr.io/distroless/static-debian12:nonroot AS runtime

LABEL org.opencontainers.image.title="simple-http-server" \
      org.opencontainers.image.description="Simple HTTP server with SSL termination, reverse proxy and load balancing" \
      org.opencontainers.image.source="https://github.com/your-org/simple-http-server" \
      org.opencontainers.image.licenses="Apache-2.0"

WORKDIR /app

COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=builder /out/httpserver /app/httpserver
COPY --chown=nonroot:nonroot configs/ /app/configs/

USER nonroot:nonroot

EXPOSE 8080 8443 9090

ENTRYPOINT ["/app/httpserver"]
CMD ["--config", "/app/configs/config.yaml"]
