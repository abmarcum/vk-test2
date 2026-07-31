# goproxy

A production-grade, single-binary HTTP/HTTPS reverse proxy and load balancer
written in Go. It terminates TLS, routes requests to named backend pools by
path prefix, load-balances across pool members using pluggable strategies,
performs active + passive health checking, and exposes Prometheus metrics
and a JSON health endpoint — all driven by a single YAML configuration file.

## Features

- **HTTP & HTTPS listeners** — run plaintext, TLS, or both simultaneously.
- **Automatic HTTP → HTTPS redirect** when TLS is enabled, with `/healthz`
  always reachable over plaintext for external load balancer probes.
- **TLS certificate sourcing** from a local file pair or from **GCP Secret
  Manager**, resolved once at startup.
- **Path-prefix routing** with longest-prefix-match semantics (most specific
  route wins), independent of declaration order in the config.
- **Load balancing strategies**: `round_robin`, `least_connections`, `random`.
- **Active health checks** — periodic HTTP probes per pool with configurable
  path, interval, timeout, and healthy/unhealthy thresholds.
- **Passive health checks** — backend health is also adjusted based on
  observed proxy request outcomes.
- **Prometheus metrics** at `/metrics` (request counts, latency histograms,
  in-flight gauges, upstream error counts, per-backend health gauges,
  response size histograms).
- **JSON health endpoint** at `/healthz` for liveness/readiness probes,
  never leaking internal topology.
- **Hardened reverse proxy plumbing** — hop-by-hop header stripping,
  `X-Forwarded-*` header handling with IP validation (anti-injection),
  connection pooling/timeouts, panic recovery middleware, and opaque
  error responses (no internal error detail, stack traces, or backend
  addresses ever reach the client).
- **Structured logging** via `log/slog`, with query strings/paths redacted
  from logs by default.
- **Graceful shutdown** with configurable grace period.

## Architecture Overview

```
                     ┌─────────────────────┐
   :8080 (HTTP)  ──▶ │  HTTPS redirect      │──▶ 301 to :8443 (except /healthz)
                     └─────────────────────┘

                     ┌─────────────────────┐
   :8443 (HTTPS) ──▶ │      ServeMux        │
                     │  /healthz  /metrics  │
                     │  /  → ProxyServer     │
                     └──────────┬──────────┘
                                │
                     ┌──────────▼──────────┐
                     │       Router         │  longest-prefix match
                     └──────────┬──────────┘
                                │
                     ┌──────────▼──────────┐
                     │        Pool          │  strategy + health check
                     │  (round_robin |       │
                     │   least_connections | │
                     │   random)             │
                     └──────────┬──────────┘
                                │
                    ┌───────────┼───────────┐
                    ▼           ▼           ▼
               Backend A   Backend B   Backend C
```

Requests are dispatched by `ProxyServer.ServeHTTP`, which attaches
request-scoped state to the context, delegates to an `httputil.ReverseProxy`
whose `Director` resolves the route/pool/backend, and whose
`ModifyResponse`/`ErrorHandler` hooks finalize accounting, header
sanitization, and safe error translation. `/healthz` and `/metrics` are
registered ahead of the catch-all proxy handler so they always take
precedence over any user-configured route.

## Getting Started

### Prerequisites

- Go 1.21+
- (Optional) A GCP project with Secret Manager enabled, if using
  `cert_source: secretmanager`.

### Build

```bash
go build -o goproxy .
```

### Run

```bash
./goproxy -config config.yaml
```

The server reads its configuration once at startup; restart the process to
pick up configuration changes.

## Configuration

Configuration is a single YAML file. See [`docs/api.md`](docs/api.md) for
the complete reference of every field, type, default, and validation rule.

### Minimal example

```yaml
server:
  http_addr: ":8080"
  https_addr: ":8443"
  enable_tls: false

routes:
  - path_prefix: "/"
    pool: "web"

pools:
  - name: "web"
    strategy: "round_robin"
    health_check:
      enabled: true
      path: "/healthz"
    backends:
      - url: "http://127.0.0.1:9001"
      - url: "http://127.0.0.1:9002"
```

### TLS from local files

```yaml
server:
  enable_tls: true
  https_addr: ":8443"
  tls:
    cert_source: "file"
    cert_file: "/etc/goproxy/tls/tls.crt"
    key_file: "/etc/goproxy/tls/tls.key"
    min_version: "1.3"
```

### TLS from GCP Secret Manager

```yaml
server:
  enable_tls: true
  tls:
    cert_source: "secretmanager"
    cert_secret_name: "projects/my-project/secrets/proxy-cert/versions/latest"
    key_secret_name:  "projects/my-project/secrets/proxy-key/versions/latest"
    min_version: "1.2"
```

The process must run with credentials (ADC, Workload Identity, etc.)
authorized to call `secretmanager.versions.access` on the referenced secrets.

### Multiple pools and routes

```yaml
routes:
  - path_prefix: "/api/"
    pool: "api"
  - path_prefix: "/"
    pool: "web"

pools:
  - name: "api"
    strategy: "least_connections"
    health_check:
      enabled: true
      path: "/internal/health"
      interval: "5s"
      timeout: "1s"
      healthy_threshold: 2
      unhealthy_threshold: 3
    backends:
      - url: "http://10.0.1.10:8080"
        weight: 1
      - url: "http://10.0.1.11:8080"
        weight: 1

  - name: "web"
    strategy: "random"
    backends:
      - url: "http://10.0.2.10:80"
```

## Operational Endpoints

| Endpoint    | Description                                                        |
|-------------|---------------------------------------------------------------------|
| `/healthz`  | Liveness/readiness probe. Returns `200 {"status":"ok"}` or `503 {"status":"unhealthy"}`. Never exposes backend detail. |
| `/metrics`  | Prometheus exposition format. See [`docs/api.md`](docs/api.md) for the full metric catalog. |

Both endpoints are registered ahead of user-configured routes and are always
reachable regardless of `routes` configuration. `/healthz` is also exempted
from the HTTP→HTTPS redirect so external load balancers can probe it over
plaintext.

## Observability

- **Metrics**: exposed on a dedicated Prometheus registry (not the global
  default registry) to keep the metrics surface hermetic and testable.
- **Logging**: structured JSON/text via `log/slog`. Every proxied request
  emits one `proxy_request` log line with method, path (query-string
  redacted), matched route/pool/backend, status, duration, bytes out,
  client IP, and user agent. Log level escalates to `WARN` on 4xx and
  `ERROR` on 5xx.
- **Panic safety**: every request is wrapped in recovery middleware; a
  panicking handler is logged and converted to a generic `500` response
  instead of crashing the process.

## Security Notes

- Backend URLs are validated at config-load time to be absolute `http`/`https`
  URLs with a resolvable host, mitigating SSRF-via-misconfiguration.
- Hop-by-hop headers (`Connection`, `Upgrade`, `Transfer-Encoding`, etc.) are
  stripped in both directions per RFC 7230.
- `X-Forwarded-For` is only ever appended to with a **validated IP literal**
  parsed from `RemoteAddr`; unparseable/spoofable values are dropped rather
  than propagated.
- `X-Forwarded-Proto` is derived primarily from the actual TLS state of the
  connection (`r.TLS != nil`), not blindly trusted from inbound headers.
- Upstream/internal errors are never surfaced to clients — all error
  responses are a fixed, opaque JSON body (`{"error":"..."}"`) with a status
  code chosen from a small, well-defined set (400s/500s), regardless of the
  underlying Go error.
- `Server` and `X-Powered-By` response headers from backends are stripped
  before responses reach the client.
- The outbound proxy transport explicitly disables environment-derived
  proxying (`Proxy: nil`) so upstream calls cannot be redirected via
  `HTTP_PROXY`/`HTTPS_PROXY` env vars.
- Config YAML parsing rejects unknown fields (`KnownFields(true)`) to catch
  typos and prevent silently-ignored misconfiguration.

## Testing

```bash
go test ./...
```

Secret Manager access is abstracted behind a small interface
(`secretAccessor`) to allow hermetic unit testing without live GCP calls.
