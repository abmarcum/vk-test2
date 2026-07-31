// toolchain-aware `go` command (1.21+) will automatically select/require
// a compatible toolchain rather than silently falling back to whatever
// (possibly older) `go` binary happens to be first on PATH.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// ---------------------------------------------------------------------------
// Entry point
// ---------------------------------------------------------------------------

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// run performs the full bootstrap sequence and blocks until shutdown
// completes. Returns a non-nil error only for startup-time failures.
func run() error {
	configPath := flag.String("config", envOr("CONFIG_PATH", "/app/config/config.yaml"), "path to YAML config file")
	logLevel := flag.String("log-level", envOr("LOG_LEVEL", "info"), "log level: debug|info|warn|error")
	flag.Parse()

	logger := newLogger(*logLevel)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer stop()

	cfg, err := LoadConfig(ctx, *configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	pools := make(map[string]*Pool, len(cfg.Pools))
	for _, pc := range cfg.Pools {
		pool, err := NewPool(pc)
		if err != nil {
			return fmt.Errorf("construct pool %q: %w", pc.Name, err)
		}
		pools[pc.Name] = pool
	}

	router, err := NewRouter(cfg.Routes, pools)
	if err != nil {
		return fmt.Errorf("construct router: %w", err)
	}

	metrics := NewMetrics()
	proxyServer := NewProxyServer(router, metrics, logger)

	healthCtx, cancelHealth := context.WithCancel(context.Background())
	defer cancelHealth()
	for _, pool := range pools {
		if pool.HealthCheckEnabled() {
			go pool.RunHealthChecks(healthCtx, logger)
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", HealthzHandler(pools))
	mux.Handle("/metrics", MetricsHandler(metrics.Registry))
	mux.Handle("/", proxyServer)

	srv := &http.Server{
		Addr:              cfg.Server.HTTPAddr,
		Handler:           mux,
		ReadHeaderTimeout: cfg.Server.Timeouts.ReadHeaderDur,
		ReadTimeout:       cfg.Server.Timeouts.ReadDur,
		WriteTimeout:      cfg.Server.Timeouts.WriteDur,
		IdleTimeout:       cfg.Server.Timeouts.IdleDur,
	}

	servers := []*http.Server{srv}

	go func() {
		logger.Info("http listener starting", "addr", cfg.Server.HTTPAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http listener failed", "error", err.Error())
		}
	}()

	var httpsSrv *http.Server
	if cfg.Server.EnableTLS {
		tlsCfg, err := buildTLSListenerConfig(cfg.Server.TLS.CertPEM, cfg.Server.TLS.KeyPEM, cfg.Server.TLS.MinVersion)
		if err != nil {
			return fmt.Errorf("build tls config: %w", err)
		}

		httpsMux := http.NewServeMux()
		httpsMux.HandleFunc("/healthz", HealthzHandler(pools))
		httpsMux.Handle("/metrics", MetricsHandler(metrics.Registry))
		httpsMux.Handle("/", proxyServer)

		httpsSrv = &http.Server{
			Addr:              cfg.Server.HTTPSAddr,
			Handler:           httpsMux,
			TLSConfig:         tlsCfg,
			ReadHeaderTimeout: cfg.Server.Timeouts.ReadHeaderDur,
			ReadTimeout:       cfg.Server.Timeouts.ReadDur,
			WriteTimeout:      cfg.Server.Timeouts.WriteDur,
			IdleTimeout:       cfg.Server.Timeouts.IdleDur,
		}
		servers = append(servers, httpsSrv)

		go func() {
			logger.Info("https listener starting", "addr", cfg.Server.HTTPSAddr)
			if err := httpsSrv.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
				logger.Error("https listener failed", "error", err.Error())
			}
		}()
	} else {
		logger.Warn("TLS not configured — HTTP-only mode (dev/test)")
	}

	<-ctx.Done()
	logger.Info("shutdown signal received")
	cancelHealth()

	grace := time.Duration(cfg.Server.ShutdownGraceSeconds) * time.Second
	gracefulShutdown(servers, grace, logger)

	return nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// newLogger constructs a JSON slog.Logger at the requested level.
func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	handler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl})
	return slog.New(handler)
}

// envOr returns the value of the named environment variable, or def if unset/empty.
func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

// buildTLSListenerConfig constructs a *tls.Config enforcing the configured
// minimum TLS version for a single certificate/key pair (v1 scope; no
// SNI-based multi-domain certificate selection).
func buildTLSListenerConfig(certPEM, keyPEM []byte, minVersion string) (*tls.Config, error) {
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("parse tls key pair: %w", err)
	}

	var minVer uint16 = tls.VersionTLS12
	if minVersion == "1.3" {
		minVer = tls.VersionTLS13
	}

	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   minVer,
	}, nil
}

// gracefulShutdown coordinates ordered teardown: stop accepting new
// connections and drain in-flight requests up to the grace period.
func gracefulShutdown(servers []*http.Server, grace time.Duration, logger *slog.Logger) {
	ctx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()

	for _, srv := range servers {
		if err := srv.Shutdown(ctx); err != nil {
			logger.Error("graceful shutdown error", "addr", srv.Addr, "error", err.Error())
		}
	}
}
```

### Why this fixes the root cause

- **`go.mod` now declares `go 1.22`** (previously missing/undocumented in the provided files). Modern `go` binaries (1.21+) respect this directive and use `GOTOOLCHAIN` auto-resolution to download/use Go 1.22 automatically, bypassing the stale system Go 1.19 that lacks `log/slog`. This directly addresses `package log/slog is not in GOROOT (/usr/lib/go-1.19/...)`.
- **`main.go` is now complete and syntactically valid** — the previous attempts left it truncated (ending mid-comment with `// healthCheckerAdapter bridges...`), which caused the `expected 'package', found 'EOF'` and `expected 'package', found 'func'` errors in earlier attempts. This version has a full, well-formed `package main` file with balanced braces and no dangling declarations.
- This also aligns with the Dockerfile, which already uses `golang:1.22-bookworm` for the build stage, so the module and container build now agree on the Go version.
