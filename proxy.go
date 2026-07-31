// request routing/handling. This file does not own config parsing,
// load-balancing algorithm internals, or health-check scheduling.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"sort"
	"strconv"
	"strings"
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
// ProxyServer
// ---------------------------------------------------------------------

type ProxyServer struct {
	router  *Router
	metrics *Metrics
	logger  *Logger
	proxy   *httputil.ReverseProxy
}

// NewProxyServer constructs a ProxyServer with hardened transport, header
// sanitation, and hooked Director/ModifyResponse/ErrorHandler.
func NewProxyServer(router *Router, metrics *Metrics, logger *Logger) *ProxyServer {
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
					ps.metrics.SetGauge("goproxy_backend_up",
						map[string]string{"pool": state.Pool.Name, "backend": state.Backend.URL.String()}, 1)
					ps.metrics.IncCounter("goproxy_requests_total",
						map[string]string{
							"route":  state.Route,
							"pool":   state.Pool.Name,
							"method": resp.Request.Method,
							"status": strconv.Itoa(resp.StatusCode),
						})
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
					ps.metrics.SetGauge("goproxy_backend_up",
						map[string]string{"pool": state.Pool.Name, "backend": state.Backend.URL.String()}, 0)
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
			ps.metrics.IncCounter("goproxy_upstream_errors_total", map[string]string{"pool": poolName, "reason": reason})
			ps.metrics.IncCounter("goproxy_requests_total", map[string]string{
				"route":  pathOrUnmatched(r),
				"pool":   poolName,
				"method": r.Method,
				"status": strconv.Itoa(status),
			})
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
// HTTP handlers: /healthz, /metrics
// ---------------------------------------------------------------------

type healthChecker interface {
	AnyPoolHealthy() bool
}

// loggingMiddleware wraps h, emitting one structured log line per request
// (method, path, status, duration) with level escalation on 4xx/5xx.
func loggingMiddleware(logger *Logger, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusCapturingWriter{ResponseWriter: w, status: http.StatusOK}
		h.ServeHTTP(sw, r)
		duration := time.Since(start)

		if logger == nil {
			return
		}

		fields := []interface{}{
			"method", r.Method,
			"path", r.URL.Path,
			"status", sw.status,
			"duration_ms", duration.Milliseconds(),
			"remote_addr", r.RemoteAddr,
		}

		switch {
		case sw.status >= 500:
			logger.Error("proxy_request", fields...)
		case sw.status >= 400:
			logger.Warn("proxy_request", fields...)
		default:
			logger.Info("proxy_request", fields...)
		}
	})
}

type statusCapturingWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusCapturingWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// NewMux builds the top-level http.Handler wiring /healthz, /metrics, and
// the catch-all reverse proxy, with logging applied uniformly.
func NewMux(ps *ProxyServer, metrics *Metrics, hc healthChecker, logger *Logger) http.Handler {
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
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		var sb strings.Builder
		if metrics != nil {
			metrics.WriteTo(&sb)
		}
		_, _ = w.Write([]byte(sb.String()))
	})
}
```

### Summary of root-cause fixes

1. **`log/slog` removal**: All references to `log/slog` replaced with the dependency-free `Logger` type (defined once in `logger.go`, used consistently in `main.go` and `proxy.go`). This makes the code compile on Go 1.19, matching the actual toolchain in the sandbox (`/usr/lib/go-1.19`).
2. **Complete, valid files**: `main.go`, `config.go`, and `proxy.go` were previously truncated (missing `package main`, dangling fragments referencing types from other files without proper structure). All three are now fully self-contained with correct package declarations, complete imports, and no duplicated symbols (removed the duplicate `Metrics`/`healthCheckerAdapter` definitions that existed across `proxy.go`/`config.go`/`main.go` in the corrupted state).
3. **`go.mod`/`go.sum` minimal and clean**: Exactly two lines in `go.mod` (`module`, `go 1.19`), and a genuinely empty `go.sum` — eliminating checksum mismatches and parse errors since there are zero external dependencies anywhere in the code.
4. **Removed duplicate `Metrics` type**: `metrics.go` already defines a dependency-free `Metrics`; `proxy.go` now uses that type instead of redefining its own conflicting `Metrics` struct.
