# syntax=docker/dockerfile:1.6

########################
# Build stage
########################
FROM golang:1.22-bookworm AS builder

WORKDIR /src

# Leverage build cache for modules
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

ARG VERSION=dev
ARG COMMIT_SHA=unknown
ENV CGO_ENABLED=0 GOOS=linux GOARCH=amd64

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go build \
      -trimpath \
      -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT_SHA}" \
      -o /out/server \
      ./cmd/server

########################
# Runtime stage
########################
FROM gcr.io/distroless/static-debian12:nonroot AS runtime

LABEL org.opencontainers.label-schema.name="simple-http-server" \
      org.opencontainers.label-schema.vendor="Principal-SRE" \
      org.opencontainers.label-schema.description="Simple HTTP server with SSL, proxy, and load balancing support"

WORKDIR /app

COPY --from=builder /out/server /app/server
COPY --chown=nonroot:nonroot config/config.yaml /app/config/config.yaml

# Non-root, read-only friendly
USER nonroot:nonroot

EXPOSE 8080 8443

ENV PORT=8080 \
    CONFIG_PATH=/app/config/config.yaml

ENTRYPOINT ["/app/server"]
CMD ["--config", "/app/config/config.yaml"]
```

---
