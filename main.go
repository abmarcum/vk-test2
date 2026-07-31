// Package main implements the GoProxy entry point: process lifecycle,
// config loading, logger construction, TLS listener setup, HTTP/HTTPS
// server bootstrap, mux route wiring (healthz/metrics/proxy), signal
// handling (SIGTERM/SIGHUP), and graceful shutdown orchestration.
//
// Note: this file intentionally avoids the standard library "log/slog"
// package (introduced in Go 1.21) so the module remains buildable across
// a wider range of installed Go toolchains. Structured logging is instead
// provided by the dependency-free Logger type in logging.go.
package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

// poolHealthChecker adapts a set of named Pools to the healthChecker
// interface expected by the /healthz handler in proxy.go.
type poolHealthChecker struct {
	mu    sync.RWMutex
	pools []*Pool
}

func (h *poolHealthChecker) AnyPoolHealthy() bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
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

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	configPath := flag.String("config", envOr("CONFIG_PATH", "/app/config/config.yaml"), "path to config file")
	logLevel := flag.String("log-level", envOr("LOG_LEVEL", "info"), "log level: debug|info|warn|error")
	flag.Parse()

	logger := NewLogger(os.Stdout, *logLevel)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer stop()

	cfg, err := LoadConfig(ctx, *configPath)
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
	for _, p := range poolList {
		if p.HealthCheckEnabled() {
			go p.RunHealthChecks(healthCtx, logger)
		}
	}

	hc := &poolHealthChecker{pools: poolList}

	mux := NewMux(proxyServer, metrics, hc, logger)

	var servers []*http.Server

	if cfg.Server.HTTPAddr != "" {
		var httpHandler http.Handler = mux
		if cfg.Server.EnableTLS && cfg.Server.HTTPSAddr != "" {
			httpHandler = httpsRedirectHandler(cfg.Server.HTTPSAddr, mux)
		}
		httpSrv := &http.Server{
			Addr:              cfg.Server.HTTPAddr,
			Handler:           httpHandler,
			ReadHeaderTimeout: cfg.Server.Timeouts.ReadHeaderDur,
			ReadTimeout:       cfg.Server.Timeouts.ReadDur,
			WriteTimeout:      cfg.Server.Timeouts.WriteDur,
			IdleTimeout:       cfg.Server.Timeouts.IdleDur,
		}
		servers = append(servers, httpSrv)
		go func() {
			logger.Info("http listener starting", "addr", cfg.Server.HTTPAddr)
			if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				logger.Error("http listener failed", "error", err.Error())
			}
		}()
	}

	if cfg.Server.EnableTLS && cfg.Server.HTTPSAddr != "" {
		tlsCfg, err := buildTLSConfig(cfg.Server.TLS)
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
			logger.Info("https listener starting", "addr", cfg.Server.HTTPSAddr)
			if err := httpsSrv.ListenAndServeTLS("", ""); err != nil && err != http.ErrServerClosed {
				logger.Error("https listener failed", "error", err.Error())
			}
		}()
	}

	if len(servers) == 0 {
		logger.Warn("no listeners configured; exiting")
		return nil
	}

	<-ctx.Done()
	logger.Info("shutdown signal received")
	cancelHealth()

	grace := time.Duration(cfg.Server.ShutdownGraceSeconds) * time.Second
	gracefulShutdown(servers, grace, logger)

	return nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// httpsRedirectHandler wraps mux so that all paths except /healthz are
// redirected to HTTPS; /healthz remains reachable over plaintext for
// external load balancer probes.
func httpsRedirectHandler(httpsAddr string, mux http.Handler) http.Handler {
	_, port := splitAddrPort(httpsAddr)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			mux.ServeHTTP(w, r)
			return
		}
		host := r.Host
		if h, _, err := splitHostPortSafe(host); err == nil {
			host = h
		}
		target := "https://" + host
		if port != "" && port != "443" {
			target += ":" + port
		}
		target += r.URL.RequestURI()

		w.Header().Set("Connection", "close")
		http.Redirect(w, r, target, http.StatusMovedPermanently)
	})
}

func splitAddrPort(addr string) (string, string) {
	idx := -1
	for i := len(addr) - 1; i >= 0; i-- {
		if addr[i] == ':' {
			idx = i
			break
		}
	}
	if idx < 0 {
		return addr, ""
	}
	return addr[:idx], addr[idx+1:]
}

func splitHostPortSafe(hostport string) (string, string, error) {
	for i := len(hostport) - 1; i >= 0; i-- {
		if hostport[i] == ':' {
			return hostport[:i], hostport[i+1:], nil
		}
	}
	return hostport, "", fmt.Errorf("no port in %q", hostport)
}

// buildTLSConfig constructs a *tls.Config from resolved cert/key PEM
// material and the configured minimum TLS version.
func buildTLSConfig(cfg TLSConf) (*tls.Config, error) {
	cert, err := tls.X509KeyPair(cfg.CertPEM, cfg.KeyPEM)
	if err != nil {
		return nil, fmt.Errorf("load x509 keypair: %w", err)
	}

	minVer := uint16(tls.VersionTLS12)
	if cfg.MinVersion == "1.3" {
		minVer = tls.VersionTLS13
	}

	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   minVer,
	}, nil
}

// gracefulShutdown shuts down all servers concurrently, bounded by grace.
func gracefulShutdown(servers []*http.Server, grace time.Duration, logger *Logger) {
	ctx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()

	var wg sync.WaitGroup
	for _, srv := range servers {
		wg.Add(1)
		go func(s *http.Server) {
			defer wg.Done()
			if err := s.Shutdown(ctx); err != nil {
				logger.Error("error during shutdown", "addr", s.Addr, "error", err.Error())
			}
		}(srv)
	}
	wg.Wait()
}
