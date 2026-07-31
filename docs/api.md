# GoProxy — API & Endpoint Reference

This document describes every HTTP-level surface exposed directly by the GoProxy process itself: the built-in operational endpoints (`/healthz`, `/metrics`) and the semantics of the reverse-proxy data plane (`/*`). It does **not** document the API surface of upstream backend services — those are opaque to GoProxy and are simply forwarded.

Both the HTTP listener (`server.http_addr`) and, when TLS is enabled, the HTTPS listener (`server.https_addr`) serve an identical routing table (the same `http.Handler` / `Mux` instance is shared by both `http.Server`s).

---

## Table of Contents

- [Base URLs](#base-urls)
- [Authentication](#authentication)
- [`GET /healthz`](#get-healthz)
- [`GET /metrics`](#get-metrics)
- [Reverse Proxy Data Plane (`/*`)](#reverse-proxy-data-plane-)
- [Routing Semantics](#routing-semantics)
- [Load Balancing Strategies](#load-balancing-strategies)
- [Health Checking](#health-checking)
- [Error Responses](#error-responses)
- [Forwarded Request Headers](#forwarded-request-headers)

---

## Base URLs

| Listener | Default Address | Enabled When |
|---|---|---|
| HTTP  | `http://<host>:8080`  | Always |
| HTTPS | `https://<host>:8443` | `server.enable_tls: true` |

Addresses are configurable via `server.http_addr` / `server.https_addr` in `config.yaml`.

## Authentication

GoProxy performs no authentication or authorization of its own on `/healthz`, `/metrics`, or the proxy data plane. Access control (if required) must be enforced at the network layer (firewall/security group), via an upstream identity-aware proxy, or by the backend services themselves.

---

## `GET /healthz`

Liveness/readiness probe endpoint intended for load balancers, container orchestrators (Kubernetes `readinessProbe`/`livenessProbe`), and uptime monitors.

**Request**

```
GET /healthz HTTP/1.1
Host: proxy.example.com
```

**Behavior**

Returns `200 OK` if **at least one backend, in any configured pool, is currently marked `Alive`**, or if **no pools are configured at all** (vacuously healthy). Returns `503 Service Unavailable` otherwise. This check exposes only a boolean signal — it never leaks pool or backend identity/topology.

**Responses**

| Status | Content-Type | Body | Condition |
|---|---|---|---|
| `200 OK` | `text/plain` | `ok` | ≥1 backend alive, or zero pools configured |
| `503 Service Unavailable` | `text/plain` | `unavailable` | All configured backends across all pools are unhealthy |

**Example**

```bash
curl -i http://localhost:8080/healthz
```

```
HTTP/1.1 200 OK
Content-Type: text/plain

ok
```

---

## `GET /metrics`

Plain-text metrics exposition endpoint for scraping by monitoring systems. Metrics are held in an in-process registry (`Metrics`) and reset on process restart.

**Request**

```
GET /metrics HTTP/1.1
Host: proxy.example.com
```

**Response**

`200 OK`, `Content-Type: text/plain`, body is the current snapshot of all counters and gauges (one metric per line; format produced by `Metrics.WriteTo`).

**Metrics emitted by the proxy data plane**

| Metric | Type | Description |
|---|---|---|
| `proxy_requests_total` | counter | Incremented once per proxied request that completes (successfully or with an upstream error already handled by `ErrorHandler`). |
| `proxy_requests_failed_total` | counter | Incremented when a request cannot be routed (`404`), no healthy backend is available (`503`), or the upstream reverse proxy reports an error (`502`). |
| `proxy_last_request_duration_seconds` | gauge | Wall-clock duration of the most recently completed proxied request, in seconds. |

**Example**

```bash
curl http://localhost:8080/metrics
```

```
proxy_requests_total 128
proxy_requests_failed_total 3
proxy_last_request_duration_seconds 0.014213
```

> Note: the exact line format is produced by the internal `Metrics.WriteTo` writer and is intended for human/simple scraper consumption; it is not guaranteed to be strict Prometheus exposition format unless the metric naming/line format already matches it.

---

## Reverse Proxy Data Plane (`/*`)

Any request path not matched by `/healthz` or `/metrics` is handled by `ProxyServer`, which:

1. Resolves the request path to a `Pool` via the `Router` (longest matching route prefix — see [Routing Semantics](#routing-semantics)).
2. Selects a healthy `Backend` from that pool via the pool's configured `Strategy` (see [Load Balancing Strategies](#load-balancing-strategies)).
3. Forwards the request to the backend using `net/http/httputil.ReverseProxy`, rewriting `Host` to the backend's host and adding standard forwarding headers (see [Forwarded Request Headers](#forwarded-request-headers)).
4. Streams the backend's response back to the client unmodified (status code, headers, and body).
5. Records passive health state (`MarkSuccess` / `MarkFailure`) and updates metrics based on the outcome.

**Request**

```
<ANY METHOD> /<any-path> HTTP/1.1
Host: proxy.example.com
```

All HTTP methods, headers, query strings, and bodies are transparently proxied; GoProxy imposes no additional constraints beyond the configured server timeouts (`server.timeouts.*`).

**Response**

The backend's raw response is returned as-is, except in the failure conditions described in [Error Responses](#error-responses).

---

## Routing Semantics

- Routes are configured as `{match: "<prefix>", pool: "<pool-name>"}` under the top-level `routes:` list.
- Matching is **longest-prefix-match**: among all routes whose `match` value is a prefix of the request path, the one with the longest prefix string wins.
- If no configured route prefix matches the request path, the proxy returns `404 Not Found` (see [Error Responses](#error-responses)).
- Each `pool` referenced by a route **must** exist in the `pools:` list; unknown pool references cause the server to fail fast at startup (`build router: route %q references unknown pool %q`).

Example: given routes `["/api/" -> api_pool, "/" -> web_pool]`, a request to `/api/users/1` matches `/api/` (longer prefix) and is routed to `api_pool`; a request to `/index.html` matches only `/` and is routed to `web_pool`.

---

## Load Balancing Strategies

Configured per-pool via `pools[].strategy`:

| Strategy | Config value | Selection Algorithm |
|---|---|---|
| Round Robin *(default)* | `round_robin` | Cycles through currently-alive backends in order using an atomic monotonically increasing cursor. |
| Least Connections | `least_connections` | Selects the alive backend with the fewest in-flight (`ActiveConns`) requests; ties resolved by first-seen order. |
| Random | `random` | Selects a uniformly pseudo-random alive backend (time-seeded, non-cryptographic — suitable only for load distribution, not security). |

Only backends currently marked `Alive` participate in selection. If a pool has zero alive backends, `Pool.Choose()` returns `ErrNoHealthyBackends`, and the proxy responds `503 Service Unavailable`.

---

## Health Checking

GoProxy combines **active** and **passive** health checking per pool:

### Active Health Checks

When `pools[].health_check.enabled: true`, a background goroutine (`Pool.RunHealthChecks`) issues a `GET` request to `health_check.path` (default `/healthz`) against every backend in the pool on a fixed `interval` (default `10s`), using a client with `timeout` (default `2s`) that does not follow redirects.

- A response with status `200–399` counts as a **success**.
- Any other status, or a transport-level error, counts as a **failure**.

### Passive Health Checks

Every live proxied request outcome also feeds the same hysteresis counters:

- `ProxyServer` calls `Pool.MarkSuccess(backend)` after a proxied request completes without a reverse-proxy transport error.
- `ProxyServer` calls `Pool.MarkFailure(backend)` when `httputil.ReverseProxy`'s `ErrorHandler` fires (upstream unreachable, timeout, etc.), and responds `502 Bad Gateway` to the client.

### Hysteresis Thresholds

| Config field | Default | Effect |
|---|---|---|
| `unhealthy_threshold` | `3` | Consecutive failures (active or passive) required before a backend transitions `Alive → false`. |
| `healthy_threshold` | `2` | Consecutive successes required before a backend transitions `Alive → true`. |

Backends start `Alive = true` at process startup (optimistic) so they can serve traffic immediately, and are only marked down after crossing `unhealthy_threshold`.

---

## Error Responses

| Status | Trigger | Body |
|---|---|---|
| `404 Not Found` | No configured route matches the request path. | `not found` |
| `503 Service Unavailable` | Route matched, but the target pool has no currently healthy (`Alive`) backend. | `no healthy backends available` |
| `502 Bad Gateway` | Route and backend resolved, but the upstream request failed (connection error, timeout, etc.). | `bad gateway` |

All error bodies are `text/plain`. Each error path also increments `proxy_requests_failed_total` and, in the `502` case, marks the selected backend's passive failure counter via `Pool.MarkFailure`.

---

## Forwarded Request Headers

For every proxied request, GoProxy's `ProxyServer.Director` sets/overrides the following headers before forwarding to the backend:

| Header | Value |
|---|---|
| `Host` | Rewritten to the backend's own host (`backend.URL.Host`) |
| `X-Forwarded-For` | The client's IP address, extracted from `r.RemoteAddr` |
| `X-Forwarded-Proto` | `https` if the inbound connection was TLS-terminated at GoProxy, otherwise `http` |
| `X-Forwarded-Host` | The original `Host` header supplied by the client |

Backends should rely on these headers (rather than the raw connection) to reconstruct the original client identity and requested scheme/host, standard practice for services sitting behind a reverse proxy.
