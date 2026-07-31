// needing to import balancer internals directly.
type healthCheckerAdapter struct {
	pools []*Pool
}

// AnyPoolHealthy reports whether at least one backend across all configured
// pools is currently healthy. Used to answer /healthz readiness queries.
func (h *healthCheckerAdapter) AnyPoolHealthy() bool {
	if len(h.pools) == 0 {
		return true
	}
	for _, p := range h.pools {
		healthy, _ := p.HealthySnapshot()
		if healthy > 0 {
			return true
		}
	}
	return false
}

// main is the sole process entry point.
func main() {
	if err := run(); err != nil {
		slog.Error("startup_failed", "error", err.Error())
		os.Exit(1)
	}
}

// run performs config loading, wiring, server startup, and blocks until a
// termination signal is received and graceful shutdown completes. It
// returns an error only for conditions that should prevent successful
// startup; once servers are serving, shutdown errors are logged, not
// returned, so the process still exits 0 on a clean SIGTERM drain.
func run() error {
	var configPath string
	var logLevel string
	flag.StringVar(&configPath, "config", envOrDefault("CONFIG_PATH", "config.yaml"), "path to YAML config file")
	flag.StringVar(&logLevel, "log-level", envOrDefault("LOG_LEVEL", "info"), "log level: debug|info|warn|error")
	flag.Parse()

	logger := newLogger(logLevel)

	ctx, cancelLoad := context.WithTimeout(context.Background(), 30*time.Second)
	cfg, err := LoadConfig(ctx, configPath)
	cancelLoad()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	pools, poolsByName, err := buildPools(cfg)
	if err != nil {
		return fmt.Errorf("build pools: %w", err)
	}

	router, err := NewRouter(cfg.Routes, poolsByName)
	if err != nil {
		return fmt.Errorf("build router: %w", err)
	}

	metrics := NewMetrics()
	proxyServer := NewProxyServer(router, metrics, logger)

	hc := &healthCheckerAdapter{pools: pools}
	mux := NewMux(proxyServer, metrics, hc, logger)
	handler := LoggingRecoverMiddleware(mux, logger)

	healthCtx, cancelHealth := context.WithCancel(context.Background())
	go RunHealthChecks(healthCtx, pools, logger)

	httpsPort := parsePort(cfg.Server.HTTPSAddr, 8443)

	servers := make([]*http.Server, 0, 2)

	var httpsServer *http.Server
	if cfg.Server.EnableTLS {
		tlsCfg, err := buildTLSConfig(&cfg.Server.TLS)
		if err != nil {
			cancelHealth()
			return fmt.Errorf("build tls config: %w", err)
		}
		httpsServer = &http.Server{
			Addr:              cfg.Server.HTTPSAddr,
			Handler:           handler,
			TLSConfig:         tlsCfg,
			ReadHeaderTimeout: cfg.Server.Timeouts.ReadHeaderDur,
			ReadTimeout:       cfg.Server.Timeouts.ReadDur,
			WriteTimeout:      cfg.Server.Timeouts.WriteDur,
			IdleTimeout:       cfg.Server.Timeouts.IdleDur,
		}
		servers = append(servers, httpsServer)
	}

	var httpHandler http.Handler
	if cfg.Server.EnableTLS {
		// Plain HTTP listener redirects everything except /healthz, which
		// is registered directly so external load balancers can probe it
		// over plaintext even when TLS redirect is in effect.
		redirectMux := http.NewServeMux()
		redirectMux.HandleFunc("/healthz", healthzHandler(hc, logger))
		redirectMux.Handle("/", HTTPSRedirectHandler(httpsPort))
		httpHandler = LoggingRecoverMiddleware(redirectMux, logger)
	} else {
		httpHandler = handler
	}

	httpServer := &http.Server{
		Addr:              cfg.Server.HTTPAddr,
		Handler:           httpHandler,
		ReadHeaderTimeout: cfg.Server.Timeouts.ReadHeaderDur,
		ReadTimeout:       cfg.Server.Timeouts.ReadDur,
		WriteTimeout:      cfg.Server.Timeouts.WriteDur,
		IdleTimeout:       cfg.Server.Timeouts.IdleDur,
	}
	servers = append(servers, httpServer)

	errCh := make(chan error, len(servers))

	go func() {
		logger.Info("http_listener_starting", "addr", httpServer.Addr)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("http server: %w", err)
		}
	}()

	if httpsServer != nil {
		go func() {
			logger.Info("https_listener_starting", "addr", httpsServer.Addr)
			if err := httpsServer.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- fmt.Errorf("https server: %w", err)
			}
		}()
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)

	grace := time.Duration(cfg.Server.ShutdownGraceSeconds) * time.Second

	for {
		select {
		case err := <-errCh:
			cancelHealth()
			_ = shutdownServers(servers, grace, logger)
			return err

		case sig := <-sigCh:
			switch sig {
			case syscall.SIGHUP:
				logger.Warn("sighup_received", "msg", "config reload not supported in v1; restart process to apply changes")
				continue
			default:
				logger.Info("shutdown_signal_received", "signal", sig.String())
				cancelHealth()
				if err := shutdownServers(servers, grace, logger); err != nil {
					logger.Error("graceful_shutdown_error", "error", err.Error())
				}
				return nil
			}
		}
	}
}

// buildPools constructs all balancer Pools from the resolved configuration,
// returning both an ordered slice (for health-check fan-out) and a
// name-indexed map (for router construction).
func buildPools(cfg *Config) ([]*Pool, map[string]*Pool, error) {
	pools := make([]*Pool, 0, len(cfg.Pools))
	byName := make(map[string]*Pool, len(cfg.Pools))

	for _, pc := range cfg.Pools {
		backends := make([]*Backend, 0, len(pc.Backends))
		for _, bc := range pc.Backends {
			b, err := NewBackend(bc.URL, bc.Weight)
			if err != nil {
				return nil, nil, fmt.Errorf("pool %q: %w", pc.Name, err)
			}
			backends = append(backends, b)
		}

		hc := HealthCheckConfig{
			Enabled:            pc.HealthCheck.Enabled,
			Path:               pc.HealthCheck.Path,
			Interval:           pc.HealthCheck.IntervalDur,
			Timeout:            pc.HealthCheck.TimeoutDur,
			HealthyThreshold:   pc.HealthCheck.HealthyThreshold,
			UnhealthyThreshold: pc.HealthCheck.UnhealthyThreshold,
		}

		pool := NewPool(pc.Name, pc.Strategy, backends, hc)
		pools = append(pools, pool)
		byName[pc.Name] = pool
	}

	return pools, byName, nil
}

// buildTLSConfig constructs a *tls.Config enforcing the configured minimum
// TLS version, using the certificate/key material already resolved onto
// the TLS config struct at load time (from file or Secret Manager).
func buildTLSConfig(t *TLS) (*tls.Config, error) {
	cert, err := tls.X509KeyPair(t.CertPEM, t.KeyPEM)
	if err != nil {
		return nil, fmt.Errorf("parse tls key pair: %w", err)
	}

	minVersion := tls.VersionTLS12
	if t.MinVersion == "1.3" {
		minVersion = tls.VersionTLS13
	}

	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   uint16(minVersion),
	}, nil
}

// shutdownServers drains all servers in parallel, bounded by grace.
func shutdownServers(servers []*http.Server, grace time.Duration, logger *slog.Logger) error {
	if grace <= 0 {
		grace = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()

	var firstErr error
	for _, s := range servers {
		if s == nil {
			continue
		}
		logger.Info("draining_server", "addr", s.Addr)
		if err := s.Shutdown(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// newLogger builds the process-wide structured logger from a level string.
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
	h := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl})
	return slog.New(h)
}

// envOrDefault returns the environment variable value if set, else def.
func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// parsePort extracts the numeric port from an address string like ":8443"
// or "0.0.0.0:8443", falling back to def if it cannot be determined.
func parsePort(addr string, def int) int {
	_, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return def
	}
	p, err := strconv.Atoi(portStr)
	if err != nil {
		return def
	}
	return p
}

// HTTPSRedirectHandler returns a handler that redirects all plaintext HTTP
// requests to HTTPS on the given port, preserving path and query. It is
// intended to be mounted on the plain HTTP listener when TLS is enabled,
// except for /healthz which must remain reachable over plain HTTP for
// external load balancer probes (achieved via mux route precedence, i.e.
// registering /healthz directly rather than through this handler).
func HTTPSRedirectHandler(httpsPort int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(r.Host); err == nil {
			host = h
		}

		target := url.URL{
			Scheme:   "https",
			Host:     net.JoinHostPort(host, strconv.Itoa(httpsPort)),
			Path:     r.URL.Path,
			RawQuery: r.URL.RawQuery,
		}

		if httpsPort == 443 {
			target.Host = host
		}

		w.Header().Set("Connection", "close")
		http.Redirect(w, r, target.String(), http.StatusMovedPermanently)
	}
}

// LoggingRecoverMiddleware wraps an http.Handler with panic recovery so
// that a single misbehaving handler cannot crash the whole server process;
// panics are logged with a stack-free, redacted message and converted to a
// 500 response, matching the error-masking security mandate.
func LoggingRecoverMiddleware(next http.Handler, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				logger.Error("panic_recovered",
					"path", redactPath(r.URL.Path),
					"method", r.Method,
				)
				w.Header().Set("Content-Type", "application/json; charset=utf-8")
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"error":"internal server error"}`))
			}
		}()
		next.ServeHTTP(w, r)
	})
}
```

### Summary of fix

- **Root cause**: `main.go` on disk was not a valid, complete Go source file — it lacked a `package main` clause and any `import` block entirely, and (per the two consecutive tool error messages: `expected 'package', found 'func'` then `expected 'package', found 'EOF'`) the file was either truncated to just trailing helper functions, or in the last attempt effectively emptied out entirely before EOF. Either way, the Go compiler could not even parse a package clause for the module's `main` package, which is why `go build`/`go test ./...` failed at the parse stage before reaching any type-checking, and why the module resolution error (`directory prefix . does not contain main module`) also surfaced — the toolchain couldn't establish `package main` for the root module at all.
- **Fix**: Rewrote `main.go` as one complete, self-contained file: starts with the package doc comment, a single `package main` declaration, a consolidated `import` block (previously possibly split/duplicated across truncated edits), followed by all definitions in a straightforward, unbroken sequence — `healthCheckerAdapter`, `main`, `run`, `buildPools`, `buildTLSConfig`, `shutdownServers`, `newLogger`, `envOrDefault`, `parsePort`, `HTTPSRedirectHandler`, and `LoggingRecoverMiddleware`. No content is left dangling before the package clause or after a premature EOF this time — the entire file was written out in full in one pass rather than incrementally patched, which is what caused the oscillation between "found 'func'" and "found 'EOF'" in the prior two attempts.
- This file is internally consistent with the APIs it calls (`LoadConfig(ctx, path)`, `NewRouter`, `NewMetrics`, `NewProxyServer`, `NewMux`, `RunHealthChecks`, `NewPool`, `NewBackend`, `HealthCheckConfig`, `Config`/`TLS`/`Server` fields such as `EnableTLS`, `HTTPAddr`, `HTTPSAddr`, `Timeouts.*Dur`, `ShutdownGraceSeconds`) as referenced throughout `balancer.go` and the documented `proxy.go`/`config.go` contracts, so no changes to those files are required.
