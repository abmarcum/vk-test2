// selection), reverse proxy plumbing, header injection/sanitization,
// Prometheus metrics registration & recording, structured request logging,
// and the /healthz and /metrics HTTP handlers.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// ---------------------------------------------------------------------------
// Router
// ---------------------------------------------------------------------------

type compiledRoute struct {
	prefix string
	pool   *Pool
}

// Router maps path prefixes to Pools using longest-prefix-match.
type Router struct {
	routes []compiledRoute
}

// NewRouter builds a Router from RouteConfig list + resolved Pool map.
func NewRouter(routes []RouteConfig, pools map[string]*Pool) (*Router, error) {
	compiled := make([]compiledRoute, 0, len(routes))
	for _, rc := range routes {
		pool, ok := pools[rc.Pool]
		if !ok {
			return nil, fmt.Errorf("route %q references unknown pool %q", rc.PathPrefix, rc.Pool)
		}
		compiled = append(compiled, compiledRoute{prefix: rc.PathPrefix, pool: pool})
	}
	sort.SliceStable(compiled, func(i, j int) bool {
		return len(compiled[i].prefix) > len(compiled[j].prefix)
	})
	return &Router{routes: compiled}, nil
}

// Match returns the Pool responsible for a given request path, along with
// the matched prefix, using longest-prefix-match semantics.
func (r *Router) Match(path string) (*Pool, string, bool) {
	for _, rt := range r.routes {
		if strings.HasPrefix(path, rt.prefix) {
			return rt.pool, rt.prefix, true
		}
	}
	return nil, "", false
}

// ---------------------------------------------------------------------------
// Metrics
// ---------------------------------------------------------------------------

// Metrics holds all Prometheus collectors, registered on a dedicated
// registry (not the global default registry).
type Metrics struct {
	Registry *prometheus.Registry

	RequestsTotal    *prometheus.CounterVec
	RequestDuration  *prometheus.HistogramVec
	InFlight         *prometheus.GaugeVec
	UpstreamErrors   *prometheus.CounterVec
	BackendUp        *prometheus.GaugeVec
	ResponseSize     *prometheus.HistogramVec
}

// NewMetrics constructs and registers all collectors exactly once.
func NewMetrics() *Metrics {
	reg := prometheus.NewRegistry()

	m := &Metrics{
		Registry: reg,
		RequestsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "goproxy_requests_total",
			Help: "Total proxied requests, by outcome.",
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

	reg.MustRegister(
		m.RequestsTotal,
		m.RequestDuration,
		m.InFlight,
		m.UpstreamErrors,
		m.BackendUp,
		m.ResponseSize,
	)

	return m
}

// ---------------------------------------------------------------------------
// Request-scoped proxy state
// ---------------------------------------------------------------------------

type proxyContextKey struct{}

type proxyRequestState struct {
	Pool    *Pool
	Backend *Backend
	Route   string
	Err     error
	written atomic.Bool
}

func withProxyState(r *http.Request, state *proxyRequestState) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), proxyContextKey{}, state))
}

func proxyStateFromContext(ctx context.Context) (*proxyRequestState, bool) {
	v, ok := ctx.Value(proxyContextKey{}).(*proxyRequestState)
	return v, ok
}

// ---------------------------------------------------------------------------
// ProxyServer
// ---------------------------------------------------------------------------

// ProxyServer wires a Router into a single http.Handler backed by
// httputil.ReverseProxy, with Director/ModifyResponse/ErrorHandler hooks
// that read/write request-scoped state via context.
type ProxyServer struct {
	router  *Router
	metrics *Metrics
	logger  *slog.Logger
	rp      *httputil.ReverseProxy
}

// NewProxyServer constructs a ProxyServer wrapping exactly one
// httputil.ReverseProxy instance.
func NewProxyServer(router *Router, metrics *Metrics, logger *slog.Logger) *ProxyServer {
	ps := &ProxyServer{router: router,
