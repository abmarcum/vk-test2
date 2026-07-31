// Package main implements the GoProxy entry point and reverse-proxy data
// plane: process lifecycle, config loading, logger construction, TLS
// listener setup, HTTP/HTTPS server bootstrap, Router/ProxyServer/Mux
// route wiring (healthz/metrics/proxy), signal handling (SIGINT/SIGTERM),
// and graceful shutdown orchestration.
//
// Uses only the standard library "log" package (not "log/slog") to
// remain compatible with Go 1.19+ build environments, and only stdlib
// packages overall (no third-party dependencies).
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ---------------------------------------------------------------------------
// healthCheckerAdapter
// ---------------------------------------------------------------------------

// healthCheckerAdapter exposes a boolean liveness signal (AnyPoolHealthy)
// derived from the live Backend.Alive state of every configured Pool, for
// use by the /healthz handler. It reports healthy if at least one backend
// across all pools is currently marked alive, or vacuously healthy if no
// pools are configured. It never exposes pool/backend identity.
type healthCheckerAdapter struct {
	mu    sync.RWMutex
	pools []*Pool
}

func newHealthCheckerAdapter(pools []*Pool) *healthCheckerAdapter {
	return &healthCheckerAdapter{pools: pools}
}

func (h *healthCheckerAdapter) AnyPoolHealthy() bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, p := range h.pools {
		for _, b := range p.Backends {
			if b.Alive.Load() {
				return true
			}
		}
	}
	return len(h.pools) == 0
}

// ---------------------------------------------------------------------------
// Router
// ---------------------------------------------------------------------------

// Router resolves an incoming request path to a target Pool using
// longest-prefix-match against the configured routes.
type Router struct {
	routes []compiledRoute
}

type compiledRoute struct {
	prefix string
	pool   *Pool
}

// NewRouter compiles route configuration against the named pool set.
func NewRouter(routes []RouteConfig, pools map[string]*Pool) (*Router, error) {
	compiled := make([]compiledRoute, 0, len(routes))
	for _, r := range routes {
		pool, ok := pools[r.Pool]
		if !ok {
			return nil, fmt.Errorf("route %q references unknown pool %q", r.Match, r.Pool)
		}
		compiled = append(compiled, compiledRoute{prefix: r.Match, pool: pool})
	}
	return &Router{routes: compiled}, nil
}

// Match returns the pool bound to the longest matching route prefix for
// path, or ok=false if no route matches.
func (rt *Router) Match(path string) (*Pool, bool) {
	var best *compiledRoute
	for i := range rt.routes {
		r := &rt.routes[i]
		if strings.HasPrefix(path, r.prefix) {
			if best == nil || len(r.prefix) > len(best.prefix) {
				best = r
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

// ProxyServer is the reverse-proxy data-plane http.Handler: it resolves a
// route, selects a healthy backend via the pool's load-balancing strategy,
// forwards the request, and records passive health / metrics outcomes.
type ProxyServer struct {
	router  *Router
	metrics *Metrics
	logger  *log.Logger
}

// NewProxyServer constructs a ProxyServer.
func NewProxyServer(router *Router, metrics *Metrics, logger *log.Logger) *ProxyServer {
	return &ProxyServer{router: router, metrics: metrics, logger: logger}
}

func schemeOf(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

func (p *ProxyServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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

	proxy := httputil.NewSingleHostReverseProxy(backend.URL)
	originalDirector := proxy.Director
	proxy.Director = func(req *http.Request) {
		originalDirector(req)
		req.Host = backend.URL.Host
		if clientIP, _, splitErr := net.SplitHostPort(r.RemoteAddr); splitErr == nil {
			req.Header.Set("X-Forwarded-For", clientIP)
		}
		req.Header.Set("X-Forwarded-Proto", schemeOf(r))
		req.Header.Set("X-Forwarded-Host", r.Host)
	}

	var upstreamFailed bool
	proxy.ErrorHandler = func(rw http.ResponseWriter, req *http.Request, proxyErr error) {
		upstreamFailed = true
		p.metrics.IncCounter("proxy_requests_failed_total", nil)
		pool.MarkFailure(backend)
		if p.logger != nil {
			p.logger.Printf("ERROR upstream proxy error backend=%s err=%v", backend.URL, proxyErr)
		}
		http.Error(rw, "bad gateway", http.StatusBadGateway)
	}

	start := time.Now()
	proxy.ServeHTTP(w, r)
	p.metrics.IncCounter("proxy_requests_total", nil)
	p.metrics.SetGauge("proxy_last_request_duration_seconds", nil, time.Since(start).Seconds())

	if !upstreamFailed {
		pool.MarkSuccess(backend)
	}
}

// ---------------------------------------------------------------------------
// Mux
// ---------------------------------------------------------------------------

// NewMux wires /healthz, /metrics, and the proxy data plane onto a single
// http.Handler shared by both the HTTP and HTTPS listeners.
func NewMux(proxy *ProxyServer, metrics *Metrics, hc *healthCheckerAdapter, logger *log.Logger) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		if hc.AnyPoolHealthy() {
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

	mux.Handle("/", proxy)

	return mux
}

// ---------------------------------------------------------------------------
// Entry point / lifecycle
// ---------------------------------------------------------------------------

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	configPath := flag.String("config", envOrDefault("CONFIG_PATH", "config.yaml"), "path to config file")
	logLevel := flag.String("log-level", envOrDefault("LOG_LEVEL", "info"), "log level (unused by std log, kept for CLI compatibility)")
	flag.Parse()
	_ = *logLevel

	logger := log.New(os.Stdout, "", log.LstdFlags|log.Lmicroseconds)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, logger, *configPath); err != nil {
		logger.Printf("ERROR fatal startup error: %v", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, logger *log.Logger, configPath string) error {
	cfg, err := LoadConfig(ctx, configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	pools := make(map[string]*Pool, len(cfg.Pools))
	poolList := make([]*Pool, 0, len(cfg.Pools))
	for _, pc := range cfg.Pools {
		pool, err := NewPool(pc)
		if err != nil {
			return fmt.Errorf("build pool %q: %w", pc.Name, err)
		}
		pools[pool.Name] = pool
		poolList = append(poolList, pool)
	}

	router, err := NewRouter(cfg.Routes, pools)
	if err != nil {
		return fmt.Errorf("build router: %w", err)
	}

	metrics := NewMetrics()
	proxyServer := NewProxyServer(router, metrics, logger)

	healthCtx, cancelHealth := context.WithCancel(ctx)
	defer cancelHealth()
	for _, pool := range poolList {
		if pool.HealthCheckEnabled() {
			go pool.RunHealthChecks(healthCtx, logger)
		}
	}

	hc := newHealthCheckerAdapter(poolList)
	mux := NewMux(proxyServer, metrics, hc, logger)

	srv := &http.Server{
		Addr:              cfg.Server.HTTPAddr,
		Handler:           mux,
		ReadHeaderTimeout: cfg.Server.Timeouts.ReadHeaderDur,
		ReadTimeout:       cfg.Server.Timeouts.ReadDur,
		WriteTimeout:      cfg.Server.Timeouts.WriteDur,
		IdleTimeout:       cfg.Server.Timeouts.IdleDur,
	}

	var servers []*http.Server
	servers = append(servers, srv)

	go func() {
		logger.Printf("INFO http listener starting addr=%s", cfg.Server.HTTPAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Printf("ERROR http listener failed: %v", err)
		}
	}()

	if cfg.Server.EnableTLS {
		tlsCfg, err := buildTLSConfig(&cfg.Server.TLS)
		if err != nil {
			return fmt.Errorf("build tls config: %w", err)
		}

		httpsSrv := &http.Server{
			Addr:              cfg.Server.HTTPSAddr,
			Handler:           mux,
			TLSConfig:         tlsCfg,
			ReadHeaderTimeout: cfg.Server.Timeouts.ReadHeaderDur,
			ReadTimeout:       cfg.Server.Timeouts.ReadDur,
			WriteTimeout:      cfg.Server.Timeouts.WriteDur,
			IdleTimeout:       cfg.Server.Timeouts.IdleDur,
		}
		servers = append(servers, httpsSrv)

		go func() {
			logger.Printf("INFO https listener starting addr=%s", cfg.Server.HTTPSAddr)
			if err := httpsSrv.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
				logger.Printf("ERROR https listener failed: %v", err)
			}
		}()
	} else {
		logger.Printf("WARN tls disabled; running http-only")
	}

	<-ctx.Done()
	logger.Printf("INFO shutdown signal received")
	cancelHealth()

	grace := time.Duration(cfg.Server.ShutdownGraceSeconds) * time.Second
	gracefulShutdown(servers, grace, logger)

	return nil
}

func buildTLSConfig(cfg *TLSConfig) (*tls.Config, error) {
	cert, err := tls.X509KeyPair(cfg.CertPEM, cfg.KeyPEM)
	if err != nil {
		return nil, fmt.Errorf("load x509 keypair: %w", err)
	}

	var minVer uint16 = tls.VersionTLS12
	if cfg.MinVersion == "1.3" {
		minVer = tls.VersionTLS13
	}

	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   minVer,
	}, nil
}

func gracefulShutdown(servers []*http.Server, grace time.Duration, logger *log.Logger) {
	ctx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()

	for _, s := range servers {
		if err := s.Shutdown(ctx); err != nil {
			logger.Printf("ERROR graceful shutdown error: %v", err)
		}
	}
}
