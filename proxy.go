func (m *Metrics) incRequests(route, pool, method, status string) {
	m.IncCounter("goproxy_requests_total", map[string]string{
		"route": route, "pool": pool, "method": method, "status": status,
	})
}

func (m *Metrics) incUpstreamErrors(pool, reason string) {
	m.IncCounter("goproxy_upstream_errors_total", map[string]string{
		"pool": pool, "reason": reason,
	})
}

func (m *Metrics) setBackendUp(pool, backend string, up bool) {
	v := 0.0
	if up {
		v = 1.0
	}
	m.SetGauge("goproxy_backend_up", map[string]string{"pool": pool, "backend": backend}, v)
}

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

func loggingMiddleware(logger *Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		dur := time.Since(start)

		if logger == nil {
			return
		}
		fields := []any{
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration_ms", dur.Milliseconds(),
			"remote_addr", r.RemoteAddr,
			"user_agent", r.UserAgent(),
		}
		switch {
		case rec.status >= 500:
			logger.Error("proxy_request", fields...)
		case rec.status >= 400:
			logger.Warn("proxy_request", fields...)
		default:
			logger.Info("proxy_request", fields...)
		}
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
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
		w.WriteHeader(http.StatusOK)
		if metrics == nil {
			return
		}
		var sb strings.Builder
		metrics.WriteTo(&sb)
		_, _ = w.Write([]byte(sb.String()))
	})
}
```

### Summary of root cause & fix

- **Real root cause**: the QA sandbox's Go toolchain is **1.19**, which predates `log/slog` (added in 1.21). Every attempt that referenced `log/slog` failed identically regardless of `go.mod`/Dockerfile content, because those don't affect the *host* toolchain actually running `go build`.
- **Fix**: removed all `log/slog` imports/usages; added a small custom `Logger` in `logging.go` compatible with any Go version this repo already uses (`atomic.Bool`/`atomic.Int64`/generics-free code, compatible back to Go 1.19).
- Also fixed the genuinely broken `main.go` (was a dangling fragment with no `func main`), which was an independent latent bug.
- Left `go.mod`/`go.sum` untouched since they are already minimal, valid, and dependency-free — the previous corruption issues in those files are not present in the current source and should not be reintroduced.
- `metrics.go`'s existing `Metrics` type is now the single source of truth; `proxy.go` no longer redeclares a conflicting `Metrics` struct, only adds convenience methods used by the proxy layer.
