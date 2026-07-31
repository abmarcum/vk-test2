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
	"strings"
	"time"
)

// compiledRoute pairs a path prefix with its resolved pool.
type compiledRoute struct {
	prefix string
	pool   *Pool
}

// Router maps path prefixes to Pools using longest-prefix-match semantics.
type Router struct {
	routes []compiledRoute
}

// NewRouter builds a Router from RouteConfig entries and a resolved pool
// map, sorting routes by descending prefix length so the most specific
// route always wins regardless of declaration order.
func NewRouter(routes []RouteConfig, pools map[string]*Pool) (*Router, error) {
	r := &Router{}
	for _, rc := range routes {
		pool, ok := pools[rc.Pool]
		if !ok {
			return nil, fmt.Errorf("route %q references undefined pool %q", rc.PathPrefix, rc.Pool)
		}
		r.routes = append(r.routes, compiledRoute{prefix: rc.PathPrefix, pool: pool})
	}
	sort.SliceStable(r.routes, func(i, j int) bool {
		return len(r.routes[i].prefix) > len(r.routes[j].prefix)
	})
	return r, nil
}

// Match returns the Pool and matched prefix responsible for a request path,
// or ok=false if no configured route matches.
func (r *Router) Match(path string) (pool *Pool, prefix string, ok bool) {
	for _, rt := range r.routes {
		if strings.HasPrefix(path, rt.prefix) {
			return rt.pool, rt.prefix, true
		}
	}
	return nil, "", false
}

// proxyContextKey is the unexported context key type for request-scoped
// proxy state.
type proxyContextKey struct{}

// proxyRequestState is stashed into the request context by the Director
// and retrieved by ModifyResponse/ErrorHandler/logging middleware.
type proxyRequestState struct {
	Pool    *Pool
	Backend *Backend
	Route   string
	Err     error

	startTime time.Time
	written   bool
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

// hopHeaders lists RFC 7230 hop-by-hop headers stripped in both directions.
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

func stripHopHeaders(h http.Header) {
	for _, hh := range hopHeaders {
		h.Del(hh)
	}
}

// ProxyServer wires a Router into an httputil.ReverseProxy with hardened
// header handling, request-scoped state propagation, metrics, and logging.
type ProxyServer struct {
	router  *Router
	metrics *Metrics
	logger  *slog.Logger
	rp      *httputil.ReverseProxy
}

// NewProxyServer constructs a ProxyServer wired to the given router.
func NewProxyServer(router *Router, metrics *Metrics, logger *slog.Logger) *ProxyServer {
	ps := &ProxyServer{
		router:  router,
		metrics: metrics,
		logger:  logger,
	}

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
		Director:       ps.buildDirector(),
		ModifyResponse: ps.buildModifyResponse(),
		ErrorHandler:   ps.buildErrorHandler(),
		Transport:      transport,
		FlushInterval:  100 * time.Millisecond,
	}
	ps.rp = rp
	return ps
}

func (ps *ProxyServer) buildDirector() func(*http.Request) {
	return func(req *http.Request) {
		pool, route, ok := ps.router.Match(req.URL.Path)
		if !ok {
			state := &proxyRequestState{Route: "unmatched", Err: errNoRoute, startTime: time.Now()}
			*req = *withProxyState(req, state)
			req.URL.Scheme = "http"
			req.URL.Host = ""
			return
		}

		backend, err := pool.Choose()
		if err != nil {
			state := &proxyRequestState{Pool: pool, Route: route, Err: ErrNoHealthyBackends, startTime: time.Now()}
			*req = *withProxyState(req, state)
			req.URL.Scheme = "http"
			req.URL.Host = ""
			return
		}

		backend.ActiveConns.Add(1)
		state := &proxyRequestState{Pool: pool, Backend: backend, Route: route, startTime: time.Now()}
		*req = *withProxyState(req, state)

		req.URL.Scheme = backend.URL.Scheme
		req.URL.Host = backend.URL.Host
		req.Host = backend.URL.Host

		stripHopHeaders(req.Header)

		if req.TLS != nil {
			req.Header.Set("X-Forwarded-Proto", "https")
		} else {
			existing := req.Header.Get("X-Forwarded-Proto")
			if existing != "http" && existing != "https" {
				req.Header.Set("X-Forwarded-Proto", "http")
			}
		}
		req.Header.Set("X-Forwarded-Host", req.Host)

		if ip := clientIP(req.RemoteAddr); ip != "" {
			req.Header.Add("X-Forwarded-For", ip)
		}
		req.Header.Set("X-Forwarded-For-Pool", pool.Name)
	}
}

var errNoRoute = errors.New("no route matched")

func clientIP(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.String()
	}
	return ""
}

func (ps *ProxyServer) buildModifyResponse() func(*http.Response) error {
	return func(resp *http.Response) error {
		state, ok := proxyStateFromContext(resp.Request.Context())
		if !ok {
			return nil
		}

		stripHopHeaders(resp.Header)
		resp.Header.Del("Server")
		resp.Header.Del("X-Powered-By")
		resp.Header.Set("X-Content-Type-Options", "nosniff")

		if state.Backend != nil && state.Pool != nil {
			state.Backend.ActiveConns.Add(-1)
			state.Pool.MarkSuccess(state.Backend)
		}

		state.status = resp.StatusCode
		state.bytesOut = resp.ContentLength
		state.written = true

		ps.recordAndLog(state, resp.StatusCode)
		return nil
	}
}

func (ps *ProxyServer) buildErrorHandler() func(http.ResponseWriter, *http.Request, error) {
	return func(w http.ResponseWriter, r *http.Request, err error) {
		state, ok := proxyStateFromContext(r.Context())
		if !ok {
			ps.writeError(w, http.StatusBadGateway, "bad gateway")
			return
		}

		switch {
		case errors.Is(state.Err, errNoRoute):
			ps.writeError(w, http.StatusNotFound, "not found")
			ps.recordAndLog(state, http.StatusNotFound)
			return
		case errors.Is(state.Err, ErrNoHealthyBackends):
			ps.writeError(w, http.StatusServiceUnavailable, "service unavailable")
			ps.recordAndLog(state, http.StatusServiceUnavailable)
			return
		}

		if state.Backend != nil && state.Pool != nil {
			state.Backend.ActiveConns.Add(-1)
			state.Pool.MarkFailure(state.Backend)
		}

		status := http.StatusBadGateway
		msg := "bad gateway"
		if errors.Is(err, context.DeadlineExceeded) {
			status = http.StatusGatewayTimeout
			msg = "gateway timeout"
		} else if errors.Is(r.Context().Err(), context.Canceled) {
			status = 499
			msg = "client closed request"
		}

		ps.writeError(w, status, msg)
		ps.recordAndLog(state, status)
	}
}

func (ps *ProxyServer) writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func (ps *ProxyServer) recordAndLog(state *proxyRequestState, status int) {
	poolName := "none"
	backendName := ""
	if state.Pool != nil {
		poolName = state.Pool.Name
	}
	if state.Backend != nil {
		backendName = state.Backend.URL.String()
	}

	ps.metrics.IncCounter("goproxy_requests_total", map[string]string{
		"route":  state.Route,
		"pool":   poolName,
		"status": fmt.Sprintf("%d", status),
	})

	duration := time.Since(state.startTime)

	level := slog.LevelInfo
	if status >= 500 {
		level = slog.LevelError
	} else if status >= 400 {
		level = slog.LevelWarn
	}

	ps.logger.Log(context.Background(), level, "proxy_request",
		"route", state.Route,
		"pool", poolName,
		"backend", backendName,
		"status", status,
		"duration_ms", duration.Milliseconds(),
	)
}

// ServeHTTP implements http.Handler, wrapping the underlying ReverseProxy
// with panic-recovery.
func (ps *ProxyServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	defer func() {
		if rec := recover(); rec != nil {
			ps.logger.Error("panic recovered", "path", r.URL.Path, "method", r.Method, "panic", fmt.Sprintf("%v", rec))
			ps.writeError(w, http.StatusInternalServerError, "internal server error")
		}
	}()
	ps.rp.ServeHTTP(w, r)
}

// healthChecker is the minimal interface NewMux depends on for /healthz.
type healthChecker interface {
	AnyPoolHealthy() bool
}

// NewMux builds the top-level http.ServeMux with /healthz and /metrics
// registered ahead of the catch-all proxy handler so they always take
// precedence over any user-configured route.
func NewMux(proxyServer *ProxyServer, metrics *Metrics, hc healthChecker, logger *slog.Logger) *http.ServeMux {
	mux := http.NewServeMux()

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		if hc.AnyPoolHealthy() {
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "unhealthy"})
	})

	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		var sb strings.Builder
		metrics.WriteTo(&sb)
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = w.Write([]byte(sb.String()))
	})

	mux.Handle("/", proxyServer)

	return mux
}
