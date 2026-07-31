# GoProxy API Documentation

GoProxy exposes three categories of HTTP surface on **both** the HTTP and
HTTPS listeners (they share the same `http.Handler`):

1. Operational endpoints: `/healthz`, `/metrics`.
2. The reverse-proxy data plane: every other path, matched against
   configured routes and forwarded to an upstream backend.

All responses are plain text unless otherwise noted. There is no
authentication layer; place GoProxy behind a trusted network boundary or
add authentication at the upstream services if required.

---

## `GET /healthz`

Liveness probe suitable for load balancer / orchestrator health checks
(e.g. Kubernetes `readinessProbe`/`livenessProbe`).

**Behavior**

- Returns `200 OK` with body `ok` if **at least one** backend, across
  **any** configured pool, currently has `Alive == true`.
- Returns `200 OK` with body `ok` if **no pools are configured** at all
  (vacuously healthy).
- Returns `503 Service Unavailable` with body `unavailable` if every
  configured backend is currently marked unhealthy.

**Request**

```
GET /healthz HTTP/1.1
```

**Response — healthy**

```
HTTP/1.1 200 OK
Content-Type: text/plain

ok
```

**Response — unhealthy**

```
HTTP/1.1 503 Service Unavailable
Content-Type: text/plain

unavailable
```

> This endpoint reports an **aggregate, anonymized** signal only — it
> never reveals individual pool or backend identity/state.

---

## `GET /metrics`

Plain-text metrics snapshot (not Prometheus-exposition-format guaranteed,
but designed to be human- and scrape-friendly). Content type is
`text/plain`.

**Request**

```
GET /metrics HTTP/1.1
```

**Response**

```
HTTP/1.1 200 OK
Content-Type: text/plain

proxy_requests_total 1523
proxy_requests_failed_total 4
proxy_last_request_duration_seconds 0.014213
```

**Metric reference**

| Metric name                              | Type    | Description |
|-------------------------------------------|---------|--------------|
| `proxy_requests_total`                    | counter | Incremented once per proxied request that reaches `ServeHTTP` completion (including upstream errors). |
| `proxy_requests_failed_total`              | counter | Incremented when a request fails to route (no matching pool → `404`), has no healthy backend (`503`), or the upstream returns a proxy/transport error (`502`). |
| `proxy_last_request_duration_seconds`      | gauge   | Wall-clock duration, in seconds, of the most recently completed proxy round trip. |

---

## Reverse-Proxy Data Plane — `ANY /*`

Every request whose path is **not** `/healthz` or `/metrics` is handled by
the `ProxyServer`:

1. **Routing** — `Router.Match` finds the **longest matching path prefix**
   among configured `routes[].match` entries and resolves the associated
   pool.
   - No match → `404 Not Found` (`not found`), and
     `proxy_requests_failed_total` is incremented.

2. **Backend selection** — `Pool.Choose` asks the pool's configured
   `Strategy` (`round_robin` | `least_connections` | `random`) for the
   next backend, considering only backends currently marked `Alive`.
   - No alive backend → `503 Service Unavailable`
     (`no healthy backends available`), and
     `proxy_requests_failed_total` is incremented.

3. **Forwarding** — the request is forwarded via
   `httputil.NewSingleHostReverseProxy` with:
   - `Host` header rewritten to the backend's host.
   - `X-Forwarded-For` set to the client's IP (parsed from
     `RemoteAddr`).
   - `X-Forwarded-Proto` set to `http` or `https` depending on the
     inbound listener.
   - `X-Forwarded-Host` set to the original `Host` header from the
     client request.

4. **Upstream error handling** — if the reverse proxy cannot reach or
   read from the backend, the client receives `502 Bad Gateway`
   (`bad gateway`), `proxy_requests_failed_total` is incremented, and the
   backend's passive failure counter is incremented
   (`Pool.MarkFailure`).

5. **Passive health accounting** — on a response that completes without
   a transport-level error, `Pool.MarkSuccess` resets the backend's
   failure streak and, once `healthy_threshold` consecutive successes
   accumulate, restores `Alive = true`. Conversely, `Pool.MarkFailure`
   resets the success streak and, once `unhealthy_threshold` consecutive
   failures accumulate, sets `Alive = false`, removing the backend from
   the healthy candidate pool used by every `Strategy`.

### Example

Given the routing/pool configuration:

```yaml
routes:
  - match: "/api"
    pool: "api_pool"
pools:
  - name: "api_pool"
    strategy: "round_robin"
    backends:
      - url: "http://10.0.0.1:9000"
      - url: "http://10.0.0.2:9000"
```

Request:

```
GET /api/users/42 HTTP/1.1
Host: proxy.example.com
```

is routed to `api_pool`, load-balanced round-robin across the two
backends, and forwarded as:

```
GET /api/users/42 HTTP/1.1
Host: 10.0.0.1:9000
X-Forwarded-For: 203.0.113.7
X-Forwarded-Proto: https
X-Forwarded-Host: proxy.example.com
```

### Response Status Summary

| Status | Condition |
|--------|-----------|
| `2xx/3xx/4xx` (from upstream) | Backend response, returned unmodified to the client. |
| `404 Not Found` | No route matches the request path. |
| `502 Bad Gateway` | Selected backend was unreachable or returned a transport-level proxy error. |
| `503 Service Unavailable` | Route matched, but no backend in the target pool is currently marked healthy. |

---

## Active Health Checks (Background, Not Client-Facing)

For pools with `health_check.enabled: true`, GoProxy independently issues
periodic `GET` requests to `health_check.path` on **every** backend
(default path `/healthz`, default interval `10s`, default timeout `2s`).
This is a **server-side probing loop**, not a client-callable API:

- HTTP status `200–399` → counts as a probe success.
- Any other status, or a request error, → counts as a probe failure.
- Redirects are **not** followed (`http.ErrUseLastResponse`).
- Threshold hysteresis (`healthy_threshold` / `unhealthy_threshold`)
  applies identically to active and passive signals, sharing the same
  `Alive` state consumed by `Pool.Choose` and reported (in aggregate) via
  `GET /healthz`.
