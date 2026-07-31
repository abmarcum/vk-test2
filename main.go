// Package main implements the GoProxy entry point: process lifecycle,
// config loading, pool/router construction, TLS listener bootstrap, the
// `healthcheck` CLI subcommand, and graceful shutdown on SIGINT/SIGTERM.
//
// The Config/LoadConfig types live in config.go, the Backend/Pool/Strategy
// types live in balancer.go, the Metrics registry lives in metrics.go, and
// the Router/ProxyServer/Mux types live in proxy.go. This file intentionally
// declares none of those symbols itself to avoid duplicate-declaration
// build errors -- it only wires them together.
//
// Uses only the standard library "log" package (not "log/slog") to remain
// compatible with Go 1.19+ build environments, and only stdlib packages
// overall (no third-party dependencies).
package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

// Populated at build time via -ldflags "-X main.version=... -X main.commit=... -X main.buildDate=...".
var (
	version   = "dev"
	commit    = "none"
	buildDate = "unknown"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(runHealthCheckCLI())
	}

	if err := run(); err != nil {
		log.Printf("ERROR fatal: %v", err)
		os.Exit(1)
	}
}

// run loads configuration, builds the pools/router/mux, starts the HTTP
// (and optionally HTTPS) listeners, and blocks until a shutdown signal is
// received, at which point it drains connections within the configured
// grace period.
func run() error {
	configPath := flag.String("config", envOr("CONFIG_PATH", "config.yaml"), "path to YAML config file")
	flag.Parse()

	logger := log.New(os.Stdout, "", 0)
	logger.Printf("INFO starting goproxy version=%s commit=%s build_date=%s config=%s", version, commit, buildDate, *configPath)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
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
		pools[pc.Name] = pool
		poolList = append(poolList, pool)
	}

	router, err := NewRouter(cfg.Routes, pools)
	if err != nil {
		return err
	}

	metrics := NewMetrics()
	proxyServer := NewProxyServer(router, metrics, logger)
	hc := newHealthCheckerAdapter(poolList)
	mux := NewMux(proxyServer, metrics, hc, logger)

	hcCtx, hcCancel := context.WithCancel(ctx)
	defer hcCancel()
	for _, pool := range poolList {
		if pool.HealthCheckEnabled() {
			go pool.RunHealthChecks(hcCtx, logger)
		}
	}

	var wg sync.WaitGroup
	var srvMu sync.Mutex
	var servers []*http.Server

	startServer := func(addr string, tlsCfg *tls.Config) {
		if addr == "" {
			return
		}
		srv := &http.Server{
			Addr:              addr,
			Handler:           mux,
			TLSConfig:         tlsCfg,
			ReadHeaderTimeout: cfg.Server.Timeouts.ReadHeaderDur,
			ReadTimeout:       cfg.Server.Timeouts.ReadDur,
			WriteTimeout:      cfg.Server.Timeouts.WriteDur,
			IdleTimeout:       cfg.Server.Timeouts.IdleDur,
		}

		srvMu.Lock()
		servers = append(servers, srv)
		srvMu.Unlock()

		wg.Add(1)
		go func() {
			defer wg.Done()
			var serveErr error
			if tlsCfg != nil {
				logger.Printf("INFO https listener starting addr=%s", addr)
				serveErr = srv.ListenAndServeTLS("", "")
			} else {
				logger.Printf("INFO http listener starting addr=%s", addr)
				serveErr = srv.ListenAndServe()
			}
			if serveErr != nil && serveErr != http.ErrServerClosed {
				logger.Printf("ERROR listener failed addr=%s err=%v", addr, serveErr)
			}
		}()
	}

	startServer(cfg.Server.HTTPAddr, nil)

	if cfg.Server.EnableTLS {
		tlsCfg, err := buildTLSConfig(cfg)
		if err != nil {
			return fmt.Errorf("build tls config: %w", err)
		}
		startServer(cfg.Server.HTTPSAddr, tlsCfg)
	}

	<-ctx.Done()
	logger.Printf("INFO shutdown signal received, draining connections")

	grace := time.Duration(cfg.Server.ShutdownGraceSeconds) * time.Second
	shutdownCtx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()

	srvMu.Lock()
	for _, srv := range servers {
		if shutdownErr := srv.Shutdown(shutdownCtx); shutdownErr != nil {
			logger.Printf("ERROR graceful shutdown failed addr=%s err=%v", srv.Addr, shutdownErr)
		}
	}
	srvMu.Unlock()

	wg.Wait()
	logger.Printf("INFO shutdown complete")
	return nil
}

// buildTLSConfig constructs the tls.Config used by the HTTPS listener from
// the loaded Config's cert/key material and minimum TLS version setting.
func buildTLSConfig(cfg *Config) (*tls.Config, error) {
	if len(cfg.Server.TLS.CertPEM) == 0 || len(cfg.Server.TLS.KeyPEM) == 0 {
		return nil, fmt.Errorf("enable_tls is true but cert_file/key_file were not both provided")
	}
	cert, err := tls.X509KeyPair(cfg.Server.TLS.CertPEM, cfg.Server.TLS.KeyPEM)
	if err != nil {
		return nil, fmt.Errorf("load x509 key pair: %w", err)
	}

	minVersion := uint16(tls.VersionTLS12)
	if cfg.Server.TLS.MinVersion == "1.3" {
		minVersion = tls.VersionTLS13
	}

	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   minVersion,
	}, nil
}

// runHealthCheckCLI implements the `healthcheck` subcommand used by the
// container HEALTHCHECK directive: it performs a GET against the local
// process's own /healthz endpoint and returns a process exit code (0 for
// healthy, 1 otherwise) since the distroless runtime image has no shell
// or curl available.
func runHealthCheckCLI() int {
	addr := envOr("CONFIG_HEALTHCHECK_ADDR", "http://127.0.0.1:8080/healthz")

	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck request failed: %v\n", err)
		return 1
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return 0
	}
	fmt.Fprintf(os.Stderr, "healthcheck non-2xx status: %d\n", resp.StatusCode)
	return 1
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
