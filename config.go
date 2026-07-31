// validation, defaulting, and TLS material resolution (from local
// file or GCP Secret Manager). It does not own HTTP handling, load
// balancing, or routing logic.
package main

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the top-level configuration tree, unmarshaled from YAML.
type Config struct {
	Server ServerConf `yaml:"server"`
	Routes []RouteConf `yaml:"routes"`
	Pools  []PoolConf  `yaml:"pools"`
}

type ServerConf struct {
	HTTPAddr             string        `yaml:"http_addr"`
	HTTPSAddr            string        `yaml:"https_addr"`
	EnableTLS            bool          `yaml:"enable_tls"`
	TLS                  TLSConf       `yaml:"tls"`
	Timeouts             TimeoutsConf  `yaml:"timeouts"`
	ShutdownGraceSeconds int           `yaml:"shutdown_grace_seconds"`
}

type TLSConf struct {
	CertSource     string `yaml:"cert_source"`
	CertFile       string `yaml:"cert_file"`
	KeyFile        string `yaml:"key_file"`
	CertSecretName string `yaml:"cert_secret_name"`
	KeySecretName  string `yaml:"key_secret_name"`
	MinVersion     string `yaml:"min_version"`

	// Resolved material, populated at load time. Never serialized.
	CertPEM []byte `yaml:"-"`
	KeyPEM  []byte `yaml:"-"`
}

type TimeoutsConf struct {
	ReadHeader string `yaml:"read_header"`
	Read       string `yaml:"read"`
	Write      string `yaml:"write"`
	Idle       string `yaml:"idle"`
	Dial       string `yaml:"dial"`
	ProxyTotal string `yaml:"proxy_total"`
}

type RouteConf struct {
	PathPrefix string `yaml:"path_prefix"`
	Pool       string `yaml:"pool"`
}

type PoolConf struct {
	Name        string          `yaml:"name"`
	Strategy    string          `yaml:"strategy"`
	HealthCheck HealthCheckConf `yaml:"health_check"`
	Backends    []BackendConf   `yaml:"backends"`
}

type HealthCheckConf struct {
	Enabled            bool   `yaml:"enabled"`
	Path               string `yaml:"path"`
	Interval           string `yaml:"interval"`
	Timeout            string `yaml:"timeout"`
	HealthyThreshold   int    `yaml:"healthy_threshold"`
	UnhealthyThreshold int    `yaml:"unhealthy_threshold"`
}

type BackendConf struct {
	URL    string `yaml:"url"`
	Weight int    `yaml:"weight"`
}

// secretAccessor abstracts GCP Secret Manager access to allow hermetic
// unit testing without live GCP calls.
type secretAccessor interface {
	AccessSecret(ctx context.Context, name string) ([]byte, error)
}

// LoadConfig reads, parses, defaults, and validates the YAML file at path.
func LoadConfig(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	var cfg Config
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	cfg.applyDefaults()

	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	if cfg.Server.EnableTLS {
		certPEM, keyPEM, err := ResolveTLSMaterial(context.Background(), cfg.Server.TLS)
		if err != nil {
			return nil, fmt.Errorf("resolve TLS material: %w", err)
		}
		cfg.Server.TLS.CertPEM = certPEM
		cfg.Server.TLS.KeyPEM = keyPEM
	}

	return &cfg, nil
}

func (c *Config) applyDefaults() {
	if c.Server.HTTPAddr == "" {
		c.Server.HTTPAddr = ":8080"
	}
	if c.Server.HTTPSAddr == "" {
		c.Server.HTTPSAddr = ":8443"
	}
	if c.Server.ShutdownGraceSeconds == 0 {
		c.Server.ShutdownGraceSeconds = 30
	}
	if c.Server.TLS.CertSource == "" {
		c.Server.TLS.CertSource = "file"
	}
	if c.Server.TLS.MinVersion == "" {
		c.Server.TLS.MinVersion = "1.2"
	}

	t := &c.Server.Timeouts
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

	for i := range c.Pools {
		p := &c.Pools[i]
		if p.Strategy == "" {
			p.Strategy = "round_robin"
		}
		hc := &p.HealthCheck
		if hc.Path == "" {
			hc.Path = "/healthz"
		}
		if hc.Interval == "" {
			hc.Interval = "10s"
		}
		if hc.Timeout == "" {
			hc.Timeout = "2s"
		}
		if hc.HealthyThreshold == 0 {
			hc.HealthyThreshold = 2
		}
		if hc.UnhealthyThreshold == 0 {
			hc.UnhealthyThreshold = 3
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
		return fmt.Errorf("at least one of server.http_addr / server.https_addr must be set")
	}

	if c.Server.EnableTLS {
		switch c.Server.TLS.CertSource {
		case "file":
			if c.Server.TLS.CertFile == "" || c.Server.TLS.KeyFile == "" {
				return fmt.Errorf("tls.cert_file and tls.key_file are required when cert_source is 'file'")
			}
		case "secretmanager":
			if c.Server.TLS.CertSecretName == "" || c.Server.TLS.KeySecretName == "" {
				return fmt.Errorf("tls.cert_secret_name and tls.key_secret_name are required when cert_source is 'secretmanager'")
			}
		default:
			return fmt.Errorf("tls.cert_source must be 'file' or 'secretmanager', got %q", c.Server.TLS.CertSource)
		}
		if c.Server.TLS.MinVersion != "1.2" && c.Server.TLS.MinVersion != "1.3" {
			return fmt.Errorf("tls.min_version must be '1.2' or '1.3'")
		}
	}

	for _, tv := range []string{
		c.Server.Timeouts.ReadHeader, c.Server.Timeouts.Read, c.Server.Timeouts.Write,
		c.Server.Timeouts.Idle, c.Server.Timeouts.Dial, c.Server.Timeouts.ProxyTotal,
	} {
		if tv != "" {
			if _, err := time.ParseDuration(tv); err != nil {
				return fmt.Errorf("invalid duration %q: %w", tv, err)
			}
		}
	}

	if len(c.Pools) == 0 {
		return fmt.Errorf("at least one pool must be configured")
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

		if p.HealthCheck.Enabled {
			if !strings.HasPrefix(p.HealthCheck.Path, "/") {
				return fmt.Errorf("pool %q: health_check.path must start with '/'", p.Name)
			}
			if _, err := time.ParseDuration(p.HealthCheck.Interval); err != nil {
				return fmt.Errorf("pool %q: invalid health_check.interval: %w", p.Name, err)
			}
			if _, err := time.ParseDuration(p.HealthCheck.Timeout); err != nil {
				return fmt.Errorf("pool %q: invalid health_check.timeout: %w", p.Name, err)
			}
			if p.HealthCheck.HealthyThreshold <= 0 || p.HealthCheck.UnhealthyThreshold <= 0 {
				return fmt.Errorf("pool %q: health_check thresholds must be > 0", p.Name)
			}
		}
	}

	if len(c.Routes) == 0 {
		return fmt.Errorf("at least one route must be configured")
	}
	for _, r := range c.Routes {
		if !strings.HasPrefix(r.PathPrefix, "/") {
			return fmt.Errorf("route path_prefix %q must start with '/'", r.PathPrefix)
		}
		if r.Pool == "" {
			return fmt.Errorf("route %q: pool must be set", r.PathPrefix)
		}
		if !poolNames[r.Pool] {
			return fmt.Errorf("route %q references unknown pool %q", r.PathPrefix, r.Pool)
		}
	}

	return nil
}

func validateBackendURL(raw string) error {
	if raw == "" {
		return fmt.Errorf("url must not be empty")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("malformed url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("url scheme must be http or https")
	}
	if u.Hostname() == "" {
		return fmt.Errorf("url must include a resolvable host")
	}
	return nil
}

// ResolveTLSMaterial returns PEM-encoded cert & key bytes, sourcing from
// local filesystem or GCP Secret Manager per TLSConf.CertSource.
func ResolveTLSMaterial(ctx context.Context, cfg TLSConf) (certPEM, keyPEM []byte, err error) {
	switch cfg.CertSource {
	case "secretmanager":
		accessor, err := newDefaultSecretAccessor(ctx)
		if err != nil {
			return nil, nil, err
		}
		certPEM, err = accessor.AccessSecret(ctx, cfg.CertSecretName)
		if err != nil {
			return nil, nil, fmt.Errorf("access cert secret: %w", err)
		}
		keyPEM, err = accessor.AccessSecret(ctx, cfg.KeySecretName)
		if err != nil {
			return nil, nil, fmt.Errorf("access key secret: %w", err)
		}
	default: // "file"
		certPEM, err = os.ReadFile(cfg.CertFile)
		if err != nil {
			return nil, nil, fmt.Errorf("read cert file: %w", err)
		}
		keyPEM, err = os.ReadFile(cfg.KeyFile)
		if err != nil {
			return nil, nil, fmt.Errorf("read key file: %w", err)
		}
	}

	if len(certPEM) == 0 || len(keyPEM) == 0 {
		return nil, nil, fmt.Errorf("resolved certificate/key material is empty")
	}
	return certPEM, keyPEM, nil
}

// newDefaultSecretAccessor is a seam for tests to override; in production
// it would construct a real cloud.google.com/go/secretmanager client. Kept
// minimal here since GCP Secret Manager is not wired as a hard dependency
// in this build.
func newDefaultSecretAccessor(ctx context.Context) (secretAccessor, error) {
	return nil, fmt.Errorf("secretmanager cert_source is not available in this build; use cert_source: file")
}
