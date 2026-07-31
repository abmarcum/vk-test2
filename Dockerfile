# syntax=docker/dockerfile:1.7
############################
# Stage 1: Build
############################
FROM golang:1.22-alpine AS builder

ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown

WORKDIR /src

RUN apk add --no-cache git ca-certificates tzdata build-base

# Leverage Docker layer caching for modules
COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath \
    -ldflags="-s -w \
      -X main.version=${VERSION} \
      -X main.commit=${COMMIT} \
      -X main.buildDate=${BUILD_DATE}" \
    -o /out/http-server ./cmd/server

############################
# Stage 2: Runtime
############################
FROM alpine:3.19 AS final

LABEL org.opencontainers.image.title="simple-http-server" \
      org.opencontainers.image.description="Simple HTTP server with SSL, reverse-proxy and load-balancing support" \
      org.opencontainers.image.vendor="Platform Engineering" \
      org.opencontainers.image.source="https://github.com/org/simple-http-server"

RUN apk add --no-cache ca-certificates tzdata curl \
    && addgroup -g 10001 -S app \
    && adduser -u 10001 -S app -G app

WORKDIR /app

COPY --from=builder /out/http-server /app/http-server
COPY configs/ /app/configs/

RUN mkdir -p /app/certs /tmp/app \
    && chown -R app:app /app /tmp/app

USER app:app

EXPOSE 8080 8443

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD curl -fsk https://localhost:8443/healthz || curl -fs http://localhost:8080/healthz || exit 1

ENTRYPOINT ["/app/http-server"]
CMD ["--config=/app/configs/config.yaml"]
