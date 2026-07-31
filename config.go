// parsing/validation, defaulting, and TLS material resolution (from local
// file or GCP Secret Manager). It does not own HTTP handling, load
// balancing algorithm internals, or routing/proxy logic.
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

// ---------------------------------------------------------------------------
// Config struct tree
// ---------------------------------------------------------------------------

type Config struct {
	Server ServerConfig  `yaml:"server"`
	Routes []RouteConfig `yaml:"routes"`
	Pools  []PoolConfig  `yaml:"pools"`
}

type ServerConfig struct {
	HTTPAddr              string        `yaml:"http_addr"`
	HTTPSAddr             string        `yaml:"https_addr"`
	EnableTLS             bool          `yaml:"enable_tls"`
	TLS                   TLSConfig     `yaml:"tls"`
	Timeouts              TimeoutConfig `yaml:"timeouts"`
	ShutdownGraceSeconds  int           `yaml:"shutdown_grace_seconds"`
}

type TLSConfig struct {
	CertSource     string `yaml:"cert_source"` // "file" | "secretmanager"
	CertFile       string `yaml:"cert_file"`
	KeyFile        string `yaml:"key_file"`
	CertSecretName string `yaml:"cert_secret_name"`
	KeySecretName  string `yaml:"key_secret_name"`
	MinVersion     string `yaml:"min_version"` // "1.2" | "1.3"

	// Resolved material, populated by ResolveTLSMaterial. Never serialized.
	CertPEM []byte `yaml:"-"`
	KeyPEM  []byte `yaml:"-"`
}

type TimeoutConfig struct {
	ReadHeader string `yaml:"read_header"`
	Read       string `yaml:"read"`
	Write      string `yaml:"write"`
	Idle       string `yaml:"idle"`
	Dial       string `yaml:"dial"`
	ProxyTotal string `yaml:"proxy_total"`

	// Resolved durations, populated during Validate()/defaulting.
	ReadHeaderDur time.Duration `yaml:"-"`
	ReadDur       time.Duration `yaml:"-"`
	WriteDur      time.Duration `yaml:"-"`
	IdleDur       time.Duration `yaml:"-"`
	DialDur       time.Duration `yaml:"-"`
	ProxyTotalDur time.Duration `yaml:"-"`
}

type RouteConfig struct {
	PathPrefix string `yaml:"path_prefix"`
	Pool       string `yaml:"pool"`
}

type PoolConfig struct {
	Name        string            `yaml:"name"`
	Strategy    string            `yaml:"strategy"`
	HealthCheck HealthCheckConfig `yaml:"health_check"`
	Backends    []BackendConfig   `yaml:"backends"`
}

type HealthCheckConfig struct {
	Enabled            bool   `yaml:"enabled"`
	Path               string `yaml:"path"`
	Interval           string `yaml:"interval"`
	Timeout            string `yaml:"timeout"`
	HealthyThreshold   int    `yaml:"healthy_threshold"`
	UnhealthyThreshold int    `yaml:"unhealthy_threshold"`

	IntervalDur time.Duration `yaml:"-"`
	TimeoutDur  time.Duration `yaml:"-"`
}

type BackendConfig struct {
	URL    string `yaml:"url"`
	Weight int    `yaml:"weight"`
}

// ---------------------------------------------------------------------------
// Loading
// ---------------------------------------------------------------------------

// LoadConfig reads, parses, defaults, validates, and resolves TLS material
// for the YAML file at path.
func LoadConfig(ctx context.Context, path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config file: %w", err)
	}

	var cfg Config
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse yaml: %w", err)
	}

	cfg.applyDefaults()

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("validate config: %w", err)
	}

	if cfg.Server.EnableTLS {
		certPEM, keyPEM, err := ResolveTLSMaterial(ctx, cfg.Server.TLS)
		if err != nil {
			return nil, fmt.Errorf("resolve tls material: %w", err)
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

// ---------------------------------------------------------------------------
// Validation
// ---------------------------------------------------------------------------

func (c *Config) Validate() error {
	if c.Server.HTTPAddr == "" && c.Server.HTTPSAddr == "" {
		return fmt.Errorf("at least one of server.http_addr/https_addr must be set")
	}

	t := &c.Server.Timeouts
	durs := []struct {
		name string
		str  string
		dst  *time.Duration
	}{
		{"read_header", t.ReadHeader, &t.ReadHeaderDur},
		{"read", t.Read, &t.ReadDur},
		{"write", t.Write, &t.WriteDur},
		{"idle", t.Idle, &t.IdleDur},
		{"dial", t.Dial, &t.DialDur},
		{"proxy_total", t.ProxyTotal, &t.ProxyTotalDur},
	}
	for _, d := range durs {
		parsed, err := time.ParseDuration(d.str)
		if err != nil {
			return fmt.Errorf("timeouts.%s: invalid duration %q: %w", d.name, d.str, err)
		}
		*d.dst = parsed
	}

	if c.Server.EnableTLS {
		switch c.Server.TLS.CertSource {
		case "file":
			if c.Server.TLS.CertFile == "" || c.Server.TLS.KeyFile == "" {
				return fmt.Errorf("tls.cert_source=file requires cert_file and key_file")
			}
		case "secretmanager":
			if c.Server.TLS.CertSecretName == "" || c.Server.TLS.KeySecretName == "" {
				return fmt.Errorf("tls.cert_source=secretmanager requires cert_secret_name and key_secret_name")
			}
		default:
			return fmt.Errorf("tls.cert_source must be 'file' or 'secretmanager', got %q", c.Server.TLS.CertSource)
		}
		if c.Server.TLS.MinVersion != "1.2" && c.Server.TLS.MinVersion != "1.3" {
			return fmt.Errorf("tls.min_version must be '1.2' or '1.3', got %q", c.Server.TLS.MinVersion)
		}
	}

	if len(c.Pools) == 0 {
		return fmt.Errorf("at least one pool must be defined")
	}

	poolNames := make(map[string]bool, len(c.Pools))
	for i := range c.Pools {
		p := &c.Pools[i]
		if p.Name == "" {
			return fmt.Errorf("pool at index %d: name is required", i)
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
		for j := range p.Backends {
			b := &p.Backends[j]
			if err := validateBackendURL(b.URL); err != nil {
				return fmt.Errorf("pool %q backend[%d]: %w", p.Name, j, err)
			}
			if b.Weight < 0 {
				return fmt.Errorf("pool %q backend[%d]: weight must be >= 0", p.Name, j)
			}
		}

		hc := &p.HealthCheck
		if !strings.HasPrefix(hc.Path, "/") {
			return fmt.Errorf("pool %q: health_check.path must start with '/'", p.Name)
		}
		ivl, err := time.ParseDuration(hc.Interval)
		if err != nil {
			return fmt.Errorf("pool %q: invalid health_check.interval: %w", p.Name, err)
		}
		hc.IntervalDur = ivl
		to, err := time.ParseDuration(hc.Timeout)
		if err != nil {
			return fmt.Errorf("pool %q: invalid health_check.timeout: %w", p.Name, err)
		}
		hc.TimeoutDur = to
		if hc.HealthyThreshold <= 0 {
			return fmt.Errorf("pool %q: health_check.healthy_threshold must be > 0", p.Name)
		}
		if hc.UnhealthyThreshold <= 0 {
			return fmt.Errorf("pool %q: health_check.unhealthy_threshold must be > 0", p.Name)
		}
	}

	if len(c.Routes) == 0 {
		return fmt.Errorf("at least one route must be defined")
	}
	for i := range c.Routes {
		r := &c.Routes[i]
		if !strings.HasPrefix(r.PathPrefix, "/") {
			return fmt.Errorf("route at index %d: path_prefix must start with '/'", i)
		}
		if r.Pool == "" {
			return fmt.Errorf("route at index %d: pool is required", i)
		}
		if !poolNames[r.Pool] {
			return fmt.Errorf("route at index %d: references unknown pool %q", i, r.Pool)
		}
	}

	return nil
}

func validateBackendURL(raw string) error {
	if raw == "" {
		return fmt.Errorf("url is required")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid url %q: %w", raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("url %q: scheme must be http or https", raw)
	}
	if u.Hostname() == "" {
		return fmt.Errorf("url %q: missing resolvable host", raw)
	}
	return nil
}

// ---------------------------------------------------------------------------
// TLS material resolution
// ---------------------------------------------------------------------------

// secretAccessor abstracts Secret Manager access to allow hermetic unit
// testing without live GCP calls.
type secretAccessor interface {
	AccessSecret(ctx context.Context, name string) ([]byte, error)
}

// ResolveTLSMaterial returns PEM-encoded cert & key bytes, sourcing from
// local filesystem or GCP Secret Manager per TLSConfig.CertSource.
func ResolveTLSMaterial(ctx context.Context, cfg TLSConfig) (certPEM, keyPEM []byte, err error) {
	switch cfg.CertSource {
	case "secretmanager":
		accessor, err := newDefaultSecretAccessor(ctx)
		if err != nil {
			return nil, nil, fmt.Errorf("init secret manager client: %w", err)
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

// newDefaultSecretAccessor is a placeholder seam for a real GCP Secret
// Manager client. It deliberately avoids a hard dependency on
// cloud.google.com/go/secretmanager to keep the module's dependency graph
// minimal; environments needing this path can supply credentials/wiring
// via an alternative build or future revision without affecting the
// "file" cert_source path used by the reference GKE deployment.
func newDefaultSecretAccessor(_ context.Context) (secretAccessor, error) {
	return nil, fmt.Errorf("secretmanager cert_source is not available in this build; use cert_source: file")
}
