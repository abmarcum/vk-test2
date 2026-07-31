package main

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	secretmanager "cloud.google.com/go/secretmanager/apiv1"
	secretmanagerpb "cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"gopkg.in/yaml.v3"
)

// -----------------------------------------------------------------------------
// Config structs
// -----------------------------------------------------------------------------

// Config is the top-level configuration object loaded from YAML.
type Config struct {
	Server Server  `yaml:"server"`
	Routes []Route `yaml:"routes"`
	Pools  []Pool  `yaml:"pools"`
}

// Server holds listener, TLS, and timeout settings.
type Server struct {
	HTTPAddr   string    `yaml:"http_addr"`
	HTTPSAddr  string    `yaml:"https_addr"`
	EnableTLS  bool      `yaml:"enable_tls"`
	TLS        TLS       `yaml:"tls"`
	Timeouts   Timeouts  `yaml:"timeouts"`
	ShutdownGraceSeconds int `yaml:"shutdown_grace_seconds"`
}

// TLS holds certificate material configuration. Certificates may be sourced
// from a local file path or from GCP Secret Manager.
type TLS struct {
	// CertSource selects "file" or "secretmanager". Defaults to "file".
	CertSource string `yaml:"cert_source"`

	// File-based sourcing.
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`

	// Secret Manager sourcing: full resource names,
	// e.g. "projects/p/secrets/cert/versions/latest"
	CertSecretName string `yaml:"cert_secret_name"`
	KeySecretName  string `yaml:"key_secret_name"`

	MinVersion string `yaml:"min_version"` // "1.2" or "1.3"

	// Resolved material (populated at load time, never serialized).
	CertPEM []byte `yaml:"-"`
	KeyPEM  []byte `yaml:"-"`
}

// Timeouts holds all duration-based tunables, expressed in the YAML as
// human-readable strings (e.g. "5s") and parsed into time.Duration.
type Timeouts struct {
	ReadHeader string `yaml:"read_header"`
	Read       string `yaml:"read"`
	Write      string `yaml:"write"`
	Idle       string `yaml:"idle"`
	Dial       string `yaml:"dial"`
	ProxyTotal string `yaml:"proxy_total"`

	ReadHeaderDur time.Duration `yaml:"-"`
	ReadDur       time.Duration `yaml:"-"`
	WriteDur      time.Duration `yaml:"-"`
	IdleDur       time.Duration `yaml:"-"`
	DialDur       time.Duration `yaml:"-"`
	ProxyTotalDur time.Duration `yaml:"-"`
}

// Route maps a path prefix to a named backend pool.
type Route struct {
	PathPrefix string `yaml:"path_prefix"`
	Pool       string `yaml:"pool"`
}

// Pool defines a group of backends and the load balancing strategy applied
// to them, plus health check configuration.
type Pool struct {
	Name        string      `yaml:"name"`
	Strategy    string      `yaml:"strategy"` // round_robin | least_connections | random
	HealthCheck HealthCheck `yaml:"health_check"`
	Backends    []Backend   `yaml:"backends"`
}

// HealthCheck configures active health checking behavior for a pool.
type HealthCheck struct {
	Enabled            bool   `yaml:"enabled"`
	Path               string `yaml:"path"`
	Interval           string `yaml:"interval"`
	Timeout            string `yaml:"timeout"`
	HealthyThreshold   int    `yaml:"healthy_threshold"`
	UnhealthyThreshold int    `yaml:"unhealthy_threshold"`

	IntervalDur time.Duration `yaml:"-"`
	TimeoutDur  time.Duration `yaml:"-"`
}

// Backend represents a single upstream server.
type Backend struct {
	URL    string `yaml:"url"`
	Weight int    `yaml:"weight"`
}

// -----------------------------------------------------------------------------
// Loading
// -----------------------------------------------------------------------------

// secretAccessor abstracts GCP Secret Manager access to allow test injection.
type secretAccessor interface {
	AccessSecretVersion(ctx context.Context, req *secretmanagerpb.AccessSecretVersionRequest) (*secretmanagerpb.AccessSecretVersionResponse, error)
	Close() error
}

// LoadConfig reads, parses, validates, and finalizes configuration from the
// given YAML file path. TLS material is resolved (file or Secret Manager) and
// all duration strings are parsed. Returns a validated, ready-to-use Config.
func LoadConfig(ctx context.Context, path string) (*Config, error) {
	raw, err := os.ReadFile(path) // #nosec G304 -- path is operator-supplied config location
	if err != nil {
		return nil, fmt.Errorf("read config file: %w", err)
	}

	var cfg Config
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true) // reject unknown fields to catch typos/misconfig early
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse config yaml: %w", err)
	}

	applyDefaults(&cfg)

	if err := parseDurations(&cfg); err != nil {
		return nil, fmt.Errorf("parse durations: %w", err)
	}

	if err := resolveTLSMaterial(ctx, &cfg.Server.TLS, cfg.Server.EnableTLS); err != nil {
		return nil, fmt.Errorf("resolve tls material: %w", err)
	}

	if err := validateConfig(&cfg); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	return &cfg, nil
}

// applyDefaults fills unset fields with safe production defaults.
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
	if cfg.Server.ShutdownGraceSeconds == 0 {
		cfg.Server.ShutdownGraceSeconds = 30
	}

	t := &cfg.Server.Timeouts
	setDefault(&t.ReadHeader, "5s")
	setDefault(&t.Read, "15s")
	setDefault(&t.Write, "15s")
	setDefault(&t.Idle, "60s")
	setDefault(&t.Dial, "5s")
	setDefault(&t.ProxyTotal, "30s")

	for i := range cfg.Pools {
		hc := &cfg.Pools[i].HealthCheck
		if cfg.Pools[i].Strategy == "" {
			cfg.Pools[i].Strategy = "round_robin"
		}
		if hc.Path == "" {
			hc.Path = "/healthz"
		}
		setDefault(&hc.Interval, "10s")
		setDefault(&hc.Timeout, "2s")
		if hc.HealthyThreshold == 0 {
			hc.HealthyThreshold = 2
		}
		if hc.UnhealthyThreshold == 0 {
			hc.UnhealthyThreshold = 3
		}
		for j := range cfg.Pools[i].Backends {
			if cfg.Pools[i].Backends[j].Weight == 0 {
				cfg.Pools[i].Backends[j].Weight = 1
			}
		}
	}
}

func setDefault(field *string, def string) {
	if *field == "" {
		*field = def
	}
}

// parseDurations converts all human-readable duration strings into
// time.Duration values, failing fast on malformed input.
func parseDurations(cfg *Config) error {
	t := &cfg.Server.Timeouts
	var err error
	if t.ReadHeaderDur, err = time.ParseDuration(t.ReadHeader); err != nil {
		return fmt.Errorf("server.timeouts.read_header: %w", err)
	}
	if t.ReadDur, err = time.ParseDuration(t.Read); err != nil {
		return fmt.Errorf("server.timeouts.read: %w", err)
	}
	if t.WriteDur, err = time.ParseDuration(t.Write); err != nil {
		return fmt.Errorf("server.timeouts.write: %w", err)
	}
	if t.IdleDur, err = time.ParseDuration(t.Idle); err != nil {
		return fmt.Errorf("server.timeouts.idle: %w", err)
	}
	if t.DialDur, err = time.ParseDuration(t.Dial); err != nil {
		return fmt.Errorf("server.timeouts.dial: %w", err)
	}
	if t.ProxyTotalDur, err = time.ParseDuration(t.ProxyTotal); err != nil {
		return fmt.Errorf("server.timeouts.proxy_total: %w", err)
	}

	for i := range cfg.Pools {
		hc := &cfg.Pools[i].HealthCheck
		if hc.IntervalDur, err = time.ParseDuration(hc.Interval); err != nil {
			return fmt.Errorf("pools[%d].health_check.interval: %w", i, err)
		}
		if hc.TimeoutDur, err = time.ParseDuration(hc.Timeout); err != nil {
			return fmt.Errorf("pools[%d].health_check.timeout: %w", i, err)
		}
	}
	return nil
}

// -----------------------------------------------------------------------------
// Validation
// -----------------------------------------------------------------------------

// validateConfig performs structural and semantic validation of the config,
// returning an aggregated error describing all violations found.
func validateConfig(cfg *Config) error {
	var errs []string

	if cfg.Server.HTTPAddr == "" && cfg.Server.HTTPSAddr == "" {
		errs = append(errs, "server: at least one of http_addr or https_addr must be set")
	}

	if cfg.Server.EnableTLS {
		switch cfg.Server.TLS.CertSource {
		case "file":
			if cfg.Server.TLS.CertFile == "" || cfg.Server.TLS.KeyFile == "" {
				errs = append(errs, "server.tls: cert_file and key_file required when cert_source=file")
			}
		case "secretmanager":
			if cfg.Server.TLS.CertSecretName == "" || cfg.Server.TLS.KeySecretName == "" {
				errs = append(errs, "server.tls: cert_secret_name and key_secret_name required when cert_source=secretmanager")
			}
		default:
			errs = append(errs, fmt.Sprintf("server.tls.cert_source: unsupported value %q", cfg.Server.TLS.CertSource))
		}
		if cfg.Server.TLS.MinVersion != "1.2" && cfg.Server.TLS.MinVersion != "1.3" {
			errs = append(errs, fmt.Sprintf("server.tls.min_version: must be 1.2 or 1.3, got %q", cfg.Server.TLS.MinVersion))
		}
		if len(cfg.Server.TLS.CertPEM) == 0 || len(cfg.Server.TLS.KeyPEM) == 0 {
			errs = append(errs, "server.tls: certificate material could not be resolved")
		}
	}

	if len(cfg.Pools) == 0 {
		errs = append(errs, "pools: at least one pool must be defined")
	}

	poolNames := make(map[string]bool, len(cfg.Pools))
	for i, p := range cfg.Pools {
		if p.Name == "" {
			errs = append(errs, fmt.Sprintf("pools[%d]: name is required", i))
		} else if poolNames[p.Name] {
			errs = append(errs, fmt.Sprintf("pools[%d]: duplicate pool name %q", i, p.Name))
		}
		poolNames[p.Name] = true

		switch p.Strategy {
		case "round_robin", "least_connections", "random":
		default:
			errs = append(errs, fmt.Sprintf("pools[%d].strategy: unsupported value %q", i, p.Strategy))
		}

		if len(p.Backends) == 0 {
			errs = append(errs, fmt.Sprintf("pools[%d]: at least one backend required", i))
		}
		for j, b := range p.Backends {
			if err := validateBackendURL(b.URL); err != nil {
				errs = append(errs, fmt.Sprintf("pools[%d].backends[%d].url: %v", i, j, err))
			}
			if b.Weight < 0 {
				errs = append(errs, fmt.Sprintf("pools[%d].backends[%d].weight: must be >= 0", i, j))
			}
		}

		if p.HealthCheck.Enabled {
			if p.HealthCheck.HealthyThreshold <= 0 || p.HealthCheck.UnhealthyThreshold <= 0 {
				errs = append(errs, fmt.Sprintf("pools[%d].health_check: thresholds must be positive", i))
			}
			if !strings.HasPrefix(p.HealthCheck.Path, "/") {
				errs = append(errs, fmt.Sprintf("pools[%d].health_check.path: must start with /", i))
			}
		}
	}

	if len(cfg.Routes) == 0 {
		errs = append(errs, "routes: at least one route must be defined")
	}
	for i, r := range cfg.Routes {
		if !strings.HasPrefix(r.PathPrefix, "/") {
			errs = append(errs, fmt.Sprintf("routes[%d].path_prefix: must start with /", i))
		}
		if r.Pool == "" {
			errs = append(errs, fmt.Sprintf("routes[%d]: pool is required", i))
		} else if !poolNames[r.Pool] {
			errs = append(errs, fmt.Sprintf("routes[%d]: references unknown pool %q", i, r.Pool))
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}

// validateBackendURL ensures a backend URL is well-formed, absolute, and
// restricted to http/https schemes, mitigating SSRF-style misconfiguration.
func validateBackendURL(raw string) error {
	if raw == "" {
		return fmt.Errorf("url is required")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("malformed url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("scheme must be http or https, got %q", u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("host is required")
	}
	return nil
}

// -----------------------------------------------------------------------------
// TLS material resolution
// -----------------------------------------------------------------------------

// resolveTLSMaterial populates CertPEM/KeyPEM from the configured source.
// No-op if TLS is disabled.
func resolveTLSMaterial(ctx context.Context, t *TLS, enabled bool) error {
	if !enabled {
		return nil
	}

	switch t.CertSource {
	case "file":
		cert, err := os.ReadFile(t.CertFile) // #nosec G304 -- operator-supplied cert path
		if err != nil {
			return fmt.Errorf("read cert_file: %w", err)
		}
		key, err := os.ReadFile(t.KeyFile) // #nosec G304 -- operator-supplied key path
		if err != nil {
			return fmt.Errorf("read key_file: %w", err)
		}
		t.CertPEM = cert
		t.KeyPEM = key
		return nil

	case "secretmanager":
		client, err := secretmanager.NewClient(ctx)
		if err != nil {
			return fmt.Errorf("create secret manager client: %w", err)
		}
		defer client.Close()

		cert, err := fetchSecret(ctx, client, t.CertSecretName)
		if err != nil {
			return fmt.Errorf("fetch cert secret: %w", err)
		}
		key, err := fetchSecret(ctx, client, t.KeySecretName)
		if err != nil {
			return fmt.Errorf("fetch key secret: %w", err)
		}
		t.CertPEM = cert
		t.KeyPEM = key
		return nil

	default:
		return fmt.Errorf("unsupported cert_source %q", t.CertSource)
	}
}

// fetchSecret retrieves a single secret payload by full resource name.
// Errors are wrapped generically to avoid leaking secret identifiers/content.
func fetchSecret(ctx context.Context, client secretAccessor, name string) ([]byte, error) {
	if name == "" {
		return nil, fmt.Errorf("secret resource name is empty")
	}
	resp, err := client.AccessSecretVersion(ctx, &secretmanagerpb.AccessSecretVersionRequest{
		Name: name,
	})
	if err != nil {
		return nil, fmt.Errorf("secret manager access failed")
	}
	if resp == nil || resp.Payload == nil || len(resp.Payload.Data) == 0 {
		return nil, fmt.Errorf("secret payload empty")
	}
	return resp.Payload.Data, nil
}
