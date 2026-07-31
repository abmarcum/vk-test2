// currently marked alive. It never exposes pool/backend identity.
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

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
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

		httpsMux := mux
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
