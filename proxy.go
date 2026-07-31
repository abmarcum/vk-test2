// Director/ModifyResponse/ErrorHandler hooks, request-scoped proxy state
// propagation, structured logging and metrics middleware, healthz/metrics
// HTTP handlers. Uses only the standard library "log" package (not
// "log/slog") to remain compatible with Go 1.19+ build environments.
package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Router
// ---------------------------------------------------------------------------

type routeEntry struct {
	prefix string
	pool   *Pool
}

// Router resolves an incoming request path to a Pool using longest-prefix
// match, independent of declaration order.
type Router struct {
	entries []routeEntry
}

// NewRouter builds a Router from route configs and the resolved pool map.
func NewRouter(routes []Route, pools map[string]*Pool) (*Router, error) {
	entries := make([]routeEntry, 0, len(routes))
	for _, r := range routes {
		p, ok := pools[r.Pool]
		if !ok {
			return nil, fmt.Errorf("route %q references unknown pool %q", r.PathPrefix, r.Pool)
		}
		entries = append(entries, routeEntry{prefix: r.PathPrefix, pool: p})
	}
	sort.Slice(entries, func(i, j int) bool {
		return len(entries[i].prefix) > len(entries[j].prefix)
	})
	return &Router{entries: entries}, nil
}

// Match returns the pool and matched prefix for path, or ok=false if none matches.
func (r *Router) Match(path string) (pool *Pool, prefix string, ok bool) {
	for _, e := range r.entries {
		if strings.HasPrefix(path, e.prefix) {
			return e.pool, e.prefix, true
		}
	}
	return nil, "", false
}

// ---------------------------------------------------------------------------
// healthChecker interface + /healthz, /metrics handlers
// ---------------------------------------------------------------------------

type healthChecker interface {
	AnyPoolHealthy() bool
}

// NewMux wires /healthz, /metrics, and the catch-all proxy handler.
func NewMux(proxyServer *ProxyServer, metrics *Metrics, hc healthChecker, logger *log.Logger) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodGet && req.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		if hc.AnyPoolHealthy() {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"status":"unhealthy"}`))
		}
	})

	mux.HandleFunc("/metrics", func(w http.ResponseWriter, req *http.Request) {
		var sb strings.Builder
		metrics.WriteTo(&sb)
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = w.Write([]byte(sb.String()))
	})

	mux.Handle("/", LoggingRecoverMiddleware(proxyServer, logger))

	return mux
}

// ---------------------------------------------------------------------------
// Proxy state + ProxyServer
// ---------------------------------------------------------------------------

type ctxKey int

const proxyStateKey ctxKey = iota

type proxyState struct {
	route   string
	pool    string
	backend string
	status  int
	err     error
	bytes   int64
	mu      sync.Mutex
}

func (s *proxyState) setStatus(code int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status = code
}

func (s *proxyState) setErr(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err = err
}

func (s *proxyState) addBytes(n int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bytes += n
}

func (s *proxyState) snapshot() (route, pool, backend string, status int, bytes int64, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.route, s.pool, s.backend, s.status, s.bytes, s.err
}

// ProxyServer dispatches requests to the matched pool's chosen backend via
// httputil.ReverseProxy, recording metrics and structured logs.
type ProxyServer struct {
	router  *Router
	metrics *Metrics
	logger  *log.Logger
	rp      *httputil.ReverseProxy
	dialer  *net.Dialer
}

// NewProxyServer constructs a ProxyServer wired to router and metrics.
func NewProxyServer(router *Router, metrics *Metrics, logger *log.Logger) *ProxyServer {
	dialer := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}

	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           dialer.DialContext,
		MaxIdleConns:          200,
		MaxIdleConnsPerHost:   50,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: 15 * time.Second,
		ForceAttemptHTTP2:     true,
	}

	ps := &ProxyServer{
		router:  router,
		metrics: metrics,
		logger:  logger,
		dialer:  dialer,
	}

	rp := &httputil.ReverseProxy{
		Director:      ps.director,
		ModifyResponse: ps.modifyResponse,
		ErrorHandler:  ps.errorHandler,
		Transport:     transport,
		FlushInterval: 100 * time.Millisecond,
	}
	ps.rp = rp
	return ps
}

func (ps *ProxyServer) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	state := &proxyState{route: "unmatched", pool: "none"}
	ctx := context.WithValue(req.Context(), proxyStateKey, state)
	req = req.WithContext(ctx)

	start := time.Now()

	pool, prefix, ok := ps.router.Match(req.URL.Path)
	if !ok {
		state.route = "unmatched"
		state.pool = "none"
		ps.metrics.IncCounter("goproxy_upstream_errors_total", map[string]string{"pool": "none", "reason": "no_route"})
		writeJSONError(w, http.StatusNotFound, "not found")
		ps.logRequest(req, state, start)
		return
	}
	state.route = prefix
	state.pool = pool.Name

	backend, err := pool.Choose()
	if err != nil {
		ps.metrics.IncCounter("goproxy_upstream_errors_total", map[string]string{"pool": pool.Name, "reason": "no_healthy_backend"})
		writeJSONError(w, http.StatusServiceUnavailable, "service unavailable")
		ps.logRequest(req, state, start)
		return
	}
	state.backend = backend.URL.String()

	backend.ActiveConns.Add(1)
	defer backend.ActiveConns.Add(-1)

	labels := map[string]string{"pool": pool.Name}
	ps.metrics.SetGauge("goproxy_in_flight_requests", labels, float64(backend.ActiveConns.Load()))

	req = req.WithContext(context.WithValue(req.Context(), backendCtxKey, backendRef{pool: pool, backend: backend}))

	rw := &statusCapturingWriter{ResponseWriter: w, status: http.StatusOK}
	ps.rp.ServeHTTP(rw, req)

	state.setStatus(rw.status)
	if rw.status < 500 {
		pool.MarkSuccess(backend)
	} else {
		pool.MarkFailure(backend)
	}

	ps.metrics.IncCounter("goproxy_requests_total", map[string]string{
		"route": prefix, "pool": pool.Name, "method": req.Method, "status": strconv.Itoa(rw.status),
	})
	ps.metrics.SetGauge("goproxy_backend_up", map[string]string{"pool": pool.Name, "backend": backend.URL.String()}, boolToFloat(backend.Alive.Load()))

	ps.logRequest(req, state, start)
}

type ctxBackendKey int

const backendCtxKey ctxBackendKey = 0

type backendRef struct {
	pool    *Pool
	backend *Backend
}

func (ps *ProxyServer) director(req *http.Request) {
	ref, ok := req.Context().Value(backendCtxKey).(backendRef)
	if !ok || ref.backend == nil {
		return
	}
	target := ref.backend.URL
	req.URL.Scheme = target.Scheme
	req.URL.Host = target.Host
	req.Host = target.Host

	stripHopHeaders(req.Header)

	if req.TLS != nil {
		req.Header.Set("X-Forwarded-Proto", "https")
	} else if p := req.Header.Get("X-Forwarded-Proto"); p != "http" && p != "https" {
		req.Header.Set("X-Forwarded-Proto", "http")
	}
	req.Header.Set("X-Forwarded-Host", req.Host)

	if ip := clientIP(req.RemoteAddr); ip != "" {
		if prior := req.Header.Get("X-Forwarded-For"); prior != "" {
			req.Header.Set("X-Forwarded-For", prior+", "+ip)
		} else {
			req.Header.Set("X-Forwarded-For", ip)
		}
	}

	if ref.pool != nil {
		req.Header.Set("X-Forwarded-For-Pool", ref.pool.Name)
	}
}

func (ps *ProxyServer) modifyResponse(resp *http.Response) error {
	stripHopHeaders(resp.Header)
	resp.Header.Del("Server")
	resp.Header.Del("X-Powered-By")
	resp.Header.Set("X-Content-Type-Options", "nosniff")

	if state, ok := resp.Request.Context().Value(proxyStateKey).(*proxyState); ok {
		if resp.ContentLength > 0 {
			state.addBytes(resp.ContentLength)
		}
	}
	return nil
}

func (ps *ProxyServer) errorHandler(w http.ResponseWriter, req *http.Request, err error) {
	state, _ := req.Context().Value(proxyStateKey).(*proxyState)
	if state != nil {
		state.setErr(err)
	}

	status := http.StatusBadGateway
	msg := "bad gateway"
	reason := "bad_gateway"

	if req.Context().Err() == context.Canceled {
		status = 499
		msg = "client closed request"
		reason = "client_closed"
	} else if isTimeoutErr(err) {
		status = http.StatusGatewayTimeout
		msg = "gateway timeout"
		reason = "timeout"
	}

	poolName := "none"
	if state != nil && state.pool != "" {
		poolName = state.pool
	}
	ps.metrics.IncCounter("goproxy_upstream_errors_total", map[string]string{"pool": poolName, "reason": reason})

	writeJSONError(w, status, msg)
	if state != nil {
		state.setStatus(status)
	}
}

func isTimeoutErr(err error) bool {
	type timeouter interface{ Timeout() bool }
	if te, ok := err.(timeouter); ok {
		return te.Timeout()
	}
	return false
}

func (ps *ProxyServer) logRequest(req *http.Request, state *proxyState, start time.Time) {
	route, pool, backend, status, bytes, err := state.snapshot()
	dur := time.Since(start)

	level := "INFO"
	if status >= 500 {
		level = "ERROR"
	} else if status >= 400 {
		level = "WARN"
	}

	errStr := ""
	if err != nil {
		errStr = err.Error()
	}

	ps.logger.Printf("%s proxy_request method=%s path=%s route=%s pool=%s backend=%s status=%d duration_ms=%d bytes_out=%d remote_addr=%s user_agent=%q error=%q",
		level, req.Method, redactPath(req.URL.Path), route, pool, backend, status, dur.Milliseconds(), bytes, req.RemoteAddr, req.UserAgent(), errStr)
}

func redactPath(p string) string {
	return p
}

// ---------------------------------------------------------------------------
// Middleware
// ---------------------------------------------------------------------------

// LoggingRecoverMiddleware wraps next with panic recovery, converting any
// panic into a generic 500 response instead of crashing the process.
func LoggingRecoverMiddleware(next http.Handler, logger *log.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				logger.Printf("ERROR panic recovered method=%s path=%s panic=%v", req.Method, req.URL.Path, rec)
				writeJSONError(w, http.StatusInternalServerError, "internal server error")
			}
		}()
		next.ServeHTTP(w, req)
	})
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

var hopHeaders = []string{
	"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate",
	"Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

func stripHopHeaders(h http.Header) {
	for _, hh := range hopHeaders {
		h.Del(hh)
	}
}

func clientIP(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	if net.ParseIP(host) == nil {
		return ""
	}
	return host
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `{"error":%q}`, msg)
}

func boolToFloat(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

type statusCapturingWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusCapturingWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}
