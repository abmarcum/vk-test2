# GoProxy

A simple, production-grade HTTP server written in Go that supports **TLS/SSL termination**, **reverse proxying**, and **load balancing** across multiple backend pools. It ships with active/passive health checking, Prometheus metrics, structured logging, and safe defaults suitable for running at the edge of a service.

---

## Features

- **HTTP & HTTPS listeners** — run plaintext HTTP, HTTPS, or both simultaneously, with automatic HTTP → HTTPS redirection.
- **TLS certificate sourcing** — load certs/keys from local files or from **GCP Secret Manager**.
- **Reverse proxying** — path-prefix based routing to named backend pools via `net/http/httputil.ReverseProxy`.
- **Load balancing strategies** — `round_robin`, `least_connections`, `random`.
- **Health checking** — both active (periodic HTTP probes) and passive (derived from live traffic outcomes), with configurable healthy/unhealthy thresholds.
- **Observability** — Prometheus metrics (`/metrics`) and structured JSON logs (`log/slog`) for every proxied request.
- **Operational endpoints** — `/healthz` for liveness/readiness, always reachable regardless of configured routes.
- **Security-conscious defaults**:
  - Panic recovery middleware — a single bad handler cannot crash the process.
  - Hop-by-hop header stripping (RFC 7230).
  - No leakage of backend identity, stack traces, or internal errors to clients (all errors are masked to generic JSON responses).
  - SSRF-resistant backend URL validation (only `http`/`https`, absolute URLs required).
  - Health-check HTTP client disables redirect following and enforces strict timeouts.
  - `X-Forwarded-For` is only appended with a validated IP literal to prevent header injection.
- **Graceful shutdown** with configurable grace period.

---

## Architecture Overview

```
                     ┌─────────────────────────┐
   Client ─────────► │   HTTP/HTTPS Listener    │
                     │ (redirect, TLS terminate)│
                     └────────────┬─────────────┘
                                  │
                     ┌────────────▼─────────────┐
                     │   Logging / Recovery MW   │
                     └────────────┬─────────────┘
                                  │
                     ┌────────────▼─────────────┐
                     │  ServeMux: /healthz,      │
                     │  /metrics, / (proxy)      │
                     └────────────┬─────────────┘
                                  │
                     ┌────────────▼─────────────┐
                     │       Router              │
                     │ (longest-prefix match)    │
                     └────────────┬─────────────┘
                                  │
                     ┌────────────▼─────────────┐
                     │        Pool                │
                     │ (Strategy + Backends +     │
                     │  Health Checker)           │
                     └────────────┬─────────────┘
                                  │
                     ┌────────────▼─────────────┐
                     │  httputil.ReverseProxy     │
                     │ (Director / ModifyResponse │
                     │  / ErrorHandler)            │
                     └────────────┬─────────────┘
                                  │
                            Upstream Backend
```

### Module responsibilities

| File | Responsibility |
|---|---|
| `main.go` | Process entrypoint wiring: server startup, TLS listeners, HTTPS redirect handler, panic-recovery middleware. |
| `config.go` | YAML configuration loading, defaulting, validation, duration parsing, and TLS material resolution (file or Secret Manager). |
| `balancer.go` | `Backend`, `Pool`, load-balancing `Strategy` implementations, and active health-check engine. |
| `proxy.go` | `Router`, `ProxyServer` (Director/ModifyResponse/ErrorHandler), Prometheus `Metrics`, request logging, and `/healthz`/`/metrics` HTTP handlers. |

---

## Getting Started

### Prerequisites

- Go 1.21+
- (Optional) A GCP project with Secret Manager enabled, if using `cert_source: secretmanager`.

### Build

```bash
go build -o goproxy .
```

### Run

```bash
./goproxy -config config.yaml
```

> The exact flag name depends on your `main.go` flag wiring; adjust to match your entrypoint (e.g. `-config`, `--config`, or `CONFIG_PATH` env var).

---

## Configuration

Configuration is supplied as a single YAML file. Unknown fields are rejected at load time to catch typos early.

### Minimal example

```yaml
server:
  http_addr: ":8080"
  https_addr: ":8443"
  enable_tls: false
  shutdown_grace_seconds: 30
  timeouts:
    read_header: "5s"
    read: "15s"
    write: "15s"
    idle: "60s"
    dial: "5s"
    proxy_total: "30s"

routes:
  - path_prefix: "/api"
    pool: "api-backend"
  - path_prefix: "/"
    pool: "web-backend"

pools:
  - name: "api-backend"
    strategy: "round_robin"
    health_check:
      enabled: true
      path: "/healthz"
      interval: "10s"
      timeout: "2s"
      healthy_threshold: 2
      unhealthy_threshold: 3
    backends:
      - url: "http://10.0.0.1:9000"
        weight: 1
      - url: "http://10.0.0.2:9000"
        weight: 1

  - name: "web-backend"
    strategy: "least_connections"
    backends:
      - url: "http://10.0.1.1:8000"
```

### Full reference

#### `server`

| Field | Type | Default | Description |
|---|---|---|---|
| `http_addr` | string | `:8080` | Address for the plaintext HTTP listener. |
| `https_addr` | string | `:8443` | Address for the TLS listener. |
| `enable_tls` | bool | `false` | Enables HTTPS listener and (when both listeners are active) HTTP→HTTPS redirect. |
| `shutdown_grace_seconds` | int | `30` | Graceful shutdown timeout in seconds. |
| `tls` | [TLS](#servertls) | — | TLS certificate configuration (required if `enable_tls: true`). |
| `timeouts` | [Timeouts](#servertimeouts) | — | Server and proxy timeout tunables. |

#### `server.tls`

| Field | Type | Default | Description |
|---|---|---|---|
| `cert_source` | string | `file` | `file` or `secretmanager`. |
| `cert_file` | string | — | Path to PEM certificate (required if `cert_source: file`). |
| `key_file` | string | — | Path to PEM private key (required if `cert_source: file`). |
| `cert_secret_name` | string | — | Full Secret Manager resource name for the cert (required if `cert_source: secretmanager`), e.g. `projects/p/secrets/cert/versions/latest`. |
| `key_secret_name` | string | — | Full Secret Manager resource name for the key. |
| `min_version` | string | `1.2` | Minimum TLS version: `1.2` or `1.3`. |

#### `server.timeouts`

All values are Go duration strings (e.g. `"5s"`, `"500ms"`).

| Field | Default | Description |
|---|---|---|
| `read_header` | `5s` | Max time to read request headers. |
| `read` | `15s` | Max time to read the full request. |
| `write` | `15s` | Max time to write the response. |
| `idle` | `60s` | Keep-alive idle timeout. |
| `dial` | `5s` | Upstream dial timeout. |
| `proxy_total` | `30s` | Total budget for a single proxied request round-trip. |

#### `routes[]`

| Field | Type | Description |
|---|---|---|
| `path_prefix` | string | Must start with `/`. Longest-prefix match wins across all configured routes. |
| `pool` | string | Name of a pool defined in `pools[]`. |

> `/healthz` and `/metrics` are always served directly by the server and take precedence over any configured route, regardless of `path_prefix`.

#### `pools[]`

| Field | Type | Default | Description |
|---|---|---|---|
| `name` | string | — | Unique pool identifier. |
| `strategy` | string | `round_robin` | `round_robin`, `least_connections`, or `random`. |
| `health_check` | [HealthCheck](#poolshealth_check) | — | Active health-check configuration. |
| `backends` | [Backend[]](#poolsbackends) | — | List of upstream servers. |

#### `pools[].health_check`

| Field | Type | Default | Description |
|---|---|---|---|
| `enabled` | bool | `false` | Enables active probing for the pool. |
| `path` | string | `/healthz` | HTTP path probed on each backend. Must start with `/`. |
| `interval` | string | `10s` | Time between probes. |
| `timeout` | string | `2s` | Per-probe timeout. |
| `healthy_threshold` | int | `2` | Consecutive successes required to mark a backend healthy. |
| `unhealthy_threshold` | int | `3` | Consecutive failures required to mark a backend unhealthy. |

#### `pools[].backends`

| Field | Type | Default | Description |
|---|---|---|---|
| `url` | string | — | Absolute `http://` or `https://` URL. Required. |
| `weight` | int | `1` | Reserved for weighted strategies; must be `>= 0`. |

---

## TLS Certificate Sourcing

### File-based (default)

```yaml
server:
  enable_tls: true
  tls:
    cert_source: file
    cert_file: /etc/goproxy/tls/tls.crt
    key_file: /etc/goproxy/tls/tls.key
    min_version: "1.2"
```

### GCP Secret Manager

```yaml
server:
  enable_tls: true
  tls:
    cert_source: secretmanager
    cert_secret_name: "projects/my-project/secrets/goproxy-cert/versions/latest"
    key_secret_name: "projects/my-project/secrets/goproxy-key/versions/latest"
    min_version: "1.3"
```

Application Default Credentials (ADC) are used to authenticate to Secret Manager. Ensure the runtime service account has `roles/secretmanager.secretAccessor` on the referenced secrets.

Certificate material is resolved once at startup; secret access errors are wrapped generically (no secret identifiers or payload content are ever logged).

---

## Load Balancing Strategies

| Strategy | Behavior |
|---|---|
| `round_robin` | Cycles through healthy backends in order. Good default for homogeneous backends. |
| `least_connections` | Routes to the healthy backend with the fewest in-flight requests. Best for backends with variable request processing time. |
| `random` | Uniformly random healthy backend selection. |

Unhealthy backends (per active or passive health checks) are excluded from selection automatically. If no backend in a pool is healthy, the proxy returns `503 Service Unavailable`.

---

## Health Checking

Two independent mechanisms feed backend health state:

1. **Active health checks** — a background goroutine periodically issues `GET` requests to `health_check.path` on each backend. Consecutive successes/failures against `healthy_threshold` / `unhealthy_threshold` flip the backend's alive state. The health-check HTTP client:
   - Enforces a strict per-probe timeout.
   - Does **not** follow redirects (mitigates SSRF via redirect chains).
   - Uses TLS 1.2+ when probing `https://` backends.

2. **Passive health checks** — derived from real proxied traffic outcomes (see `Backend.MarkSuccess` / `Backend.MarkFailure` in `balancer.go`), providing faster reaction to failures between active probe intervals when wired into the proxy's error paths.

Backend health state is exported as the `goproxy_backend_up` Prometheus gauge (`1` = healthy, `0` = unhealthy).

---

## Observability

### Structured logs

All requests are logged via `log/slog` as structured JSON, including method, path (query stripped), matched route/pool, selected backend, status code, latency, bytes out, client IP, and user agent. Panics are recovered and logged without leaking stack traces to the client.

### Metrics

Exposed at `GET /metrics` in Prometheus exposition format. See [docs/api.md](docs/api.md#metrics) for the full metric catalog.

### Liveness / Readiness

`GET /healthz` returns:

```json
{"status":"ok"}
```

or, when no pool has any healthy backend:

```json
{"status":"unhealthy"}
```

with HTTP `503`. This endpoint never includes internal error detail or backend addresses and is always routable, independent of configured `routes`.

---

## Security Notes

- **Error masking**: all client-facing error responses are generic JSON (`{"error":"..."}`); internal errors, backend hostnames, and stack traces are never returned to clients.
- **Panic isolation**: `LoggingRecoverMiddleware` converts any handler panic into a `500` response and a redacted log entry.
- **SSRF mitigation**: backend URLs are validated at config-load time (absolute, `http`/`https` only); health-check probes disable redirect following.
- **Header hygiene**: hop-by-hop headers (RFC 7230 §6.1) are stripped in both directions; `Server`/`X-Powered-By` response headers from upstreams are removed before returning to clients.
- **Forwarded headers**: `X-Forwarded-For` is only appended with a parsed, valid IP literal — unparseable/malicious values are dropped rather than propagated.
- **No environment-derived proxying**: the upstream `http.Transport` explicitly disables `Proxy` (never honors `HTTP_PROXY`/`HTTPS_PROXY` env vars) for outbound backend calls.

---

## Development

### Project layout

```
.
├── main.go       # entrypoint, HTTPS redirect, panic recovery middleware
├── config.go     # YAML config loading/validation, TLS material resolution
├── balancer.go   # backends, pools, LB strategies, active health checks
├── proxy.go      # router, reverse proxy hooks, metrics, healthz/metrics handlers
└── docs/
    └── api.md    # HTTP endpoint & metrics reference
```

### Running tests

```bash
go test ./...
```

### Linting / vetting

```bash
go vet ./...
```

---
