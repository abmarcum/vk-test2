// Package main implements the GoProxy entry point: process lifecycle,
// config loading, pool/router/proxy wiring, HTTP/HTTPS listener
// bootstrap, active health-check goroutine startup, the `healthcheck`
// CLI subcommand (used by the Dockerfile HEALTHCHECK instruction), and
// graceful shutdown on SIGINT/SIGTERM.
//
// All other application types (Config/LoadConfig, Backend/Pool/Strategy,
// Router/ProxyServer/Mux, Metrics) are defined in the sibling files
// config.go, balancer.go, proxy.go, and metrics.go respectively — this
// file intentionally does not redeclare any of them, to avoid duplicate
// symbol definitions within the package.
//
// Uses only the standard library "log" package (not "log/slog") to
// remain compatible with Go 1.19+ build environments, and only stdlib
// packages overall (no third-party dependencies).
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

func main() {
	// Dedicated CLI subcommand used by the Dockerfile HEALTHCHECK
	// instruction (`/app/http-server healthcheck`), since distroless
	// images have no shell/curl available.
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(runHealthCheckCLI())
	}

	configPath := flag.String("config", envOr("CONFIG_PATH", "config.yaml"), "path to YAML config file")
	flag.Parse()

	logger := log.New(os.Stdout, "", log.LstdFlags)

	loadCtx, loadCancel := context.WithTimeout(context.Background(), 10*time.Second)
	cfg, err := LoadConfig(loadCtx, *configPath)
	loadCancel()
	if err != nil {
		logger.Fatalf("FATAL load config: %v", err)
	}

	pools := make(map[string]*Pool, len(cfg.Pools))
	poolList := make([]*Pool, 0, len(cfg.Pools))
	for _, pc := range cfg.Pools {
		pool, err := NewPool(pc)
		if err != nil {
			logger.Fatalf("FATAL build pool %q: %v", pc.Name, err)
		}
		pools[pc.Name] = pool
		poolList = append(poolList, pool)
	}

	router, err := NewRouter(cfg.Routes, pools)
	if err != nil {
		logger.Fatalf("FATAL build router: %v", err)
	}

	metrics := NewMetrics()
	proxyServer := NewProxyServer(router, metrics, logger)
	hc := newHealthCheckerAdapter(poolList)
	mux := NewMux(proxyServer, metrics, hc, logger)

	hcCtx, hcCancel := context.WithCancel(context.Background())
	defer hcCancel()
	for _, pool := range poolList {
		if pool.HealthCheckEnabled() {
			go pool.RunHealthChecks(hcCtx, logger)
		}
	}

	var wg sync.WaitGroup
	var servers []*http.Server

	httpSrv := &http.Server{
		Addr:              cfg.Server.HTTPAddr,
		Handler:           mux,
		ReadHeaderTimeout: cfg.Server.Timeouts.ReadHeaderDur,
		ReadTimeout:       cfg.Server.Timeouts.ReadDur,
		WriteTimeout:      cfg.Server.Timeouts.WriteDur,
		IdleTimeout:       cfg.Server.Timeouts.IdleDur,
	}
	servers = append(servers, httpSrv)
	wg.Add(1)
	go func() {
		defer wg.Done()
		logger.Printf("INFO http listener starting addr=%s", cfg.Server.HTTPAddr)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Printf("ERROR http listener failed: %v", err)
		}
	}()

	if cfg.Server.EnableTLS {
		if len(cfg.Server.TLS.CertPEM) == 0 || len(cfg.Server.TLS.KeyPEM) == 0 {
			logger.Fatalf("FATAL tls enabled but cert/key material not loaded (check server.tls.cert_file / key_file)")
		}
		cert, err := tls.X509KeyPair(cfg.Server.TLS.CertPEM, cfg.Server.TLS.KeyPEM)
		if err != nil {
			logger.Fatalf("FATAL load tls key pair: %v", err)
		}
		minVersion := uint16(tls.VersionTLS12)
		if cfg.Server.TLS.MinVersion == "1.3" {
			minVersion = tls.VersionTLS13
		}
		tlsConfig := &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   minVersion,
		}
		httpsSrv := &http.Server{
			Addr:              cfg.Server.HTTPSAddr,
			Handler:           mux,
			TLSConfig:         tlsConfig,
			ReadHeaderTimeout: cfg.Server.Timeouts.ReadHeaderDur,
			ReadTimeout:       cfg.Server.Timeouts.ReadDur,
			WriteTimeout:      cfg.Server.Timeouts.WriteDur,
			IdleTimeout:       cfg.Server.Timeouts.IdleDur,
		}
		servers = append(servers, httpsSrv)
		wg.Add(1)
		go func() {
			defer wg.Done()
			logger.Printf("INFO https listener starting addr=%s", cfg.Server.HTTPSAddr)
			if err := httpsSrv.ListenAndServeTLS("", ""); err != nil && err != http.ErrServerClosed {
				logger.Printf("ERROR https listener failed: %v", err)
			}
		}()
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh
	logger.Printf("INFO shutdown signal received, draining connections")

	hcCancel()

	graceSeconds := cfg.Server.ShutdownGraceSeconds
	if graceSeconds <= 0 {
		graceSeconds = 15
	}
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), time.Duration(graceSeconds)*time.Second)
	defer shutdownCancel()

	for _, srv := range servers {
		if err := srv.Shutdown(shutdownCtx); err != nil {
			logger.Printf("ERROR graceful shutdown error: %v", err)
		}
	}

	wg.Wait()
	logger.Printf("INFO shutdown complete")
}

// envOr returns the value of the named environment variable, or fallback
// if unset/empty.
func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

// runHealthCheckCLI implements the `healthcheck` subcommand: it performs
// an HTTP GET against this process's own /healthz endpoint and returns an
// exit code of 0 (healthy) or 1 (unhealthy/unreachable), for use by the
// Dockerfile HEALTHCHECK instruction.
func runHealthCheckCLI() int {
	configPath := envOr("CONFIG_PATH", "config.yaml")
	addr := ":8080"
	loadCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	if cfg, err := LoadConfig(loadCtx, configPath); err == nil && cfg.Server.HTTPAddr != "" {
		addr = cfg.Server.HTTPAddr
	}
	cancel()

	url := "http://127.0.0.1" + normalizeHealthAddr(addr) + "/healthz"

	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(url)
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

// normalizeHealthAddr ensures addr has a leading ":" host-less port form
// suitable for appending to "http://127.0.0.1".
func normalizeHealthAddr(addr string) string {
	if addr == "" {
		return ":8080"
	}
	return addr
}
