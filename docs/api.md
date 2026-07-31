# GoProxy — HTTP API Reference

This document describes every HTTP surface exposed by the GoProxy
server: the operational control-plane endpoints (`/healthz`,
`/metrics`) served on both the HTTP and HTTPS listeners, and the
data-plane reverse-proxy behavior for all other routes.

All endpoints are served from the same `Mux` (see `proxy.go` /
`NewMux`) on **both** listeners configured in `config.yaml`:

- `server.http_addr` (plain HTTP, e.g. `:8080`)
- `server.https_addr` (TLS, e.g. `:8443`, only if `server.enable_tls: true`)

---

## Table of Contents

- [Conventions](#conventions)
- [GET /healthz](#get-healthz)
- [GET /metrics](#get-metrics)
- [Reverse Proxy Routing (`*`)](#reverse-proxy-routing-)
- [Load Balancing Strategies](#load-balancing-strategies)
- [Health Checking Behavior](#health-checking-behavior)
- [Error Responses](#error-responses)
- [TLS Notes](#tls-notes)

---

## Conventions

- Base URL examples assume default ports: `http://localhost:8080` and
  `https://localhost:8443`.
- All operational endpoints return `Content-Type: text/plain` unless
  otherwise noted.
- Timestamps and durations in configuration are Go duration strings
  (e.g. `"10s"`, `"2m"`).
- Proxied requests/responses are pass-through: request method, headers,
  query string, and body are forwarded unmodified except where noted
  under [Reverse Proxy Routing](#reverse-proxy-routing-).

---

## GET /healthz

Reports overall server liveness, aggregated across all configured
backend pools. Backed by `healthCheckerAdapter.AnyPoolHealthy()` in
`main.go`.

**Request**

```
GET /healthz HTTP/1.1
Host: localhost:8080
```

**Behavior**

- Returns `200 OK` if:
  - at least one backend across all pools is currently marked `Alive`, **or**
  - zero pools are configured (vacuously healthy).
- Returns `503 Service Unavailable` if one or more pools exist and
  **none** of their backends are currently alive.
- This endpoint **never** discloses pool names, backend URLs, or
  per-backend state — it is a pure boolean liveness signal suitable for
  load balancer / orchestrator health probes (e.g. Kubernetes
  `readinessProbe` / `livenessProbe`).

**Response — Healthy**

```
HTTP/1.1 200 OK
Content-Type: text/plain

ok
```

**Response — Unhealthy**

```
HTTP/1.1 503 Service Unavailable
Content-Type: text/plain

unavailable
```

**Kubernetes probe example**

```yaml
readinessProbe:
  httpGet:
    path: /healthz
    port: 8080
  periodSeconds: 5
```

---

## GET /metrics

Exposes basic operational counters collected by the `Metrics` type
(`NewMetrics()` in `proxy.go`). Intended for scraping by a metrics
collector (e.g. Prometheus) or manual inspection.

**Request**

```
GET /metrics HTTP/1.1
Host: localhost:8080
```

**Response**

```
HTTP/1.1 200 OK
Content-Type: text/plain

# Example metric fields (exact field set depends on Metrics implementation)
proxy_requests_total 1024
proxy_requests_failed_total 3
proxy_active_connections 12
backend_alive_count 4
backend_dead_count 0
```

> The precise metric names/format are defined by the `Metrics` type in
> `proxy.go`. If integrating with Prometheus, wrap or extend this
> endpoint to emit the standard Prometheus exposition format
> (`# HELP` / `# TYPE` comments + `name{labels} value`).

---

## Reverse Proxy Routing (`*`)

Any request path not matched by `/healthz` or `/metrics` is handled by
the proxy data plane:

1. **Route resolution** — the `Router` (built via
   `NewRouter(cfg.Routes, pools)`) matches the incoming request
   (typically by path prefix, and optionally host) against the
   `routes:` list in `config.yaml`, in order, selecting the first
   match. If no route matches, the proxy returns `404 Not Found`.

2. **Pool selection** — the matched route identifies a named `Pool`.

3. **Backend selection** — the pool's configured `Strategy`
   (`round_robin`, `least_connections`, or `random`) selects a single
   healthy `Backend` from the pool via `Pool.Choose()`.

4. **Proxying** — the request is forwarded upstream to the selected
   backend's URL, preserving method, headers, query string, and body.
   Standard reverse-proxy headers are set/forwarded (e.g.
   `X-Forwarded-For`, `X-Forwarded-Proto`, `X-Forwarded-Host`) so
   backends can recover original client information.

5. **Passive health accounting** — after the upstream responds (or
   fails to respond):
   - A successful response invokes `Pool.MarkSuccess(backend)`,
     resetting the consecutive-failure counter and, once the
     `healthy_threshold` is reached, marking the backend `Alive`.
   - A failed/erroring response invokes `Pool.MarkFailure(backend)`,
     incrementing the consecutive-failure counter and, once the
     `unhealthy_threshold` is reached, marking the backend dead
     (excluded from selection until it recovers).

**Example — proxied request**

```
GET /api/users/42 HTTP/1.1
Host: localhost:8080
```

Given the example config's route `match: "/api/"` → `pool: api-pool`,
this request is forwarded to one of `api-pool`'s backends, e.g.:

```
GET /api/users/42 HTTP/1.1
Host: 10.0.1.10:9000
X-Forwarded-For: 203.0.113.7
X-Forwarded-Proto: http
```

The backend's response (status, headers, body) is streamed back to the
client unmodified.

---

## Load Balancing Strategies

Configured per-pool via `pools[].strategy`:

| Strategy value       | Behavior                                                                 |
|-----------------------|---------------------------------------------------------------------------|
| `round_robin` (default) | Cycles through currently-alive backends in order using an atomic counter. |
| `least_connections`   | Selects the alive backend with the fewest in-flight (`ActiveConns`) requests. |
| `random`              | Selects a uniformly random alive backend (time-seeded, non-cryptographic; not used for security purposes). |

If **no backend in the pool is currently alive**, backend selection
fails with `ErrNoHealthyBackends`, and the proxy returns
`502 Bad Gateway` / `503 Service Unavailable` for that request (see
[Error Responses](#error-responses)).

---

## Health Checking Behavior

Each pool may enable **active health checks** via
`pools[].health_check`:

```yaml
health_check:
  enabled: true
  path: "/healthz"
  interval: "10s"
  timeout: "2s"
  unhealthy_threshold: 3
  healthy_threshold: 2
```

- Every `interval`, a goroutine issues a `GET <backend_url><path>` to
  each backend in the pool, using a hardened client that:
  - does **not** follow redirects (`http.ErrUseLastResponse`),
  - enforces the configured request `timeout`.
- A response with status `200–399` counts as a success
  (`Pool.MarkSuccess`); anything else (including request errors,
  timeouts, and connection failures) counts as a failure
  (`Pool.MarkFailure`).
- **Hysteresis**: a backend only flips from alive→dead after
  `unhealthy_threshold` consecutive failures, and only flips from
  dead→alive after `healthy_threshold` consecutive successes. This
  prevents flapping on transient errors.
- Health-check goroutines are started per pool in `main.go`
  (`pool.RunHealthChecks`) and are canceled cleanly on shutdown
  (`SIGINT`/`SIGTERM`) via context cancellation.
- Backends also start `Alive = true` at boot so they can serve traffic
  immediately, before the first health-check cycle completes.

---

## Error Responses

| Condition                                              | Status Code | Body (indicative)         |
|----------------------------------------------------------|:------------:|-----------------------------|
| No route matches the request path                        | `404 Not Found` | `not found`               |
| Route matched, but pool has no healthy backends (`ErrNoHealthyBackends`) | `503 Service Unavailable` | `no healthy backends available` |
| Upstream backend connection error / timeout during proxying | `502 Bad Gateway` | `bad gateway`             |
| `/healthz` — no pool has any alive backend                | `503 Service Unavailable` | `unavailable`             |

Exact body text may be adapted by the `Mux`/`ProxyServer`
implementation in `proxy.go`; status codes are the stable contract.

---

## TLS Notes

- When `server.enable_tls: true`, the HTTPS listener uses a
  certificate/key pair loaded from `server.tls.cert_file` /
  `server.tls.key_file` (`tls.X509KeyPair`).
- Minimum TLS version is controlled by `server.tls.min_version`:
  - `"1.3"` → enforces TLS 1.3 minimum.
  - anything else (including unset) → defaults to **TLS 1.2** minimum.
- The HTTP and HTTPS listeners share the **same** `Mux`/route table —
  behavior of `/healthz`, `/metrics`, and proxied routes is identical
  regardless of which listener receives the request; only the
  transport (plaintext vs TLS) differs.
- Health-check probes issued *by* GoProxy against HTTPS backends use a
  standard `http.Client` with a per-check `timeout` and redirect
  suppression; no client certificate is presented unless added to a
  future health-check client configuration.
