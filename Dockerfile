# syntax=docker/dockerfile:1.6
############################################
# Build stage
############################################
FROM golang:1.22-alpine AS builder

RUN apk add --no-cache git ca-certificates tzdata && update-ca-certificates

WORKDIR /src

# Leverage build cache for dependencies
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath \
    -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.buildDate=${BUILD_DATE}" \
    -o /out/simple-http-server ./cmd/server

############################################
# Runtime stage (distroless, non-root)
############################################
FROM gcr.io/distroless/static-debian12:nonroot AS runtime

LABEL org.opencontainers.image.title="simple-http-server" \
      org.opencontainers.image.description="Simple HTTP server supporting SSL termination, reverse proxying and load balancing" \
      org.opencontainers.image.source="https://github.com/your-org/simple-http-server" \
      org.opencontainers.image.licenses="Apache-2.0"

WORKDIR /app

COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=builder /out/simple-http-server /app/simple-http-server
COPY config/config.yaml /app/config/config.yaml

USER nonroot:nonroot

EXPOSE 8080 8443

ENTRYPOINT ["/app/simple-http-server"]
CMD ["--config", "/app/config/config.yaml"]
