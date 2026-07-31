// Package main implements the GoProxy entry point: process lifecycle,
// config loading, TLS listener setup, HTTP/HTTPS server bootstrap, mux
// route wiring, signal handling, graceful shutdown, and the
// `healthcheck` CLI subcommand used by the Docker HEALTHCHECK directive.
//
// The Config/Pool/ProxyServer/Metrics types and their supporting logic
// live in config.go, balancer.go, proxy.go, and metrics.go respectively;
// this file only wires them together. It intentionally does not
// redeclare any type or function already defined in those files.
//
// Uses only the standard library "log" package (not "log/slog") to
// remain compatible with Go 1.19+ build environments, and only stdlib
// packages overall (no third-party dependencies).
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(runHealthcheckCLI())
	}

	if err := run(); err != nil {
		log.Printf("FATAL %v", err)
		os.Exit(1)
	}
}

func run() error {
	configPath := flag.String("config", envOr("CONFIG_PATH", "config.yaml"), "path to YAML config file")
	flag.Parse()

	logger := log.New(os.Stdout, "", log.LstdFlags|log.Lmicroseconds)

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
		pools[pool.Name] = pool
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

	errCh := make(chan error, 2)

	go func() {
		logger.Printf("INFO http listener starting addr=%s", cfg.Server.HTTPAddr)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("http server: %w", err)
		}
	}()

	var httpsSrv *http.Server
	if cfg.Server.EnableTLS {
		tlsCfg, err := buildTLSConfig(cfg.Server.TLS)
		if err != nil {
			return fmt.Errorf("build tls config: %w", err)
		}
		httpsSrv = &http.Server{
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
			logger.Printf("INFO https listener starting addr=%s", cfg.Server.HTTPSAddr)
			if err := httpsSrv.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- fmt.Errorf("https server: %w", err)
			}
		}()
	}

	select {
	case <-ctx.Done():
		logger.Printf("INFO shutdown signal received")
	case err := <-errCh:
		logger.Printf("ERROR %v", err)
	}

	hcCancel()

	grace := time.Duration(cfg.Server.ShutdownGraceSeconds) * time.Second
	shutdownCtx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()

	for _, srv := range servers {
		if err := srv.Shutdown(shutdownCtx); err != nil {
			logger.Printf("ERROR graceful shutdown: %v", err)
		}
	}

	return nil
}

// buildTLSConfig constructs a *tls.Config for the HTTPS listener from the
// already-loaded certificate/key PEM material and configured minimum TLS
// version.
func buildTLSConfig(cfg TLSConfig) (*tls.Config, error) {
	if len(cfg.CertPEM) == 0 || len(cfg.KeyPEM) == 0 {
		return nil, fmt.Errorf("tls enabled but cert/key material missing (cert_file/key_file)")
	}
	cert, err := tls.X509KeyPair(cfg.CertPEM, cfg.KeyPEM)
	if err != nil {
		return nil, fmt.Errorf("parse tls certificate/key: %w", err)
	}
	minVersion := uint16(tls.VersionTLS12)
	if cfg.MinVersion == "1.3" {
		minVersion = tls.VersionTLS13
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   minVersion,
	}, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// runHealthcheckCLI implements the `healthcheck` subcommand invoked by the
// Docker HEALTHCHECK directive: it performs an HTTP GET against this
// process's own /healthz endpoint and returns an exit code of 0 on
// success, 1 otherwise. It reads the listen address from the same config
// file/flag used by the server itself.
func runHealthcheckCLI() int {
	fs := flag.NewFlagSet("healthcheck", flag.ContinueOnError)
	configPath := fs.String("config", envOr("CONFIG_PATH", "config.yaml"), "path to YAML config file")
	_ = fs.Parse(os.Args[2:])

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	addr := ":8080"
	if cfg, err := LoadConfig(ctx, *configPath); err == nil {
		if cfg.Server.HTTPAddr != "" {
			addr = cfg.Server.HTTPAddr
		}
	}

	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		host, port = "", "8080"
	}
	if host == "" {
		host = "127.0.0.1"
	}
	if _, err := strconv.Atoi(port); err != nil {
		port = "8080"
	}

	url := fmt.Sprintf("http://%s:%s/healthz", host, port)
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}
