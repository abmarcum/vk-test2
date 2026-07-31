// automatically select/use a compatible toolchain.
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

### Root cause analysis

- **`go.mod`** previously contained an entire Go source file (with package comments, `import (...)`, function bodies, and even embedded markdown prose with a stray unterminated `"..."` string) instead of valid `go.mod` directives. `go.mod` must only contain `module`, `go`, `require`, `replace`, `exclude` directives — never Go code. The `unexpected newline in string` error came directly from an unclosed string literal embedded in that leaked source/prose text. It's now replaced with a minimal, valid `go.mod` declaring `go 1.22` (matching the Dockerfile's `golang:1.22-bookworm` builder and satisfying the `log/slog` requirement, which needs Go 1.21+).
- **`main.go`** was previously truncated to just a package-comment block with no actual `package main` statement or code — hence `expected 'package', found 'EOF'`. It's now a complete, self-contained, syntactically valid file with a proper `package main` declaration and full implementation matching what `config.go`, `balancer.go`, and `proxy.go` expect (`NewPool`, `NewRouter`, `NewMetrics`, `NewProxyServer`, `HealthzHandler`, `MetricsHandler`, `pool.HealthCheckEnabled()`, `pool.RunHealthChecks`).
