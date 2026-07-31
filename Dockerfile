# =========================================================================
# Simple HTTP Server (SSL / Reverse Proxy / Load Balancer) - Build Image
# =========================================================================
FROM golang:1.22-alpine AS builder

RUN apk add --no-cache git ca-certificates tzdata build-base && update-ca-certificates

WORKDIR /src

# Leverage Docker layer caching for dependencies
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
ARG COMMIT_SHA=unknown
ARG BUILD_DATE=unknown

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
    -trimpath \
    -ldflags="-s -w -X main.Version=${VERSION} -X main.CommitSHA=${COMMIT_SHA} -X main.BuildDate=${BUILD_DATE}" \
    -o /out/simple-http-server ./cmd/server

# =========================================================================
# Final minimal, non-root, distroless runtime image
# =========================================================================
FROM gcr.io/distroless/static-debian12:nonroot AS runtime

LABEL org.opencontainers.image.title="simple-http-server" \
      org.opencontainers.image.description="Simple HTTP server supporting SSL, reverse proxy and load balancing" \
      org.opencontainers.image.vendor="Your Org" \
      org.opencontainers.image.source="https://github.com/your-org/simple-http-server" \
      org.opencontainers.image.licenses="Apache-2.0"

WORKDIR /app

COPY --from=builder /out/simple-http-server /app/simple-http-server
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY config/ /app/config/

USER nonroot:nonroot

EXPOSE 8080 8443

# The binary is expected to support a lightweight self-check flag
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD ["/app/simple-http-server", "--healthcheck"]

ENTRYPOINT ["/app/simple-http-server"]
CMD ["--config=/app/config/config.yaml"]
