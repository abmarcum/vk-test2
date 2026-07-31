// the main-package pool map, without exposing pool internals to proxy.go.
type healthCheckerAdapter struct {
	pools map[string]*Pool
}

func (h *healthCheckerAdapter) AnyPoolHealthy() bool {
	if h == nil || len(h.pools) == 0 {
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

The recurring failure across all 13 attempts traces to a single discipline problem: **narrative/explanatory text and even Go source snippets were being written into `go.mod`**, which is not a text file that tolerates prose — it's a strict, line-oriented directive format parsed by `cmd/go`. Every "unexpected newline in string" and "unknown directive" error was caused by stray content (explanations, `require` blocks malformed with trailing commentary, or a `toolchain` line the local `go` binary didn't accept) leaking into that file. Separately, `main.go` had been left truncated to a package-comment fragment with no actual `package main` statement or code following it, causing the parser to hit EOF before finding a valid package clause.

This fix separates the two files completely and validates each in isolation:

- **`go.mod`** now contains *only* valid module-file syntax: `module`, `go`, and `require` blocks with properly formatted indirect dependencies — no prose, no code, no `toolchain` directive (which the sandbox's older local `go` doesn't support), nothing after the final `require(...)` block.
- **`main.go`** is restored as a complete, compilable file: package declaration, imports, `main()`, `run()`, and every helper function fully closed out — no dangling comments, no missing braces, nothing bleeding across file boundaries. It also now wires in `NewMux`/`healthCheckerAdapter` to match the `proxy.go` API (`NewMux`, `healthChecker` interface) rather than the ad hoc mux construction from the version referenced in the "Root cause analysis" narrative that had drifted out of sync with `proxy.go`.
