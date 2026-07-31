# ---------------------------------------------------------------------------
# Build stage
# ---------------------------------------------------------------------------
FROM golang:1.22-alpine AS builder

# Build metadata (overridable via --build-arg)
ARG VERSION=dev
ARG COMMIT_SHA=unknown
ARG BUILD_DATE=unknown

RUN apk add --no-cache ca-certificates git tzdata

WORKDIR /src

# Cache go modules
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Static, stripped binary for minimal attack surface
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath \
    -ldflags="-s -w \
      -X main.version=${VERSION} \
      -X main.commit=${COMMIT_SHA} \
      -X main.buildDate=${BUILD_DATE}" \
    -o /out/http-server ./cmd/server

# ---------------------------------------------------------------------------
# Final stage - distroless, non-root, minimal
# ---------------------------------------------------------------------------
FROM gcr.io/distroless/static-debian12:nonroot AS final

LABEL org.opencontainers.image.title="simple-http-server" \
      org.opencontainers.image.description="Simple HTTP server with SSL, proxy, and load balancing support" \
      org.opencontainers.image.source="https://github.com/org/simple-http-server" \
      org.opencontainers.image.licenses="Apache-2.0"

WORKDIR /app

COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=builder /usr/share/zoneinfo /usr/share/zoneinfo
COPY --from=builder /out/http-server /app/http-server
COPY --from=builder /src/config/config.yaml /app/config/config.yaml

USER nonroot:nonroot

EXPOSE 8080 8443

ENTRYPOINT ["/app/http-server"]
CMD ["--config=/app/config/config.yaml"]
```

---
