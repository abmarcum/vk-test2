# GoProxy

A single-binary, SSL-terminating HTTP reverse proxy and load balancer written in Go. GoProxy sits in front of your backend services, terminates TLS, distributes traffic across pools of upstream servers, and provides operational visibility through health checks and Prometheus metrics — all with zero external runtime dependencies beyond the compiled binary and a YAML config file.

## Features

- **SSL/TLS termination** — TLS 1.2+ enforced, hardened cipher suite selection, HTTP→HTTPS redirect support.
- **Certificate sourcing** — load certs/keys from local files or GCP Secret Manager.
- **Reverse proxying** — path-prefix based routing with longest-prefix-match semantics.
- **Load balancing** — round robin, least connections, and random strategies per backend pool.
- **Active + passive health checking** — background probing plus request-outcome-driven health state, with configurable thresholds.
- **Graceful lifecycle management** — SIGTERM/SIGINT graceful shutdown with configurable grace period, SIGHUP hot config reload with no dropped connections.
- **Observability** — structured JSON logging (`log/slog`), Prometheus metrics at `/metrics`, liveness/readiness at `/healthz`.
- **Security hardening** — hop-by-hop header stripping, OWASP baseline security headers, error masking (no internal details leaked to clients), header-injection-safe `X-Forwarded-For` handling, secret redaction in logs.

## Architecture Overview

GoProxy is organized into four core modules:

| File          | Responsibility |
|---------------|----------------|
| `main.go`     | Process lifecycle: startup, signal handling, graceful shutdown, HTTP/HTTPS server bootstrap, route wiring. |
| `config.go`   | YAML configuration loading, validation, defaulting, and TLS certificate material resolution (file or Secret Manager). |
| `proxy.go`    | HTTP routing, `httputil.ReverseProxy` Director/ModifyResponse/ErrorHandler hooks, Prometheus metrics, request logging middleware. |
| `balancer.go` | Backend and pool abstractions, load-balancing strategies, active health-check engine. |

### Request Lifecycle

1. Request arrives on the HTTP or HTTPS listener.
2. Security headers middleware applies baseline hardening headers.
3. The router matches the request path against configured route prefixes (longest-prefix-match wins).
4. The matched pool's load-balancing strategy selects a healthy backend.
5. The reverse proxy forwards the request, stripping hop-by-hop headers and adding `X-Forwarded-*` headers.
6. The response is relayed back to the client with sanitized headers; metrics and structured logs are emitted.
7. If no route matches or no healthy backend is available, a safe, opaque error response (404/503/502/504) is returned — no internal details are ever leaked.

## Installation

### Build from source

Requires Go 1.21+.

```bash
git clone https://github.com/your-org/goproxy.git
cd goproxy
go build -ldflags "-X main.version=$(git describe --tags --always)" -o goproxy .
```

### Run

```bash
./goproxy -config /etc/goproxy/config.yaml
```

Or via environment variable:

```bash
GOPROXY_CONFIG=/etc/goproxy/config.yaml ./goproxy
```

### Check version

```bash
./goproxy -version
```

## Configuration

GoProxy is configured via a single YAML file. See [`docs/api.md`](docs/api.md) for the full schema reference.

### Minimal example

```yaml
server:
  http_addr: ":8080"
  https_addr: ":8443"
  enable_tls: false
  log_level: "info"

routes:
  - path_prefix: "/"
    pool: "web"

pools:
  - name: "web"
    strategy: "round_robin"
    health_check:
      enabled: true
      path: "/healthz"
      interval: "10s"
      timeout: "2s"
      healthy_threshold: 2
      unhealthy_threshold: 3
    backends:
      - url: "http://10.0.0.1:8000"
        weight: 1
      - url: "http://10.0.0.2:8000"
        weight: 1
```

### TLS-enabled example (file-based certs)

```yaml
server:
  http_addr: ":8080"
  https_addr: ":8443"
  enable_tls: true
  log_level: "info"
  tls:
    cert_source: "file"
    cert_file: "/etc/certs/tls.crt"
    key_file: "/etc/certs/tls.key"
    min_version: "1.2"
    redirect_http: true

routes:
  - path_prefix: "/api"
    pool: "api-backend"
  - path_prefix: "/"
    pool: "web"

pools:
  - name: "api-backend"
    strategy: "least_connections"
    backends:
      - url: "https://api1.internal:443"
      - url: "https://api2.internal:443"
  - name: "web"
    strategy: "round_robin"
    backends:
      - url: "http://web1.internal:8080"
      - url: "http://web2.internal:8080"
```

### TLS via GCP Secret Manager

```yaml
server:
  enable_tls: true
  tls:
    cert_source: "secretmanager"
    cert_secret_name: "projects/my-project/secrets/tls-cert/versions/latest"
    key_secret_name: "projects/my-project/secrets/tls-key/versions/latest"
    min_version: "1.3"
```

The runtime must have Google Cloud Application Default Credentials with `roles/secretmanager.secretAccessor` on the referenced secrets.

## Operations

### Health & Readiness

`GET /healthz` returns:

- `200 {"status":"ok"}` if at least one backend across configured pools is healthy.
- `503 {"status":"unhealthy"}` otherwise.

This endpoint is always served on both HTTP and HTTPS listeners and is never redirected, even when `redirect_http` is enabled — suitable for Kubernetes liveness/readiness probes and cloud load balancer health checks.

### Metrics

`GET /metrics` exposes Prometheus exposition format metrics, including:

- `goproxy_requests_total{route,pool,method,status}`
- `goproxy_request_duration_seconds{route,pool,method}`
- `goproxy_in_flight_requests{pool}`
- `goproxy_upstream_errors_total{pool,reason}`
- `goproxy_backend_up{pool,backend}`
- `goproxy_response_size_bytes{route,pool}`

Plus standard Go runtime and process collectors.

### Graceful Reload (SIGHUP)

```bash
kill -HUP <pid>
```

Re-reads the configuration file, rebuilds the pool manager, proxy handler, and health-check loop, and atomically swaps them in without dropping in-flight connections. If the new configuration fails validation or fails to load, the previous configuration continues serving traffic and an error is logged.

### Graceful Shutdown (SIGINT/SIGTERM)

```bash
kill -TERM <pid>
```

Stops accepting new connections, cancels background health checks, and waits up to `shutdown_grace_seconds` (default 30s) for in-flight requests to complete before forcing termination.

## Security Notes

- TLS 1.2 is the enforced minimum; a curated modern cipher suite list is used (ECDHE + AES-GCM/ChaCha20-Poly1305 only).
- Backend URLs are validated at config load time to require `http`/`https` schemes and a non-empty host, reducing SSRF-via-misconfiguration risk.
- Client-supplied `X-Forwarded-For` values are validated as parseable IP literals before being appended, preventing header injection.
- Error responses to clients are always generic (`{"error":"..."}`) — internal error strings, backend addresses, and stack traces are never returned to callers; details go only to structured logs.
- Log output redacts common secret-bearing keys (`password`, `secret`, `token`, `api_key`, `private_key`, `authorization`).
- A panic in any request handler is recovered and converted into a `500` response rather than crashing the process.

## Load Balancing Strategies

| Strategy | Behavior |
|---|---|
| `round_robin` | Cycles through healthy backends in order (default). |
| `least_connections` | Routes to the healthy backend with the fewest in-flight requests. |
| `random` | Selects a uniformly random healthy backend. |

Unknown/unspecified strategy values default to `round_robin` to fail safe.

## Development

```bash
go vet ./...
go test ./...
go build ./...
```

### Project layout

```
.
├── main.go       # lifecycle, servers, signal handling
├── config.go     # YAML config loading & validation
├── proxy.go      # routing, reverse proxy hooks, metrics, middleware
├── balancer.go   # backends, pools, LB strategies, health checks
└── docs/
    └── api.md    # configuration schema & HTTP endpoint reference
