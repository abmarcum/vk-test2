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

// routeEntry associates a path-prefix match with a resolved backend Pool.
type routeEntry struct {
	prefix string
	pool   *Pool
}

// Router resolves an inbound request path to a backend Pool using
// longest-prefix-match semantics over the configured routes.
type Router struct {
	routes []routeEntry
}

// NewRouter builds a Router from route configs, resolving each route's pool
// name against the supplied pools map. It fails fast if a route references
// an unknown pool.
func NewRouter(routes []RouteConfig, pools map[string]*Pool) (*Router, error) {
	r := &Router{}
	for _, rc := range routes {
		pool, ok := pools[rc.Pool]
		if !ok {
			return nil, fmt.Errorf("build router: route %q references unknown pool %q", rc.Match, rc.Pool)
		}
		r.routes = append(r.routes, routeEntry{prefix: rc.Match, pool: pool})
	}
	return r, nil
}

// Match returns the Pool for the longest configured route prefix matching
// path, or nil if no route matches.
func (r *Router) Match(path string) *Pool {
	var best *routeEntry
	for i := range r.routes {
		e := &r.routes[i]
		if strings.HasPrefix(path, e.prefix) {
			if best == nil || len(e.prefix) > len(best.prefix) {
				best = e
			}
		}
	}
	if best == nil {
		return nil
	}
	return best.pool
}

// ---------------------------------------------------------------------------
// ProxyServer
// ---------------------------------------------------------------------------

// ProxyServer is the http.Handler implementing the reverse-proxy data
// plane: route resolution, backend selection, request forwarding via
// httputil.ReverseProxy, and passive health/metrics recording.
type ProxyServer struct {
	router  *Router
	metrics *Metrics
	logger  *log.Logger
}

// NewProxyServer constructs a ProxyServer.
func NewProxyServer(router *Router, metrics *Metrics, logger *log.Logger) *ProxyServer {
	return &ProxyServer{router: router, metrics: metrics, logger: logger}
}

func (p *ProxyServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	pool := p.router.Match(r.URL.Path)
	if pool == nil {
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

	isTLS := r.TLS != nil
	origHost := r.Host

	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = backend.URL.Scheme
			req.URL.Host = backend.URL.Host
			req.Host = backend.URL.Host

			clientIP, _, err := net.SplitHostPort(r.RemoteAddr)
			if err != nil {
				clientIP = r.RemoteAddr
			}
			req.Header.Set("X-Forwarded-For", clientIP)
			if isTLS {
				req.Header.Set("X-Forwarded-Proto", "https")
			} else {
				req.Header.Set("X-Forwarded-Proto", "http")
			}
			req.Header.Set("X-Forwarded-Host", origHost)
		},
		ModifyResponse: func(resp *http.Response) error {
			pool.MarkSuccess(backend)
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			pool.MarkFailure(backend)
			p.metrics.IncCounter("proxy_requests_failed_total", nil)
			if p.logger != nil {
				p.logger.Printf("ERROR proxy upstream error backend=%s err=%v", backend.URL, err)
			}
			http.Error(w, "bad gateway", http.StatusBadGateway)
		},
	}

	proxy.ServeHTTP(w, r)

	p.metrics.IncCounter("proxy_requests_total", nil)
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

// Healthy reports overall liveness per the /healthz semantics described in
// docs/api.md.
func (h *healthCheckerAdapter) Healthy() bool {
	if len(h.pools) == 0 {
		return true
	}
	for _, p := range h.pools {
		for _, b := range p.Backends {
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

// NewMux builds the shared http.Handler wiring /healthz, /metrics, and the
// proxy data plane (all other paths), used by both the HTTP and HTTPS
// listeners.
func NewMux(proxy *ProxyServer, metrics *Metrics, hc *healthCheckerAdapter, logger *log.Logger) http.Handler {
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
		_, _ = w.Write([]byte(sb.String()))
	})

	mux.Handle("/", proxy)

	return mux
}
