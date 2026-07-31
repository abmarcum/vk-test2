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
	"strings"
	"syscall"
	"time"
)

// buildTLSListenerConfig constructs a *tls.Config enforcing a minimum TLS
// version (per cfg.Server.TLS.MinVersion) and presenting a single
// certificate/key pair resolved at startup.
func buildTLSListenerConfig(certPEM, keyPEM []byte, minVersion string) (*tls.Config, error) {
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("parse tls key pair: %w", err)
	}

	var minVer uint16
	switch minVersion {
	case "1.3":
		minVer = tls.VersionTLS13
	default:
		minVer = tls.VersionTLS12
	}

	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   minVer,
	}, nil
}

// redirectToHTTPSHandler issues an unconditional 301 redirect to the HTTPS
// equivalent of the request for every path it receives. It contains no
// path-inspection logic; the /healthz carve-out is achieved purely via mux
// registration precedence in main().
func redirectToHTTPSHandler(httpsAddr string) http.Handler {
	_, httpsPort := splitHostPort(httpsAddr)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _ := splitHostPort(host); h != "" {
			host = h
		}

		target := "https://" + host
		if httpsPort != "" && httpsPort != "443" {
			target += ":" + httpsPort
		}
		target += r.URL.Path
		if r.URL.RawQuery != "" {
			target += "?" + r.URL.RawQuery
		}

		w.Header().Set("Connection", "close")
		http.Redirect(w, r, target, http.StatusMovedPermanently)
	})
}

// splitHostPort is a defensive host:port splitter that tolerates addresses
// without an explicit host (e.g. ":8443").
func splitHostPort(addr string) (host, port string) {
	if addr == "" {
		return "", ""
	}
	idx := strings.LastIndex(addr, ":")
	if idx < 0 {
		return addr, ""
	}
	return addr[:idx], addr[idx+1:]
}

// healthzHandler serves GET/HEAD /healthz, reporting overall liveness based
// on whether at least one backend across all configured pools is healthy.
// It never exposes backend addresses, pool names, or error detail.
func healthzHandler(pools map[string]*Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")

		healthy := len(pools) == 0
		for _, p := range pools {
			if p.AnyHealthy() {
				healthy = true
				break
			}
		}

		if !healthy {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"status":"unhealthy"}`))
			return
		}

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}
}

// gracefulShutdown coordinates ordered teardown of all listening servers,
// draining in-flight requests up to the configured grace period.
func gracefulShutdown(ctx context.Context, servers []*http.Server, cancelHealth context.CancelFunc, grace time.Duration) {
	cancelHealth()

	shutdownCtx, cancel := context.WithTimeout(ctx, grace)
	defer cancel()

	for _, srv := range servers {
		if srv == nil {
			continue
		}
		_ = srv.Shutdown(shutdownCtx)
	}
}

func main() {
	configPath := flag.String("config", envOrDefault("CONFIG_PATH", "config.yaml"), "path to YAML config file")
	logLevel := flag.String("log-level", envOrDefault("LOG_LEVEL", "info"), "log level: debug|info|warn|error")
	flag.Parse()

	logger := newLogger(*logLevel)

	ctx, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stopSignals()

	cfg, err := LoadConfig(ctx, *configPath)
	if err != nil {
		logger.Error("failed to load config", "error", err.Error())
		os.Exit(1)
	}

	pools := make(map[string]*Pool, len(cfg.Pools))
	for _, pc := range cfg.Pools {
		pool, err := NewPool(pc)
		if err != nil {
			logger.Error("failed to construct pool", "pool", pc.Name, "error", err.Error())
			os.Exit(1)
		}
		pools[pc.Name] = pool
	}

	router, err := NewRouter(cfg.Routes, pools)
	if err != nil {
		logger.Error("failed to construct router", "error", err.Error())
		os.Exit(1)
	}

	metrics := NewMetrics()
	proxyServer := NewProxyServer(router, metrics, logger)

	healthCtx, cancelHealth := context.WithCancel(context.Background())
	for _, pool := range pools {
		p := pool
		if p.HealthCheckEnabled() {
			go p.RunHealthChecks(healthCtx, logger)
		}
	}

	var servers []*http.Server

	httpsMux := http.NewServeMux()
	httpsMux.HandleFunc("/healthz", healthzHandler(pools))
	httpsMux.Handle("/metrics", promHandler(metrics))
	httpsMux.Handle("/", proxyServer)

	httpMux := http.NewServeMux()
	httpMux.HandleFunc("/healthz", healthzHandler(pools))

	var httpsServer *http.Server
	if cfg.Server.EnableTLS {
		tlsCfg, err := buildTLSListenerConfig(cfg.Server.TLS.CertPEM, cfg.Server.TLS.KeyPEM, cfg.Server.TLS.MinVersion)
		if err != nil {
			logger.Error("failed to build tls config", "error", err.Error())
			cancelHealth()
			os.Exit(1)
		}

		httpsServer = &http.Server{
			Addr:              cfg.Server.HTTPSAddr,
			Handler:           httpsMux,
			TLSConfig:         tlsCfg,
			ReadHeaderTimeout: cfg.Server.Timeouts.ReadHeaderDur,
			ReadTimeout:       cfg.Server.Timeouts.ReadDur,
			WriteTimeout:      cfg.Server.Timeouts.WriteDur,
			IdleTimeout:       cfg.Server.Timeouts.IdleDur,
		}
		servers = append(servers, httpsServer)

		httpMux.Handle("/", redirectToHTTPSHandler(cfg.Server.HTTPSAddr))
	} else {
		logger.Warn("TLS not configured — HTTP-only mode (dev/test)")
		httpMux.Handle("/metrics", promHandler(metrics))
		httpMux.Handle("/", proxyServer)
	}

	httpServer := &http.Server{
		Addr:              cfg.Server.HTTPAddr,
		Handler:           httpMux,
		ReadHeaderTimeout: cfg.Server.Timeouts.ReadHeaderDur,
		ReadTimeout:       cfg.Server.Timeouts.ReadDur,
		WriteTimeout:      cfg.Server.Timeouts.WriteDur,
		IdleTimeout:       cfg.Server.Timeouts.IdleDur,
	}
	servers = append(servers, httpServer)

	go func() {
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server error", "error", err.Error())
		}
	}()

	if httpsServer != nil {
		go func() {
			if err := httpsServer.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
				logger.Error("https server error", "error", err.Error())
			}
		}()
	}

	<-ctx.Done()
	logger.Info("shutdown signal received, draining connections")

	grace := time.Duration(cfg.Server.ShutdownGraceSeconds) * time.Second
	gracefulShutdown(context.Background(), servers, cancelHealth, grace)

	logger.Info("shutdown complete")
}

// newLogger constructs a slog.Logger writing JSON to stdout at the given
// level (debug|info|warn|error, defaulting to info on unrecognized input).
func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn", "warning":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}

	handler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl})
	return slog.New(handler)
}

// envOrDefault returns the value of the named environment variable, or def
// if it is unset/empty.
func envOrDefault(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}
```

Now I need to add the small `promHandler` helper (Prometheus HTTP handler wrapper) since `main.go` references it and it belongs alongside the metrics/proxy wiring in `proxy.go`:
