# syntax=docker/dockerfile:1.6

########################
# Build stage
########################
FROM golang:1.22-bookworm AS builder

WORKDIR /src

# Cache deps
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
ARG COMMIT_SHA=unknown

ENV CGO_ENABLED=0 \
    GOOS=linux \
    GOARCH=amd64

RUN --mount=type=cache,target=/root/.cache/go-build \
    go build -trimpath \
    -ldflags="-s -w -X main.Version=${VERSION} -X main.CommitSHA=${COMMIT_SHA}" \
    -o /out/http-server ./cmd/server

########################
# Runtime stage
########################
FROM gcr.io/distroless/static-debian12:nonroot AS runtime

LABEL org.opencontainers.image.title="simple-http-server" \
      org.opencontainers.image.description="Simple HTTP server with SSL, proxy, and load balancing support" \
      org.opencontainers.image.source="https://example.com/simple-http-server" \
      org.opencontainers.image.licenses="Apache-2.0"

WORKDIR /app

# Config, TLS certs, and static content directories
COPY --from=builder /out/http-server /app/http-server
COPY --chown=nonroot:nonroot config/ /app/config/

USER nonroot:nonroot

EXPOSE 8080 8443

ENV HTTP_ADDR=":8080" \
    HTTPS_ADDR=":8443" \
    CONFIG_PATH="/app/config/config.yaml" \
    TLS_CERT_PATH="/app/config/tls/tls.crt" \
    TLS_KEY_PATH="/app/config/tls/tls.key" \
    LOG_LEVEL="info"

ENTRYPOINT ["/app/http-server"]
CMD ["--config", "/app/config/config.yaml"]
