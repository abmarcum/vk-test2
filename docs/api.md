# goproxy Reference

This document is the authoritative reference for:

1. The YAML configuration schema.
2. The HTTP operational endpoints (`/healthz`, `/metrics`).
3. The Prometheus metrics catalog.
4. Internal HTTP behaviors relevant to operators and integrators
   (headers, error responses, redirect semantics).

---

## 1. Configuration Schema

The server is configured via a single YAML file, loaded once at process
startup via `LoadConfig`. Unknown top-level or nested keys cause a load-time
error (strict schema). All duration fields are Go duration strings (e.g.
`"5s"`, `"250ms"`, `"2m"`).

### Top level

```yaml
server: Server      # required
routes: [Route]      # required, at least one
pools:  [Pool]        # required, at least one
```

### `server`

| Field                    | Type    | Default | Description |
|--------------------------|---------|---------|--------------|
| `http_addr`              | string  | `":8080"` | Listen address for plaintext HTTP. |
| `https_addr`             | string  | `":8443"` | Listen address for HTTPS. |
| `enable_tls`             | bool    | `false` | Enables the HTTPS listener and cert resolution. |
| `tls`                    | TLS     | see below | TLS configuration block. |
| `timeouts`               | Timeouts | see below | Server + proxy timing tunables. |
| `shutdown_grace_seconds` | int     | `30` | Seconds allowed for graceful shutdown to drain in-flight requests. |

**Validation**: at least one of `http_addr`/`https_addr` must be non-empty.

### `server.tls`

| Field              | Type   | Default  | Description |
|--------------------|--------|----------|--------------|
| `cert_source`      | string | `"file"` | `"file"` or `"secretmanager"`. |
| `cert_file`        | string | —        | Required when `cert_source: file`. Path to PEM certificate. |
| `key_file`         | string | —        | Required when `cert_source: file`. Path to PEM private key. |
| `cert_secret_name` | string | —        | Required when `cert_source: secretmanager`. Full resource name, e.g. `projects/p/secrets/cert/versions/latest`. |
| `key_secret_name`  | string | —        | Required when `cert_source: secretmanager`. Same format as above. |
| `min_version`      | string | `"1.2"`  | Minimum TLS version. Must be `"1.2"` or `"1.3"`. |

**Validation** (only enforced when `enable_tls: true`):
- `cert_source` must be `file` or `secretmanager`.
- The corresponding cert/key fields for the chosen source must be set.
- `min_version` must be `"1.2"` or `"1.3"`.
- Resolved certificate/key material must be non-empty after loading
  (from disk or Secret Manager).

Certificate material is resolved **once**, at config load time, and cached
in memory (`CertPEM`/`KeyPEM`); these fields are never serialized back out.

### `server.timeouts`

All values are duration strings.

| Field          | Default | Applies to |
|----------------|---------|------------|
| `read_header`  | `5s`   | `http.Server.ReadHeaderTimeout` |
| `read`         | `15s`  | `http.Server.ReadTimeout` |
| `write`        | `15s`  | `http.Server.WriteTimeout` |
| `idle`         | `60s`  | `http.Server.IdleTimeout` |
| `dial`         | `5s`   | Outbound dial timeout to backends |
| `proxy_total`  | `30s`  | Overall per-request proxy budget |

### `routes` (list of `Route`)

| Field         | Type   | Required | Description |
|---------------|--------|----------|--------------|
| `path_prefix` | string | yes      | Must start with `/`. Matched using longest-prefix-match. |
| `pool`        | string | yes      | Name of a pool defined in `pools`. Must reference an existing pool. |

Routing precedence: the **longest matching `path_prefix` wins**, regardless
of the order routes are declared in the file. `/healthz` and `/metrics` are
always handled by the server itself and take precedence over any configured
route, even `path_prefix: "/"`.

### `pools` (list of `Pool`)

| Field          | Type          | Default        | Description |
|----------------|---------------|-----------------|--------------|
| `name`         | string        | —               | Required, unique across all pools. |
| `strategy`     | string        | `"round_robin"` | One of `round_robin`, `least_connections`, `random`. |
| `health_check` | HealthCheck   | see below       | Active health check configuration. |
| `backends`     | list[Backend] | —               | Required, at least one entry. |

### `pools[].health_check`

| Field                 | Type   | Default      | Description |
|-----------------------|--------|--------------|--------------|
| `enabled`             | bool   | `false`      | Enables active probing for this pool. |
| `path`                | string | `"/healthz"` | Must start with `/`. Probed relative to each backend's base URL. |
| `interval`            | string | `"10s"`      | Time between probe rounds. |
| `timeout`             | string | `"2s"`       | Per-probe timeout. |
| `healthy_threshold`   | int    | `2`          | Consecutive successes required to mark a backend healthy. |
| `unhealthy_threshold` | int    | `3`          | Consecutive failures required to mark a backend unhealthy. |

A backend is considered healthy on probe response status `200–399` unless a
specific expected status is configured internally. Thresholds also govern
**passive** health adjustments derived from live proxied traffic outcomes
(not just active probes).

### `pools[].backends[]` (`Backend`)

| Field    | Type   | Default | Description |
|----------|--------|---------|--------------|
| `url`    | string | —       | Required. Absolute URL, scheme must be `http` or `https`, non-empty host. |
| `weight` | int    | `1`     | Currently informational; must be `>= 0`. |

**Validation**: `url` is parsed and rejected if malformed, missing a scheme,
using an unsupported scheme, or missing a resolvable host — including edge
cases like `http://:8080` or `http://user@/path` where the URL parses but no
real hostname is present.

### Full example

```yaml
server:
  http_addr: ":8080"
  https_addr: ":8443"
  enable_tls: true
  tls:
    cert_source: "file"
    cert_file: "/etc/goproxy/tls.crt"
    key_file: "/etc/goproxy/tls.key"
    min_version: "1.3"
  timeouts:
    read_header: "5s"
    read: "15s"
    write: "15s"
    idle: "60s"
    dial: "5s"
    proxy_total: "30s"
  shutdown_grace_seconds: 30

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
    strategy: "round_robin"
    health_check:
      enabled: true
    backends:
      - url: "http://10.0.2.10:80"
      - url: "http://10.0.2.11:80"
```

---

## 2. HTTP Endpoints

### `GET|HEAD /healthz`

Liveness/readiness probe. Always registered, always takes precedence over
any configured route, and remains reachable over plaintext HTTP even when
the HTTP listener is otherwise redirecting to HTTPS.

**Responses**

| Status | Body                          | Meaning |
|--------|-------------------------------|---------|
| `200`  | `{"status":"ok"}`             | At least one backend across configured pools is healthy (or no health checker is wired). |
| `503`  | `{"status":"unhealthy"}`      | No pool currently has a healthy backend. |
| `405`  | `method not allowed` (plain text) | Method other than GET/HEAD; response includes `Allow: GET, HEAD`. |

Headers: `Content-Type: application/json; charset=utf-8`,
`Cache-Control: no-store`.

This endpoint **never** includes backend addresses, pool names, or error
detail — only a boolean-derived overall status, by design.

### `GET /metrics`

Prometheus exposition-format metrics, served from a dedicated registry (see
§3 below for the full catalog). Standard `promhttp` content negotiation
applies (supports gzip when requested).

### `/` (catch-all — reverse proxy)

Every other request is matched against the configured `routes` using
longest-prefix-match and forwarded to a backend selected from the matched
pool's load-balancing strategy. See §4 for detailed proxy behavior.

### HTTP → HTTPS Redirect

When TLS is enabled, the plaintext HTTP listener should be mounted with the
HTTPS-redirect handler (excluding `/healthz`, which is registered directly).
For any other path:

- Responds `301 Moved Permanently`.
- `Location` preserves the original path and query string.
- Target host is the request's host, on the configured HTTPS port (omitted
  from the URL entirely if that port is `443`).
- Sets `Connection: close` on the redirect response.

Example:

```
GET http://example.com/api/orders?x=1
→ 301 Location: https://example.com/api/orders?x=1   (if https_addr resolves to :443)
→ 301 Location: https://example.com:8443/api/orders?x=1  (otherwise)
```

---

## 3. Metrics Catalog

All metrics are under the `goproxy_` namespace and registered on a
dedicated Prometheus registry (not the global default registry).

| Metric | Type | Labels | Description |
|--------|------|--------|--------------|
| `goproxy_requests_total` | Counter | `route`, `pool`, `method`, `status` | Total proxied requests, by outcome. `route`/`pool` are `"unmatched"`/`"none"` when no route/pool was resolved. |
| `goproxy_request_duration_seconds` | Histogram | `route`, `pool`, `method` | End-to-end request latency as observed by the proxy. Buckets: `.001`…`5` seconds. |
| `goproxy_in_flight_requests` | Gauge | `pool` | Number of requests currently being proxied to a given pool. |
| `goproxy_upstream_errors_total` | Counter | `pool`, `reason` | Count of proxy-side error outcomes. `reason` ∈ `no_healthy_backend`, `no_route`, `timeout`, `client_closed`, `bad_gateway`. |
| `goproxy_backend_up` | Gauge | `pool`, `backend` | `1` if backend is currently considered healthy, `0` otherwise. Updated by both active and passive health checks. |
| `goproxy_response_size_bytes` | Histogram | `route`, `pool` | Response body size written to the client. Exponential buckets starting at 128 bytes (×4, 8 buckets). |

Additionally, standard Go runtime and process collectors are registered
(`go_*`, `process_*` metric families) for baseline runtime observability.

### Example queries

Error rate by pool over 5 minutes:

```promql
sum(rate(goproxy_upstream_errors_total[5m])) by (pool, reason)
```

p99 latency for a specific route:

```promql
histogram_quantile(0.99,
  sum(rate(goproxy_request_duration_seconds_bucket{route="/api/"}[5m])) by (le)
)
```

Backend health at a glance:

```promql
goproxy_backend_up == 0
```

---

## 4. Proxy Behavior Reference

### Routing

- Matching is **prefix-based, longest-match-wins**, computed once at startup
  by sorting configured routes by descending `path_prefix` length.
- `/api` will match `/api/v1/x` (no trailing-slash boundary requirement),
  consistent with common ingress/reverse-proxy conventions.
- If no route matches the request path, the proxy returns `404`.
- If a route matches but its pool has no healthy backend, the proxy returns
  `503`.

### Load Balancing Strategies

| Strategy | Behavior |
|----------|----------|
| `round_robin` | Cycles through healthy backends in order using an atomic counter. Default if unset or unrecognized. |
| `least_connections` | Selects the healthy backend with the fewest current in-flight requests; ties broken by first-seen order. |
| `random` | Selects a uniformly random healthy backend. |

Backend health is re-evaluated on every selection call — unhealthy backends
are excluded from candidate sets entirely, not merely deprioritized.

### Health Checking

Two complementary mechanisms feed the same per-backend health state
(`goproxy_backend_up`):

1. **Active probing** (`RunHealthChecks`): for each pool with
   `health_check.enabled: true`, a dedicated goroutine probes every backend
   concurrently on `interval`, issuing a `GET` to `<backend base>/<health
   check path>` with a hardened client (`healthCheckClient`) that:
   - Enforces `timeout` per probe.
   - Does **not** follow redirects (`http.ErrUseLastResponse`), preventing
     redirect-based probe abuse.
   - Enforces minimum TLS 1.2 for HTTPS backend probes.
   - Treats HTTP status `200–399` as healthy by default.
   - Requires `healthy_threshold` consecutive successes to transition
     unhealthy → healthy, and `unhealthy_threshold` consecutive failures to
     transition healthy → unhealthy (hysteresis prevents flapping).

2. **Passive observation**: live proxied request outcomes also feed
   `MarkSuccess`/`MarkFailure` on a `Backend`, using the same
   healthy/unhealthy threshold semantics, so a backend that starts failing
   real traffic is demoted even between active probe intervals.

A freshly constructed `Backend` starts **alive** so it can serve traffic
immediately at startup, before the first probe cycle completes.

### Header Handling

**Inbound → upstream:**

- All RFC 7230 hop-by-hop headers are stripped (`Connection`,
  `Proxy-Connection`, `Keep-Alive`, `Proxy-Authenticate`,
  `Proxy-Authorization`, `Te`, `Trailer`, `Transfer-Encoding`, `Upgrade`).
- `X-Forwarded-Proto` is set from the actual connection state (`https` if
  `r.TLS != nil`), falling back to an existing valid value only if already
  `http`/`https`.
- `X-Forwarded-Host` is set to the inbound `Host` header.
- `X-Forwarded-For` is **appended to** (not replaced) with the immediate
  client IP, parsed from `RemoteAddr` and validated as a real IP literal —
  unparseable or spoofed values are dropped rather than forwarded, to
  prevent header-injection style abuse.
- `X-Forwarded-For-Pool` is set to the resolved pool name (internal
  diagnostic header).

**Upstream → client:**

- Hop-by-hop headers are stripped again on the response path.
- `Server` and `X-Powered-By` are removed to avoid leaking backend software
  identity.
- `X-Content-Type-Options: nosniff` is added to every proxied response.

### Error Handling & Status Code Mapping

The proxy **never** returns raw upstream error text, stack traces, or
backend host/port information to clients. All error responses share the
shape:

```json
{"error": "<opaque message>"}
```

with `Content-Type: application/json; charset=utf-8`.

| Condition | Status | Message | Metric `reason` |
|-----------|--------|---------|-------------------|
| No route matches the path | `404` | `not found` | `no_route` |
| Route matched, but pool has no healthy backend | `503` | `service unavailable` | `no_healthy_backend` |
| Client disconnected before response completed | `499` | `client closed request` | `client_closed` |
| Upstream round trip exceeded context deadline | `504` | `gateway timeout` | `timeout` |
| Any other upstream/transport failure | `502` | `bad gateway` | `bad_gateway` |

An in-process panic anywhere in the handler chain is recovered by
`LoggingRecoverMiddleware`, logged (path/method only, no stack trace in the
response), and converted to:

```
500 {"error":"internal server error"}
```

### Structured Logging

Each completed proxy request emits one log record (`proxy_request`) with an
`http` attribute group containing:

`method`, `path` (query string stripped), `route`, `pool`, `backend`,
`status`, `duration_ms`, `bytes_out`, `remote_addr`, `user_agent`, and
`error` (only if one was recorded on the request's internal proxy state).

Log level is `INFO` for `<400`, `WARN` for `400–499`, `ERROR` for `≥500`.

### Connection Pooling & Timeouts (Upstream Transport)

The reverse proxy's `http.RoundTripper` is configured with:

- `Proxy: nil` — upstream calls **never** honor `HTTP_PROXY`/`HTTPS_PROXY`
  environment variables, preventing environment-based traffic redirection.
- Dial timeout `5s`, keep-alive `30s`.
- `MaxIdleConns: 200`, `MaxIdleConnsPerHost: 50`, `IdleConnTimeout: 90s`.
- `TLSHandshakeTimeout: 5s`, `ExpectContinueTimeout: 1s`,
  `ResponseHeaderTimeout: 15s`.
- HTTP/2 attempted by default (`ForceAttemptHTTP2: true`).
- `FlushInterval: 100ms` on the `ReverseProxy` to support streaming
  responses (e.g. Server-Sent Events) without unbounded buffering.
