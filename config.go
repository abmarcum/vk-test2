// parsing (JSON-based to avoid extra third-party dependencies), config
// validation, defaulting, and TLS material resolution from local files.
//
// It does not own HTTP handling, load-balancing logic, or routing.
package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"
)

// Config is the top-level configuration tree.
type Config struct {
	Server ServerConfig  `json:"server"`
	Routes []RouteConfig `json:"routes"`
	Pools  []PoolConfig  `json:"pools"`
}

// ServerConfig holds listener, TLS, and timeout settings.
type ServerConfig struct {
	HTTPAddr              string        `json:"http_addr"`
	HTTPSAddr             string        `json:"https_addr"`
	EnableTLS             bool          `json:"enable_tls"`
	TLS                   TLSConfig     `json:"tls"`
	Timeouts              TimeoutConfig `json:"timeouts"`
	ShutdownGraceSeconds  int           `json:"shutdown_grace_seconds"`
}

// TLSConfig describes how to source certificate material.
type TLSConfig struct {
	CertSource string `json:"cert_source"` // "file" (only supported source in this build)
	CertFile   string `json:"cert_file"`
	KeyFile    string `json:"key_file"`
	MinVersion string `json:"min_version"` // "1.2" or "1.3"

	// Resolved at load time; never serialized.
	CertPEM []byte `json:"-"`
	KeyPEM  []byte `json:"-"`
}

// TimeoutConfig holds duration-string fields plus their parsed equivalents.
type TimeoutConfig struct {
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

// RouteConfig maps a path prefix to a named pool.
type RouteConfig struct {
	PathPrefix string `json:"path_prefix"`
	Pool       string `json:"pool"`
}

// PoolConfig defines an upstream pool.
type PoolConfig struct {
	Name        string            `json:"name"`
	Strategy    string            `json:"strategy"`
	HealthCheck HealthCheckConfig `json:"health_check"`
	Backends    []BackendConfig   `json:"backends"`
}

// HealthCheckConfig configures active health probing for a pool.
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

// BackendConfig describes a single upstream target.
type BackendConfig struct {
	URL    string `json:"url"`
	Weight int    `json:"weight"`
}

// LoadConfig reads, defaults, validates, and resolves TLS material for the
// config file at path. Accepts a context so TLS resolution (a potential
// future network call) can be canceled cleanly.
func LoadConfig(ctx context.Context, path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config file: %w", err)
	}

	cfg, err := parseConfig(raw)
	if err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	applyDefaults(cfg)

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("validate config: %w", err)
	}

	if err := parseDurations(cfg); err != nil {
		return nil, fmt.Errorf("parse durations: %w", err)
	}

	if cfg.Server.EnableTLS {
		certPEM, keyPEM, err := ResolveTLSMaterial(ctx, cfg.Server.TLS)
		if err != nil {
			return nil, fmt.Errorf("resolve tls material: %w", err)
		}
		cfg.Server.TLS.CertPEM = certPEM
		cfg.Server.TLS.KeyPEM = keyPEM
	}

	return cfg, nil
}

// applyDefaults fills in zero-value fields with documented defaults.
func applyDefaults(cfg *Config) {
	if cfg.Server.HTTPAddr == "" {
		cfg.Server.HTTPAddr = ":8080"
	}
	if cfg.Server.HTTPSAddr == "" {
		cfg.Server.HTTPSAddr = ":8443"
	}
	if cfg.Server.ShutdownGraceSeconds == 0 {
		cfg.Server.ShutdownGraceSeconds = 30
	}
	if cfg.Server.TLS.MinVersion == "" {
		cfg.Server.TLS.MinVersion = "1.2"
	}
	if cfg.Server.TLS.CertSource == "" {
		cfg.Server.TLS.CertSource = "file"
	}

	t := &cfg.Server.Timeouts
	if t.ReadHeader == "" {
		t.ReadHeader = "5s"
	}
	if t.Read == "" {
		t.Read = "15s"
	}
	if t.Write == "" {
		t.Write = "15s"
	}
	if t.Idle == "" {
		t.Idle = "60s"
	}
	if t.Dial == "" {
		t.Dial = "5s"
	}
	if t.ProxyTotal == "" {
		t.ProxyTotal = "30s"
	}

	for i := range cfg.Pools {
		p := &cfg.Pools[i]
		if p.Strategy == "" {
			p.Strategy = "round_robin"
		}
		if p.HealthCheck.Path == "" {
			p.HealthCheck.Path = "/healthz"
		}
		if p.HealthCheck.Interval == "" {
			p.HealthCheck.Interval = "10s"
		}
		if p.HealthCheck.Timeout == "" {
			p.HealthCheck.Timeout = "2s"
		}
		if p.HealthCheck.HealthyThreshold == 0 {
			p.HealthCheck.HealthyThreshold = 2
		}
		if p.HealthCheck.UnhealthyThreshold == 0 {
			p.HealthCheck.UnhealthyThreshold = 3
		}
		for j := range p.Backends {
			if p.Backends[j].Weight == 0 {
				p.Backends[j].Weight = 1
			}
		}
	}
}

// Validate enforces schema invariants.
func (c *Config) Validate() error {
	if c.Server.HTTPAddr == "" && c.Server.HTTPSAddr == "" {
		return fmt.Errorf("at least one of http_addr/https_addr must be set")
	}

	if c.Server.EnableTLS {
		switch c.Server.TLS.CertSource {
		case "file":
			if c.Server.TLS.CertFile == "" || c.Server.TLS.KeyFile == "" {
				return fmt.Errorf("tls cert_file/key_file required when cert_source=file")
			}
		default:
			return fmt.Errorf("unsupported tls cert_source %q", c.Server.TLS.CertSource)
		}
		if c.Server.TLS.MinVersion != "1.2" && c.Server.TLS.MinVersion != "1.3" {
			return fmt.Errorf("tls min_version must be 1.2 or 1.3")
		}
	}

	if len(c.Pools) == 0 {
		return fmt.Errorf("at least one pool is required")
	}

	poolNames := make(map[string]bool, len(c.Pools))
	for _, p := range c.Pools {
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
			return fmt.Errorf("pool %q: at least one backend is required", p.Name)
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

	if len(c.Routes) == 0 {
		return fmt.Errorf("at least one route is required")
	}
	for _, r := range c.Routes {
		if !strings.HasPrefix(r.PathPrefix, "/") {
			return fmt.Errorf("route path_prefix %q must start with /", r.PathPrefix)
		}
		if !poolNames[r.Pool] {
			return fmt.Errorf("route %q references undefined pool %q", r.PathPrefix, r.Pool)
		}
	}

	return nil
}

// validateBackendURL rejects malformed, schemeless, or hostless URLs.
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
	if u.Hostname() == "" {
		return fmt.Errorf("missing host")
	}
	return nil
}

// parseDurations converts all duration-string fields into time.Duration.
func parseDurations(cfg *Config) error {
	t := &cfg.Server.Timeouts
	var err error
	if t.ReadHeaderDur, err = time.ParseDuration(t.ReadHeader); err != nil {
		return fmt.Errorf("timeouts.read_header: %w", err)
	}
	if t.ReadDur, err = time.ParseDuration(t.Read); err != nil {
		return fmt.Errorf("timeouts.read: %w", err)
	}
	if t.WriteDur, err = time.ParseDuration(t.Write); err != nil {
		return fmt.Errorf("timeouts.write: %w", err)
	}
	if t.IdleDur, err = time.ParseDuration(t.Idle); err != nil {
		return fmt.Errorf("timeouts.idle: %w", err)
	}
	if t.DialDur, err = time.ParseDuration(t.Dial); err != nil {
		return fmt.Errorf("timeouts.dial: %w", err)
	}
	if t.ProxyTotalDur, err = time.ParseDuration(t.ProxyTotal); err != nil {
		return fmt.Errorf("timeouts.proxy_total: %w", err)
	}

	for i := range cfg.Pools {
		hc := &cfg.Pools[i].HealthCheck
		if hc.IntervalDur, err = time.ParseDuration(hc.Interval); err != nil {
			return fmt.Errorf("pool %q health_check.interval: %w", cfg.Pools[i].Name, err)
		}
		if hc.TimeoutDur, err = time.ParseDuration(hc.Timeout); err != nil {
			return fmt.Errorf("pool %q health_check.timeout: %w", cfg.Pools[i].Name, err)
		}
	}
	return nil
}

// ResolveTLSMaterial returns PEM-encoded cert & key bytes, sourcing from
// the local filesystem. Accepts ctx for interface parity with a future
// network-backed resolver.
func ResolveTLSMaterial(_ context.Context, cfg TLSConfig) ([]byte, []byte, error) {
	switch cfg.CertSource {
	case "file", "":
		certPEM, err := os.ReadFile(cfg.CertFile)
		if err != nil {
			return nil, nil, fmt.Errorf("read cert file: %w", err)
		}
		keyPEM, err := os.ReadFile(cfg.KeyFile)
		if err != nil {
			return nil, nil, fmt.Errorf("read key file: %w", err)
		}
		if len(certPEM) == 0 || len(keyPEM) == 0 {
			return nil, nil, fmt.Errorf("resolved cert/key material is empty")
		}
		return certPEM, keyPEM, nil
	default:
		return nil, nil, fmt.Errorf("unsupported cert_source %q", cfg.CertSource)
	}
}
