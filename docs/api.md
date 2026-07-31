# GoProxy Configuration & API Reference

This document describes the complete YAML configuration schema and the HTTP endpoints exposed by GoProxy.

---

## Table of Contents

- [Configuration Schema](#configuration-schema)
  - [`server`](#server)
  - [`server.tls`](#servertls)
  - [`server.timeouts`](#servertimeouts)
  - [`routes`](#routes)
  - [`pools`](#pools)
  - [`pools[].health_check`](#poolshealth_check)
  - [`pools[].backends`](#poolsbackends)
- [Validation Rules](#validation-rules)
- [HTTP Endpoints](#http-endpoints)
  - [`GET /healthz`](#get-healthz)
  - [`GET /metrics`](#get-metrics)
  - [Proxy routes (`/*`)](#proxy-routes-)
- [Process Signals](#process-signals)
- [Prometheus Metrics Reference](#prometheus-metrics-reference)
- [HTTP Headers](#http-headers)

---

## Configuration Schema

Configuration is a single YAML document with three top-level keys: `server`, `routes`, and `pools`. Unknown fields are rejected at load time (`KnownFields(true)`) to catch typos early.

### `server`

| Field | Type | Default | Description |
|---|---|---|---|
| `http_addr` | string | `:8080` | Listen address for the plaintext HTTP server. |
| `https_addr` | string | `:8443` | Listen address for the TLS HTTPS server. |
| `enable_tls` | bool | `false` | Whether the HTTPS listener and TLS validation are active. |
| `log_level` | string | `info` | One of `debug`, `info`, `warn`/`warning`, `error`. Unrecognized values fall back to `info`. |
| `tls` | [TLS](#servertls) | — | TLS certificate sourcing and version configuration. |
| `timeouts` | [Timeouts](#servertimeouts) | — | Server and proxy timeout tunables. |
| `shutdown_grace_seconds` | int | `30` | Maximum seconds to wait for graceful shutdown before forcing exit. |

### `server.tls`

| Field | Type | Default | Description |
|---|---|---|---|
| `cert_source` | string | `file` | Either `file` or `secretmanager`. |
| `cert_file` | string | — | Path to PEM certificate file. Required when `cert_source: file`. |
| `key_file` | string | — | Path to PEM private key file. Required when `cert_source: file`. |
| `cert_secret_name` | string | — | Full GCP Secret Manager resource name for the certificate (e.g. `projects/p/secrets/cert/versions/latest`). Required when `cert_source: secretmanager`. |
| `key_secret_name` | string | — | Full GCP Secret Manager resource name for the private key. Required when `cert_source: secretmanager`. |
| `min_version` | string | `1.2` | Minimum TLS version to accept: `1.2` or `1.3`. |
| `redirect_http` | bool | `false` | If `true` and TLS is enabled, plaintext HTTP requests are redirected to HTTPS with `301`. `/healthz` and `/metrics` are exempt and always reachable over plain HTTP. |

> **Note:** Regardless of `cert_source`, certificate and key material is resolved once at startup (and again on `SIGHUP` reload) into memory (`CertPEM`/`KeyPEM`) and is never re-read from disk/Secret Manager during a running process lifetime except on reload.

### `server.timeouts`

All duration fields accept Go duration strings (e.g. `"5s"`, `"250ms"`, `"1m"`).

| Field | Default | Applies to |
|---|---|---|
| `read_header` | `5s` | Time allowed to read request headers (`ReadHeaderTimeout`). |
| `read` | `15s` | Full request read timeout (`ReadTimeout`). |
| `write` | `15s` | Response write timeout (`WriteTimeout`). |
| `idle` | `60s` | Keep-alive idle timeout (`IdleTimeout`). |
| `dial` | `5s` | Upstream TCP dial timeout. |
| `proxy_total` | `30s` | Overall upstream round-trip budget. |

### `routes`

A list of path-prefix-to-pool bindings. At least one route is required.

| Field | Type | Description |
|---|---|---|
| `path_prefix` | string | Must start with `/`. Matched using **longest-prefix-match** — the most specific matching prefix wins, regardless of declaration order. |
| `pool` | string | Name of a pool defined in `pools`. Must reference an existing pool. |

**Example:**

```yaml
routes:
  - path_prefix: "/api/v2"
    pool: "api-v2"
  - path_prefix: "/api"
    pool: "api-v1"
  - path_prefix: "/"
    pool: "web"
```

A request to `/api/v2/users` matches `api-v2` (longest matching prefix), `/api/legacy` matches `api-v1`, and `/anything-else` matches `web`.

> `/healthz` and `/metrics` are reserved and always handled by the built-in operational handlers — they take precedence over any user-defined route with the same or overlapping prefix.

### `pools`

A list of named backend groups. At least one pool is required.

| Field | Type | Default | Description |
|---|---|---|---|
| `name` | string | — | Unique pool identifier. Required, must be unique across all pools. |
| `strategy` | string | `round_robin` | One of `round_robin`, `least_connections`, `random`. |
| `health_check` | [HealthCheck](#poolshealth_check) | — | Active health-check configuration. |
| `backends` | [][Backend](#poolsbackends) | — | List of upstream servers. At least one required. |

### `pools[].health_check`

| Field | Type | Default | Description |
|---|---|---|---|
| `enabled` | bool | `false` | Enables the background active health-check probe loop for this pool. |
| `path` | string | `/healthz` | HTTP path probed on each backend. Must start with `/`. |
| `interval` | duration string | `10s` | Time between probe rounds. |
| `timeout` | duration string | `2s` | Per-probe request timeout. |
| `healthy_threshold` | int | `2` | Consecutive successful probes required to mark a backend healthy. |
| `unhealthy_threshold` | int | `3` | Consecutive failed probes required to mark a backend unhealthy. |

A backend is considered healthy for probe-status purposes if the response status is in the `[200, 400)` range (or matches `ExpectStatus` if configured internally). Health checks use a dedicated HTTP client that does **not** follow redirects (mitigating SSRF via redirect chains) and enforces TLS 1.2+ for HTTPS backend probes.

Backends also track **passive** health via request-outcome observation (`MarkSuccess`/`MarkFailure`), using the same threshold values, providing faster detection between active probe intervals.

### `pools[].backends`

| Field | Type | Default | Description |
|---|---|---|---|
| `url` | string | — | Absolute URL (`http://` or `https://`) of the backend. Required; validated for well-formed scheme and host. |
| `weight` | int | `1` | Reserved for weighted strategies; must be `>= 0`. Currently informational for `round_robin`/`random`/`least_connections`. |

All backends start in a healthy (`alive`) state at startup, so traffic can flow immediately before the first health-check round completes.

---

## Validation Rules

Configuration is rejected at load time (process exits with code `1`, error logged) if any of the following hold:

- Neither `http_addr` nor `https_addr` is set.
- `enable_tls: true` and:
  - `cert_source` is not `file` or `secretmanager`.
  - Required file paths or secret names for the chosen source are missing.
  - `min_version` is not `1.2` or `1.3`.
  - Certificate/key material fails to resolve or load.
- No pools are defined, a pool has no name, a pool name is duplicated, or a pool's `strategy` is not one of the supported values.
- A pool has zero backends, or any backend URL fails scheme/host validation, or has a negative weight.
- A pool's health check is enabled but thresholds are non-positive or its path doesn't start with `/`.
- No routes are defined, a route's `path_prefix` doesn't start with `/`, a route has no `pool`, or a route references a pool name that doesn't exist.

All violations are aggregated into a single error message (semicolon-separated) rather than failing on the first one, to reduce config-fix iteration cycles.

---

## HTTP Endpoints

### `GET /healthz`

Liveness/readiness endpoint. Reserved — always takes precedence over user-defined routes on both the HTTP and HTTPS listeners, and is **never** subject to the HTTPS redirect even when `redirect_http: true`.

- **Methods:** `GET`, `HEAD` (others return `405 Method Not Allowed`)
- **Response `200`:**
  ```json
  {"status":"ok"}
  ```
  Returned when at least one backend across all configured pools is healthy.
- **Response `503`:**
  ```json
  {"status":"unhealthy"}
  ```
  Returned when no backend anywhere is currently healthy.

Headers: `Content-Type: application/json; charset=utf-8`, `Cache-Control: no-store`.

### `GET /metrics`

Prometheus exposition-format metrics endpoint. Reserved — takes precedence over user-defined routes. See [Prometheus Metrics Reference](#prometheus-metrics-reference) below for the full metric catalog.

### Proxy routes (`/*`)

Any path not matched by `/healthz` or `/metrics` is evaluated against the configured `routes` table using longest-prefix-match.

**On successful match and healthy backend selection:**
- Request is forwarded upstream with hop-by-hop headers stripped and `X-Forwarded-Proto`, `X-Forwarded-Host`, `X-Forwarded-For` set/appended.
- Response is relayed back with hop-by-hop headers, `Server`, and `X-Powered-By` stripped; `X-Content-Type-Options: nosniff` added.

**Error responses (always JSON, always generic — no internal details leaked):**

| Status | Condition | Body |
|---|---|---|
| `404 Not Found` | No route matches the request path. | `{"error":"not found"}` |
| `503 Service Unavailable` | Route matched, but its pool has no healthy backend. | `{"error":"service unavailable"}` |
| `502 Bad Gateway` | Upstream connection/transport failure. | `{"error":"bad gateway"}` |
| `504 Gateway Timeout` | Upstream request exceeded the deadline. | `{"error":"gateway timeout"}` |
| `499` (non-standard, nginx convention) | Client disconnected before the response completed. | `{"error":"client closed request"}` |
| `500 Internal Server Error` | A handler panic was recovered. | `{"error":"internal server error"}` |

---

## Process Signals

| Signal | Effect |
|---|---|
| `SIGINT` / `SIGTERM` | Initiates graceful shutdown: stops health checks, stops accepting new connections, waits up to `shutdown_grace_seconds` for in-flight requests, then force-closes remaining connections. |
| `SIGHUP` | Reloads configuration from the original `-config` path. On success, atomically swaps the pool manager, proxy handler, and logger, and restarts health checks against the new pool set. On failure, the running configuration is left untouched and an error is logged. |

---

## Prometheus Metrics Reference

All metrics are under the `goproxy_` namespace and registered on a dedicated registry (not the global default), exposed at `/metrics`.

| Metric | Type | Labels | Description |
|---|---|---|---|
| `goproxy_requests_total` | Counter | `route`, `pool`, `method`, `status` | Total proxied requests, by outcome. |
| `goproxy_request_duration_seconds` | Histogram | `route`, `pool`, `method` | End-to-end request latency. Buckets: `.001`–`5` seconds. |
| `goproxy_in_flight_requests` | Gauge | `pool` | Current number of in-flight requests per pool. |
| `goproxy_upstream_errors_total` | Counter | `pool`, `reason` | Upstream failures, categorized (`no_healthy_backend`, `no_route`, `timeout`, `client_closed`, `bad_gateway`). |
| `goproxy_backend_up` | Gauge | `pool`, `backend` | `1` if backend is healthy, `0` otherwise. |
| `goproxy_response_size_bytes` | Histogram | `route`, `pool` | Response body size distribution. Exponential buckets starting at 128 bytes. |

Standard Go runtime (`go_*`) and process (`process_*`) collectors are also registered.

`route` and `pool` labels report `"unmatched"` / `"none"` respectively when a request could not be routed.

---

## HTTP Headers

### Applied to all responses (proxy + operational endpoints)

| Header | Value |
|---|---|
| `X-Content-Type-Options` | `nosniff` |
| `X-Frame-Options` | `DENY` |
| `Referrer-Policy` | `no-referrer-when-downgrade` |
| `X-XSS-Protection` | `0` |

### Stripped from both directions (hop-by-hop, per RFC 7230 §6.1)

`Connection`, `Proxy-Connection`, `Keep-Alive`, `Proxy-Authenticate`, `Proxy-Authorization`, `Te`, `Trailer`, `Transfer-Encoding`, `Upgrade`

### Added to upstream requests

| Header | Value |
|---|---|
| `X-Forwarded-Proto` | `https` or `http`, based on the original connection. |
| `X-Forwarded-Host` | Original `Host` header value. |
| `X-Forwarded-For` | Client IP appended to any existing chain; only validated IP literals are appended (prevents header injection). |

### Stripped from upstream responses before returning to client

`Server`, `X-Powered-By` (in addition to the hop-by-hop set above).
