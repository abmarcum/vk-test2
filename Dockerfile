# syntax=docker/dockerfile:1.6

########################
# Stage 1: Build
########################
FROM golang:1.22-bookworm AS builder

WORKDIR /src

# Cache go modules
COPY go.mod go.sum ./
RUN go mod download

# Copy source
COPY . .

ENV CGO_ENABLED=0 \
    GOOS=linux \
    GOARCH=amd64

RUN --mount=type=cache,target=/root/.cache/go-build \
    go build -trimpath -ldflags="-s -w -X main.version=$(git describe --tags --always 2>/dev/null || echo dev)" \
    -o /out/simple-http-server ./cmd/server

########################
# Stage 2: Runtime
########################
FROM gcr.io/distroless/static-debian12:nonroot AS runtime

LABEL org.opencontainers.image.title="simple-http-server" \
      org.opencontainers.image.description="Simple HTTP server with SSL, proxy, and load balancing support" \
      org.opencontainers.image.vendor="engineering" \
      org.opencontainers.image.source="https://github.com/org/simple-http-server"

WORKDIR /app

COPY --from=builder /out/simple-http-server /app/simple-http-server
COPY --chown=nonroot:nonroot config/ /app/config/

USER nonroot:nonroot

EXPOSE 8080 8443

ENTRYPOINT ["/app/simple-http-server"]
CMD ["--config=/app/config/config.yaml"]
```

---
