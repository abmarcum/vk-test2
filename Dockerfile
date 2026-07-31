# syntax=docker/dockerfile:1.6

########################
# Build stage
########################
FROM golang:1.22-alpine AS builder

# Required for CGO_ENABLED=0 static builds and healthcheck tooling
RUN apk add --no-cache ca-certificates git tzdata && update-ca-certificates

WORKDIR /src

# Cache go modules
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
ARG COMMIT_SHA=unknown
ARG BUILD_DATE=unknown

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
    -trimpath \
    -ldflags="-s -w \
      -X main.Version=${VERSION} \
      -X main.CommitSHA=${COMMIT_SHA} \
      -X main.BuildDate=${BUILD_DATE}" \
    -o /out/simple-http-server ./cmd/server

########################
# Final minimal stage
########################
FROM gcr.io/distroless/static-debian12:nonroot AS final

LABEL org.opencontainers.image.title="simple-http-server" \
      org.opencontainers.image.description="Simple HTTP server with SSL, reverse proxy, and load balancing" \
      org.opencontainers.image.source="https://github.com/your-org/simple-http-server"

WORKDIR /app

COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=builder /out/simple-http-server /app/simple-http-server
COPY --from=builder /src/configs /app/configs

USER nonroot:nonroot

EXPOSE 8080 8443

ENTRYPOINT ["/app/simple-http-server"]
CMD ["--config=/app/configs/config.yaml"]
