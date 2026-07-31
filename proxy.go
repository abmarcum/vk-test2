type healthCheckerAdapter struct {
	pools map[string]*Pool
}

// AnyPoolHealthy reports true if at least one backend across any pool is
// currently alive, or if there are no pools/backends configured at all
// (fail-open for liveness purposes when health checking is not wired up).
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
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// run performs the full bootstrap sequence and blocks until shutdown
// completes. Returns a non-nil error only for startup-time failures.
func run() error {
	configPath := flag.String("config", envOr("CONFIG_PATH", "/app/config/config.yaml"), "path to config file")
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
	hc := &healthCheckerAdapter{pools: pools}

	healthCtx, cancelHealth := context.WithCancel(context.Background())
	defer cancelHealth()
	for _, pool := range pools {
		if pool.HealthCheckEnabled() {
			go pool.RunHealthChecks(healthCtx, logger)
		}
	}

	mux := NewMux(proxyServer, metrics, hc, logger)

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

	if cfg.Server.EnableTLS {
		tlsCfg, err := buildTLSListenerConfig(cfg.Server.TLS.CertPEM, cfg.Server.TLS.KeyPEM, cfg.Server.TLS.MinVersion)
		if err != nil {
			return fmt.Errorf("build tls config: %w", err)
		}

		httpsMux := NewMux(proxyServer, metrics, hc, logger)

		httpsSrv := &http.Server{
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

// newLogger constructs a JSON slog.Logger at the requested level.
func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lv
