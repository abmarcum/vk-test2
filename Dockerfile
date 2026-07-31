# syntax=docker/dockerfile:1.6

##############################
# Stage 1: Build
##############################
FROM golang:1.22-alpine AS builder

# Build metadata (overridden via --build-arg in CI)
ARG VERSION=dev
ARG COMMIT_SHA=unknown
ARG BUILD_DATE=unknown

WORKDIR /src

# Leverage layer caching for dependencies
COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN apk add --no-cache git ca-certificates \
    && update-ca-certificates

# Static, stripped, reproducible binary
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build \
      -trimpath \
      -ldflags="-s -w \
        -X main.version=${VERSION} \
        -X main.commit=${COMMIT_SHA} \
        -X main.buildDate=${BUILD_DATE}" \
      -o /out/simple-http-server ./cmd/server

##############################
# Stage 2: Runtime (distroless, non-root)
##############################
FROM gcr.io/distroless/static-debian12:nonroot AS runtime

LABEL org.opencontainers.image.title="simple-http-server" \
      org.opencontainers.image.description="Simple HTTP server with SSL, reverse proxy and load balancing support" \
      org.opencontainers.image.source="https://github.com/your-org/simple-http-server" \
      org.opencontainers.image.licenses="Apache-2.0"

WORKDIR /app

COPY --from=builder /out/simple-http-server /app/simple-http-server
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY config/config.yaml /app/config.yaml

# Non-root, read-only friendly
USER nonroot:nonroot

EXPOSE 8080 8443

ENTRYPOINT ["/app/simple-http-server"]
CMD ["--config", "/app/config.yaml"]
