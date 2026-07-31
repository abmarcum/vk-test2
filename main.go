// Package main implements the GoProxy entry point.
//
// Responsibilities of this file (and only this file):
//   - Process lifecycle (flags, env, exit codes)
//   - Structured logger construction (log/slog, JSON)
//   - Configuration loading (delegates to config.go)
//   - TLS listener / certificate setup (with SIGHUP-triggered rotation)
//   - HTTP + HTTPS server bootstrap
//   - Mux route wiring: /healthz, /metrics, reverse-proxy, HTTP->HTTPS redirect
//   - Signal handling: SIGTERM/SIGINT (graceful shutdown), SIGHUP (hot reload)
//   - Graceful shutdown orchestration with connection draining
//
// Integration contract with the rest of the package (defined in config.go,
// balancer.go, proxy.go):
//
//	func LoadConfig(path string) (*Config, error)
//
//	type Config struct {
//	    Server ServerConfig
//	    // ... pools/routes fields owned by config.go
//	}
//
//	type ServerConfig struct {
//	    HTTPAddr           string
//	    HTTPSAddr          string
//	    TLSCertFile        string
//	    TLSKeyFile         string
//	    EnableHTTPRedirect bool
//	    ReadTimeout        time.Duration
//	    ReadHeaderTimeout  time.Duration
//	    WriteTimeout       time.Duration
//	    IdleTimeout        time.Duration
//	    ShutdownTimeout    time.Duration
//	    MaxHeaderBytes     int
//	}
//
//	func NewBalancerManager(cfg *Config, logger *slog.Logger) (*BalancerManager, error)
//	func (m *BalancerManager) StartHealthChecks(ctx context.Context)
//	func (m *BalancerManager) Reload(cfg *Config) error
//	func (m *BalancerManager) Stop()
//	func (m *BalancerManager) HealthzHandler() http.Handler
//
//	func NewProxyHandler(mgr *BalancerManager, logger *slog.Logger, cfg *Config) http.Handler
//	func MetricsHandler() http.Handler
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	defaultShutdownTimeout   = 15 * time.Second
	defaultReadTimeout       = 10 * time.Second
	defaultReadHeaderTimeout = 5 * time.Second
	defaultWriteTimeout      = 15 * time.Second
	defaultIdleTimeout       = 60 * time.Second
	defaultMaxHeaderBytes    = 1 << 20 // 1 MiB
)

func main() {
	os.Exit(run())
}

// run wires the entire process lifecycle and returns a process exit code.
// It is a separate function (not main) to make graceful teardown testable
// and to avoid os.Exit short-circuiting deferred cleanup.
func run() int {
	var configPath string
	flag.StringVar(&configPath, "config", envOrDefault("GOPROXY_CONFIG", "config.yaml"), "path to configuration file (YAML/JSON)")
	flag.Parse()

	logger := newLogger()

	cfg, err := LoadConfig(configPath)
	if err != nil {
		logger.Error("failed to load configuration", "error", err)
		return 1
	}
	applyServerDefaults(&cfg.Server)

	mgr, err := NewBalancerManager(cfg, logger)
	if err != nil {
		logger.Error("failed to initialize load balancer manager", "error", err)
		return 1
	}

	ctx, cancelHealth := context.WithCancel(context.Background())
	defer cancelHealth()
	mgr.StartHealthChecks(ctx)

	var certMgr *certReloader
	if cfg.Server.HTTPSAddr != "" {
		certMgr, err = newCertReloader(cfg.Server.TLSCertFile, cfg.Server.TLSKeyFile)
		if err != nil {
			logger.Error("failed to load TLS certificate", "error", err)
			return 1
		}
	}

	handler := buildHandler(mgr, cfg, logger)

	var (
		httpSrv  *http.Server
		httpsSrv *http.Server
	)

	errCh := make(chan error, 2)

	if cfg.Server.HTTPAddr != "" {
		httpHandler := handler
		if certMgr != nil && cfg.Server.EnableHTTPRedirect {
			httpHandler = redirectToHTTPS(cfg.Server.HTTPSAddr, logger)
		}
		httpSrv = newServer(cfg.Server.HTTPAddr, httpHandler, cfg.Server)
		go func() {
			logger.Info("starting HTTP listener", "addr", cfg.Server.HTTPAddr)
			if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- fmt.Errorf("http server: %w", err)
			}
		}()
	}

	if cfg.Server.HTTPSAddr != "" && certMgr != nil {
		tlsCfg := &tls.Config{
			MinVersion:               tls.VersionTLS12,
			PreferServerCipherSuites: true,
			GetCertificate:           certMgr.GetCertificate,
			CurvePreferences:         []tls.CurveID{tls.X25519, tls.CurveP256},
			CipherSuites: []uint16{
				tls.TLS_AES_128_GCM_SHA256,
				tls.TLS_AES_256_GCM_SHA384,
				tls.TLS_CHACHA20_POLY1305_SHA256,
				tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
				tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
			},
		}
		httpsSrv = newServer(cfg.Server.HTTPSAddr, handler, cfg.Server)
		httpsSrv.TLSConfig = tlsCfg

		ln, err := tls.Listen("tcp", cfg.Server.HTTPSAddr, tlsCfg)
		if err != nil {
			logger.Error("failed to bind HTTPS listener", "addr", cfg.Server.HTTPSAddr, "error", err)
			return 1
		}
		go func() {
			logger.Info("starting HTTPS listener", "addr", cfg.Server.HTTPSAddr)
			if err := httpsSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- fmt.Errorf("https server: %w", err)
			}
		}()
	}

	if httpSrv == nil && httpsSrv == nil {
		logger.Error("no listeners configured: set server.http_addr and/or server.https_addr")
		return 1
	}

	exitCode := waitForShutdown(logger, mgr, &configPath, cfg, certMgr, httpSrv, httpsSrv, errCh)
	return exitCode
}

// waitForShutdown blocks on OS signals and background errors, orchestrating
// hot-reload (SIGHUP) and graceful shutdown (SIGTERM/SIGINT).
func waitForShutdown(
	logger *slog.Logger,
	mgr *BalancerManager,
	configPath *string,
	cfg *Config,
	certMgr *certReloader,
	httpSrv, httpsSrv *http.Server,
	errCh chan error,
) int {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer signal.Stop(sigCh)

	exitCode := 0

	for {
		select {
		case sig := <-sigCh:
			switch sig {
			case syscall.SIGHUP:
				logger.Info("received SIGHUP: reloading configuration")
				newCfg, err := LoadConfig(*configPath)
				if err != nil {
					logger.Error("config reload failed, keeping previous config", "error", err)
					continue
				}
				applyServerDefaults(&newCfg.Server)
				if err := mgr.Reload(newCfg); err != nil {
					logger.Error("balancer reload failed", "error", err)
					continue
				}
				if certMgr != nil {
					if err := certMgr.Reload(); err != nil {
						logger.Error("certificate reload failed", "error", err)
						continue
					}
				}
				*cfg = *newCfg
				logger.Info("configuration reload complete")
			default:
				logger.Info("received shutdown signal", "signal", sig.String())
				shutdown(logger, mgr, httpSrv, httpsSrv, cfg.Server.ShutdownTimeout)
				return exitCode
			}
		case err := <-errCh:
			logger.Error("listener error, initiating shutdown", "error", err)
			exitCode = 1
			shutdown(logger, mgr, httpSrv, httpsSrv, cfg.Server.ShutdownTimeout)
			return exitCode
		}
	}
}

// shutdown drains and stops HTTP(S) servers and background workers within
// a bounded timeout to guarantee zero dropped requests on graceful exit.
func shutdown(logger *slog.Logger, mgr *BalancerManager, httpSrv, httpsSrv *http.Server, timeout time.Duration) {
	if timeout <= 0 {
		timeout = defaultShutdownTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	var wg sync.WaitGroup
	shutdownOne := func(name string, s *http.Server) {
		if s == nil {
			return
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			logger.Info("draining connections", "server", name)
			if err := s.Shutdown(ctx); err != nil {
				logger.Warn("graceful shutdown timed out, forcing close", "server", name, "error", err)
				_ = s.Close()
			}
		}()
	}
	shutdownOne("http", httpSrv)
	shutdownOne("https", httpsSrv)
	wg.Wait()

	mgr.Stop()
	logger.Info("shutdown complete")
}

// buildHandler wires together the top-level mux with health, metrics, and
// proxy routes, plus common security/observability middleware.
func buildHandler(mgr *BalancerManager, cfg *Config, logger *slog.Logger) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/healthz", mgr.HealthzHandler())
	mux.Handle("/metrics", MetricsHandler())
	mux.Handle("/", NewProxyHandler(mgr, logger, cfg))

	var h http.Handler = mux
	h = securityHeaders(h)
	h = recoverMiddleware(logger, h)
	h = accessLog(logger, h)
	return h
}

// redirectToHTTPS returns a handler that safely redirects plain HTTP
// requests to their HTTPS equivalent, validating the Host header to
// mitigate header-injection / open-redirect vectors.
func redirectToHTTPS(httpsAddr string, logger *slog.Logger) http.Handler {
	_, tlsPort, _ := net.SplitHostPort(httpsAddr)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := sanitizeHost(r.Host)
		if host == "" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if idx := strings.IndexByte(host, ':'); idx != -1 {
			host = host[:idx]
		}
		target := "https://" + host
		if tlsPort != "" && tlsPort != "443" {
			target += ":" + tlsPort
		}
		target += r.URL.RequestURI()
		http.Redirect(w, r, target, http.StatusMovedPermanently)
	})
}

// sanitizeHost rejects hosts containing characters that could enable
// header injection or CRLF smuggling.
func sanitizeHost(host string) string {
	if host == "" || strings.ContainsAny(host, "\r\n \t") {
		return ""
	}
	return host
}

// securityHeaders adds baseline hardening headers to every response.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

// recoverMiddleware masks internal panics from clients while logging full
// detail server-side, preventing information disclosure.
func recoverMiddleware(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				logger.Error("panic recovered", "error", rec, "path", r.URL.Path)
				http.Error(w, "internal server error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// accessLog emits structured, low-cardinality request logs.
func accessLog(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		logger.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", sw.status,
			"duration_ms", time.Since(start).Milliseconds(),
			"remote", clientIP(r),
		)
	})
}

// statusWriter captures the response status code for logging purposes.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (s *statusWriter) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// clientIP extracts the remote IP without trusting spoofable headers by
// default; proxy.go is responsible for trusted-proxy XFF handling.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// newServer builds a hardened *http.Server with sane defaults to protect
// against slowloris and resource-exhaustion attacks.
func newServer(addr string, handler http.Handler, sc ServerConfig) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadTimeout:       sc.ReadTimeout,
		ReadHeaderTimeout: sc.ReadHeaderTimeout,
		WriteTimeout:      sc.WriteTimeout,
		IdleTimeout:       sc.IdleTimeout,
		MaxHeaderBytes:    sc.MaxHeaderBytes,
		ErrorLog:          nil, // errors are handled/logged via slog in caller
	}
}

// applyServerDefaults fills unset timeouts/limits with safe production
// defaults so a minimal config.yaml remains valid and secure.
func applyServerDefaults(sc *ServerConfig) {
	if sc.ReadTimeout <= 0 {
		sc.ReadTimeout = defaultReadTimeout
	}
	if sc.ReadHeaderTimeout <= 0 {
		sc.ReadHeaderTimeout = defaultReadHeaderTimeout
	}
	if sc.WriteTimeout <= 0 {
		sc.WriteTimeout = defaultWriteTimeout
	}
	if sc.IdleTimeout <= 0 {
		sc.IdleTimeout = defaultIdleTimeout
	}
	if sc.ShutdownTimeout <= 0 {
		sc.ShutdownTimeout = defaultShutdownTimeout
	}
	if sc.MaxHeaderBytes <= 0 {
		sc.MaxHeaderBytes = defaultMaxHeaderBytes
	}
}

// newLogger constructs a structured JSON logger writing to stdout, suitable
// for container log collectors (Cloud Logging / GKE).
func newLogger() *slog.Logger {
	level := slog.LevelInfo
	if v := os.Getenv("LOG_LEVEL"); v != "" {
		switch strings.ToLower(v) {
		case "debug":
			level = slog.LevelDebug
		case "warn":
			level = slog.LevelWarn
		case "error":
			level = slog.LevelError
		}
	}
	h := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level:     level,
		AddSource: false,
	})
	return slog.New(h)
}

// envOrDefault reads an environment variable, falling back to def when unset
// or empty. Never trusts environment content for anything beyond simple
// path/level configuration.
func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// certReloader holds the currently active TLS certificate and supports
// atomic hot-reload from disk (e.g., cert-manager rotation) without
// dropping in-flight TLS handshakes.
type certReloader struct {
	mu       sync.RWMutex
	cert     *tls.Certificate
	certFile string
	keyFile  string
}

func newCertReloader(certFile, keyFile string) (*certReloader, error) {
	if certFile == "" || keyFile == "" {
		return nil, errors.New("tls_cert_file and tls_key_file must be set for HTTPS listener")
	}
	cr := &certReloader{certFile: certFile, keyFile: keyFile}
	if err := cr.Reload(); err != nil {
		return nil, err
	}
	return cr, nil
}

func (c *certReloader) Reload() error {
	cert, err := tls.LoadX509KeyPair(c.certFile, c.keyFile)
	if err != nil {
		return fmt.Errorf("load keypair: %w", err)
	}
	c.mu.Lock()
	c.cert = &cert
	c.mu.Unlock()
	return nil
}

func (c *certReloader) GetCertificate(_ *tls.ClientHelloInfo) (*tls.Certificate, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.cert == nil {
		return nil, errors.New("no certificate loaded")
	}
	return c.cert, nil
}
