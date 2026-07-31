# GoProxy

GoProxy is a simple, production-grade HTTP/HTTPS server written in Go that
supports:

- **TLS termination** (HTTP and HTTPS listeners, TLS 1.2/1.3)
- **Reverse proxying** to configurable upstream backend pools
- **Load balancing** across backends (round-robin, least-connections, random)
- **Active health checks** (periodic probing with hysteresis-based
  alive/dead flipping)
- **Passive health checks** (failure/success counting on live traffic)
- **Graceful shutdown** on `SIGINT`/`SIGTERM`
- **Operational endpoints**: `/healthz` and `/metrics`

It is implemented entirely with the Go standard library (no third-party
dependencies), targets **Go 1.19+**, and uses the classic `log` package
(not `log/slog`) for maximum compatibility with older toolchains.

---

## Table of Contents

- [Architecture Overview](#architecture-overview)
- [Project Layout](#project-layout)
- [Prerequisites](#prerequisites)
- [Build Instructions](#build-instructions)
- [Configuration](#configuration)
- [Running Locally](#running-locally)
- [Deployment Instructions](#deployment-instructions)
  - [Docker](#docker)
  - [Docker Compose](#docker-compose)
  - [Kubernetes](#kubernetes)
  - [Terraform (optional infra provisioning)](#terraform-optional-infra-provisioning)
- [Operational Endpoints](#operational-endpoints)
- [Signals & Lifecycle](#signals--lifecycle)
- [License & Documentation](#license--documentation)

---

## Architecture Overview

```
                      ┌─────────────────────────────┐
                      │           main.go           │
                      │  process lifecycle, signals │
                      │  server bootstrap, shutdown │
                      └──────────────┬──────────────┘
                                     │
              ┌──────────────────────┼──────────────────────┐
              │                      │                      │
      ┌───────▼───────┐     ┌────────▼────────┐    ┌────────▼────────┐
      │   config.go    │     │   balancer.go   │    │    proxy.go     │
      │ YAML/env config│     │ Pool / Backend  │    │ ReverseProxy,   │
      │ parsing        │     │ Strategy + HC   │    │ Router, Mux,    │
      │                │     │ loop            │    │ Metrics         │
      └────────────────┘     └─────────────────┘    └─────────────────┘
```

- **`main.go`** — entry point. Parses CLI flags/env vars, loads config,
  constructs pools/router/mux, starts HTTP and (optionally) HTTPS
  listeners, wires signal handling, and performs graceful shutdown.
- **`config.go`** — defines `Config`, `ServerConfig`, `TLSConfig`,
  `TimeoutsConfig`, `RouteConfig`, `PoolConfig`, `HealthCheckConfig`,
  `BackendConfig`, and `LoadConfig(ctx, path)` which parses the on-disk
  config file (YAML) with environment variable overrides.
- **`balancer.go`** — `Backend` and `Pool` state, the `Strategy`
  interface (round-robin / least-connections / random), the active
  health-check goroutine loop, and passive failure/success accounting.
- **`proxy.go`** — the reverse-proxy request handler, `Router`
  (route → pool resolution), `Mux` (HTTP route wiring for `/healthz`,
  `/metrics`, and proxied traffic), and `Metrics` collection.

## Project Layout

```
.
├── main.go        # process entry point, lifecycle, TLS/HTTP bootstrap
├── config.go      # config structs + LoadConfig
├── balancer.go    # Pool/Backend, Strategy, active health checks
├── proxy.go       # ReverseProxy, Router, Mux, Metrics
├── config.yaml    # example runtime configuration (create per environment)
├── go.mod
├── go.sum
└── docs/
    └── api.md     # HTTP API / operational endpoint reference
```

## Prerequisites

- **Go 1.19 or newer** (verify with `go version`)
- A POSIX-like build environment (Linux/macOS) or Windows with Go toolchain
- (Optional) **Docker** 20.10+ for containerized builds/deployment
- (Optional) **kubectl** + access to a Kubernetes cluster for K8s deployment
- (Optional) **Terraform** 1.3+ if provisioning cloud infrastructure

No external Go modules are required — the project only imports the
standard library.

## Build Instructions

1. **Clone the repository**

   ```bash
   git clone https://github.com/your-org/goproxy.git
   cd goproxy
   ```

2. **Verify Go toolchain version**

   ```bash
   go version   # must report go1.19 or higher
   ```

3. **Initialize / verify modules** (no external dependencies expected,
   but this validates `go.mod`/`go.sum` integrity)

   ```bash
   go mod tidy
   go mod verify
   ```

4. **Run static checks (recommended)**

   ```bash
   go vet ./...
   gofmt -l .
   ```

5. **Build the binary**

   ```bash
   go build -o bin/goproxy .
   ```

   For a fully static binary (useful for scratch/distroless Docker images):

   ```bash
   CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
     -ldflags="-s -w" \
     -o bin/goproxy .
   ```

6. **Run the test suite** (if/when tests are added under `*_test.go`)

   ```bash
   go test ./... -v
   ```

7. **Execute the binary**

   ```bash
   ./bin/goproxy -config ./config.yaml -log-level info
   ```

   or via `go run`:

   ```bash
   go run . -config ./config.yaml
   ```

## Configuration

GoProxy is configured via a YAML file (default path `config.yaml`,
overridable with `-config` flag or `CONFIG_PATH` env var) and two CLI
flags:

| Flag           | Env Var       | Default        | Description                              |
|----------------|---------------|----------------|-------------------------------------------|
| `-config`      | `CONFIG_PATH` | `config.yaml`  | Path to the YAML configuration file       |
| `-log-level`   | `LOG_LEVEL`   | `info`         | Accepted for CLI compatibility (currently informational; the std `log` package does not filter by level) |

Example `config.yaml`:

```yaml
server:
  http_addr: ":8080"
  https_addr: ":8443"
  enable_tls: true
  shutdown_grace_seconds: 15
  tls:
    cert_file: "/etc/goproxy/tls/server.crt"
    key_file: "/etc/goproxy/tls/server.key"
    min_version: "1.2"      # "1.2" or "1.3"
  timeouts:
    read_header: "5s"
    read: "15s"
    write: "15s"
    idle: "60s"

routes:
  - match: "/api/"
    pool: "api-pool"
  - match: "/"
    pool: "web-pool"

pools:
  - name: "api-pool"
    strategy: "least_connections"   # round_robin | least_connections | random
    backends:
      - url: "http://10.0.1.10:9000"
      - url: "http://10.0.1.11:9000"
    health_check:
      enabled: true
      path: "/healthz"
      interval: "10s"
      timeout: "2s"
      unhealthy_threshold: 3
      healthy_threshold: 2

  - name: "web-pool"
    strategy: "round_robin"
    backends:
      - url: "http://10.0.2.10:8081"
      - url: "http://10.0.2.11:8081"
    health_check:
      enabled: true
      path: "/healthz"
      interval: "10s"
      timeout: "2s"
      unhealthy_threshold: 3
      healthy_threshold: 2
```

> **Note:** `cert_file`/`key_file` are read at startup and loaded into
> `TLSConfig.CertPEM` / `TLSConfig.KeyPEM` for the TLS listener. Ensure
> the process has read access to these paths.

## Running Locally

```bash
# Generate a self-signed cert for local testing (optional)
mkdir -p certs
openssl req -x509 -newkey rsa:2048 -nodes \
  -keyout certs/server.key -out certs/server.crt \
  -days 365 -subj "/CN=localhost"

# Start GoProxy
go run . -config ./config.yaml

# In another terminal
curl http://localhost:8080/healthz
curl -k https://localhost:8443/healthz
curl http://localhost:8080/metrics
```

## Deployment Instructions

### Docker

Create a `Dockerfile` in the project root:

```dockerfile
# ---- build stage ----
FROM golang:1.21-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /out/goproxy .

# ---- runtime stage ----
FROM gcr.io/distroless/static-debian12
WORKDIR /app
COPY --from=builder /out/goproxy /app/goproxy
COPY config.yaml /app/config.yaml
EXPOSE 8080 8443
ENTRYPOINT ["/app/goproxy", "-config", "/app/config.yaml"]
```

Build and run the image:

```bash
# Build the Docker image
docker build -t goproxy:latest .

# Run the container (mount TLS certs and config as needed)
docker run --rm -d \
  --name goproxy \
  -p 8080:8080 -p 8443:8443 \
  -v "$(pwd)/config.yaml:/app/config.yaml:ro" \
  -v "$(pwd)/certs:/etc/goproxy/tls:ro" \
  goproxy:latest

# Verify
curl http://localhost:8080/healthz
docker logs -f goproxy
```

### Docker Compose

`docker-compose.yml`:

```yaml
version: "3.9"
services:
  goproxy:
    build: .
    image: goproxy:latest
    container_name: goproxy
    ports:
      - "8080:8080"
      - "8443:8443"
    volumes:
      - ./config.yaml:/app/config.yaml:ro
      - ./certs:/etc/goproxy/tls:ro
    environment:
      - CONFIG_PATH=/app/config.yaml
      - LOG_LEVEL=info
    restart: unless-stopped
```

Deploy:

```bash
docker compose build
docker compose up -d
docker compose logs -f goproxy
docker compose down   # to stop and remove
```

### Kubernetes

1. **Build and push the image** to your registry:

   ```bash
   docker build -t your-registry/goproxy:1.0.0 .
   docker push your-registry/goproxy:1.0.0
   ```

2. **Create a ConfigMap** for `config.yaml`:

   ```bash
   kubectl create configmap goproxy-config \
