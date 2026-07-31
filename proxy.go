// Director/ModifyResponse/ErrorHandler hooks, request-scoped proxy state
// propagation, structured logging and metrics middleware, an in-process
// metrics registry, healthz/metrics HTTP handlers, and the HTTPS redirect
// handler. It does not own config parsing, load-balancing algorithm
// internals, or health-check scheduling.
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
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
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
			return nil, fmt.Errorf("route references unknown pool: %s", r.Pool)
		}
		compiled = append(compiled, compiledRoute{prefix: r.PathPrefix, pool: pool})
	}
	sort.SliceStable(compiled, func(i, j int) bool {
		return len(compiled[i].prefix) > len(compiled[j].prefix)
	})
	return &Router{routes: compiled}, nil
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

var errNoRoute = errors.New("no route matched")

// ---------------------------------------------------------------------
// Metrics (dependency-free, in-process counters exposed as Prometheus
// exposition-format text on /metrics; no third-party client library).
// ---------------------------------------------------------------------

type counterKey string

// Metrics bundles simple in-process counters/gauges keyed by label tuples.
type Metrics struct {
	mu             sync.Mutex
	requestsTotal  map[counterKey]int64
	upstreamErrors map[counterKey]int64
	backendUp      map[counterKey]float64
	inFlight       map[counterKey]int64
	durationCount  map[counterKey]int64
	durationSumMs  map[counterKey]int64
}

// NewMetrics constructs a fresh, empty metrics registry.
func NewMetrics() *Metrics {
	return &Metrics{
		requestsTotal:  make(map[counterKey]int64),
		upstreamErrors: make(map[counterKey]int64),
		backendUp:      make(map[counterKey]float64),
		inFlight:       make(map[counterKey]int64),
		durationCount:  make(map[counterKey]int64),
		durationSumMs:  make(map[counterKey]int64),
	}
}

func (m *Metrics) incRequests(route, pool, method, status string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := counterKey(route + "|" + pool + "|" + method + "|" + status)
	m.requestsTotal[k]++
}

func (m *Metrics) incUpstreamErrors(pool, reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := counterKey(pool + "|" + reason)
	m.upstreamErrors[k]++
}

func (m *Metrics) setBackendUp(pool, backend string, up bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := counterKey(pool + "|" + backend)
	if up {
		m.backendUp[k] = 1
	} else {
		m.backendUp[k] = 0
	}
}

func (m *Metrics) observeDuration(route, pool, method string, d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := counterKey(route + "|" + pool + "|" + method)
	m.durationCount[k]++
	m.durationSumMs[k] += d.Milliseconds()
}

// render produces a minimal Prometheus text-exposition-format snapshot.
func (m *Metrics) render() []byte {
	m.mu.Lock()
	defer m.mu.Unlock()

	var b strings.Builder
	b.WriteString("# HELP goproxy_requests_total Total proxied requests by outcome.\n")
	b.WriteString("# TYPE goproxy_requests_total counter\n")
	for k, v := range m.requestsTotal {
		parts := strings.SplitN(string(k), "|", 4)
		fmt.Fprintf(&b, "goproxy_requests_total{route=%q,pool=%q,method=%q,status=%q} %d\n",
			parts[0], parts[1], parts[2], parts[3], v)
	}

	b.WriteString("# HELP goproxy_upstream_errors_total Count of proxy-side error outcomes.\n")
	b.WriteString("# TYPE goproxy_upstream_errors_total counter\n")
	for k, v := range m.upstreamErrors {
		parts := strings.SplitN(string(k), "|", 2)
		fmt.Fprintf(&b, "goproxy_upstream_errors_total{pool=%q,reason=%q} %d\n", parts[0], parts[1], v)
	}

	b.WriteString("# HELP goproxy_backend_up 1 if backend is healthy, 0 otherwise.\n")
	b.WriteString("# TYPE goproxy_backend_up gauge\n")
	for k, v := range m.backendUp {
		parts := strings.SplitN(string(k), "|", 2)
		fmt.Fprintf(&b, "goproxy_backend_up{pool=%q,backend=%q} %g\n", parts[0], parts[1], v)
	}

	b.WriteString("# HELP goproxy_request_duration_seconds_avg Average request latency.\n")
	b.WriteString("# TYPE goproxy_request_duration_seconds_avg gauge\n")
	for k, count := range m.durationCount {
		parts := strings.SplitN(string(k), "|", 3)
		sum := m.durationSumMs[k]
		avgSec := 0.0
		if count > 0 {
			avgSec = (float64(sum) / float64(count)) / 1000.0
		}
		fmt.Fprintf(&b, "goproxy_request_duration_seconds_avg{route=%q,pool=%q,method=%q} %g\n",
			parts[0], parts[1], parts[2], avgSec)
	}

	return []byte(b.String())
}

// ---------------------------------------------------------------------
// ProxyServer
// ---------------------------------------------------------------------

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
		Transport:      transport,
		FlushInterval:  100 * time.Millisecond,
		Director:       ps.buildDirector(),
		ModifyResponse: ps.buildModifyResponse(),
		ErrorHandler:   ps.buildErrorHandler(),
	}
	ps.proxy = rp
	return ps
}

func (ps *ProxyServer) buildDirector() func(*http.Request) {
	return func(req *http.Request) {
		start := time.Now()
		_ = start

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
				if ps.metrics != nil {
					ps.metrics.setBackendUp(state.Pool.Name, state.Backend.URL.String(), true)
					ps.metrics.incRequests(state.Route, state.Pool.Name, resp.Request.Method, itoa(resp.StatusCode))
				}
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
				if ps.metrics != nil {
					ps.metrics.setBackendUp(state.Pool.Name, state.Backend.URL.String(), false)
				}
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
			ps.metrics.incUpstreamErrors(poolName, reason)
			ps.metrics.incRequests(pathOrUnmatched(r), poolName, r.Method, itoa(status))
		}

		writeJSONError(w, status, msg)
	}
}

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

// MetricsHandler serves GET /metrics in Prometheus text exposition format.
func MetricsHandler(metrics *Metrics) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "
