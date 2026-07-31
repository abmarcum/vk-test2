# GoProxy

A dependency-free HTTP/HTTPS reverse proxy and load balancer written in pure Go standard library. GoProxy provides:

- **SSL/TLS termination** for HTTPS traffic (TLS 1.2 / 1.3)
- **Reverse proxying** to configurable upstream backend pools
- **Load balancing** with round-robin, least-connections, and random strategies
- **Active health checks** (periodic probing) and **passive health checks** (failure/success tracking on live traffic)
- **Path-prefix based routing** to named backend pools
- **Built-in `/healthz` and `/metrics` endpoints**
- **Graceful shutdown** on `SIGINT` / `SIGTERM`

No third-party Go modules are required — the entire application, including its own minimal YAML-subset config parser, is implemented using only the Go standard library.

---

## Table of Contents

- [Architecture Overview](#architecture-overview)
- [Configuration](#configuration)
- [Build Instructions](#build-instructions)
- [Running the Server](#running-the-server)
- [Deployment Instructions](#deployment-instructions)
  - [Docker](#docker)
  - [docker-compose](#docker-compose)
  - [Kubernetes](#kubernetes)
- [Operational Endpoints](#operational-endpoints)
- [License & Documentation](#license--documentation)

---

## Architecture Overview

| File          | Responsibility |
|---------------|-----------------|
| `main.go`     | Placeholder — entry point wiring lives in `proxy.go`. |
| `config.go`   | `Config` structs + dependency-free YAML-subset parser (`LoadConfig`). |
| `proxy.go`    | Process lifecycle, `Router`, `ProxyServer` (reverse-proxy handler), `Mux` (routes `/healthz`, `/metrics`, `/`), TLS listener setup, graceful shutdown. |
| `balancer.go` | `Backend` / `Pool` state, `Strategy` interface (round-robin, least-connections, random), active + passive health checking. |
| `metrics.go`  | In-process counter/gauge metrics registry exposed via `/metrics` (plain-text exposition). |

Request flow:

```
Client --> [HTTP :8080 / HTTPS :8443] --> Mux
                                            ├── /healthz  -> healthCheckerAdapter
                                            ├── /metrics  -> Metrics registry
                                            └── /*        -> ProxyServer
                                                              ├── Router.Match(path)      -> Pool
                                                              ├── Pool.Choose()           -> Strategy -> Backend
                                                              └── httputil.ReverseProxy   -> Backend upstream
```

---

## Configuration

GoProxy reads a YAML configuration file (path via `-config` flag or `CONFIG_PATH` env var, default `config.yaml`). Only a subset of YAML is supported (block mappings, block sequences, scalars, `#` comments) — sufficient for the schema below.

```yaml
server:
  http_addr: ":8080"
  https_addr: ":8443"
  enable_tls: true
  shutdown_grace_seconds: 15
  tls:
    cert_file: "certs/server.crt"
    key_file: "certs/server.key"
    min_version: "1.2"          # "1.2" or "1.3"
  timeouts:
    read_header: "5s"
    read: "15s"
    write: "15s"
    idle: "60s"

routes:
  - match: "/api/"
    pool: "api_pool"
  - match: "/"
    pool: "web_pool"

pools:
  - name: "api_pool"
    strategy: "round_robin"     # round_robin | least_connections | random
    backends:
      - url: "http://10.0.0.10:9000"
      - url: "http://10.0.0.11:9000"
    health_check:
      enabled: true
      path: "/healthz"
      interval: "10s"
      timeout: "2s"
      unhealthy_threshold: 3
      healthy_threshold: 2

  - name: "web_pool"
    strategy: "least_connections"
    backends:
      - url: "http://10.0.0.20:8000"
    health_check:
      enabled: false
```

### Config Reference

| Key | Type | Default | Description |
|---|---|---|---|
| `server.http_addr` | string | `:8080` | HTTP listener address |
| `server.https_addr` | string | `:8443` | HTTPS listener address |
| `server.enable_tls` | bool | `false` | Enable the HTTPS listener |
| `server.shutdown_grace_seconds` | int | `15` | Graceful shutdown timeout (seconds) |
| `server.tls.cert_file` / `key_file` | string | — | PEM cert/key file paths (required if `enable_tls: true`) |
| `server.tls.min_version` | string | `1.2` | Minimum TLS version: `1.2` or `1.3` |
| `server.timeouts.*` | duration string | `5s/15s/15s/60s` | `ReadHeaderTimeout`, `ReadTimeout`, `WriteTimeout`, `IdleTimeout` |
| `routes[].match` | string | — | Path-prefix to match (longest-prefix wins) |
| `routes[].pool` | string | — | Name of a pool defined in `pools` |
| `pools[].name` | string | — | Unique pool identifier |
| `pools[].strategy` | string | `round_robin` | `round_robin`, `least_connections`, or `random` |
| `pools[].backends[].url` | string | — | Upstream backend base URL |
| `pools[].health_check.enabled` | bool | `false` | Enable active health probing |
| `pools[].health_check.path` | string | `/healthz` | Probe path appended to backend URL |
| `pools[].health_check.interval` | duration | `10s` | Probe interval |
| `pools[].health_check.timeout` | duration | `2s` | Probe timeout |
| `pools[].health_check.unhealthy_threshold` | int | `3` | Consecutive failures before marking backend down |
| `pools[].health_check.healthy_threshold` | int | `2` | Consecutive successes before marking backend up |

---

## Build Instructions

### Prerequisites

- [Go](https://go.dev/dl/) **1.19 or later** (no third-party modules are required — `go.mod` has an empty dependency graph)
- A POSIX shell (Linux/macOS) or PowerShell (Windows)

### 1. Clone the repository

```bash
git clone https://github.com/your-org/goproxy.git
cd goproxy
```

### 2. Verify the module

```bash
go mod tidy
go vet ./...
```

### 3. Build the binary

```bash
# Build for the current platform
go build -o bin/goproxy .

# Or produce a statically linked Linux binary (useful for containers)
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/goproxy .
```

### 4. Run the test suite (if present)

```bash
go test ./...
```

---

## Running the Server

```bash
./bin/goproxy -config config.yaml
```

Or via environment variables:

```bash
export CONFIG_PATH=/etc/goproxy/config.yaml
./bin/goproxy
```

Available flags:

| Flag | Env Var | Default | Description |
|---|---|---|---|
| `-config` | `CONFIG_PATH` | `config.yaml` | Path to the YAML configuration file |
| `-log-level` | `LOG_LEVEL` | `info` | Reserved for CLI compatibility (stdlib `log` does not filter by level) |

The process listens on `server.http_addr` (always) and `server.https_addr` (when `enable_tls: true`), and shuts down gracefully within `shutdown_grace_seconds` on `SIGINT`/`SIGTERM`.

---

## Deployment Instructions

### Docker

**Dockerfile** (multi-stage, produces a minimal static image):

```dockerfile
# syntax=docker/dockerfile:1
FROM golang:1.21-alpine AS builder
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/goproxy .

FROM alpine:3.19
RUN adduser -D -u 10001 goproxy
COPY --from=builder /out/goproxy /usr/local/bin/goproxy
COPY config.yaml /etc/goproxy/config.yaml
USER goproxy
EXPOSE 8080 8443
ENTRYPOINT ["/usr/local/bin/goproxy"]
CMD ["-config", "/etc/goproxy/config.yaml"]
```

Build and run:

```bash
docker build -t goproxy:latest .

docker run -d \
  --name goproxy \
  -p 8080:8080 -p 8443:8443 \
  -v $(pwd)/config.yaml:/etc/goproxy/config.yaml:ro \
  -v $(pwd)/certs:/etc/goproxy/certs:ro \
  goproxy:latest
```

### docker-compose

```yaml
