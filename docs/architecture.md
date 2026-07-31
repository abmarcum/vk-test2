# GoProxy — Software Architecture Document (Revision 3)

## `docs/architecture.md`

---

## 0. Revision Notes (v2 → v3)

This revision resolves all findings from the Product Manager's Revision 2 review. This document remains the **single canonical architecture specification**. Changes in this revision:

1. **Resolved `/healthz` vs. redirect-handler ambiguity** — explicit statement that exclusion is achieved via mux route precedence in `main.go`, not internal branching in `redirectToHTTPSHandler`.
2. **K8s probe port decided and documented** — probes target port 8080 (HTTP listener) explicitly, never 8443.
3. **CSI vs. in-process Secret Manager path decision stated explicitly** — GKE deployment uses `cert_source: "file"` (CSI mount); in-process `secretmanager` API path is documented as the non-CSI/GCE alternative, not a co-equal default.
4. **Removed speculative `Backend.mu sync.RWMutex` field** — deleted entirely; will be reintroduced only if a genuine non-atomic mutable field is added in a future revision.
5. **Health-check logging dependency wired explicitly** — `RunHealthChecks` signature now accepts a `*slog.Logger` parameter; no ambient/package-level logger variable is relied upon.
6. **`ErrNoHealthyBackends` → 503 mechanics fully specified** — `Director` cannot short-circuit response writing directly (per `net/http/httputil.ReverseProxy` semantics); the sentinel-in-context → `ErrorHandler`-writes-503 flow is now explicit end-to-end in §4.2.1 and §4.3.

---

## 1. Purpose & Scope

This document defines the complete system architecture for GoProxy, a single-binary SSL-terminating HTTP reverse proxy and load balancer. It specifies the directory structure, file responsibilities, module interfaces, data structures, concurrency model, and deployment topology. This document is the binding contract for the Developer Agent implementation phase.

**Hard constraint:** Exactly **4 core Go source files** (`main.go`, `config.go`, `proxy.go`, `balancer.go`). No additional core files, packages, or subdirectories for application logic are permitted. Test files (`*_test.go`) counterpart each core file and are exempt from the count.

---

## 2. Directory Structure

```
goproxy/
├── main.go                      # Entry point, bootstrap, wiring, graceful shutdown
├── config.go                    # Config structs, YAML parsing, validation, secret loading
├── proxy.go                     # Reverse proxy, routing, middleware, metrics, logging
├── balancer.go                  # LB strategies, pool state, health checking
│
├── main_test.go                 # Integration-style tests: bootstrap, shutdown
├── config_test.go               # Unit tests: parsing, validation, defaults
├── proxy_test.go                # Unit tests: routing, header rewriting, proxy behavior
├── balancer_test.go             # Unit tests: RR/least-conn selection, health transitions
│
├── go.mod
├── go.sum
│
├── config.yaml.example          # Sample configuration (matches PRD §7 schema)
│
├── Dockerfile                   # Multi-stage build → distroless final image
├── .dockerignore
│
├── Makefile                     # build / test / lint / docker / fmt / vet targets
│
├── k8s/
│   ├── deployment.yaml          # GKE Deployment (goproxy pods)
│   ├── service.yaml             # LoadBalancer / ClusterIP Service
│   ├── configmap.yaml           # Mounts config.yaml into pods
│   ├── secretproviderclass.yaml # CSI driver mapping → GCP Secret Manager (TLS cert/key)
│   └── serviceaccount.yaml      # Workload Identity binding, least-priv IAM
│
├── scripts/
│   └── file-count-lint.sh       # CI guard: fails if 5th core .go file introduced
│
├── .github/
│   └── workflows/
│       └── ci.yaml              # lint, vet, test, file-count check, docker build
│
├── README.md                    # Usage, deployment, config reference
└── docs/
    └── architecture.md          # This document (canonical, single source of truth)
```

**Design rationale:**
- No `/internal`, `/pkg`, or `/cmd` layout — a multi-directory Go module structure would imply package boundaries beyond the 4-file constraint. All application code lives in `package main` at repo root.
- `k8s/`, `scripts/`, `.github/` are operational/infra artifacts, explicitly excluded from the "core code file" count per PRD §5.
- Test files live alongside their subject file (idiomatic Go) and are exempt per PRD §5.

---

## 3. Module Responsibility Matrix

| File | Owns | Does NOT own |
|---|---|---|
| `main.go` | Process lifecycle: flag/env parsing, config load invocation, TLS listener construction, HTTP server(s) start, signal handling (SIGTERM/SIGHUP), graceful shutdown/drain, top-level wiring of `Router`↔`BalancerPool`↔`HealthChecker`, mux route precedence (including `/healthz` carve-out on the HTTP listener) | Config parsing internals, LB algorithm internals, proxy header rewriting |
| `config.go` | `Config` struct tree, YAML unmarshal, schema validation, defaulting, GCP Secret Manager client calls, TLS material resolution (file vs secretmanager), duration parsing | HTTP handling, LB logic, request routing |
| `proxy.go` | `Router` (path-prefix matching → pool selection), `httputil.ReverseProxy` construction & `Director`/`ModifyResponse`/`ErrorHandler` hooks, proxy header injection (`X-Forwarded-*`), request-scoped backend/pool context propagation, no-healthy-backend sentinel handling, Prometheus metrics registration & recording, `slog`-based structured request logging middleware, `/healthz` and `/metrics` HTTP handlers | Backend selection algorithm, health-check scheduling, config parsing |
| `balancer.go` | `Pool` type (backend set + strategy), `Backend` state (healthy/unhealthy, active connections, consecutive fail/success counters), `Strategy` interface + Round Robin / Least Connections / Random implementations, active health-check goroutine loop (with explicit logger dependency), passive failure marking hook (invoked by `proxy.go` on dial/proxy error) | HTTP routing, TLS, logging middleware, metrics emission (exposes state; `proxy.go` reads it for metrics) |

---

## 4. Core Interfaces & Data Structures

> Interfaces only — full implementations deferred to Developer Agent. This is the **canonical** definition; no abbreviated variant of these structs is authoritative.

### 4.1 `config.go`

## config.go
```go
package main

import "context"

// Top-level configuration tree, unmarshaled from YAML.
type Config struct {
	Server ServerConfig  `yaml:"server"`
	Routes []RouteConfig `yaml:"routes"`
	Pools  []PoolConfig  `yaml:"pools"`
}

type ServerConfig struct {
	HTTPAddr            string        `yaml:"http_addr"`
	HTTPSAddr           string        `yaml:"https_addr"`
	TLS                 TLSConfig     `yaml:"tls"`
	Timeouts            TimeoutConfig `yaml:"timeouts"`
	ShutdownGracePeriod string        `yaml:"shutdown_grace_period"` // default "15s"

	// HTTPRedirectToHTTPS controls behavior of the plain-HTTP listener
	// (HTTPAddr) when TLS is configured. See §5.1 for full decision matrix.
	//   true  (default when TLS configured): HTTP requests receive 301 → https://host+path
	//                                        (except /healthz — see §5.1/§5.2)
	//   false: HTTP listener proxies traffic identically to HTTPS (not recommended for prod)
	// Ignored (treated as false / plain-proxy) when TLS is not configured at all.
	HTTPRedirectToHTTPS bool `yaml:"http_redirect_to_https"`
}

// TLSConfig — v1 supports exactly one certificate/key pair, terminated at
// the process. SNI-based multi-domain certificate selection (i.e., a
// GetCertificate callback backed by a hostname→cert map) is EXPLICITLY
// DEFERRED / OUT OF SCOPE for v1 per PRD §3.1 "optional/desired" framing.
// A single default cert is presented for all SNI names. Multi-cert SNI
// is a candidate for a future PRD revision and would require a 5th
// architectural concept (cert map) — not introduced here.
//
// DEPLOYMENT DECISION (resolves PM Revision 2 finding #3): the recommended
// GKE deployment topology (§7) uses CertSource == "file", with the CSI
// Secret Store driver mounting GCP Secret Manager secrets as local files
// (see k8s/secretproviderclass.yaml) at CertPath/KeyPath. The in-process
// CertSource == "secretmanager" path (direct API call via
// cloud.google.com/go/secretmanager) is the ALTERNATIVE for non-CSI
// environments (e.g., bare GCE VMs without the CSI driver installed), not
// a co-equal default. Both paths remain implemented for portability, but
// "file" is the prescribed choice for the GKE reference deployment.
type TLSConfig struct {
	CertSource string `yaml:"cert_source"` // "file" | "secretmanager" | ""
	CertPath   string `yaml:"cert_path"`
	KeyPath    string `yaml:"key_path"`
	CertSecret string `yaml:"cert_secret"`
	KeySecret  string `yaml:"key_secret"`
}

type TimeoutConfig struct {
	ReadTimeout  string `yaml:"read_timeout"`
	WriteTimeout string `yaml:"write_timeout"`
	IdleTimeout  string `yaml:"idle_timeout"`
}

type RouteConfig struct {
	PathPrefix string `yaml:"path_prefix"`
	Pool       string `yaml:"pool"`
}

// PoolConfig.Strategy accepts "round_robin" (M2, default), "least_conn" (M3),
// "random" (M3, stretch — see PRD §3.3). All three are implemented in this
// architecture; only round_robin is required for M2 milestone completion.
type PoolConfig struct {
	Name        string            `yaml:"name"`
	Strategy    string            `yaml:"strategy"` // round_robin | least_conn | random
	HealthCheck HealthCheckConfig `yaml:"health_check"`
	Backends    []BackendConfig   `yaml:"backends"`
}

type HealthCheckConfig struct {
	Path               string `yaml:"path"`
	Interval           string `yaml:"interval"`
	Timeout            string `yaml:"timeout"`
	UnhealthyThreshold int    `yaml:"unhealthy_threshold"`
	HealthyThreshold   int    `yaml:"healthy_threshold"`
}

type BackendConfig struct {
	URL string `yaml:"url"`
}

// --- Exported functions (public surface consumed by main.go) ---

// LoadConfig reads, parses, defaults, and validates the YAML file at path.
func LoadConfig(path string) (*Config, error)

// Validate enforces schema invariants (non-empty pools, valid strategy names,
// route→pool references resolve, duration strings parseable, threshold > 0).
func (c *Config) Validate() error

// ResolveTLSMaterial returns PEM-encoded cert & key bytes, sourcing from
// local filesystem or GCP Secret Manager per TLSConfig.CertSource. In the
// prescribed GKE deployment this reads CSI-mounted files (CertSource=="file");
// the secretmanager branch is exercised only in non-CSI environments.
// Uses cloud.google.com/go/secretmanager internally; no secret bytes logged.
func ResolveTLSMaterial(ctx context.Context, cfg TLSConfig) (certPEM, keyPEM []byte, err error)
```

### 4.2 `balancer.go`

## balancer.go
```go
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/url"
	"sync/atomic"
)

// Backend represents one upstream target and its live health state.
//
// CANONICAL DEFINITION — Alive is atomic.Bool (boolean health flag), NOT
// atomic.Int64. This is the single authoritative struct definition; any
// abbreviated restatement elsewhere must match this field-for-field.
//
// NOTE (resolves PM Revision 2 finding #4): no reserved/speculative mutex
// field is declared here. All current fields are atomic and require no
// external locking. If a future revision introduces a genuinely non-atomic
// mutable field, a sync.Mutex/RWMutex should be added AT THAT TIME, scoped
// specifically to the new field(s) it protects — not speculatively in
// advance.
type Backend struct {
	URL *url.URL

	Alive       atomic.Bool  // true = eligible for selection
	ActiveConns atomic.Int64 // in-flight request count; see §4.2.1 contract below

	consecFails     atomic.Int32 // passive + active failure hysteresis counter
	consecSuccesses atomic.Int32 // passive + active success hysteresis counter
}

// Strategy selects the next backend from a candidate slice.
// Implementations must be goroutine-safe and O(1)-ish per call.
// Implementations MUST filter to Alive==true backends internally (or accept
// a pre-filtered slice from Pool.Choose — see NewPool/Choose contract).
type Strategy interface {
	Next(backends []*Backend) (*Backend, error) // returns ErrNoHealthyBackends if none alive
}

// RoundRobinStrategy — required for M2. Cursor is an internal atomic counter,
// incremented per call, modulo len(backends), skipping non-Alive entries.
type RoundRobinStrategy struct {
	cursor atomic.Uint64
}

// LeastConnStrategy — M3. Selects the Alive backend with the lowest
// ActiveConns snapshot at call time. See §4.2.1 for the increment/decrement
// contract this strategy depends on for correctness.
type LeastConnStrategy struct{}

// RandomStrategy — M3, STRETCH GOAL per PRD §3.3. Not required for milestone
// completion but implemented here for interface completeness; scheduling
// owner (Developer Agent) may defer this specific strategy's implementation
// to end of M3 without blocking milestone sign-off.
type RandomStrategy struct{}

// Pool groups backends under one strategy and health-check policy.
type Pool struct {
	Name        string
	Backends    []*Backend
	Strategy    Strategy
	HealthCheck HealthCheckConfig
}

// NewPool constructs a Pool from PoolConfig, resolving Strategy by name.
func NewPool(cfg PoolConfig) (*Pool, error)

// Choose returns the next healthy backend per the pool's strategy.
// Does NOT mutate ActiveConns — that is the caller's (proxy.go Director's)
// responsibility per the contract in §4.2.1.
func (p *Pool) Choose() (*Backend, error)

// MarkFailure implements passive circuit-breaker-lite: called by proxy.go
// on dial/transport error; increments failure counter, may flip Alive=false
// immediately (fast passive trip) pending next active health-check recovery.
func (p *Pool) MarkFailure(b *Backend)

// MarkSuccess resets passive failure counters on a successful proxied response.
func (p *Pool) MarkSuccess(b *Backend)

// RunHealthChecks starts a blocking loop (intended to run in its own goroutine)
// that periodically GETs HealthCheck.Path on each backend, applying
// UnhealthyThreshold / HealthyThreshold hysteresis to flip Alive state.
// Exits cleanly when ctx is canceled (used for graceful shutdown).
//
// logger is REQUIRED (resolves PM Revision 2 finding #5) and MUST be used to
// emit a structured log line on every Alive state transition, e.g.:
//   logger.Info("backend health transition", "pool", p.Name, "backend", b.URL.String(),
//               "from", wasAlive, "to", nowAlive, "consec_fails", n, "consec_successes", m)
// Silent health flips are considered a debuggability defect; callers (main.go)
// MUST pass a non-nil logger. There is no package-level/ambient logger
// variable in balancer.go — the dependency is always explicit.
func (p *Pool) RunHealthChecks(ctx context.Context, logger *slog.Logger)

var ErrNoHealthyBackends = errors.New("no healthy backends available")
```

#### 4.2.1 `ActiveConns` Increment/Decrement Contract (Least Connections correctness)

This contract is binding on the `proxy.go` implementation and MUST be followed exactly to prevent count leaks or double-counting:

| Lifecycle Point | Action | Owner |
|---|---|---|
| Immediately after `pool.Choose()` succeeds inside `Director` | `backend.ActiveConns.Add(1)` | `proxy.go` `buildDirector` |
| `ModifyResponse` fires (successful upstream response received) | `backend.ActiveConns.Add(-1)` exactly once | `proxy.go` `buildModifyResponse` |
| `ErrorHandler` fires (upstream dial/write/read error, `ModifyResponse` never reached) | `backend.ActiveConns.Add(-1)` exactly once | `proxy.go` `buildErrorHandler` |

**Invariant:** Exactly one of `ModifyResponse` or `ErrorHandler` fires per proxied request (per `net/http/httputil.ReverseProxy` semantics) — never both, never neither. The decrement therefore occurs exactly once per request that reached the increment step. `ModifyResponse` and `ErrorHandler` MUST both retrieve the same `*Backend` pointer that `Director` stored in the request context (§4.3) to guarantee they decrement the identical counter instance that was incremented.

**No-healthy-backend short-circuit mechanics (resolves PM Revision 2 finding #6 — full end-to-end specification):**

`Director`'s signature (`func(*http.Request)`) has **no return value and cannot write an HTTP response or abort the round trip directly** — this is a hard constraint of `net/http/httputil.ReverseProxy`. Therefore, when `pool.Choose()` returns `ErrNoHealthyBackends`, the following exact sequence applies:

1. `Director` does **not** increment `ActiveConns` (no backend was selected).
2. `Director` stashes a **sentinel `proxyRequestState`** into the request context via `withProxyState`, with `Backend == nil`, `Pool` set to the matched pool (for metrics/logging), and a new field `Err error` set to `ErrNoHealthyBackends` (see revised `proxyRequestState` in §4.3).
3. `Director` sets `req.URL = nil` is **not** used (would panic downstream); instead `Director` leaves the request otherwise unmodified (scheme/host untouched) — this deliberately causes the underlying `http.Transport.RoundTrip` to fail fast (e.g., dialing an empty/invalid host), which **guarantees** `ReverseProxy` invokes `ErrorHandler` next, never `ModifyResponse`.
   - Concretely: `Director` sets `req.URL.Scheme = "http"` and `req.URL.Host = ""` (empty host), which `http.Transport` reliably rejects with a transport error (`http: no Host in request URL` or equivalent dial error), triggering `ErrorHandler`.
4. `buildErrorHandler`'s closure, on invocation, retrieves `proxyRequestState` from `r.Context()`:
   - If `state.Err == ErrNoHealthyBackends`: write **503 Service Unavailable** directly (skip `pool.MarkFailure`, since no specific backend was involved — only record `goproxy_requests_total{pool=state.Pool.Name, backend="", status="503"}`).
   - Else (a real backend was selected but dial/proxy failed): decrement `ActiveConns` (§4.2.1 table above), call `pool.MarkFailure(backend)`, write **502 Bad Gateway**, record `status="502"`.

This makes the distinction between "no healthy backend available" (503, no `MarkFailure` call, no `ActiveConns` touch) and "selected backend failed mid-request" (502, `MarkFailure` + decrement) fully deterministic and testable, closing the ambiguity flagged in review.

**Concurrency model:** `Backend.Alive` and `ActiveConns` are `atomic` types — read/written from the health-check goroutine, the proxy request path, and metrics scrape concurrently without locks. Strategy implementations must not mutate shared backend state beyond atomics (RoundRobin's cursor is its own atomic counter; LeastConn reads `ActiveConns` snapshot only, never writes it).

### 4.3 `proxy.go`

## proxy.go
```go
package main

import (
	"context"
	"net/http"
	"time"
)

// Router maps path prefixes to Pools (longest-prefix-match).
type Router struct {
	routes []compiledRoute // sorted by prefix length, descending
	pools  map[string]*Pool
}

// NewRouter builds a Router from RouteConfig list + resolved Pool map.
func NewRouter(routes []RouteConfig, pools map[string]*Pool) (*Router, error)

// Match returns the Pool responsible for a given request path.
func (r *Router) Match(path string) (*Pool, bool)

// --- Request-scoped context propagation ---
//
// ReverseProxy.Director, ModifyResponse, and ErrorHandler are all configured
// ONCE at construction time on a single *httputil.ReverseProxy instance
// (per Router, not per-request). Because backend selection via Pool.Choose()
// happens per-request inside Director, the chosen *Pool and *Backend MUST be
// communicated to ModifyResponse/ErrorHandler via the request's context
// (NOT via closure-captured variables, which would be shared/racy across
// concurrent requests).

type proxyContextKey struct{}

// proxyRequestState is stashed into the request context by Director and
// retrieved by ModifyResponse / ErrorHandler / logging / metrics middleware.
//
// Err is non-nil ONLY in the no-healthy-backend short-circuit case (see
// §4.2.1). When Err == ErrNoHealthyBackends, Backend is nil and MUST NOT be
// dereferenced; consumers must check Err first.
type proxyRequestState struct {
	Pool    *Pool
	Backend *Backend // nil iff Err == ErrNoHealthyBackends
	Route   string   // matched path prefix, for metrics label
	Err     error    // set by Director when Choose() fails; nil otherwise
}

// withProxyState returns a new request with proxyRequestState attached.
func withProxyState(r *http.Request, state *proxyRequestState) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), proxyContextKey{}, state))
}

// proxyStateFromContext retrieves the state stashed by withProxyState.
// Returns (nil, false) if absent (e.g., request never reached Director).
func proxyStateFromContext(ctx context.Context) (*proxyRequestState, bool)

// NewReverseProxyHandler wires Router + metrics + logging into a single
// http.Handler used for both HTTP and HTTPS listeners. Constructs exactly
// one *httputil.ReverseProxy internally, with Director/ModifyResponse/
// ErrorHandler all reading chosen backend from request context per above.
func NewReverseProxyHandler(router *Router, timeouts TimeoutConfig) http.Handler

// buildDirector returns the ReverseProxy.Director closure. Responsibilities:
//   1. Match request path → Pool via router.Match(path).
//   2. pool.Choose():
//        - On success: backend.ActiveConns.Add(1) [increment — §4.2.1],
//          stash proxyRequestState{Pool, Backend, Route, Err: nil}, rewrite
//          req.URL.Scheme/Host to backend.URL, rewrite req.Host, inject
//          X-Forwarded-* headers.
//        - On ErrNoHealthyBackends: DO NOT touch ActiveConns. Stash
//          proxyRequestState{Pool: pool, Backend: nil, Route: route,
//          Err: ErrNoHealthyBackends}. Force the transport to fail fast by
//          setting req.URL.Scheme = "http" and req.URL.Host = "" (empty),
//          guaranteeing http.Transport.RoundTrip errors and ErrorHandler
//          (never ModifyResponse) is invoked next. See §4.2.1 for full
//          rationale — Director has no mechanism to write a response or
//          abort the round trip directly.
//   3. Context is attached via the standard idiom: *req = *req.WithContext(...)
//      (Director receives *http.Request and mutates it in place).
func buildDirector(router *Router) func(*http.Request)

// buildModifyResponse returns the ReverseProxy.ModifyResponse hook. This
// hook is invoked ONLY on a successful round trip, which (per buildDirector
// above) is only possible when a real Backend was selected — i.e.
// state.Err is always nil here; state.Backend is always non-nil.
//   1. Retrieve proxyRequestState from resp.Request.Context().
//   2. backend.ActiveConns.Add(-1)  [decrement — see §4.2.1 contract]
//   3. pool.MarkSuccess(backend)
//   4. Record goproxy_requests_total{pool, backend, status=resp.StatusCode}.
func buildModifyResponse() func(*http.Response) error

// buildErrorHandler returns the ReverseProxy.ErrorHandler hook (bound once
// per ReverseProxy instance, NOT per backend — retrieves backend from
// request context, not from a closure argument). Handles TWO distinct cases,
// disambiguated via proxyRequestState.Err (see §4.2.1):
//
//   Case A — state.Err == ErrNoHealthyBackends (no backend was ever selected):
//     1. Do NOT touch ActiveConns. Do NOT call pool.MarkFailure (no specific
//        backend to blame).
//     2. Record goproxy_requests_total{pool=state.Pool.Name, backend="", status="503"}.
//     3. Write 503 Service Unavailable to w.
//
//   Case B — state.Err == nil but state.Backend != nil (a backend was
//   selected and the round trip subsequently failed — dial/write/read error):
//     1. backend.ActiveConns.Add(-1) [decrement — §4.2.1].
//     2. pool.MarkFailure(backend).
//     3. Record goproxy_requests_total{pool, backend, status="502"}.
//     4. Write 502 Bad Gateway to w.
//
//   Case C — proxyRequestState absent entirely (should not normally occur;
//   defensive fallback): log at Warn level, write 502, record status="502"
//   with backend="unknown".
func buildErrorHandler() func(http.ResponseWriter, *http.Request, error)

// loggingMiddleware wraps a handler, emitting one structured slog JSON line
// per request: method, path, upstream (pool/backend from context, or
// "none"/"<pool>:" when state.Err == ErrNoHealthyBackends), status,
// latency_ms, remote_addr.
func loggingMiddleware(next http.Handler) http.Handler

// metricsMiddleware wraps a handler, recording Prometheus counters/histograms
// (goproxy_request_duration_seconds; goproxy_requests_total is recorded in
// buildModifyResponse/buildErrorHandler where status is definitively known).
func metricsMiddleware(next http.Handler) http.Handler

// registerMetrics registers all package-level Prometheus collectors against
// a dedicated prometheus.Registry exactly once. This is the ONLY sanctioned
// registration entry point — there is deliberately no init()-based
// registration path, to avoid global side effects that complicate resetting
// state between table-driven tests. Must be called explicitly from main.go
// before serving traffic; calling it twice must be safe (idempotent guard
// via sync.Once) but is not the intended usage pattern.
func registerMetrics(reg *prometheus.Registry)

// redirectToHTTPSHandler returns a handler that issues 301 Moved Permanently
// to https://<host><path>?<query> for EVERY request it receives, with no
// internal path exceptions or branching logic whatsoever.
//
// EXCLUSION MECHANISM (resolves PM Revision 2 finding #1): the fact that
// GET /healthz remains reachable on the plain-HTTP listener even when this
// handler is active is achieved ENTIRELY via mux route registration
// precedence in main.go — specifically, main.go registers "/healthz" on the
// HTTP listener's mux BEFORE/SEPARATELY FROM the catch-all "/" route that
// points to this handler (see §5 step 10 and §5.2). This handler itself
// contains NO knowledge of "/healthz" and performs NO path inspection or
// carve-out logic. Developer Agent implementation MUST NOT add any
// conditional branching for specific paths inside this function; the
// carve-out is a routing-table concern owned exclusively by main.go.
func redirectToHTTPSHandler() http.Handler

// HealthzHandler serves GET /healthz (proxy liveness/readiness, independent
// of backend health). Mounted on BOTH the HTTP and HTTPS listener muxes in
// main.go, always as a specific route registration distinct from the
// catch-all "/" route.
func HealthzHandler(w http.ResponseWriter, r *http.Request)

// MetricsHandler serves GET /metrics via promhttp.HandlerFor(registry, ...),
// using the explicit registry populated by registerMetrics (not the global
// prometheus.DefaultRegisterer).
func MetricsHandler(reg *prometheus.Registry) http.Handler
```

**Prometheus metric names (package-level `var`s in `proxy.go`, registered exclusively via `registerMetrics()`):**

| Metric | Type | Labels |
|---|---|---|
| `goproxy_requests_total` | Counter | `pool`, `backend`, `status` |
| `goproxy_request_duration_seconds` | Histogram | `pool`, `route` |
| `goproxy_upstream_healthy` | Gauge | `pool`, `backend` |
| `goproxy_active_connections` | Gauge | `pool`, `backend` |

### 4.4 `main.go`

## main.go
```go
package main

import (
	"context"
	"crypto/tls"
	"net/http"
	"time"
)

// main is the sole process entry point:
//  1. Parse flags/env (config path, log level).
//  2. cfg := LoadConfig(path); cfg.Validate()
//  3. Build a package-scoped *slog.Logger (JSON handler, stdout).
//  4. Build Pools from cfg.Pools (balancer.go), Router from cfg.Routes (proxy.go).
//  5. registerMetrics(registry) — exactly once, before serving.
//  6. Launch one health-check goroutine per Pool, bound to a cancelable ctx,
//     passing the logger explicitly: go pool.RunHealthChecks(ctx, logger).
//  7. Resolve TLS material (config.go) if configured; else HTTP-only mode.
//  8. Construct http.Server(s) and mux route tables per §5 / §5.1 / §5.2.
//  9. Start listeners in goroutines; block on signal channel.
// 10. On SIGTERM: ctx cancel → health checkers stop; server.Shutdown(gracePeriod).
// 11. On SIGHUP: v1 behavior is LOG-ONLY ("reload not supported, restart
//     process required"). No hot-reload is performed in v1.
func main()

// buildTLSListenerConfig constructs *tls.Config enforcing TLS1.2+ and an
// approved cipher suite list, for a SINGLE certificate/key pair (v1 scope).
//
// NOTE — SNI multi-domain certificate support (a GetCertificate callback
// keyed by SNI ServerName) is EXPLICITLY DEFERRED / OUT OF SCOPE for v1.
// This function does not implement or stub such a callback. All TLS
// connections receive the same certificate regardless of SNI ServerName.
func buildTLSListenerConfig(certPEM, keyPEM []byte) (*tls.Config, error)

// gracefulShutdown coordinates ordered teardown: stop accepting new conns,
// drain in-flight requests up to grace period, cancel health-check contexts.
func gracefulShutdown(ctx context.Context, servers []*http.Server, cancelHealth context.CancelFunc, grace time.Duration)

// --- NON-V1 / STRETCH DESIGN SKETCH — NOT IMPLEMENTED IN v1 ---
//
// The following describes a POSSIBLE future hot-reload mechanism for SIGHUP
// handling. It is explicitly OUT OF SCOPE for v1 and MUST NOT be built
// during initial implementation milestones. It is documented here only to
// avoid an undocumented dead-end if a future revision picks this up.
//
//   On SIGHUP (future):
//     1. newCfg := LoadConfig(path); newCfg.Validate() — on failure, log
//        and continue serving old config (never crash on bad reload).
//     2. Build new Pools + Router from newCfg.
//     3. Start new health-check goroutines against a fresh ctx (with logger);
//        wait for at least one health-check pass before cutover.
//     4. Atomically swap the live router pointer: a package-level
//        atomic.Pointer[Router] read by the request-serving handler on
//        every request, written once by the reload path.
//     5. Cancel old health-check ctx; allow old Pool objects to be GC'd.
//
// This is a design sketch only. v1 ships with restart-only config changes.
```

---

## 5. Runtime Wiring Sequence (main.go orchestration)

```
1. LoadConfig(path) ──► Config
2. Config.Validate()
3. logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
4. For each PoolConfig → NewPool() ──► map[string]*Pool
5. NewRouter(RouteConfig, pools) ──► *Router
6. registry := prometheus.NewRegistry(); registerMetrics(registry)   // explicit call, no init()
7. ctx, cancelHealth := context.WithCancel(context.Background())
8. For each Pool → go pool.RunHealthChecks(ctx, logger)   // logger passed explicitly — §4.2
9. If TLS configured:
     certPEM, keyPEM := ResolveTLSMaterial(ctx, cfg.Server.TLS)   // "file" path (CSI mount) prescribed for GKE
     tlsCfg := buildTLSListenerConfig(certPEM, keyPEM)   // single cert; no SNI map (v1)
     httpsServer := &http.Server{Addr: HTTPSAddr, TLSConfig: tlsCfg, Handler: httpsMux}
   Else:
     logger.Warn("TLS not configured — HTTP-only mode (dev/test)")

10. proxyHandler := loggingMiddleware(metricsMiddleware(NewReverseProxyHandler(router, timeouts)))

11. httpsMux (only constructed if TLS configured):
      httpsMux.Handle("/healthz", http.HandlerFunc(HealthzHandler))
      httpsMux.Handle("/metrics", MetricsHandler(registry))
      httpsMux.Handle("/", proxyHandler)

12. httpMux construction (see §5.1 / §5.2 decision matrix — route registration
    ORDER/SPECIFICITY is what implements the /healthz carve-out, NOT any
    branching logic inside redirectToHTTPSHandler itself):

      If TLS configured AND cfg.Server.HTTPRedirectToHTTPS (default true):
          httpMux.Handle("/healthz", http.HandlerFunc(HealthzHandler))  // registered BEFORE catch-all
          httpMux.Handle("/", redirectToHTTPSHandler())                // catch-all, everything else redirects
      Else (TLS not configured, OR HTTPRedirectToHTTPS explicitly false):
          httpMux.Handle("/healthz", http.HandlerFunc(HealthzHandler))
          httpMux.Handle("/metrics", MetricsHandler(registry))
          httpMux.Handle("/", proxyHandler)

13. httpServer := &http.Server{Addr: HTTPAddr, Handler: httpMux}
14. go httpServer.ListenAndServe()
15. If TLS configured: go httpsServer.ListenAndServeTLS("", "") // certs already in tlsCfg
16. signal.Notify(sigCh, SIGTERM, SIGHUP)
17. select on sigCh:
      SIGTERM → cancelHealth(); gracefulShutdown(servers, grace=cfg.Server.ShutdownGracePeriod); os.Exit(0)
      SIGHUP  → logger.Warn("config reload not supported in v1; restart process to apply changes")
```

### 5.1 HTTP Listener Behavior — Decision Matrix (unchanged from Revision 2, now cross-referenced from §5.2)

| TLS Configured? | `HTTPRedirectToHTTPS` | Port 8080 (HTTP) behavior | Port 8443 (HTTPS) behavior |
|---|---|---|---|
| Yes | `true` (**default**) | `/healthz` → 200 (always answerable); all other paths → 301 redirect to `https://` equivalent URL | Full reverse-proxy + `/healthz` + `/metrics` |
| Yes | `false` (explicit opt-out) | Full reverse-proxy, identical to HTTPS listener (not recommended for production; documented as a dev/debug escape hatch) | Full reverse-proxy + `/healthz` + `/metrics` |
| No (dev/test) | n/a (ignored) | Full reverse-proxy + `/healthz` + `/metrics` (sole listener) | Not started |

This is a **decided, explicit** behavior — the Developer Agent must implement exactly this matrix, not infer intent from ambiguous comments.

### 5.2 `/healthz` Carve-Out Mechanism (new — resolves PM Revision 2 finding #1)

To eliminate any ambiguity about **how** `/healthz` remains reachable on the HTTP listener while all other paths redirect:

- **`redirectToHTTPSHandler()` (proxy.go) is unconditional.** It issues a 301 for literally every request it receives. It contains **zero path-inspection logic**. It must never be modified to special-case `/healthz` internally.
- **The carve-out is achieved exclusively by `main.go`'s mux route registration order/specificity** (§5 step 12): `/healthz` is registered as its own explicit route on `httpMux`, and `redirectToHTTPSHandler()` is registered only at the catch-all `"/"` pattern. Standard Go `http.ServeMux` semantics (or equivalent router) dispatch the more specific `/healthz` registration for that exact path, never reaching the `"/"` handler.
- **Rationale:** keeping `redirectToHTTPSHandler` unconditional keeps it trivially unit-testable in isolation (assert 301 for arbitrary input paths) without needing to encode routing knowledge into it. All routing/precedence concerns live in exactly one place — `main.go`'s mux construction — consistent with `main.go`'s ownership of "mux route precedence" per §3's responsibility matrix.
- `/metrics` is **not** carved out on the HTTP listener when `HTTPRedirectToHTTPS=true` (per §5.1 table, only `/healthz` is exempted) — this is a deliberate scope decision: metrics scraping is expected to occur via the HTTPS listener or a separate internal-only path in production, keeping the redirect-mode HTTP listener's attack surface minimal.

---

## 6. Cross-Cutting Concerns

**Metrics registry:** A single `*prometheus.Registry` (not the global `prometheus.DefaultRegisterer`) is created in `main.go` and passed to `registerMetrics(registry)`, called **exactly once**, explicitly, before any server starts accepting traffic. **There is no `init()`-based registration path** — `init()` registration is explicitly disallowed to keep test setup/teardown deterministic.

**Logging:** One `*slog.Logger` (JSON handler, stdout) constructed in `main.go`. It is passed **explicitly as a function parameter** everywhere it is needed:
- Into `pool.RunHealthChecks(ctx, logger)` (balancer.go) — required parameter, not optional, not ambient (resolves PM Revision 2 finding #5).
- Into `proxy.go`'s `loggingMiddleware` construction (via closure capture at construction time in `main.go`'s wiring step, since `loggingMiddleware` wraps a handler built once at startup — this is safe because the *logger itself* is immutable/shared-safe by design, unlike per-request backend state which must go through context).

There is no package-level/global `var logger *slog.Logger` in any file — the dependency is always explicit at the function signature level, making test setups (which may inject a `slog.New(slog.NewTextHandler(io.Discard, nil))` no-op logger) straightforward and collision-free.

No secret material (cert/key bytes) ever passed to logger.

**Request-scoped state propagation:** All per-request backend/pool selection state, including the no-healthy-backend sentinel case, flows through `r.Context()` via `withProxyState`/`proxyStateFromContext` (see §4.3, §4.2.1). This is the only sanctioned mechanism for `Director` → `ModifyResponse`/`ErrorHandler` → logging/metrics middleware communication. No closure-captured per-request mutable state is permitted.

**Error handling convention:** All exported functions return `(T, error)`; no `panic` in request-serving paths. `main.go` may `log.Fatal`/`os.Exit(1)` only during startup (config/TLS load failure) before serving begins.

**Config validation failures:** Fail fast at startup (`main.go` exits non-zero with a clear `slog.Error` message) — never partially start with an invalid pool/route graph.

**Testing strategy per file:**
- `config_test.go`: table-driven YAML fixtures (valid/invalid), secret resolution mocked via interface seam.
- `balancer_test.go`: strategy fairness (RR distribution), least-conn under simulated concurrent load (explicitly asserting the increment/decrement contract in §4.2.1 never leaks or double-counts), health-check threshold state machine transitions, **assert every Alive transition emits exactly one structured log line via an injected test logger**.
- `proxy_test.go`: `httptest.Server` backends, header injection assertions, router prefix-match precedence, context-based backend propagation (`Director`→`ModifyResponse`/`ErrorHandler`), passive-trip behavior, HTTP→HTTPS redirect matrix (§5.1) verified for all three states, **`/healthz` reachability verified specifically via mux registration (not via any redirectToHTTPSHandler internal logic) — test asserts `redirectToHTTPSHandler` itself returns 301 for `/healthz` when invoked directly, proving the carve-out lives only in mux wiring**, and **explicit test for the `ErrNoHealthyBackends` → 503 path distinguishing it from the 502 dial-failure path** (asserting `MarkFailure` is NOT called in the 503 case).
- `main_test.go`: end-to-end smoke test — spin up config + in-process backends, hit `/healthz` on both listeners per §5.1/§5.2, verify graceful shutdown drains an in-flight slow request within grace period; verify SIGHUP logs a warning and does not crash or reload.

---

## 7. Deployment Topology (GCP)

```
Internet
   │ HTTPS
   ▼
GCP HTTP(S) LB / GKE Service (LoadBalancer type)
   │
   ▼
┌────────────────────────────┐
│ GKE Deployment: goproxy    │  (N replicas, stateless)
│  - Pod: goproxy container  │
│    - config.yaml (ConfigMap mount)
│    - TLS cert/key (CSI Secret Store → GCP Secret Manager mount)
│    - Service Account: goproxy-sa (roles/secretmanager.secretAccessor only)
└──────────────┬─────────────┘
               │ HTTP
    ┌──────────┼──────────┐
    ▼          ▼          ▼
 Backend A  Backend B  Backend C   (separate GKE Services / GCE instances)
```

**TLS material sourcing decision (resolves PM Revision 2 finding #3):** In this GKE reference deployment, `config.yaml`'s `server.tls.cert_source` is set to `"file"`, with `cert_path`/`key_path` pointing at the CSI Secret Store volume mount (see `k8s/secretproviderclass.yaml`), which transparently syncs from GCP Secret Manager to local files inside the pod. The in-process `cert_source: "secretmanager"` path (direct `cloud.google.com/go/secretmanager` API calls from within `config.go`) is documented as the **alternative for non-CSI environments only** (e.g., a bare GCE VM running the binary without the CSI driver available) — it is not wired as a co-equal default in the GKE manifests and should not be assumed as the primary path by the Developer Agent.

**Health probe port decision (resolves PM Revision 2 finding #2):** `k8s/deployment.yaml` readiness AND liveness probes both target **port 8080 (the plain-HTTP listener), path `/healthz`, scheme HTTP** — never port 8443/HTTPS. This is guaranteed safe per §5.1: `/healthz` is answerable on port
