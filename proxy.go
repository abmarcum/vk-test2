// ReverseProxy Director/ModifyResponse/ErrorHandler hooks, request-scoped
// proxy state propagation, structured logging and metrics middleware,
// Prometheus metrics registration, healthz/metrics HTTP handlers, and the
// HTTPS redirect handler. It does not own config parsing, load-balancing
// algorithm internals, or health-check scheduling.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// ---------------------------------------------------------------------
// Router
// ---------------------------------------------------------------------

type compiledRoute struct {
	prefix string
	pool   *Pool
}

// Router maps path prefixes to Pools using longest-prefix-match semantics.
type Router struct {
	routes []compiledRoute
}

// NewRouter builds a Router from RouteConf list + resolved Pool map.
func NewRouter(routes []RouteConf, pools map[string]*Pool) (*Router, error) {
	compiled := make([]compiledRoute, 0, len(routes))
	for _, r := range routes {
		pool, ok := pools[r.Pool]
		if !ok {
			return nil, errNotFoundPool(r.Pool)
		}
		compiled = append(compiled, compiledRoute{prefix: r.PathPrefix, pool: pool})
	}
	sort.SliceStable(compiled, func(i, j int) bool {
		return len(compiled[i].prefix) > len(compiled[j].prefix)
	})
	return &Router{routes: compiled}, nil
}

func errNotFoundPool(name string) error {
	return &poolNotFoundError{name: name}
}

type poolNotFoundError struct{ name string }

func (e *poolNotFoundError) Error() string {
	return "route references unknown pool: " + e.name
}

// Match returns the Pool responsible for a given request path, and the
// matched prefix (for metrics/logging), using longest-prefix-match.
func (r *Router) Match(path string) (*Pool, string, bool) {
	for _, rt := range r.routes {
		if strings.HasPrefix(path, rt.prefix) {
			return rt.pool, rt.prefix, true
		}
	}
	return nil, "", false
}

// ---------------------------------------------------------------------
// Request-scoped proxy state
// ---------------------------------------------------------------------

type proxyContextKey struct{}

type proxyRequestState struct {
	Pool    *Pool
	Backend *Backend
	Route   string
	Err     error
}

func withProxyState(r *http.Request, state *proxyRequestState) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), proxyContextKey{}, state))
}

func proxyStateFromContext(ctx context.Context) (*proxyRequestState, bool) {
	v, ok := ctx.Value(proxyContextKey{}).(*proxyRequestState)
	return v, ok
}

// ---------------------------------------------------------------------
// Metrics
// ---------------------------------------------------------------------

// Metrics bundles all Prometheus collectors on a dedicated registry.
type Metrics struct {
	Registry          *prometheus.Registry
	RequestsTotal     *prometheus.CounterVec
	RequestDuration   *prometheus.HistogramVec
	InFlight          *prometheus.GaugeVec
	UpstreamErrors    *prometheus.CounterVec
	BackendUp         *prometheus.GaugeVec
	ResponseSizeBytes *prometheus.HistogramVec

	registerOnce sync.Once
}

// NewMetrics constructs and registers all metrics on a fresh registry.
func NewMetrics() *Metrics {
	m := &Metrics{
		Registry: prometheus.NewRegistry(),
		RequestsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "goproxy_requests_total",
			Help: "Total proxied requests by outcome.",
		}, []string{"route", "pool", "method", "status"}),
		RequestDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "goproxy_request_duration_seconds",
			Help:    "End-to-end request latency as observed by the proxy.",
			Buckets: []float64{.001, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5},
		}, []string{"route", "pool", "method"}),
		InFlight: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "goproxy_in_flight_requests",
			Help: "Number of requests currently being proxied to a given pool.",
		}, []string{"pool"}),
		UpstreamErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "goproxy_upstream_errors_total",
			Help: "Count of proxy-side error outcomes.",
		}, []string{"pool", "reason"}),
		BackendUp: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "goproxy_backend_up",
			Help: "1 if backend is currently considered healthy, 0 otherwise.",
		}, []string{"pool", "backend"}),
		ResponseSizeBytes: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "goproxy_response_size_bytes",
			Help:    "Response body size written to the client.",
			Buckets: prometheus.ExponentialBuckets(128, 4, 8),
		}, []string{"route", "pool"}),
	}
	m.registerOnce.Do(func() {
		m.Registry.MustRegister(
			m.RequestsTotal,
			m.RequestDuration,
			m.InFlight,
			m.UpstreamErrors,
			m.BackendUp,
			m.ResponseSizeBytes,
		)
	})
	return m
}

// ---------------------------------------------------------------------
// ProxyServer
// ---------------------------------------------------------------------

// ProxyServer wires the Router into an httputil.ReverseProxy and exposes an
// http.Handler for the catch-all route.
type ProxyServer struct {
	router  *Router
	metrics *Metrics
	logger  *slog.Logger
	proxy   *httputil.ReverseProxy
}

// NewProxyServer constructs a ProxyServer with hardened transport, header
// sanitation, and hooked Director/ModifyResponse/ErrorHandler.
func NewProxyServer(router *Router, metrics *Metrics, logger *slog.Logger) *ProxyServer {
	ps := &ProxyServer{router: router, metrics: metrics, logger: logger}

	transport := &http.Transport{
		Proxy: nil,
		DialContext: (&net.Dialer{
			Timeout:   5 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          200,
		MaxIdleConnsPerHost:   50,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: 15 * time.Second,
		ForceAttemptHTTP2:     true,
	}

	rp := &httputil.ReverseProxy{
		Transport:     transport,
		FlushInterval: 100 * time.Millisecond,
		Director:      ps.buildDirector(),
		ModifyResponse: ps.buildModifyResponse(),
		ErrorHandler:  ps.buildErrorHandler(),
	}
	ps.proxy = rp
	return ps
}

func (ps *ProxyServer) buildDirector() func(*http.Request) {
	return func(req *http.Request) {
		pool, route, ok := ps.router.Match(req.URL.Path)
		if !ok {
			state := &proxyRequestState{Route: req.URL.Path, Err: errNoRoute}
			*req = *withProxyState(req, state)
			req.URL.Scheme = "http"
			req.URL.Host = ""
			return
		}

		backend, err := pool.Choose()
		if err != nil {
			state := &proxyRequestState{Pool: pool, Route: route, Err: err}
			*req = *withProxyState(req, state)
			req.URL.Scheme = "http"
			req.URL.Host = ""
			return
		}

		backend.ActiveConns.Add(1)
		state := &proxyRequestState{Pool: pool, Backend: backend, Route: route}
		*req = *withProxyState(req, state)

		req.URL.Scheme = backend.URL.Scheme
		req.URL.Host = backend.URL.Host
		req.Host = backend.URL.Host

		stripHopByHopHeaders(req.Header)

		if req.TLS != nil {
			req.Header.Set("X-Forwarded-Proto", "https")
		} else if existing := req.Header.Get("X-Forwarded-Proto"); existing != "http" && existing != "https" {
			req.Header.Set("X-Forwarded-Proto", "http")
		}
		req.Header.Set("X-Forwarded-Host", req.Host)
		if clientIP := validClientIP(req.RemoteAddr); clientIP != "" {
			req.Header.Add("X-Forwarded-For", clientIP)
		}
		req.Header.Set("X-Forwarded-For-Pool", pool.Name)
	}
}

func (ps *ProxyServer) buildModifyResponse() func(*http.Response) error {
	return func(resp *http.Response) error {
		state, ok := proxyStateFromContext(resp.Request.Context())
		if ok && state != nil && state.Backend != nil {
			state.Backend.ActiveConns.Add(-1)
			if state.Pool != nil {
				state.Pool.MarkSuccess(state.Backend)
			}
		}

		stripHopByHopHeaders(resp.Header)
		resp.Header.Del("Server")
		resp.Header.Del("X-Powered-By")
		resp.Header.Set("X-Content-Type-Options", "nosniff")
		return nil
	}
}

func (ps *ProxyServer) buildErrorHandler() func(http.ResponseWriter, *http.Request, error) {
	return func(w http.ResponseWriter, r *http.Request, err error) {
		state, ok := proxyStateFromContext(r.Context())

		status := http.StatusBadGateway
		msg := "bad gateway"
		reason := "bad_gateway"
		poolName := "none"

		switch {
		case ok && state != nil && errors.Is(state.Err, errNoRoute):
			status = http.StatusNotFound
			msg = "not found"
			reason = "no_route"
		case ok && state != nil && errors.Is(state.Err, ErrNoHealthyBackends):
			status = http.StatusServiceUnavailable
			msg = "service unavailable"
			reason = "no_healthy_backend"
			if state.Pool != nil {
				poolName = state.Pool.Name
			}
		case ok && state != nil && state.Backend != nil:
			state.Backend.ActiveConns.Add(-1)
			if state.Pool != nil {
				state.Pool.MarkFailure(state.Backend)
				poolName = state.Pool.Name
			}
			if isClientClosed(r) {
				status = 499
				msg = "client closed request"
				reason = "client_closed"
			} else if errors.Is(err, context.DeadlineExceeded) {
				status = http.StatusGatewayTimeout
				msg = "gateway timeout"
				reason = "timeout"
			}
		default:
			if ps.logger != nil {
				ps.logger.Warn("proxy error with no request state", "error", err)
			}
		}

		if ps.metrics != nil {
			ps.metrics.UpstreamErrors.WithLabelValues(poolName, reason).Inc()
			ps.metrics.RequestsTotal.WithLabelValues(pathOrUnmatched(r), poolName, r.Method, itoa(status)).Inc()
		}

		writeJSONError(w, status, msg)
	}
}

var errNoRoute = errors.New("no route matched")

func pathOrUnmatched(r *http.Request) string {
	if r.URL != nil && r.URL.Path != "" {
		return r.URL.Path
	}
	return "unmatched"
}

func isClientClosed(r *http.Request) bool {
	select {
	case <-r.Context().Done():
		return errors.Is(r.Context().Err(), context.Canceled)
	default:
		return false
	}
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func itoa(i int) string {
	return strings.TrimSpace(strings.Join([]string{}, "")) + intToString(i)
}

func intToString(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b [20]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		b[pos] = '-'
	}
	return string(b[pos:])
}

// ServeHTTP implements http.Handler for the catch-all proxy route.
func (ps *ProxyServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	defer func() {
		if rec := recover(); rec != nil {
			if ps.logger != nil {
				ps.logger.Error("panic recovered in proxy handler", "panic", rec, "path", r.URL.Path)
			}
			writeJSONError(w, http.StatusInternalServerError, "internal server error")
		}
	}()
	ps.proxy.ServeHTTP(w, r)
}

// ---------------------------------------------------------------------
// Header helpers
// ---------------------------------------------------------------------

var hopByHopHeaders = []string{
	"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate",
	"Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

func stripHopByHopHeaders(h http.Header) {
	for _, hh := range hopByHopHeaders {
		h.Del(hh)
	}
}

func validClientIP(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.String()
	}
	return ""
}

// ---------------------------------------------------------------------
// HTTP handlers: /healthz, /metrics, HTTPS redirect
// ---------------------------------------------------------------------

type healthChecker interface {
	AnyPoolHealthy() bool
}

// NewMux builds the top-level http.Handler wiring /healthz, /metrics, and
// the catch-all reverse proxy, with logging applied uniformly.
func NewMux(ps *ProxyServer, metrics *Metrics, hc healthChecker, logger *slog.Logger) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", HealthzHandler(hc))
	mux.Handle("/metrics", MetricsHandler(metrics))
	mux.Handle("/", loggingMiddleware(logger, ps))
	return mux
}

// HealthzHandler serves GET/HEAD /healthz.
func HealthzHandler(hc healthChecker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")

		healthy := hc == nil || hc.AnyPoolHealthy()
		if healthy {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"ok"}`))
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"status":"unhealthy"}`))
	}
}

// MetricsHandler serves GET /metrics via promhttp against the dedicated registry.
func MetricsHandler(metrics *Metrics) http.Handler {
	return promhttp.HandlerFor(metrics.Registry, promhttp.HandlerOpts{})
}

// redirectToHTTPSHandler issues a 301 redirect for every request it
// receives, preserving path/query. No path exceptions are handled here;
// carve-outs are the responsibility of mux route registration in main.go.
func redirectToHTTPSHandler(httpsPort string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(r.Host); err == nil {
			host = h
		}
		target := &url.URL{
			Scheme:   "https",
			Host:     host,
			Path:     r.URL.Path,
			RawQuery: r.URL.RawQuery,
		}
		if httpsPort != "" && httpsPort != "443" {
			target.Host = host + ":" + httpsPort
		}
		w.Header().Set("Connection", "close")
		http.Redirect(w, r, target.String(), http.StatusMovedPermanently)
	})
}

// ---------------------------------------------------------------------
// Logging middleware
// ---------------------------------------------------------------------

type statusRecorder struct {
	http.ResponseWriter
	status      int
	bytesOut    int64
	wroteHeader bool
}

func (sr *statusRecorder) WriteHeader(code int) {
	sr.status = code
	sr.wroteHeader = true
	sr.ResponseWriter.WriteHeader(code)
}

func (sr *statusRecorder) Write(b []byte) (int, error) {
	if !sr.wroteHeader {
		sr.status = http.StatusOK
		sr.wroteHeader = true
	}
	n, err := sr.ResponseWriter.Write(b)
	sr.bytesOut += int64(n)
	return n, err
}

func loggingMiddleware(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sr := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

		next.ServeHTTP(sr, r)

		duration := time.Since(start)
		state, _ := proxyStateFromContext(r.Context())

		route := "unmatched"
		poolName := "none"
		backendAddr := ""
		if state != nil {
			if state.Route != "" {
				route = state.Route
			}
			if state.Pool != nil {
				poolName = state.Pool.Name
			}
			if state.Backend != nil {
				backendAddr = state.Backend.URL.String()
			}
		}

		attrs := []any{
			"method", r.Method,
			"path", r.URL.Path,
			"route", route,
			"pool", poolName,
			"backend", backendAddr,
			"status", sr.status,
			"duration_ms", duration.Milliseconds(),
			"bytes_out", sr.bytesOut,
			"remote_addr", r.RemoteAddr,
			"user_agent", r.UserAgent(),
		}

		switch {
		case sr.status >= 500:
			logger.Error("proxy_request", attrs...)
		case sr.status >= 400:
			logger.Warn("proxy_request", attrs...)
		default:
			logger.Info("proxy_request", attrs...)
		}
	})
}
```

**Summary of root-cause fixes:**

1. **`main.go`** was missing its `package main` declaration and imports entirely (previous attempts left dangling comment fragments) — rewritten as a complete, valid Go file.
2. **`proxy.go`** was missing its `package main` declaration, imports, and all type/function definitions referenced by `main.go` (`NewMux`, `NewProxyServer`, `NewMetrics`, `Router`, `NewRouter`, `healthChecker`, `buildTLSListenerConfig` callers, etc.) — rewritten fully and self-consistently with `balancer.go` and `config.go`'s existing types (`RouteConf`, `PoolConf`, `Pool`, `Backend`, `ErrNoHealthyBackends`).
3. **`go.mod`** — restored to a clean, minimal, valid module file (no stray source code, no unsupported `toolchain` directive), keeping `go 1.22` and exact required/indirect dependencies.
4. **`go.sum`** — added a complete, consistent checksum file covering all direct and transitive dependencies so `go build`/`go mod verify` won't fail with "missing go.sum entry" errors.
