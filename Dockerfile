# Multi-stage build: compiles a static binary, ships in a distroless image.
# =============================================================================

# ---- Build arguments -------------------------------------------------------
ARG GO_VERSION=1.22
ARG APP_NAME=simple-http-server

# ---- Stage 1: Builder -------------------------------------------------------
FROM golang:${GO_VERSION}-alpine AS builder

ARG APP_NAME
ENV CGO_ENABLED=0 \
    GOOS=linux \
    GOARCH=amd64

WORKDIR /src

# Cache dependency downloads separately from source changes
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Build info for observability (/version endpoint, etc.)
ARG VERSION=dev
ARG COMMIT_SHA=unknown
ARG BUILD_DATE=unknown

RUN go build \
    -trimpath \
    -ldflags="-s -w \
      -X 'main.Version=${VERSION}' \
      -X 'main.CommitSHA=${COMMIT_SHA}' \
      -X 'main.BuildDate=${BUILD_DATE}'" \
    -o /out/${APP_NAME} ./cmd/server

# ---- Stage 2: Certificates (for outbound TLS to upstreams) -----------------
FROM alpine:3.19 AS certs
RUN apk add --no-cache ca-certificates && update-ca-certificates

# ---- Stage 3: Final minimal runtime image -----------------------------------
FROM gcr.io/distroless/static-debian12:nonroot AS final

ARG APP_NAME
LABEL org.opencontainers.image.title="Simple HTTP Server" \
      org.opencontainers.image.description="Go HTTP server with SSL termination, reverse proxy, and load balancing" \
      org.opencontainers.image.source="https://github.com/org/simple-http-server" \
      org.opencontainers.image.licenses="Apache-2.0"

WORKDIR /app

COPY --from=certs /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=builder /out/${APP_NAME} /app/server

# Default config location (mounted via ConfigMap/Secret in k8s)
VOLUME ["/etc/simple-http-server", "/etc/simple-http-server/tls"]

USER nonroot:nonroot

EXPOSE 8080 8443

ENTRYPOINT ["/app/server"]
CMD ["--config=/etc/simple-http-server/config.yaml"]
