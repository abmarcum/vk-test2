// httputil.ReverseProxy Director/ModifyResponse/ErrorHandler hooks, header
// sanitization, request-scoped proxy state propagation, Prometheus metrics,
// structured request logging, and the /healthz and /metrics HTTP handlers.
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

// ---------------------------------------------------------------------------
// Router
// ---------------------------------------------------------------------------

// compiledRoute is a single path-prefix -> pool mapping, pre-resolved to a
// *Pool pointer for O(1) dispatch after matching.
type compiledRoute struct {
	prefix string
	pool   *Pool
}

// Router maps path prefixes to Pools using longest-prefix-match semantics,
// independent of declaration order in the config.
type Router struct {
	routes []compiledRoute
}

// NewRouter builds a Router from RouteConfig list + resolved Pool map,
// sorting routes by descending prefix length once at construction time.
func NewRouter(routes []RouteConfig, pools map[string]*Pool) (*Router, error) {
	compiled := make([]compiledRoute, 0, len(routes))
	for _, r := range routes {
		pool, ok := pools[r.Pool]
		if !ok {
			return nil, errors.New("route references undefined pool: " + r.Pool)
		}
		compiled = append(compiled, compiledRoute{prefix: r.PathPrefix, pool: pool})
	}
	sort.SliceStable(compiled, func(i, j int) bool {
		return len(compiled[i].prefix) > len(compiled[j].prefix)
	})
	return &Router{routes: compiled}, nil
}

// Match returns the Pool and matched prefix responsible for a given request
// path, using longest-prefix-match. Returns ok=false if nothing matches.
func (r *Router) Match(path string) (pool *Pool, prefix string, ok bool) {
	for _, rt := range r.routes {
		if strings.HasPrefix(path, rt.prefix) {
			return rt.pool, rt.prefix, true
		}
	}
	return nil, "", false
}

// ---------------------------------------------------------------------------
// Request-scoped proxy state
// ---------------------------------------------------------------------------

type proxyContextKey struct{}

// proxyRequestState is stashed into the request context by the Director and
// retrieved by ModifyResponse/ErrorHandler/logging/metrics.
type proxyRequestState struct {
	Pool    *Pool
	Backend *Backend
	Route   string
	Err     error

	mu        sync.Mutex
	startTime time.Time
	status    int
	bytesOut  int64
}

func withProxyState(r *http.Request, state *proxyRequestState) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), proxyContextKey{}, state))
}

func proxyStateFromContext(ctx context.Context) (*proxyRequestState, bool) {
	s, ok := ctx.Value(proxyContextKey{}).(*proxyRequestState)
	return s, ok
}

// ---------------------------------------------------------------------------
// Metrics
// ---------------------------------------------------------------------------

// Metrics holds all Prometheus collectors registered on a dedicated
// registry (not the global default registry).
type Metrics struct {
	Registry *prometheus.Registry

	RequestsTotal    *prometheus.CounterVec
	RequestDuration  *prometheus.HistogramVec
	InFlightRequests *prometheus.GaugeVec
	UpstreamErrors   *prometheus.CounterVec
	BackendUp        *prometheus.GaugeVec
	ResponseSize     *prometheus.HistogramVec
}

// NewMetrics constructs and registers all metrics on a fresh registry.
func NewMetrics() *Metrics {
	reg := prometheus.NewRegistry()

	m := &Metrics{
		Registry: reg,
		RequestsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "goproxy_requests_total",
			Help: "Total proxied requests by route, pool, method, and status.",
		}, []string{"route", "pool", "method", "status"}),
		RequestDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "goproxy_request_duration_seconds",
			Help:    "End-to-end proxied request latency.",
			Buckets: []float64{.001, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5},
		}, []string{"route", "pool", "method"}),
		InFlightRequests: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "goproxy_in_flight_requests",
			Help: "Requests currently being proxied to a given pool.",
		}, []string{"pool"}),
		UpstreamErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "goproxy_upstream_errors_total",
			Help: "Count of proxy-side error outcomes by pool and reason.",
		}, []string{"pool", "reason"}),
		BackendUp: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "goproxy_backend_up",
			Help: "1 if backend is currently healthy, 0 otherwise.",
		}, []string{"pool", "backend"}),
		ResponseSize: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "goproxy_response_size_bytes",
			Help:    "Response body size written to the client.",
			Buckets: prometheus.ExponentialBuckets(128, 4, 8),
		}, []string{"route", "pool"}),
	}

	reg.MustRegister(
		m.RequestsTotal,
		m.RequestDuration,
		m.InFlightRequests,
		m.UpstreamErrors,
		m.BackendUp,
		m.ResponseSize,
	)

	return m
}

// ---------------------------------------------------------------------------
// ProxyServer
// ---------------------------------------------------------------------------

// ProxyServer wires a Router into an httputil.ReverseProxy, handling
// backend selection, header sanitization, and error translation.
type ProxyServer struct {
	router  *Router
	metrics *Metrics
	logger  *slog.Logger
	proxy   *httputil.ReverseProxy
}

// hopByHopHeaders lists headers that must never be forwarded end-to-end,
// per RFC 7230.
var hopByHopHeaders = []string{
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

// NewProxyServer constructs a ProxyServer with a single internally-built
// httputil.ReverseProxy, using request-context propagation for
// Director -> ModifyResponse/ErrorHandler communication.
func NewProxyServer(router *Router, metrics *Metrics, logger *slog.Logger) *ProxyServer {
	ps := &ProxyServer{router: router, metrics: metrics, logger: logger}

	transport := &http.Transport{
		Proxy: nil, // never honor HTTP_PROXY/HTTPS_PROXY env vars
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
		Director:       ps.director,
		ModifyResponse: ps.modifyResponse,
		ErrorHandler:   ps.errorHandler,
		Transport:      transport,
		FlushInterval:  100 * time.Millisecond,
	}
	ps.proxy = rp
	return ps
}

// ServeHTTP attaches request-scoped state to the context and delegates to
// the internal httputil.ReverseProxy.
func (ps *ProxyServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ps.proxy.ServeHTTP(w, r)
}

// director resolves the route/pool/backend for a request, rewrites the
// request for upstream dispatch, and stashes proxyRequestState in context.
func (ps *ProxyServer) director(req *http.Request) {
	path := req.URL.Path
	pool, prefix, ok := ps.router.Match(path)
	if !ok {
		state := &proxyRequestState{Pool: nil, Route: "unmatched", Err: errNoRouteMatched, startTime: time.Now()}
		*req = *withProxyState(req, state)
		req.URL.Scheme = "http"
		req.URL.Host = ""
		return
	}

	backend, err := pool.NextBackend()
	if err != nil {
		state := &proxyRequestState{Pool: pool, Route: prefix, Err: err, startTime: time.Now()}
		*req = *withProxyState(req, state)
		// Force the transport to fail fast so ErrorHandler (never
		// ModifyResponse) is invoked next.
		req.URL.Scheme = "http"
		req.URL.Host = ""
		return
	}

	release := backend.AcquireConn()
	state := &proxyRequestState{Pool: pool, Backend: backend, Route: prefix, startTime: time.Now()}
	*req = *withProxyState(req, state)

	// Stash the release func via a second context value so ModifyResponse/
	// ErrorHandler can release exactly once.
	ctx := context.WithValue(req.Context(), releaseFuncKey{}, release)
	*req = *req.WithContext(ctx)

	stripHopByHopHeaders(req.Header)

	req.URL.Scheme = backend.URL.Scheme
	req.URL.Host = backend.URL.Host
	req.Host = backend.URL.Host

	setForwardedHeaders(req)
	req.Header.Set("X-Forwarded-For-Pool", pool.Name)
}

type releaseFuncKey struct{}

var errNoRouteMatched = errors.New("no route matched")

// modifyResponse finalizes accounting for a successful round trip: releases
// the connection slot, marks the backend as passively successful, sanitizes
// response headers, and records metrics.
func (ps *ProxyServer) modifyResponse(resp *http.Response) error {
	req := resp.Request
	state, ok := proxyStateFromContext(req.Context())
	if !ok || state.Backend == nil {
		return nil
	}

	releaseConn(req)

	state.Pool.Strategy.Name() // no-op touch to keep interface usage explicit
	state.Backend.MarkSuccess(state.Pool.PassiveRiseThreshold, ps.logger)

	stripHopByHopHeaders(resp.Header)
	resp.Header.Del("Server")
	resp.Header.Del("X-Powered-By")
	resp.Header.Set("X-Content-Type-Options", "nosniff")

	state.mu.Lock()
	state.status = resp.StatusCode
	state.bytesOut = resp.ContentLength
	state.mu.Unlock()

	ps.recordMetrics(state, req.Method, resp.StatusCode, resp.ContentLength)

	return nil
}

// errorHandler translates upstream/transport errors into opaque, safe
// client responses, never leaking internal error detail.
func (ps *ProxyServer) errorHandler(w http.ResponseWriter, r *http.Request, err error) {
	state, ok := proxyStateFromContext(r.Context())

	status := http.StatusBadGateway
	reason := "bad_gateway"
	poolName := "none"
	routeName := "unmatched"

	switch {
	case ok && errors.Is(state.Err, errNoRouteMatched):
		status = http.StatusNotFound
		reason = "no_route"
		routeName = state.Route

	case ok && errors.Is(state.Err, ErrNoHealthyBackends):
		status = http.StatusServiceUnavailable
		reason = "no_healthy_backend"
		if state.Pool != nil {
			poolName = state.Pool.Name
		}
		routeName = state.Route

	case ok && state.Backend != nil:
		releaseConn(r)
		poolName = state.Pool.Name
		routeName = state.Route
		if errors.Is(err, context.DeadlineExceeded) {
			status = http.StatusGatewayTimeout
			reason = "timeout"
		} else if isClientDisconnect(r, err) {
			status = 499
			reason = "client_closed"
		} else {
			status = http.StatusBadGateway
			reason = "bad_gateway"
		}
		state.Backend.MarkFailure(state.Pool.PassiveFailThreshold, ps.logger, err)

	default:
		ps.logger.Warn("proxy_error_no_state", "error", err.Error())
	}

	if ps.metrics != nil {
		ps.metrics.UpstreamErrors.WithLabelValues(poolName, reason).Inc()
		ps.metrics.RequestsTotal.WithLabelValues(routeName, poolName, r.Method, statusLabel(status)).Inc()
	}

	if state != nil {
		state.mu.Lock()
		state.status = status
		state.mu.Unlock()
	}

	writeJSONError(w, status, errorMessageFor(status))
}

// recordMetrics records the standard request-outcome metrics after a known
// status code is available.
func (ps *ProxyServer) recordMetrics(state *proxyRequestState, method string, status int, size int64) {
	if ps.metrics == nil {
		return
	}
	poolName := "none"
	if state.Pool != nil {
		poolName = state.Pool.Name
	}
	ps.metrics.RequestsTotal.WithLabelValues(state.Route, poolName, method, statusLabel(status)).Inc()
	if size > 0 {
		ps.metrics.ResponseSize.WithLabelValues(state.Route, poolName).Observe(float64(size))
	}
}

func releaseConn(r *http.Request) {
	if v := r.Context().Value(releaseFuncKey{}); v != nil {
		if release, ok := v.(func()); ok {
			release()
		}
	}
}

func statusLabel(status int) string {
	switch status {
	case 200:
		return "200"
	default:
		return httpStatusText(status)
	}
}

func httpStatusText(status int) string {
	return http.StatusText(status)
}

func isClientDisconnect(r *http.Request, err error) bool {
	select {
	case <-r.Context().Done():
		return errors.Is(r.Context().Err(), context.Canceled)
	default:
		return false
	}
}
