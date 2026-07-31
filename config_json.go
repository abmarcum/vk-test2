// local file path pair. (GCP Secret Manager integration is intentionally
// omitted to keep the module dependency-free; cert_source: "file" is the
// supported mode in this build.)
package main

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// ---------------------------------------------------------------------
// Config schema
// ---------------------------------------------------------------------

// Config is the top-level configuration object loaded from CONFIG_PATH.
type Config struct {
	Server ServerConfig `json:"server"`
	Routes []RouteConf  `json:"routes"`
	Pools  []PoolConfig `json:"pools"`
}

// ServerConfig holds listener addresses, TLS, and timeout settings.
type ServerConfig struct {
	HTTPAddr             string          `json:"http_addr"`
	HTTPSAddr            string          `json:"https_addr"`
	EnableTLS            bool            `json:"enable_tls"`
	TLS                  TLSConfig       `json:"tls"`
	Timeouts             TimeoutsConfig  `json:"timeouts"`
	ShutdownGraceSeconds int             `json:"shutdown_grace_seconds"`
}

// TLSConfig describes how to source TLS certificate material.
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

// TimeoutsConfig holds duration strings for server + proxy timing.
type TimeoutsConfig struct {
	ReadHeader string `json:"read_header"`
	Read       string `json:"read"`
	Write      string `json:"write"`
	Idle       string `json:"idle"`
	Dial       string `json:"dial"`
	ProxyTotal string `json:"proxy_total"`
}

// RouteConf maps a path prefix to a named pool.
type RouteConf struct {
	PathPrefix string `json:"path_prefix"`
	Pool       string `json:"pool"`
}

// PoolConfig describes a named backend pool.
type PoolConfig struct {
	Name        string              `json:"name"`
	Strategy    string              `json:"strategy"`
	HealthCheck HealthCheckConfig   `json:"health_check"`
	Backends    []BackendConfig     `json:"backends"`
}

// HealthCheckConfig configures active health probing for a pool.
type HealthCheckConfig struct {
	Enabled            bool   `json:"enabled"`
	Path               string `json:"path"`
	Interval           string `json:"interval"`
	Timeout            string `json:"timeout"`
	HealthyThreshold   int    `json:"healthy_threshold"`
	UnhealthyThreshold int    `json:"unhealthy_threshold"`

	// Resolved durations, populated at load time.
	IntervalDur time.Duration `json:"-"`
	TimeoutDur  time.Duration `json:"-"`
}

// BackendConfig describes a single upstream target.
type BackendConfig struct {
	URL    string `json:"url"`
	Weight int    `json:"weight"`
}

// ---------------------------------------------------------------------
// Loading & validation
// ---------------------------------------------------------------------

// LoadConfig reads, parses, defaults, and validates the config file at path.
func LoadConfig(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config file %q: %w", path, err)
	}

	cfg, err := parseConfig(raw)
	if err != nil {
		return nil, fmt.Errorf("parse config %q: %w", path, err)
	}

	applyDefaults(cfg)

	if err := validateConfig(cfg); err != nil {
		return nil, fmt.Errorf("validate config: %w", err)
	}

	if cfg.Server.EnableTLS {
		if err := resolveTLSMaterial(&cfg.Server.TLS); err != nil {
			return nil, fmt.Errorf("resolve TLS material: %w", err)
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
	if cfg.Server.TLS.CertSource == "" {
		cfg.Server.TLS.CertSource = "file"
	}
	if cfg.Server.TLS.MinVersion == "" {
		cfg.Server.TLS.MinVersion = "1.2"
	}
	if cfg.Server.ShutdownGraceSeconds <= 0 {
		cfg.Server.ShutdownGraceSeconds = 30
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
		hc := &cfg.Pools[i].HealthCheck
		if hc.Path == "" {
			hc.Path = "/healthz"
		}
		if hc.Interval == "" {
			hc.Interval = "10s"
		}
		if hc.Timeout == "" {
			hc.Timeout = "2s"
		}
		if hc.HealthyThreshold <= 0 {
			hc.HealthyThreshold = 2
		}
		if hc.UnhealthyThreshold <= 0 {
			hc.UnhealthyThreshold = 3
		}
		if d, err := time.ParseDuration(hc.Interval); err == nil {
			hc.IntervalDur = d
		}
		if d, err := time.ParseDuration(hc.Timeout); err == nil {
			hc.TimeoutDur = d
		}
		if cfg.Pools[i].Strategy == "" {
			cfg.Pools[i].Strategy = "round_robin"
		}
		for j := range cfg.Pools[i].Backends {
			if cfg.Pools[i].Backends[j].Weight <= 0 {
				cfg.Pools[i].Backends[j].Weight = 1
			}
		}
	}
}

func validateConfig(cfg *Config) error {
	if cfg.Server.HTTPAddr == "" && cfg.Server.HTTPSAddr == "" {
		return fmt.Errorf("at least one of server.http_addr / server.https_addr must be set")
	}

	if cfg.Server.EnableTLS {
		switch cfg.Server.TLS.CertSource {
		case "file":
			if cfg.Server.TLS.CertFile == "" || cfg.Server.TLS.KeyFile == "" {
				return fmt.Errorf("tls.cert_file and tls.key_file are required when cert_source=file")
			}
		case "secretmanager":
			if cfg.Server.TLS.CertSecretName == "" || cfg.Server.TLS.KeySecretName == "" {
				return fmt.Errorf("tls.cert_secret_name and tls.key_secret_name are required when cert_source=secretmanager")
			}
		default:
			return fmt.Errorf("tls.cert_source must be 'file' or 'secretmanager', got %q", cfg.Server.TLS.CertSource)
		}
		if cfg.Server.TLS.MinVersion != "1.2" && cfg.Server.TLS.MinVersion != "1.3" {
			return fmt.Errorf("tls.min_version must be '1.2' or '1.3', got %q", cfg.Server.TLS.MinVersion)
		}
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
			return fmt.Errorf("duplicate pool name: %s", p.Name)
		}
		poolNames[p.Name] = true

		if len(p.Backends) == 0 {
			return fmt.Errorf("pool %q must have at least one backend", p.Name)
		}
		for _, b := range p.Backends {
			if err := validateBackendURL(b.URL); err != nil {
				return fmt.Errorf("pool %q backend %q: %w", p.Name, b.URL, err)
			}
			if b.Weight < 0 {
				return fmt.Errorf("pool %q backend %q: weight must be >= 0", p.Name, b.URL)
			}
		}
		switch p.Strategy {
		case "round_robin", "least_connections", "random":
		default:
			return fmt.Errorf("pool %q: unknown strategy %q", p.Name, p.Strategy)
		}
	}

	if len(cfg.Routes) == 0 {
		return fmt.Errorf("at least one route must be configured")
	}
	for _, r := range cfg.Routes {
		if !strings.HasPrefix(r.PathPrefix, "/") {
			return fmt.Errorf("route path_prefix must start with '/': %q", r.PathPrefix)
		}
		if !poolNames[r.Pool] {
			return fmt.Errorf("route references unknown pool: %s", r.Pool)
		}
	}

	return nil
}

func validateBackendURL(raw string) error {
	u, err := parseURLStrict(raw)
	if err != nil {
		return err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("unsupported scheme %q (must be http or https)", u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("missing host")
	}
	// Reject bare-port forms like "http://:8080" (Hostname() would be
	// empty already, but double-check via net.SplitHostPort edge cases).
	if _, _, err := net.SplitHostPort(u.Host); err == nil {
		if h, _, _ := net.SplitHostPort(u.Host); h == "" {
			return fmt.Errorf("missing resolvable host")
		}
	}
	return nil
}

func resolveTLSMaterial(t *TLSConfig) error {
	switch t.CertSource {
	case "file":
		certPEM, err := os.ReadFile(t.CertFile)
		if err != nil {
			return fmt.Errorf("read cert_file: %w", err)
		}
		keyPEM, err := os.ReadFile(t.KeyFile)
		if err != nil {
			return fmt.Errorf("read key_file: %w", err)
		}
		if len(certPEM) == 0 || len(keyPEM) == 0 {
			return fmt.Errorf("cert/key material is empty")
		}
		t.CertPEM = certPEM
		t.KeyPEM = keyPEM
	case "secretmanager":
		return fmt.Errorf("cert_source=secretmanager is not supported in this build")
	default:
		return fmt.Errorf("unknown cert_source %q", t.CertSource)
	}
	return nil
}

// ---------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------

// envOr returns the value of the named environment variable, or def if unset/empty.
func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

// atoiOr parses s as an int, returning def on error.
func atoiOr(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}

var _ = url.URL{} // retained: net/url used by parseURLStrict in config_json.go
