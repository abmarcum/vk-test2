# GoProxy — HTTP API & Metrics Reference

This document describes the HTTP surface exposed by GoProxy: operational endpoints (`/healthz`, `/metrics`), the proxied data-plane behavior, and the full Prometheus metrics catalog.

---

## Endpoint Precedence

The top-level router (`NewMux`) registers endpoints in the following precedence order, **regardless of user-configured `routes`**:

1. `GET|HEAD /healthz` — liveness/readiness probe.
2. `GET /metrics` — Prometheus metrics.
3. `/*` — catch-all, dispatched to the configured reverse proxy router.

This guarantees operational endpoints remain reachable even if an operator configures a route with `path_prefix: "/"` or `path_prefix: "/healthz"`.

---

## `GET /healthz`

Liveness/readiness probe endpoint. Always available on both HTTP and HTTPS listeners (including when the HTTP listener is otherwise redirecting all traffic to HTTPS).

### Request

- Methods: `GET`, `HEAD`
- No parameters, headers, or body required.

### Responses

| Status | Condition | Body |
|---|---|---|
| `200 OK` | At least one backend across all pools is healthy (or no `HealthChecker` is configured). | `{"status":"ok"}` |
| `503 Service Unavailable` | No pool has any healthy backend. | `{"status":"unhealthy"}` |
| `405 Method Not Allowed` | Method other than `GET`/`HEAD`. | `method not allowed` (plain text), `Allow: GET, HEAD` header set. |

### Response headers

- `Content-Type: application/json; charset=utf-8`
- `Cache-Control: no-store`

### Notes

- This endpoint never returns per-backend detail, internal errors, or pool names — only an aggregate boolean status, by design (avoids leaking topology to unauthenticated callers).
- Intended for use as a Kubernetes readiness/liveness probe or external load-balancer health check target.

### Example

```bash
curl -i http://localhost:8080/healthz
```

```
HTTP/1.1 200 OK
Content-Type: application/json; charset=utf-8
Cache-Control: no-store

{"status":"ok"}
```

---

## `GET /metrics`

Exposes all application metrics in Prometheus text exposition format. Backed by a dedicated `prometheus.Registry` (not the global default registry), so scraping this endpoint returns exactly the metrics documented below plus standard Go/process collectors.

### Request

- Method: `GET`
- No parameters.

### Response

- `Content-Type: text/plain; version=0.0.4` (standard Prometheus exposition format)
- Compression: supported (gzip, if requested via `Accept-Encoding`).

### Example

```bash
curl -s http://localhost:8080/metrics | grep goproxy_
```

---

## Metrics Catalog

All application metrics are namespaced with `goproxy_`. Standard Go runtime (`go_*`) and process (`process_*`) collectors are also registered.

### `goproxy_requests_total`

**Type:** Counter

Total number of proxied HTTP requests, labeled by outcome.

| Label | Description |
|---|---|
| `route` | Matched route path prefix, or `unmatched` if no route matched the request path. |
| `pool` | Matched pool name, or `none` if no pool was resolved. |
| `method` | HTTP method (`GET`, `POST`, etc.). |
| `status` | HTTP response status code sent to the client. |

---

### `goproxy_request_duration_seconds`

**Type:** Histogram

End-to-end latency of proxied requests, from entry into `ProxyServer.ServeHTTP` to completion of the response write.

| Label | Description |
|---|---|
| `route` | Matched route path prefix, or `unmatched`. |
| `pool` | Matched pool name, or `none`. |
| `method` | HTTP method. |

**Buckets (seconds):** `0.001, 0.002, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5`

---

### `goproxy_in_flight_requests`

**Type:** Gauge

Number of requests currently in flight to a given pool. Incremented when a backend is selected by the Director, decremented in `ModifyResponse` (success path) or `ErrorHandler` (failure path).

| Label | Description |
|---|---|
| `pool` | Pool name. |

---

### `goproxy_upstream_errors_total`

**Type:** Counter

Total count of upstream/proxy errors, categorized by reason.

| Label | Description |
|---|---|
| `pool` | Pool name, or `none` if no pool was resolved. |
| `reason` | One of: `no_healthy_backend`, `no_route`, `timeout`, `client_closed`, `bad_gateway`. |

**Reason → client status mapping:**

| `reason` | HTTP status returned to client |
|---|---|
| `no_healthy_backend` | `503 Service Unavailable` |
| `no_route` | `404 Not Found` |
| `timeout` | `504 Gateway Timeout` |
| `client_closed` | `499` (nginx convention; client disconnected before response) |
| `bad_gateway` | `502 Bad Gateway` (default/fallback) |

---

### `goproxy_backend_up`

**Type:** Gauge

Current health state of an individual backend, as determined by active and/or passive health checking.

| Label | Description |
|---|---|
| `pool` | Pool name. |
| `backend` | Backend URL string. |

**Values:** `1` = healthy, `0` = unhealthy.

---

### `goproxy_response_size_bytes`

**Type:** Histogram

Size, in bytes, of the response body returned to the client for proxied requests.

| Label | Description |
|---|---|
| `route` | Matched route path prefix, or `unmatched`. |
| `pool` | Matched pool name, or `none`. |

**Buckets:** Exponential, starting at 128 bytes, factor 4, 8 buckets (`128, 512, 2048, 8192, 32768, 131072, 524288, 2097152`).

---

## Data-Plane (Proxy) Behavior

All requests not matched by `/healthz` or `/metrics` are handled by `ProxyServer`, which performs:

1. **Routing** — longest-prefix match of `r.URL.Path` against configured `routes[].path_prefix`. No match → `404 Not Found`.
2. **Backend selection** — the matched pool's configured `Strategy` (`round_robin` / `least_connections` / `random`) selects a healthy backend. No healthy backend → `503 Service Unavailable`.
3. **Header processing**:
   - Hop-by-hop headers (RFC 7230 §6.1) are stripped from both the inbound request and the upstream response.
   - `X-Forwarded-Proto`, `X-Forwarded-Host`, and `X-Forwarded-For` are set/appended on the outbound request.
   - `X-Forwarded-For-Pool` is set to the matched pool name.
   - Response headers `Server` and `X-Powered-By` are stripped before returning to the client.
   - `X-Content-Type-Options: nosniff` is added to all proxied responses.
4. **Error handling** — all upstream/backend failures are translated to one of the status codes in the [`goproxy_upstream_errors_total`](#goproxy_upstream_errors_total) table above. Response bodies are always generic JSON:

   ```json
   {"error":"service unavailable"}
   ```

   No backend hostnames, stack traces, or internal error strings are ever included in client-facing responses.

5. **Streaming** — the proxy supports streaming responses (e.g. Server-Sent Events) via periodic flush (`FlushInterval: 100ms`) and `http.Flusher` passthrough.

### Example error response

```bash
curl -i http://localhost:8080/api/unknown-path
```

```
HTTP/1.1 404 Not Found
Content-Type: application/json; charset=utf-8

{"error":"not found"}
```

```bash
# All backends in the matched pool are down
curl -i http://localhost:8080/api/orders
```

```
HTTP/1.1 503 Service Unavailable
Content-Type: application/json; charset=utf-8

{"error":"service unavailable"}
```

---

## HTTPS Redirect Behavior

When TLS is enabled and both listeners are active, plaintext HTTP requests (other than `/healthz`) are expected to be redirected to HTTPS by `HTTPSRedirectHandler`:

- Method: any
- Response: `301 Moved Permanently`
- `Location`: `https://<host>[:<https_port>]<path>?<query>` (port omitted if `https_port == 443`)
- `Connection: close` is set to prevent connection reuse across the scheme switch.

### Example

```bash
curl -i http://example.com/api/orders?id=42
```

```
HTTP/1.1 301 Moved Permanently
Location: https://example.com/api/orders?id=42
Connection: close
```

---

## Logging Reference

Every proxied request produces a single structured log line (via `log/slog`) under the `proxy_request` message with an `http` attribute group containing:

| Field | Description |
|---|---|
| `method` | HTTP method. |
| `path` | Request path (query string always stripped). |
| `route` | Matched route prefix, or `unmatched`. |
| `pool` | Matched pool name, or `none`. |
| `backend` | Selected backend URL, if any. |
| `status` | Final HTTP status returned to the client. |
| `duration_ms` | Request duration in milliseconds (float). |
| `bytes_out` | Bytes written to the client. |
| `remote_addr` | Best-effort client IP (from `X-Forwarded-For` first hop, or `RemoteAddr`). |
| `user_agent` | Client `User-Agent` header. |
| `error` | Present only when an error occurred; sanitized error string. |

Log level is `INFO` for `2xx`/`3xx`, `WARN` for `4xx`, and `ERROR` for `5xx` responses.

Panics recovered by `LoggingRecoverMiddleware` are logged as `panic_recovered` with `path` and `method` only — no stack trace or panic value is included in logs or responses.
