// Package main implements the GoProxy reverse-proxy data plane: Router
// construction and path matching, the ProxyServer reverse-proxy handler
// (Director/ErrorHandler hooks, forwarded-header rewriting, passive
// health/metrics recording), and the Mux wiring /healthz, /metrics, and
// the proxy data plane onto a single http.Handler shared by both the
// HTTP and HTTPS listeners.
//
// Uses only the standard library "log" package (not "log/slog") to
// remain compatible with Go 1.19+ build environments, and only stdlib
// packages overall (no third-party dependencies).
package main

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Router
// ---------------------------------------------------------------------------

// routeEntry pairs a configured path-prefix match with its resolved Pool.
type routeEntry struct {
	prefix string
	pool   *Pool
}

// Router resolves an incoming request path to a backend Pool using
// longest-prefix-match semantics over the configured routes.
type Router struct {
	routes []routeEntry
}

// NewRouter builds a Router from the given route configs, resolving each
// route's pool name against the provided pools map. It fails fast if any
// route references a pool that does not exist.
func NewRouter(routes []RouteConfig, pools map[string]*Pool) (*Router, error) {
	r := &Router{routes: make([]routeEntry, 0, len(routes))}
	for _, rc := range routes {
		pool, ok := pools[rc.Pool]
		if !ok {
			return nil, fmt.Errorf("build router: route %q references unknown pool %q", rc.Match, rc.Pool)
		}
		r.routes = append(r.routes, routeEntry{prefix: rc.Match, pool: pool})
	}
	return r, nil
}

// Match returns the Pool associated with the longest configured route
// prefix that matches path, and true. If no route matches, it returns
// (nil, false).
func (r *Router) Match(path string) (*Pool, bool) {
	var best *routeEntry
	for i := range r.routes {
		entry := &r.routes[i]
		if strings.HasPrefix(path, entry.prefix) {
			if best == nil || len(entry.prefix) > len(best.prefix) {
				best = entry
			}
		}
	}
	if best == nil {
		return nil, false
	}
	return best.pool, true
}

// ---------------------------------------------------------------------------
// ProxyServer
// ---------------------------------------------------------------------------

// ProxyServer is the http.Handler implementing the reverse-proxy data
// plane: it resolves a route, selects a healthy backend via the pool's
// load-balancing strategy, forwards the request via httputil.ReverseProxy,
// and records passive health state + metrics based on the outcome.
type ProxyServer struct {
	router  *Router
	metrics *Metrics
	logger  *log.Logger
}

// NewProxyServer constructs a ProxyServer.
func NewProxyServer(router *Router, metrics *Metrics, logger *log.Logger) *ProxyServer {
	return &ProxyServer{router: router, metrics: metrics, logger: logger}
}

// ServeHTTP implements http.Handler.
func (p *ProxyServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	pool, ok := p.router.Match(r.URL.Path)
	if !ok {
		p.metrics.IncCounter("proxy_requests_failed_total", nil)
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	backend, err := pool.Choose()
	if err != nil {
		p.metrics.IncCounter("proxy_requests_failed_total", nil)
		http.Error(w, "no healthy backends available", http.StatusServiceUnavailable)
		return
	}

	backend.ActiveConns.Add(1)
	defer backend.ActiveConns.Add(-1)

	failed := false
	isTLS := r.TLS != nil

	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = backend.URL.Scheme
			req.URL.Host = backend.URL.Host
			req.Host = backend.URL.Host

			clientIP := req.RemoteAddr
			if host, _, splitErr := net.SplitHostPort(req.RemoteAddr); splitErr == nil {
				clientIP = host
			}
			req.Header.Set("X-Forwarded-For", clientIP)

			if isTLS {
				req.Header.Set("X-Forwarded-Proto", "https")
			} else {
				req.Header.Set("X-Forwarded-Proto", "http")
			}
			req.Header.Set("X-Forwarded-Host", r.Host)
		},
		ErrorHandler: func(rw http.ResponseWriter, req *http.Request, err error) {
			failed = true
			pool.MarkFailure(backend)
			p.metrics.IncCounter("proxy_requests_failed_total", nil)
			if p.logger != nil {
				p.logger.Printf("ERROR proxy upstream error: pool=%s backend=%s err=%v", pool.Name, backend.URL.Host, err)
			}
			http.Error(rw, "bad gateway", http.StatusBadGateway)
		},
	}

	proxy.ServeHTTP(w, r)

	if !failed {
		pool.MarkSuccess(backend)
		p.metrics.IncCounter("proxy_requests_total", nil)
	}
	p.metrics.SetGauge("proxy_last_request_duration_seconds", nil, time.Since(start).Seconds())
}

// ---------------------------------------------------------------------------
// healthCheckerAdapter
// ---------------------------------------------------------------------------

// healthCheckerAdapter answers the top-level /healthz liveness question:
// healthy if at least one backend across all pools is Alive, or if there
// are no pools configured at all (vacuously healthy).
type healthCheckerAdapter struct {
	pools []*Pool
}

// newHealthCheckerAdapter constructs a healthCheckerAdapter over the given
// pools.
func newHealthCheckerAdapter(pools []*Pool) *healthCheckerAdapter {
	return &healthCheckerAdapter{pools: pools}
}

// Healthy reports whether the process should be considered live/ready.
func (h *healthCheckerAdapter) Healthy() bool {
	if len(h.pools) == 0 {
		return true
	}
	for _, pool := range h.pools {
		for _, b := range pool.Backends {
			if b.Alive.Load() {
				return true
			}
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Mux
// ---------------------------------------------------------------------------

// NewMux builds the shared http.Handler serving /healthz, /metrics, and the
// proxy data plane (everything else), used by both the HTTP and HTTPS
// listeners in main.go.
func NewMux(proxyServer *ProxyServer, metrics *Metrics, hc *healthCheckerAdapter, logger *log.Logger) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		if hc.Healthy() {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok"))
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("unavailable"))
	})

	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		var sb strings.Builder
		metrics.WriteTo(&sb)
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(sb.String()))
	})

	mux.Handle("/", proxyServer)

	return mux
}
