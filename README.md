# GoProxy

A dependency-free HTTP/HTTPS reverse proxy and load balancer written in pure Go standard library. GoProxy provides:

- **SSL/TLS termination** for HTTPS traffic (TLS 1.2 / 1.3)
- **Reverse proxying** to configurable upstream backend pools
- **Load balancing** with round-robin, least-connections, and random strategies
- **Active health checks** (periodic probing) and **passive health checks** (failure/success tracking on live traffic)
- **Path-prefix based routing** to named backend pools
- **Built-in `/healthz` and `/metrics` endpoints**
- **Graceful shutdown** on `SIGINT` / `SIGTERM`

No third-party Go modules are required — the entire application, including its own minimal YAML-subset config parser, is implemented using only the Go standard library. All source files live at the module root (package `main`).

---

## Table of Contents

- [Architecture Overview](#architecture-overview)
- [Configuration](#configuration)
- [Build Instructions](#build-instructions)
- [Running the Server](#running-the-server)
- [Deployment Instructions](#deployment-instructions)
- [Operational Endpoints](#operational-endpoints)
- [Signals & Lifecycle](#signals--lifecycle)
- [License & Documentation](#license--documentation)

---

## Architecture Overview

| File          | Responsibility |
|---------------|-----------------|
| `main.go`     | Process lifecycle, config loading, TLS/listener bootstrap, graceful shutdown, and the `healthcheck` CLI subcommand. |
| `config.go`   | `Config` structs + dependency-free YAML-subset parser (`LoadConfig`). |
| `proxy.go`    | `Router`, `ProxyServer` (reverse-proxy handler), `Mux` (routes `/healthz`, `/metrics`, `/`). |
| `balancer.go` | `Backend` / `Pool` state, `Strategy` interface (round-robin, least-connections, random), active + passive health checking. |
| `metrics.go`  | In-process counter/gauge metrics registry exposed via `/metrics`. |

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

GoProxy reads a YAML configuration file (path via `-config` flag or `CONFIG_PATH` env var, default `config.yaml`). Only a subset of YAML is supported (block mappings, block sequences, scalars, `#` comments).

```yaml
server:
  http_addr: ":8080"
  https_addr: ":8443"
  enable_tls: true
  shutdown_grace_seconds: 15
  tls:
    cert_file: "certs/server.crt"
    key_file: "certs/server.key"
    min_version: "1.2"
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
    strategy: "round_robin"
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
| `
