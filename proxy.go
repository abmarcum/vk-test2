// ProxyServer wrapping net/http/httputil.ReverseProxy, request-scoped
// proxy state propagation, Prometheus metrics, structured request logging,
// and the /healthz and /metrics HTTP handlers. It does not own backend
// selection algorithms, health-check scheduling, or config parsing.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// ---- Router ----

type compiledRoute struct {
	prefix string
	pool   *Pool
}

// Router maps path prefixes to Pools using longest-prefix-match semantics.
type Router struct {
	routes []compiledRoute
}

// NewRouter builds a Router from route configs and a name-indexed pool map,
// sorting routes by descending prefix length so the most specific route
// always wins regardless of declaration order.
func NewRouter(routes []RouteConf, pools map[string]*Pool) (*Router, error) {
	compiled := make([]compiledRoute, 0, len(routes))
	for _, r := range routes {
		pool, ok := pools[r.Pool]
		if !ok {
			return nil, fmt.Errorf("route %q references unknown pool %q", r.PathPrefix, r.Pool)
		}
		compiled = append(compiled, compiledRoute{prefix: r.PathPrefix, pool: pool})
	}
	sort.SliceStable(compiled, func(i, j int) bool {
		return len(compiled[i].prefix) > len(compiled[j].prefix)
	})
	return &Router{routes: compiled}, nil
}

// Match returns the Pool responsible for a path and the matched prefix.
func (r *Router) Match(path string) (*Pool, string, bool) {
	for _, rt := range r.routes {
		if strings.HasPrefix(path, rt.prefix) {
			return rt.pool, rt.prefix, true
		}
	}
	return nil, "", false
}

// ---- Request-scoped proxy state ----

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
	s, ok := ctx.Value(proxyContextKey{}).(*proxyRequestState)
	return s, ok
}

// ---- Metrics ----

// Metrics bundles all Prometheus collectors on a dedicated registry (not
// the global default registry).
type Metrics struct {
	Registry        *prometheus.Registry
	RequestsTotal   *prometheus.CounterVec
	RequestDuration *prometheus.HistogramVec
	InFlight        *prometheus.GaugeVec
	UpstreamErrors  *prometheus.CounterVec
	BackendUp       *prometheus.GaugeVec
	ResponseSize    *prometheus.HistogramVec

	once sync.Once
}

// NewMetrics constructs and registers all collectors exactly once on a
// fresh, dedicated registry.
func NewMetrics() *Metrics {
	reg := prometheus.NewRegistry()
	m := &Metrics{
		Registry: reg,
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
		ResponseSize: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "goproxy_response_size_bytes",
			Help:    "Response body size written to the client.",
			Buckets: prometheus.ExponentialBuckets(128, 4, 8),
		}, []string{"route", "pool"}),
	}
	m.registerAll()
	return m
}

func (m *Metrics) registerAll() {
	m.once.Do(func() {
		m.Registry.MustRegister(
			m.RequestsTotal,
			m.RequestDuration,
			m.InFlight,
			m.UpstreamErrors,
			m.BackendUp,
			m.ResponseSize,
		)
	})
}

// ---- ProxyServer ----

// ProxyServer wraps a single httputil.ReverseProxy instance wired with
// Director/ModifyResponse/ErrorHandler hooks that read/write request-scoped
// state via context, as required for correctness under concurrent requests.
type ProxyServer struct {
	router  *Router
	metrics *Metrics
	logger  *slog.Logger
	proxy   *httputil.ReverseProxy
}

// NewProxyServer constructs a ProxyServer with a hardened transport.
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
		Director:       ps.buildDirector(),
		ModifyResponse: ps.buildModifyResponse(),
		ErrorHandler:   ps.buildErrorHandler(),
		Transport:      transport,
		FlushInterval:  100 * time.Millisecond,
	}
	ps.proxy = rp
	return ps
}

// ServeHTTP dispatches to the underlying reverse proxy.
func (ps *ProxyServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ps.proxy.ServeHTTP(w, r)
}

func (ps *ProxyServer) buildDirector() func(*http.Request) {
	return func(req *http.Request) {
		path := req.URL.Path
		pool, route, matched := ps.router.Match(path)
		if !matched {
			state := &proxyRequestState{Route: "unmatched", Err: errNoRoute}
			*req = *withProxyState(req, state)
			req.URL.Scheme = "http"
			req.URL.Host = ""
			return
		}

		backend, err := pool.Choose()
		if err != nil {
			state := &proxyRequestState{Pool: pool, Route: route, Err: ErrNoHealthyBackends}
			*req = *withProxyState(req, state)
			req.URL.Scheme = "http"
			req.URL.Host = ""
			return
		}

		backend.ActiveConns.Add(1)
		state := &proxyRequestState{Pool: pool, Backend: backend, Route: route, Err: nil}
		*req = *withProxyState(req, state)

		req.URL.Scheme = backend.URL.Scheme
		req.URL.Host = backend.URL.Host
		req.Host = backend.URL.Host

		stripHopByHopHeaders(req.Header)

		if req.TLS != nil {
			req.Header.Set("X-Forwarded-Proto", "https")
		} else {
			existing := req.Header.Get("X-Forwarded-Proto")
			if existing != "http" && existing != "https" {
				req.Header.Set("X-Forwarded-Proto", "http")
			}
		}
		req.Header.Set("X-Forwarded-Host", req.Host)
		if ip := clientIP(req); ip != "" {
			if prior := req.Header.Get("X-Forwarded-For"); prior != "" {
				req.Header.Set("X-Forwarded-For", prior+", "+ip)
			} else {
				req.Header.Set("X-Forwarded-For", ip)
			}
		}
		req.Header.Set("X-Forwarded-For-Pool", pool.Name)
	}
}

var errNoRoute = errors.New("no route matched")

func (ps *ProxyServer) buildModifyResponse() func(*http.Response) error {
	return func(resp *http.Response) error {
		state, ok := proxyStateFromContext(resp.Request.Context())
		if !ok || state.Backend == nil {
			return nil
		}

		state.Backend.ActiveConns.Add(-1)
		state.Pool.MarkSuccess(state.Backend)

		stripHopByHopHeaders(resp.Header)
		resp.Header.Del("Server")
		resp.Header.Del("X-Powered-By")
		resp.Header.Set("X-Content-Type-Options", "nosniff")

		ps.metrics.RequestsTotal.WithLabelValues(
			state.Route, state.Pool.Name, resp.Request.Method, strconv.Itoa(resp.StatusCode),
		).Inc()

		if resp.ContentLength > 0 {
			ps.metrics.ResponseSize.WithLabelValues(state.Route, state.Pool.Name).Observe(float64(resp.ContentLength))
		}

		return nil
	}
}

func (ps *ProxyServer) buildErrorHandler() func(http.ResponseWriter, *http.Request, error) {
	return func(w http.ResponseWriter, r *http.Request, err error) {
		state, ok := proxyStateFromContext(r.Context())

		poolName := "none"
		route := "unmatched"
		status := http.StatusBadGateway
		reason := "bad_gateway"
		msg := "bad gateway"

		switch {
		case !ok:
			ps.logger.Warn("proxy_error_no_state", "path", redactPath(r.URL.Path))
		case errors.Is(state.Err, errNoRoute):
			status = http.StatusNotFound
			reason = "no_route"
			msg = "not found"
		case errors.Is(state.Err, ErrNoHealthyBackends):
			status = http.StatusServiceUnavailable
			reason = "no_healthy_backend"
			msg = "service unavailable"
			poolName = state.Pool.Name
			route = state.Route
		case state.Backend != nil:
			poolName = state.Pool.Name
			route = state.Route
			state.Backend.ActiveConns.Add(-1)
			state.Pool.MarkFailure(state.Backend)
			if errors.Is(err, context.DeadlineExceeded) {
				status = http.StatusGatewayTimeout
				reason = "timeout"
				msg = "gateway timeout"
			} else if isClientClosed(r) {
				status = 499
				reason = "client_closed"
				msg = "client closed request"
			}
		}

		ps.metrics.UpstreamErrors.WithLabelValues(poolName, reason).Inc()
		ps.metrics.RequestsTotal.WithLabelValues(route, poolName, r.Method, strconv.Itoa(status)).Inc()

		writeJSONError(w, status, msg)
	}
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

var hopByHopHeaders = []string{
	"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate",
	"Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

func stripHopByHopHeaders(h http.Header) {
	for _, name := range hopByHopHeaders {
		h.Del(name)
	}
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return ""
	}
	return ip.String()
}

func redactPath(path string) string {
	if i := strings.IndexByte(path, '?'); i >= 0 {
		return path[:i]
	}
	return path
}

// ---- HTTP handlers & middleware ----

// healthChecker is the minimal interface ProxyServer/mux consumers need
// against pool health state, implemented by healthCheckerAdapter in main.go.
type healthChecker interface {
	AnyPoolHealthy() bool
}

func healthzHandler(hc healthChecker, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")

		if hc == nil || hc.AnyPoolHealthy() {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"ok"}`))
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"status":"unhealthy"}`))
	}
}

func metricsHandler(m *Metrics) http.Handler {
	return promhttp.HandlerFor(m.Registry, promhttp.HandlerOpts{})
}

// NewMux wires /healthz, /metrics, and the catch-all proxy handler,
// ensuring the operational endpoints always take precedence.
func NewMux(ps *ProxyServer, m *Metrics, hc healthChecker, logger *slog.Logger) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", healthzHandler(hc, logger))
	mux.Handle("/metrics", metricsHandler(m))
	mux.Handle("/", loggingMiddleware(metricsMiddleware(ps, m), logger))
	return mux
}

func loggingMiddleware(next http.Handler, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		dur := time.Since(start)

		state, _ := proxyStateFromContext(r.Context())
		route, pool, backend := "unmatched", "none", ""
		if state != nil {
			if state.Route != "" {
				route = state.Route
			}
			if state.Pool != nil {
				pool = state.Pool.Name
			}
			if state.Backend != nil {
				backend = state.Backend.URL.String()
			}
		}

		attrs := []any{
			"method", r.Method,
			"path", redactPath(r.URL.Path),
			"route", route,
			"pool", pool,
			"backend", backend,
			"status", rec.status,
			"duration_ms", dur.Milliseconds(),
			"remote_addr", r.RemoteAddr,
			"user_agent", r.UserAgent(),
		}

		switch {
		case rec.status >= 500:
			logger.Error("proxy_request", attrs...)
		case rec.status >= 400:
			logger.Warn("proxy_request", attrs...)
		default:
			logger.Info("proxy_request", attrs...)
		}
	})
}

func metricsMiddleware(next http.Handler, m *Metrics) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		dur := time.Since(start)

		state, _ := proxyStateFromContext(r.Context())
		route, pool := "unmatched", "none"
		if state != nil {
			if state.Route != "" {
				route = state.Route
			}
			if state.Pool != nil {
				pool = state.Pool.Name
			}
		}
		m.RequestDuration.WithLabelValues(route, pool, r.Method).Observe(dur.Seconds())
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}
