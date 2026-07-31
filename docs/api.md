# GoProxy API & Configuration Reference

This document specifies the runtime HTTP surface exposed by GoProxy (operational
endpoints and reverse-proxy data-plane behavior) and the full on-disk YAML
configuration schema consumed via `LoadConfig`.

## Table of Contents

- [HTTP Endpoints](#http-endpoints)
  - [GET /healthz](#get-healthz)
  - [GET /metrics](#get-metrics)
  - [Reverse Proxy Data Plane (`/`)](#reverse-proxy-data-plane-)
- [Request Forwarding Contract](#request-forwarding-contract)
- [Error Responses](#error-responses)
- [Load Balancing Strategies](#load-balancing-strategies)
- [Health Checking](#health-checking)
- [Configuration Schema](#configuration-schema)
- [Process Signals](#process-signals)

---

## HTTP Endpoints

All endpoints are served on **both** the HTTP and HTTPS listeners defined in
`server.http_addr` / `server.https_addr`. There is no separate admin port.

### GET /healthz

Reports aggregate process liveness derived from backend health state.

**Request**

```
GET /healthz HTTP/1.1
Host: proxy.example.com
```

**Responses**

| Status | Body           | Condition                                                              |
|--------|----------------|-------------------------------------------------------------------------|
| `200`  | `ok`           | At least one backend across all configured pools is currently `Alive`, or no pools are configured. |
| `503`  | `unavailable`  | Every configured backend across every pool is currently marked unhealthy. |

`Content-Type: text/plain` in both cases. This endpoint performs **no**
active probe of its own — it reflects the live state maintained by each
pool's active/passive health checking (see [Health Checking](#health-checking)).

Suitable for use directly as a Kubernetes `readinessProbe` / `livenessProbe`,
or a cloud load balancer health check target.

### GET /metrics

Returns a plaintext snapshot of process counters and gauges. Format is a
simple `name value` (or `name{labels} value`) line-oriented text format,
intentionally dependency-free (no Prometheus client library required).

**Request**

```
GET /metrics HTTP/1.1
Host: proxy.example.com
```

**Response** — `200 OK`, `Content-Type: text/plain`

```
proxy_requests_total 1523
proxy_requests_failed_total 4
proxy_last_request_duration_seconds 0.0123
```

| Metric                                    | Type    | Description                                                     |
|--------------------------------------------|---------|-------------------------------------------------------------------|
| `proxy_requests_total`                     | counter | Total requests successfully forwarded to an upstream backend.     |
| `proxy_requests_failed_total`               | counter | Total requests that failed: no matching route, no healthy backend, or upstream I/O error. |
| `proxy_last_request_duration_seconds`       | gauge   | Wall-clock duration of the most recently completed proxied request. |

### Reverse Proxy Data Plane (`/`)

Any request not matching `/healthz` or `/metrics` is handled by the proxy
data plane:

1. **Route resolution** — the request's `URL.Path` is matched against all
   configured `routes[].match` prefixes; the **longest matching prefix**
   wins (longest-prefix-match routing, similar to typical L7 proxies).
2. **Backend selection** — the matched pool's configured `Strategy`
   (`round_robin`, `least_connections`, or `random`) selects one currently
   healthy (`Alive == true`) backend.
3. **Forwarding** — the request is forwarded via a single-host reverse
   proxy to the chosen backend, with the headers described below.
4. **Outcome accounting** — a successful upstream response increments the
   backend's passive success counter (see [Health Checking](#health-checking));
   a transport-level error increments its passive failure counter and
   returns `502 Bad Gateway` to the client.

## Request Forwarding Contract

For every forwarded request, GoProxy sets/overwrites the following headers
before dispatching to the backend:

| Header               | Value                                                             |
|-----------------------|--------------------------------------------------------------------|
| `Host`                | The backend's own host (`backend.URL.Host`), not the original client's `Host`. |
| `X-Forwarded-For`     | The immediate client's IP address (parsed from `RemoteAddr`; not chained/appended from any existing header, to avoid spoofing via client-supplied XFF). |
| `X-Forwarded-Proto`   | `http` or `https`, reflecting the scheme of the *inbound* connection to GoProxy. |
| `X-Forwarded-Host`    | The original `Host` header value presented by the client.         |

The response from the backend (status code, headers, and body) is streamed
back to the client unmodified, aside from the baseline security headers
GoProxy adds to every response it emits:

| Header                     | Value          |
|-----------------------------|----------------|
| `X-Content-Type-Options`    | `nosniff`      |
| `X-Frame-Options`           | `DENY`         |
| `Referrer-Policy`           | `no-referrer`  |

## Error Responses

| Status | Body                          | Condition                                                       |
|--------|-------------------------------|--------------------------------------------------------------------|
| `400`  | `bad request`                 | Malformed `Host` header on the HTTP→HTTPS redirect handler (CRLF/control characters rejected). |
| `404`  | `not found`                   | No configured route prefix matches the request path.               |
| `502`  | `bad gateway`                 | The selected backend was unreachable or returned a transport-level error. |
| `503`  | `no healthy backends available` | The matched pool has zero backends currently marked `Alive`.     |
| `500`  | `internal server error`       | An unhandled panic was recovered by the server's recovery middleware (detail is logged server-side only, never disclosed to the client). |

## Load Balancing Strategies

Configured per-pool via `pools[].strategy`:

| Strategy              | Config value          | Behavior                                                                 |
|------------------------|------------------------|-----------------------------------------------------------------------------|
| Round Robin (default)  | `round_robin`          | Cycles through healthy backends in order using an atomic counter.           |
| Least Connections      | `least_connections`    | Selects the healthy backend with the fewest in-flight active connections.   |
| Random                 | `random`               | Selects a uniformly random healthy backend (time-seeded, non-cryptographic; not for security-sensitive selection). |

Only backends currently marked `Alive` participate in selection. If none are
alive, `Choose()` returns `ErrNoHealthyBackends`, surfaced to clients as
`503 no healthy backends available`.

## Health Checking

Each pool independently maintains backend liveness via two complementary
mechanisms:

### Active health checks (`pools[].health_check`)

- Runs on a fixed `interval` (default `10s`), probing each backend with an
  HTTP `GET` request to `path` (default `/healthz`), using a per-request
  `timeout` (default `2s`).
- The probe client **does not follow redirects** (`http.ErrUseLastResponse`)
  and enforces standard Go TLS defaults for HTTPS backend targets.
- A response with status `200–399` counts as a success; anything else
  (including transport errors) counts as a failure.
- Consecutive successes/failures are tracked per-backend; state flips are
  governed by hysteresis thresholds (see below).

### Passive health checks

- Every proxied request's outcome (success vs. upstream transport error)
  also feeds the same success/failure counters, so backends that begin
  failing real traffic are demoted between active probe cycles.

### Hysteresis thresholds

| Field                 | Default | Effect                                                              |
|------------------------|---------|------------------------------------------------------------------------|
| `unhealthy_threshold`  | `3`     | Consecutive failures required before a backend is marked `Alive = false`. |
| `healthy_threshold`    | `2`     | Consecutive successes required before a backend already marked unhealthy is restored to `Alive = true`. |

A fresh backend starts `Alive = true` so it can serve traffic immediately at
startup, before the first health-check cycle completes.

## Configuration Schema

Full YAML schema consumed by `LoadConfig(path)`. All fields are optional
unless noted; sane production defaults are applied for anything omitted.

```yaml
server:
  http_addr: string            # default ":8080" — HTTP listener address; empty disables HTTP
  https_addr: string           # default ":8443" — HTTPS listener address; empty disables HTTPS
  enable_tls: bool             # default false — must be true to activate the HTTPS listener
  shutdown_grace_seconds: int  # default 15 — max seconds to drain connections on SIGTERM/SIGINT
  tls:
    cert_file: string          # PEM certificate path (required if enable_tls: true)
    key_file: string           # PEM private key path (required if enable_tls: true)
    min_version: string        # "1.2" (default) or "1.3"
  timeouts:
    read_header: duration      # default "5s"  (e.g. "5s", "500ms")
    read: duration             # default "15s"
    write: duration            # default "15s"
    idle: duration             # default "60s"

routes:                        # ordered list; longest `match` prefix wins at request time
  - match: string               # required — URL path prefix, e.g. "/api/"
    pool: string                # required — must reference a pools[].name

pools:
  - name: string                 # required, unique — referenced by routes[].pool
    strategy: string             # "round_robin" (default) | "least_connections" | "random"
    backends:
      - url: string               # required — full upstream base URL, e.g. "http://10.0.1.10:9000"
    health_check:
      enabled: bool               # default false — enables the active probe loop
      path: string                 # default "/healthz" — probed relative to each backend's URL
      interval: duration           # default "10s"
      timeout: duration            # default "2s"
      unhealthy_threshold: int     # default 3
      healthy_threshold: int       # default 2
```

Duration fields use Go's `time.ParseDuration` syntax (`"250ms"`, `"5s"`,
`"1m30s"`). Any unparsable or empty duration falls back silently to its
documented default.

### Minimal valid configuration

```yaml
server:
  http_addr: ":8080"

routes:
  - match: "/"
    pool: "default"

pools:
  - name: "default"
    backends:
      - url: "http://127.0.0.1:9000"
```

This yields an HTTP-only proxy on `:8080`, round-robin load balancing across
a single backend, with health checking disabled (the backend is assumed
always alive unless passive failures accumulate).

## Process Signals

| Signal            | Effect                                                                                 |
|--------------------|-------------------------------------------------------------------------------------------|
| `SIGTERM` / `SIGINT` | Initiates graceful shutdown: stops accepting new connections, drains in-flight requests up to `shutdown_grace_seconds`, then exits. |
| `SIGHUP`           | Reloads `config.yaml` from disk (routes, pools, backends, TLS certificate) without restarting the process or dropping active connections. On parse/apply failure, the previous configuration remains active and an error is logged. |
