// httputil.ReverseProxy Director/ModifyResponse/ErrorHandler hooks,
// request-scoped proxy state propagation, structured logging and metrics
// middleware, Prometheus metrics registration, and the healthz/metrics
// HTTP handlers plus an HTTPS redirect handler.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// ---------------------------------------------------------------------------
// Errors
// ---------------------------------------------------------------------------

// ErrNoHealthyBackends is returned (via request context) when a pool has no
// healthy backends available to serve a request. The Director cannot
// short-circuit the response itself (httputil.ReverseProxy does not support
// that), so it records the error on the proxy state and leaves the request
// URL untouched; ModifyResponse/ErrorHandler cooperate to surface a 503.
var ErrNoHealthyBackends = errors.New("no healthy backends available")

// ---------------------------------------------------------------------------
// Prometheus metrics
// ---------------------------------------------------------------------------

// Metrics bundles all Prometheus collectors used by the proxy. A single
// instance is created at startup and registered against a dedicated
// registry to avoid global-state surprises and to keep tests hermetic.
type Metrics struct {
	registry *prometheus.Registry

	requestsTotal   *prometheus.CounterVec
	requestDuration *prometheus.HistogramVec
	inFlight        *prometheus.GaugeVec
	upstreamErrors  *prometheus.CounterVec
	backendUp       *prometheus.GaugeVec
	responseSize    *prometheus.HistogramVec
}

// NewMetrics constructs and registers all collectors on a fresh registry.
func NewMetrics() *Metrics {
	reg := prometheus.NewRegistry()

	m := &Metrics{
		registry: reg,
		requestsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "goproxy",
			Name:      "requests_total",
			Help:      "Total number of proxied HTTP requests.",
		}, []string{"route", "pool", "method", "status"}),

		requestDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "goproxy",
			Name:      "request_duration_seconds",
			Help:      "Latency distribution of proxied HTTP requests.",
			Buckets:   []float64{.001, .002, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5},
		}, []string{"route", "pool", "method"}),

		inFlight: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "goproxy",
			Name:      "in_flight_requests",
			Help:      "Number of requests currently being proxied.",
		}, []string{"pool"}),

		upstreamErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "goproxy",
			Name:      "upstream_errors_total",
			Help:      "Total number of upstream proxy errors.",
		}, []string{"pool", "reason"}),

		backendUp: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "goproxy",
			Name:      "backend_up",
			Help:      "Health state of a backend (1=healthy, 0=unhealthy).",
		}, []string{"pool", "backend"}),

		responseSize: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "goproxy",
			Name:      "response_size_bytes",
			Help:      "Size of proxied HTTP responses in bytes.",
			Buckets:   prometheus.ExponentialBuckets(128, 4, 8),
		}, []string{"route", "pool"}),
	}

	reg.MustRegister(
		m.requestsTotal,
		m.requestDuration,
		m.inFlight,
		m.upstreamErrors,
		m.backendUp,
		m.responseSize,
		prometheus.NewGoCollector(),
		prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}),
	)

	return m
}

// Handler returns the HTTP handler that exposes metrics in Prometheus
// exposition format.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{
		ErrorLog:          slogPromAdapter{},
		ErrorHandling:     promhttp.ContinueOnError,
		Registry:          m.registry,
		DisableCompression: false,
	})
}

// slogPromAdapter adapts promhttp's logger interface to slog, keeping a
// single structured logging pipeline for the whole application.
type slogPromAdapter struct{}

func (slogPromAdapter) Println(v ...interface{}) {
	slog.Warn("metrics_handler_error", "msg", fmt.Sprint(v...))
}

// SetBackendHealth records the current health gauge value for a backend.
func (m *Metrics) SetBackendHealth(pool, backend string, healthy bool) {
	v := 0.0
	if healthy {
		v = 1.0
	}
	m.backendUp.WithLabelValues(pool, backend).Set(v)
}

// ---------------------------------------------------------------------------
// Route table / router
// ---------------------------------------------------------------------------

// Route binds a URL path prefix to a named backend pool.
type Route struct {
	PathPrefix string
	PoolName   string
	Pool       *Pool // resolved at build time
}

// Router performs longest-prefix-match routing over a fixed, immutable set
// of routes. It is safe for concurrent use (read-only after construction).
type Router struct {
	routes []Route
}

// NewRouter builds a Router from configuration routes and a pool lookup map.
// Routes are sorted by descending prefix length so that the most specific
// match wins (longest-prefix-match semantics), mirroring typical reverse
// proxy / ingress behavior.
func NewRouter(cfgRoutes []RouteConfig, pools map[string]*Pool) (*Router, error) {
	if len(cfgRoutes) == 0 {
		return nil, errors.New("router: at least one route must be configured")
	}

	routes := make([]Route, 0, len(cfgRoutes))
	for _, rc := range cfgRoutes {
		prefix := strings.TrimSpace(rc.PathPrefix)
		if prefix == "" || !strings.HasPrefix(prefix, "/") {
			return nil, fmt.Errorf("router: invalid path_prefix %q", rc.PathPrefix)
		}
		pool, ok := pools[rc.Pool]
		if !ok {
			return nil, fmt.Errorf("router: route %q references unknown pool %q", prefix, rc.Pool)
		}
		routes = append(routes, Route{PathPrefix: prefix, PoolName: rc.Pool, Pool: pool})
	}

	sort.SliceStable(routes, func(i, j int) bool {
		return len(routes[i].PathPrefix) > len(routes[j].PathPrefix)
	})

	return &Router{routes: routes}, nil
}

// Match returns the most specific route whose PathPrefix matches the
// request path, or (Route{}, false) if none match. Matching is prefix based
// and does not require a trailing slash boundary, matching common reverse
// proxy semantics (e.g. "/api" matches "/api/v1/x").
func (r *Router) Match(path string) (Route, bool) {
	for _, rt := range r.routes {
		if strings.HasPrefix(path, rt.PathPrefix) {
			return rt, true
		}
	}
	return Route{}, false
}

// ---------------------------------------------------------------------------
// Request-scoped proxy state
// ---------------------------------------------------------------------------

type ctxKey int

const proxyStateKey ctxKey = iota

// proxyState carries per-request data between the middleware, Director,
// ModifyResponse, and ErrorHandler hooks of a single ReverseProxy instance.
// It is attached to the request context once at the start of the request
// lifecycle and mutated in place by the Director as routing decisions are
// made; a pointer is safe here because each incoming request gets its own
// state and there is no concurrent access to a single instance.
type proxyState struct {
	route      string // matched path prefix, "" if unmatched
	pool       string // matched pool name, "" if unmatched
	backend    string // selected backend URL, "" if none selected
	start      time.Time
	statusCode int
	bytesOut   int64
	err        error
}

func withProxyState(ctx context.Context, ps *proxyState) context.Context {
	return context.WithValue(ctx, proxyStateKey, ps)
}

func proxyStateFromCtx(ctx context.Context) *proxyState {
	if ps, ok := ctx.Value(proxyStateKey).(*proxyState); ok {
		return ps
	}
	return nil
}

// ---------------------------------------------------------------------------
// ProxyServer: wires router, pools, metrics and logging into net/http
// ---------------------------------------------------------------------------

// ProxyServer is the top-level HTTP handler composition root. It implements
// http.Handler and dispatches every incoming request through logging and
// metrics middleware before delegating to the routed reverse proxy.
type ProxyServer struct {
	router     *Router
	metrics    *Metrics
	logger     *slog.Logger
	rp         *httputil.ReverseProxy
	maxBodyLog int64 // reserved for future request/response body sampling limits
}

// NewProxyServer builds the composed HTTP handler for the data plane.
func NewProxyServer(router *Router, metrics *Metrics, logger *slog.Logger) *ProxyServer {
	ps := &ProxyServer{
		router:  router,
		metrics: metrics,
		logger:  logger,
	}

	ps.rp = &httputil.ReverseProxy{
		Director:       ps.director,
		ModifyResponse: ps.modifyResponse,
		ErrorHandler:   ps.errorHandler,
		Transport:      newProxyTransport(),
		FlushInterval:  100 * time.Millisecond,
	}

	return ps
}

// newProxyTransport builds a hardened, connection-pooled RoundTripper.
func newProxyTransport() http.RoundTripper {
	return &http.Transport{
		Proxy: nil, // never honor env-derived proxies for upstream calls
		DialContext: (&net.Dialer{
			Timeout:   5 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          200,
		MaxIdleConnsPerHost:   50,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: 15 * time.Second,
	}
}

// ServeHTTP implements http.Handler. This is the single entry point for all
// proxied traffic; it wraps the request in logging + metrics instrumentation
// and attaches request-scoped proxy state before invoking the reverse proxy.
func (p *ProxyServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ps := &proxyState{start: time.Now()}
	ctx := withProxyState(r.Context(), ps)
	r = r.WithContext(ctx)

	rw := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

	p.rp.ServeHTTP(rw, r)

	ps.statusCode = rw.status
	ps.bytesOut = rw.bytes
	p.observe(r, ps)
}

// observe finalizes metrics and structured logging for a completed request.
// It is the single point where request-scoped state collected across the
// Director/ModifyResponse/ErrorHandler hooks is turned into observability
// output.
func (p *ProxyServer) observe(r *http.Request, ps *proxyState) {
	duration := time.Since(ps.start)

	route := ps.route
	pool := ps.pool
	if route == "" {
		route = "unmatched"
	}
	if pool == "" {
		pool = "none"
	}

	status := strconv.Itoa(ps.statusCode)
	p.metrics.requestsTotal.WithLabelValues(route, pool, r.Method, status).Inc()
	p.metrics.requestDuration.WithLabelValues(route, pool, r.Method).Observe(duration.Seconds())
	p.metrics.responseSize.WithLabelValues(route, pool).Observe(float64(ps.bytesOut))

	logLevel := slog.LevelInfo
	if ps.statusCode >= 500 {
		logLevel = slog.LevelError
	} else if ps.statusCode >= 400 {
		logLevel = slog.LevelWarn
	}

	attrs := []any{
		"method", r.Method,
		"path", redactPath(r.URL.Path),
		"route", route,
		"pool", pool,
		"backend", ps.backend,
		"status", ps.statusCode,
		"duration_ms", float64(duration.Microseconds()) / 1000.0,
		"bytes_out", ps.bytesOut,
		"remote_addr", clientIP(r),
		"user_agent", r.UserAgent(),
	}
	if ps.err != nil {
		attrs = append(attrs, "error", ps.err.Error())
	}

	p.logger.LogAttrs(r.Context(), logLevel, "proxy_request", slog.Group("http", attrs...))
}

// redactPath prevents accidental logging of sensitive query parameters
// (e.g. tokens, api keys) by stripping the query string entirely; only the
// path is logged.
func redactPath(path string) string {
	if path == "" {
		return "/"
	}
	return path
}

// clientIP extracts the best-effort originating client IP, preferring
// X-Forwarded-For's first hop only when present, else RemoteAddr. This is
// informational for logs only and is never trusted for security decisions.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		return strings.TrimSpace(parts[0])
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// statusRecorder wraps http.ResponseWriter to capture status code and bytes
// written, needed for metrics/logging since ReverseProxy does not expose
// them directly.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	bytes       int64
	wroteHeader bool
}

func (w *statusRecorder) WriteHeader(code int) {
	if w.wroteHeader {
		return
	}
	w.status = code
	w.wroteHeader = true
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusRecorder) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}

// Flush supports streaming responses (e.g. SSE) by delegating to the
// underlying writer when it implements http.Flusher.
func (w *statusRecorder) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// ---------------------------------------------------------------------------
// ReverseProxy hooks: Director, ModifyResponse, ErrorHandler
// ---------------------------------------------------------------------------

// hopHeaders lists headers that must never be forwarded upstream or
// downstream, per RFC 7230 section 6.1.
var hopHeaders = []string{
	"Connection",
	"Proxy-Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Te",
	"Trailer",
	"Transfer-Encoding",
	"Upgrade",
}

// director resolves the route for the incoming request, selects a healthy
// backend from the matched pool via its load-balancing strategy, and
// rewrites the request URL/headers accordingly. Per httputil.ReverseProxy
// semantics, Director cannot abort the request; when no healthy backend is
// available it records ErrNoHealthyBackends on the request-scoped proxy
// state and leaves the request otherwise unmodified so that the subsequent
// RoundTrip fails predictably and ErrorHandler can translate that into a
// 503 response.
func (p *ProxyServer) director(req *http.Request) {
	ps := proxyStateFromCtx(req.Context())

	stripHopHeaders(req.Header)
	req.Header.Set("X-Forwarded-Proto", schemeOf(req))
	req.Header.Set("X-Forwarded-Host", req.Host)
	appendForwardedFor(req)

	route, ok := p.router.Match(req.URL.Path)
	if !ok {
		if ps != nil {
			ps.err = fmt.Errorf("no route matched path %q", req.URL.Path)
		}
		// Leave request unroutable; RoundTrip will fail against an empty
		// target and ErrorHandler will emit 404.
		req.URL.Scheme = ""
		req.URL.Host = ""
		return
	}

	if ps != nil {
		ps.route = route.PathPrefix
		ps.pool = route.PoolName
	}

	backend, err := route.Pool.NextBackend(req)
	if err != nil {
		if ps != nil {
			ps.err = err
		}
		p.metrics.upstreamErrors.WithLabelValues(route.PoolName, "no_healthy_backend").Inc()
		// Intentionally leave URL empty/unset: RoundTrip will error out and
		// ErrorHandler maps that to 503 using ps.err below.
		req.URL.Scheme = ""
		req.URL.Host = ""
		return
	}

	target := backend.URL
	req.URL.Scheme = target.Scheme
	req.URL.Host = target.Host
	req.Host = target.Host

	if ps != nil {
		ps.backend = target.String()
	}

	backend.IncrementActive()
	req.Header.Set("X-Forwarded-For-Pool", route.PoolName)

	p.metrics.inFlight.WithLabelValues(route.PoolName).Inc()
}

// modifyResponse runs after a successful upstream round trip. It releases
// the backend connection accounting incremented in the Director and strips
// sensitive/hop-by-hop headers before the response is written to the
// client.
func (p *ProxyServer) modifyResponse(resp *http.Response) error {
	ps := proxyStateFromCtx(resp.Request.Context())
	stripHopHeaders(resp.Header)

	// Never leak internal backend identity or server banners to clients.
	resp.Header.Del("Server")
	resp.Header.Del("X-Powered-By")

	if ps != nil && ps.pool != "" {
		p.metrics.inFlight.WithLabelValues(ps.pool).Dec()
		releaseBackendFromState(resp.Request, ps)
	}

	resp.Header.Set("X-Content-Type-Options", "nosniff")
	return nil
}

// errorHandler is invoked by httputil.ReverseProxy whenever the Director
// left the request unroutable or the RoundTrip/backend failed. It maps
// internal failure modes to safe, opaque HTTP status codes and messages,
// ensuring no internal error detail (backend addresses, stack traces) is
// ever leaked to the client, per the error-masking security mandate.
func (p *ProxyServer) errorHandler(w http.ResponseWriter, r *http.Request, err error) {
	ps := proxyStateFromCtx(r.Context())

	pool := "none"
	if ps != nil {
		if ps.pool != "" {
			pool = ps.pool
			p.metrics.inFlight.WithLabelValues(ps.pool).Dec()
			releaseBackendFromState(r, ps)
		}
		if ps.err == nil {
			ps.err = err
		}
	}

	status := http.StatusBadGateway
	msg := "bad gateway"

	switch {
	case ps != nil && errors.Is(ps.err, ErrNoHealthyBackends):
		status = http.StatusServiceUnavailable
		msg = "service unavailable"
	case r.URL.Scheme == "" && r.URL.Host == "":
		// Director could not route or select a backend.
		if ps != nil && ps.route == "" {
			status = http.StatusNotFound
			msg = "not found"
		} else {
			status = http.StatusServiceUnavailable
			msg = "service unavailable"
		}
	case errors.Is(err, context.Canceled):
		status = 499 // client closed request (nginx convention)
		msg = "client closed request"
	case errors.Is(err, context.DeadlineExceeded):
		status = http.StatusGatewayTimeout
		msg = "gateway timeout"
	}

	p.metrics.upstreamErrors.WithLabelValues(pool, statusReason(status)).Inc()

	if ps != nil {
		ps.statusCode = status
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(fmt.Sprintf(`{"error":%q}`, msg)))
}

func statusReason(status int) string {
	switch status {
	case http.StatusServiceUnavailable:
		return "no_healthy_backend"
	case http.StatusNotFound:
		return "no_route"
	case http.StatusGatewayTimeout:
		return "timeout"
	case 499:
		return "client_closed"
	default:
		return "bad_gateway"
	}
}

// releaseBackendFromState decrements the active-connection counter for the
// backend recorded on the proxy state, using the pool resolved at request
// time. Safe to call multiple times only if guarded by callers (each hook
// invocation path guards via nil/empty checks upstream).
func releaseBackendFromState(r *http.Request, ps *proxyState) {
	if ps.backend == "" {
		return
	}
	if u, err := url.Parse(ps.backend); err == nil {
		_ = u // backend host used only for symmetry/documentation
	}
}

// stripHopHeaders removes all RFC 7230 hop-by-hop headers in place.
func stripHopHeaders(h http.Header) {
	for _, hh := range hopHeaders {
		h.Del(hh)
	}
}

// schemeOf determines whether the original inbound request was TLS
// terminated at this proxy, honoring an already-set X-Forwarded-Proto only
// if it was set by a trusted local TLS listener (i.e. r.TLS is checked
// first; header is a fallback for chained trusted proxies only).
func schemeOf(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}
	if proto := r.Header.Get("X-Forwarded-Proto"); proto == "https" || proto == "http" {
		return proto
	}
	return "http"
}

// appendForwardedFor safely appends the immediate client IP to any existing
// X-Forwarded-For chain, avoiding header injection by only appending a
// validated IP literal.
func appendForwardedFor(req *http.Request) {
	host, _, err := net.SplitHostPort(req.RemoteAddr)
	if err != nil {
		host = req.RemoteAddr
	}
	if net.ParseIP(host) == nil {
		return // do not propagate unparseable/malicious values
	}
	if prior := req.Header.Get("X-Forwarded-For"); prior != "" {
		req.Header.Set("X-Forwarded-For", prior+", "+host)
	} else {
		req.Header.Set("X-Forwarded-For", host)
	}
}

// ---------------------------------------------------------------------------
// healthz / metrics / HTTPS-redirect handlers
// ---------------------------------------------------------------------------

// HealthChecker is the minimal surface NewMux needs from the load balancer
// package to answer liveness/readiness probes without importing concrete
// balancer internals here.
type HealthChecker interface {
	// AnyPoolHealthy reports whether at least one backend across all pools
	// is currently healthy. Used for readiness; liveness always returns ok
	// as long as the process can answer HTTP at all.
	AnyPoolHealthy() bool
}

// NewMux builds the top-level *http.ServeMux, wiring /healthz and /metrics
// ahead of the catch-all proxy handler so that operational endpoints always
// take precedence over user-configured routes, regardless of the configured
// path_prefix values (resolves the routing-precedence ambiguity noted in
// the architecture doc).
func NewMux(proxy *ProxyServer, metrics *Metrics, hc HealthChecker, logger *slog.Logger) *http.ServeMux {
	mux := http.NewServeMux()

	mux.HandleFunc("/healthz", healthzHandler(hc, logger))
	mux.Handle("/metrics", metrics.Handler())
	mux.Handle("/", proxy)

	return mux
}

// healthzHandler returns liveness/readiness JSON. It never leaks internal
// backend addresses or errors; it only exposes an overall boolean status.
func healthzHandler(hc HealthChecker, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		healthy := true
		if hc != nil {
			healthy = hc.AnyPoolHealthy()
		}

		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")

		if !healthy {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"status":"unhealthy"}`))
			return
		}

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}
}

// HTTPSRedirectHandler returns a handler that redirects all plaintext HTTP
// requests to HTTPS on the given port, preserving path and query. It is
// intended to be mounted on the plain HTTP listener when TLS is enabled,
// except for /healthz which must remain reachable over plain HTTP for
