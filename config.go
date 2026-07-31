// Timeouts, Routes, Pools, HealthCheck, Backend), JSON-based config
// parsing via LoadConfig, config validation, and TLS material resolution
// from a local file path pair. (GCP Secret Manager integration is stubbed
// as an explicit unsupported error to keep the dependency graph at zero
// external packages; local "file" cert_source is fully supported.)
package main

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Config structs
// ---------------------------------------------------------------------------

type Config struct {
	Server Server  `json:"server"`
	Routes []Route `json:"routes"`
	Pools  []PoolConfig `json:"pools"`
}

type Server struct {
	HTTPAddr             string   `json:"http_addr"`
	HTTPSAddr            string   `json:"https_addr"`
	EnableTLS            bool     `json:"enable_tls"`
	TLS                  TLSConfig `json:"tls"`
	Timeouts             Timeouts `json:"timeouts"`
	ShutdownGraceSeconds int      `json:"shutdown_grace_seconds"`
}

type TLSConfig struct {
	CertSource     string `json:"cert_source"`
	CertFile       string `json:"cert_file"`
	KeyFile        string `json:"key_file"`
	CertSecretName string `json:"cert_secret_name"`
	KeySecretName  string `json:"key_secret_name"`
	MinVersion     string `json:"min_version"`

	// Resolved at load time; never serialized back out.
	CertPEM []byte `json:"-"`
	KeyPEM  []byte `json:"-"`
}

type Timeouts struct {
	ReadHeader string `json:"read_header"`
	Read       string `json:"read"`
	Write      string `json:"write"`
	Idle       string `json:"idle"`
	Dial       string `json:"dial"`
	ProxyTotal string `json:"proxy_total"`

	ReadHeaderDur time.Duration `json:"-"`
	ReadDur       time.Duration `json:"-"`
	WriteDur      time.Duration `json:"-"`
	IdleDur       time.Duration `json:"-"`
	DialDur       time.Duration `json:"-"`
	ProxyTotalDur time.Duration `json:"-"`
}

type Route struct {
	PathPrefix string `json:"path_prefix"`
	Pool       string `json:"pool"`
}

type PoolConfig struct {
	Name        string            `json:"name"`
	Strategy    string            `json:"strategy"`
	HealthCheck HealthCheckConfig `json:"health_check"`
	Backends    []BackendConfig   `json:"backends"`
}

type HealthCheckConfig struct {
	Enabled            bool   `json:"enabled"`
	Path               string `json:"path"`
	Interval           string `json:"interval"`
	Timeout            string `json:"timeout"`
	HealthyThreshold   int    `json:"healthy_threshold"`
	UnhealthyThreshold int    `json:"unhealthy_threshold"`

	IntervalDur time.Duration `json:"-"`
	TimeoutDur  time.Duration `json:"-"`
}

type BackendConfig struct {
	URL    string `json:"url"`
	Weight int    `json:"weight"`
}

// ---------------------------------------------------------------------------
// Loading
// ---------------------------------------------------------------------------

// LoadConfig reads, parses, defaults, and validates the config file at path.
func LoadConfig(ctx context.Context, path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config file %q: %w", path, err)
	}

	cfg, err := parseConfig(raw)
	if err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	applyDefaults(cfg)

	if err := validate(cfg); err != nil {
		return nil, fmt.Errorf("validate config: %w", err)
	}

	if cfg.Server.EnableTLS {
		if err := resolveTLSMaterial(ctx, &cfg.Server.TLS); err != nil {
			return nil, fmt.Errorf("resolve tls material: %w", err)
		}
	}

	return cfg, nil
}

func applyDefaults(cfg *Config) {
	if cfg.Server.HTTPAddr == "" {
		cfg.Server.HTTPAddr = ":8080"
	}
	if cfg.Server.HTTPSAddr == "" {
		cfg.Server.HTTPSAddr = ":8443"
	}
	if cfg.Server.ShutdownGraceSeconds <= 0 {
		cfg.Server.ShutdownGraceSeconds = 30
	}
	if cfg.Server.TLS.CertSource == "" {
		cfg.Server.TLS.CertSource = "file"
	}
	if cfg.Server.TLS.MinVersion == "" {
		cfg.Server.TLS.MinVersion = "1.2"
	}

	t := &cfg.Server.Timeouts
	t.ReadHeaderDur = durOrDefault(t.ReadHeader, 5*time.Second)
	t.ReadDur = durOrDefault(t.Read, 15*time.Second)
	t.WriteDur = durOrDefault(t.Write, 15*time.Second)
	t.IdleDur = durOrDefault(t.Idle, 60*time.Second)
	t.DialDur = durOrDefault(t.Dial, 5*time.Second)
	t.ProxyTotalDur = durOrDefault(t.ProxyTotal, 30*time.Second)

	for i := range cfg.Pools {
		p := &cfg.Pools[i]
		if p.Strategy == "" {
			p.Strategy = "round_robin"
		}
		hc := &p.HealthCheck
		if hc.Path == "" {
			hc.Path = "/healthz"
		}
		hc.IntervalDur = durOrDefault(hc.Interval, 10*time.Second)
		hc.TimeoutDur = durOrDefault(hc.Timeout, 2*time.Second)
		if hc.HealthyThreshold <= 0 {
			hc.HealthyThreshold = 2
		}
		if hc.UnhealthyThreshold <= 0 {
			hc.UnhealthyThreshold = 3
		}
		for j := range p.Backends {
			if p.Backends[j].Weight <= 0 {
				p.Backends[j].Weight = 1
			}
		}
	}
}

func durOrDefault(s string, fallback time.Duration) time.Duration {
	if s == "" {
		return fallback
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return fallback
	}
	return d
}

func validate(cfg *Config) error {
	if cfg.Server.HTTPAddr == "" && cfg.Server.HTTPSAddr == "" {
		return fmt.Errorf("at least one of http_addr/https_addr must be set")
	}

	if len(cfg.Pools) == 0 {
		return fmt.Errorf("at least one pool must be configured")
	}

	poolNames := make(map[string]bool, len(cfg.Pools))
	for _, p := range cfg.Pools {
		if p.Name == "" {
			return fmt.Errorf("pool name must not be empty")
		}
		if poolNames[p.Name] {
			return fmt.Errorf("duplicate pool name %q", p.Name)
		}
		poolNames[p.Name] = true

		switch p.Strategy {
		case "round_robin", "least_connections", "random":
		default:
			return fmt.Errorf("pool %q: invalid strategy %q", p.Name, p.Strategy)
		}

		if len(p.Backends) == 0 {
			return fmt.Errorf("pool %q: at least one backend required", p.Name)
		}
		for _, b := range p.Backends {
			if err := validateBackendURL(b.URL); err != nil {
				return fmt.Errorf("pool %q: backend %q: %w", p.Name, b.URL, err)
			}
			if b.Weight < 0 {
				return fmt.Errorf("pool %q: backend %q: weight must be >= 0", p.Name, b.URL)
			}
		}
	}

	if len(cfg.Routes) == 0 {
		return fmt.Errorf("at least one route must be configured")
	}
	for _, r := range cfg.Routes {
		if !strings.HasPrefix(r.PathPrefix, "/") {
			return fmt.Errorf("route path_prefix %q must start with '/'", r.PathPrefix)
		}
		if !poolNames[r.Pool] {
			return fmt.Errorf("route %q references unknown pool %q", r.PathPrefix, r.Pool)
		}
	}

	if cfg.Server.EnableTLS {
		switch cfg.Server.TLS.CertSource {
		case "file":
			if cfg.Server.TLS.CertFile == "" || cfg.Server.TLS.KeyFile == "" {
				return fmt.Errorf("tls cert_source=file requires cert_file and key_file")
			}
		case "secretmanager":
			if cfg.Server.TLS.CertSecretName == "" || cfg.Server.TLS.KeySecretName == "" {
				return fmt.Errorf("tls cert_source=secretmanager requires cert_secret_name and key_secret_name")
			}
		default:
			return fmt.Errorf("invalid tls cert_source %q", cfg.Server.TLS.CertSource)
		}
		if cfg.Server.TLS.MinVersion != "1.2" && cfg.Server.TLS.MinVersion != "1.3" {
			return fmt.Errorf("invalid tls min_version %q", cfg.Server.TLS.MinVersion)
		}
	}

	return nil
}

func validateBackendURL(raw string) error {
	if raw == "" {
		return fmt.Errorf("url must not be empty")
	}
	u, err := parseURLStrict(raw)
	if err != nil {
		return err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("unsupported scheme %q", u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("missing host")
	}
	// Reject hosts that don't look resolvable/parseable at all (best
	// effort; a literal IP or hostname string is acceptable, we just
	// guard against empty/garbage values slipping through url.Parse).
	if net.ParseIP(host) == nil && !isPlausibleHostname(host) {
		return fmt.Errorf("host %q does not look like a valid hostname or IP", host)
	}
	return nil
}

func isPlausibleHostname(h string) bool {
	if h == "" || len(h) > 253 {
		return false
	}
	for _, r := range h {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '.') {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// TLS material resolution
// ---------------------------------------------------------------------------

// resolveTLSMaterial populates CertPEM/KeyPEM on cfg from the configured
// source. GCP Secret Manager is not wired into this build (kept
// dependency-free); "file" is fully supported.
func resolveTLSMaterial(ctx context.Context, cfg *TLSConfig) error {
	_ = ctx
	switch cfg.CertSource {
	case "file":
		certPEM, err := os.ReadFile(cfg.CertFile)
		if err != nil {
			return fmt.Errorf("read cert_file: %w", err)
		}
		keyPEM, err := os.ReadFile(cfg.KeyFile)
		if err != nil {
			return fmt.Errorf("read key_file: %w", err)
		}
		if len(certPEM) == 0 || len(keyPEM) == 0 {
			return fmt.Errorf("resolved cert/key material is empty")
		}
		cfg.CertPEM = certPEM
		cfg.KeyPEM = keyPEM
		return nil
	case "secretmanager":
		return fmt.Errorf("cert_source=secretmanager is not supported in this build")
	default:
		return fmt.Errorf("unknown cert_source %q", cfg.CertSource)
	}
}

// unused import guard (keeps url import if parseURLStrict signature changes)
var _ = url.URL{}
