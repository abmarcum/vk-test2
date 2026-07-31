// Package main is the entry point for goproxy: process lifecycle,
// config loading, logger construction, TLS listener setup, HTTP/HTTPS
// server bootstrap, mux route wiring (healthz/metrics/proxy/redirect),
// signal handling (SIGTERM/SIGHUP), and graceful shutdown orchestration.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// healthCheckerAdapter provides a minimal AnyPoolHealthy() view over a set
// of live pools, used by the /healthz handler to determine overall status
// without leaking backend topology.
type healthCheckerAdapter struct {
	pools []*Pool
}

func (h *healthCheckerAdapter) AnyPoolHealthy() bool {
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
	configPath := flag.String("config", envOrDefault("CONFIG_PATH", "/app/config/config.yaml"), "path to config YAML file")
	flag.Parse()

	logLevel := envOrDefault("LOG_LEVEL", "info")
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: parseLogLevel(logLevel),
	}))

	cfg, err := LoadConfig(*configPath)
	if err != nil {
		logger.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	pools := make(map[string]*Pool, len(cfg.Pools))
	var allPools []*Pool
	for _, pc := range cfg.Pools {
		pool, err := NewPool(pc)
		if err != nil {
			logger.Error("failed to build pool", "error", err)
			os.Exit(1)
		}
		pools[pool.Name] = pool
		allPools = append(allPools, pool)
	}

	router, err := NewRouter(cfg.Routes, pools)
	if err != nil {
		logger.Error("failed to build router", "error", err)
		os.Exit(1)
	}

	metrics := NewMetrics()
	proxyServer := NewProxyServer(router, metrics, logger)
	hc := &healthCheckerAdapter{pools: allPools}

	healthCtx, cancelHealth := context.WithCancel(context.Background())
	for _, p := range allPools {
		go p.RunHealthChecks(healthCtx, logger)
	}

	mux := NewMux(proxyServer, metrics, hc, logger)

	var servers []*http.Server

	timeouts := cfg.Server.Timeouts
	readHeaderTimeout := mustParseDuration(timeouts.ReadHeader, 5*time.Second)
	readTimeout := mustParseDuration(timeouts.Read, 15*time.Second)
	writeTimeout := mustParseDuration(timeouts.Write, 15*time.Second)
	idleTimeout := mustParseDuration(timeouts.Idle, 60*time.Second)

	httpServer := &http.Server{
		Addr:              cfg.Server.HTTPAddr,
		Handler:           mux,
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
	}
	servers = append(servers, httpServer)

	go func() {
		logger.Info("starting HTTP listener", "addr", cfg.Server.HTTPAddr)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server error", "error", err)
		}
	}()

	var httpsServer *http.Server
	if cfg.Server.EnableTLS {
		tlsCfg, err := buildTLSListenerConfig(cfg.Server.TLS.CertPEM, cfg.Server.TLS.KeyPEM, cfg.Server.TLS.MinVersion)
		if err != nil {
			logger.Error("failed to build TLS config", "error", err)
			os.Exit(1)
		}

		httpsServer = &http.Server{
			Addr:              cfg.Server.HTTPSAddr,
			Handler:           mux,
			TLSConfig:         tlsCfg,
			ReadHeaderTimeout: readHeaderTimeout,
			ReadTimeout:       readTimeout,
			WriteTimeout:      writeTimeout,
			IdleTimeout:       idleTimeout,
		}
		servers = append(servers, httpsServer)

		go func() {
			logger.Info("starting HTTPS listener", "addr", cfg.Server.HTTPSAddr)
			if err := httpsServer.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
				logger.Error("https server error", "error", err)
			}
		}()
	} else {
		logger.Warn("TLS not configured — HTTP-only mode (dev/test)")
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)

	grace := time.Duration(cfg.Server.ShutdownGraceSeconds) * time.Second

	for sig := range sigCh {
		switch sig {
		case syscall.SIGHUP:
			logger.Warn("config reload not supported in v1; restart process to apply changes")
		case syscall.SIGTERM, syscall.SIGINT:
			logger.Info("shutdown signal received, draining", "signal", sig.String())
			cancelHealth()
			gracefulShutdown(context.Background(), servers, grace, logger)
			return
		}
	}
}

func gracefulShutdown(ctx context.Context, servers []*http.Server, grace time.Duration, logger *slog.Logger) {
	shutdownCtx, cancel := context.WithTimeout(ctx, grace)
	defer cancel()

	for _, s := range servers {
		if s == nil {
			continue
		}
		if err := s.Shutdown(shutdownCtx); err != nil {
			logger.Error("error during graceful shutdown", "error", err)
		}
	}
}

func buildTLSListenerConfig(certPEM, keyPEM []byte, minVersion string) (*tls.Config, error) {
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, err
	}

	minVer := uint16(tls.VersionTLS12)
	if minVersion == "1.3" {
		minVer = tls.VersionTLS13
	}

	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   minVer,
	}, nil
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func parseLogLevel(level string) slog.Level {
	switch level {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func mustParseDuration(s string, def time.Duration) time.Duration {
	if s == "" {
		return def
	}
	d, err := parseDurationSafe(s)
	if err != nil {
		return def
	}
	return d
}

func parseDurationSafe(s string) (time.Duration, error) {
	return time.ParseDuration(s)
}
