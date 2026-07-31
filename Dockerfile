# syntax=docker/dockerfile:1.6

########################
# Build stage
########################
FROM golang:1.22-bookworm AS builder

WORKDIR /src

# Cache go modules
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/root/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go mod download

COPY . .

ARG VERSION=dev
ARG COMMIT_SHA=unknown

RUN --mount=type=cache,target=/root/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath \
    -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT_SHA}" \
    -o /out/httpserver ./cmd/server

########################
# Runtime stage
########################
FROM gcr.io/distroless/static-debian12:nonroot AS runtime

LABEL org.opencontainers.image.title="simple-http-server" \
      org.opencontainers.image.description="Simple HTTP server with SSL, proxy, and load balancing support" \
      org.opencontainers.image.source="https://github.com/example/simple-http-server"

WORKDIR /app

COPY --from=builder /out/httpserver /app/httpserver
COPY --chown=nonroot:nonroot configs/config.yaml /etc/httpserver/config.yaml

USER nonroot:nonroot

EXPOSE 8080 8443

ENTRYPOINT ["/app/httpserver"]
CMD ["--config=/etc/httpserver/config.yaml"]
