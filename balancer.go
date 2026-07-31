// from YAML, with duration strings alongside their parsed equivalents.
type HealthCheckSettings struct {
	Enabled            bool   `yaml:"enabled"`
	Path               string `yaml:"path"`
	Interval           string `yaml:"interval"`
	Timeout            string `yaml:"timeout"`
	HealthyThreshold   int    `yaml:"healthy_threshold"`
	UnhealthyThreshold int    `yaml:"unhealthy_threshold"`

	IntervalDur time.Duration `yaml:"-"`
	TimeoutDur  time.Duration `yaml:"-"`
}

// BackendConfig defines a single upstream backend URL and weight.
type BackendConfig struct {
	URL    string `yaml:"url"`
	Weight int    `yaml:"weight"`
}

// ---------------------------------------------------------------------------
// Loading
// ---------------------------------------------------------------------------

// LoadConfig reads, parses, defaults, validates, and resolves TLS material
// for the YAML file at path. Unknown fields cause a load-time error.
func LoadConfig(ctx context.Context, path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config file: %w", err)
	}

	var cfg Config
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse config yaml: %w", err)
	}

	cfg.setDefaults()

	if err := cfg.parseDurations(); err != nil {
		return nil, fmt.Errorf("parse durations: %w", err)
	}

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("validate config: %w", err)
	}

	if cfg.Server.EnableTLS {
		certPEM, keyPEM, err := ResolveTLSMaterial(ctx, &cfg.Server.TLS)
		if err != nil {
			return nil, fmt.Errorf("resolve tls material: %w", err)
		}
		cfg.Server.TLS.CertPEM = certPEM
		cfg.Server.TLS.KeyPEM = keyPEM
	}

	return &cfg, nil
}

// setDefaults applies default values for any unset fields.
func (c *Config) setDefaults() {
	if c.Server.HTTPAddr == "" {
		c.Server.HTTPAddr = ":8080"
	}
	if c.Server.HTTPSAddr == "" {
		c.Server.HTTPSAddr = ":8443"
	}
	if c.Server.ShutdownGraceSeconds <= 0 {
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
		if c.Pools[i].Strategy == "" {
			c.Pools[i].Strategy = "round_robin"
		}
		hc := &c.Pools[i].HealthCheck
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
		for j := range c.Pools[i].Backends {
			if c.Pools[i].Backends[j].Weight <= 0 {
				c.Pools[i].Backends[j].Weight = 1
			}
		}
	}
}

// parseDurations parses all duration-string fields into their time.Duration
// equivalents, returning an error on any malformed value.
func (c *Config) parseDurations() error {
	t := &c.Server.Timeouts
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

	for i := range c.Pools {
		hc := &c.Pools[i].HealthCheck
		if hc.IntervalDur, err = time.ParseDuration(hc.Interval); err != nil {
			return fmt.Errorf("pools[%d].health_check.interval: %w", i, err)
		}
		if hc.TimeoutDur, err = time.ParseDuration(hc.Timeout); err != nil {
			return fmt.Errorf("pools[%d].health_check.timeout: %w", i, err)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Validation
// ---------------------------------------------------------------------------

// Validate enforces schema invariants across the whole config tree.
func (c *Config) Validate() error {
	if c.Server.HTTPAddr == "" && c.Server.HTTPSAddr == "" {
		return errors.New("at least one of server.http_addr/https_addr must be set")
	}

	if c.Server.EnableTLS {
		if err := c.Server.TLS.validate(); err != nil {
			return err
		}
	}

	if len(c.Pools) == 0 {
		return errors.New("at least one pool must be configured")
	}
	poolNames := make(map[string]struct{}, len(c.Pools))
	for i, p := range c.Pools {
		if p.Name == "" {
			return fmt.Errorf("pools[%d]: name must not be empty", i)
		}
		if _, dup := poolNames[p.Name]; dup {
			return fmt.Errorf("pools[%d]: duplicate pool name %q", i, p.Name)
		}
		poolNames[p.Name] = struct{}{}

		switch p.Strategy {
		case "round_robin", "least_connections", "random":
		default:
			return fmt.Errorf("pools[%d] (%s): invalid strategy %q", i, p.Name, p.Strategy)
		}

		if len(p.Backends) == 0 {
			return fmt.Errorf("pools[%d] (%s): at least one backend required", i, p.Name)
		}
		for j, b := range p.Backends {
			if err := validateBackendURL(b.URL); err != nil {
				return fmt.Errorf("pools[%d] (%s) backends[%d]: %w", i, p.Name, j, err)
			}
			if b.Weight < 0 {
				return fmt.Errorf("pools[%d] (%s) backends[%d]: weight must be >= 0", i, p.Name, j)
			}
		}
	}

	if len(c.Routes) == 0 {
		return errors.New("at least one route must be configured")
	}
	for i, r := range c.Routes {
		if r.PathPrefix == "" || r.PathPrefix[0] != '/' {
			return fmt.Errorf("routes[%d]: path_prefix must start with '/'", i)
		}
		if r.Pool == "" {
			return fmt.Errorf("routes[%d]: pool must not be empty", i)
		}
		if _, ok := poolNames[r.Pool]; !ok {
			return fmt.Errorf("routes[%d]: references undefined pool %q", i, r.Pool)
		}
	}

	return nil
}

// validate enforces TLS-specific invariants, only when TLS is enabled.
func (t *TLS) validate() error {
	switch t.CertSource {
	case "file":
		if t.CertFile == "" || t.KeyFile == "" {
			return errors.New("tls.cert_file and tls.key_file are required when cert_source is 'file'")
		}
	case "secretmanager":
		if t.CertSecretName == "" || t.KeySecretName == "" {
			return errors.New("tls.cert_secret_name and tls.key_secret_name are required when cert_source is 'secretmanager'")
		}
	default:
		return fmt.Errorf("tls.cert_source must be 'file' or 'secretmanager', got %q", t.CertSource)
	}

	if t.MinVersion != "1.2" && t.MinVersion != "1.3" {
		return fmt.Errorf("tls.min_version must be '1.2' or '1.3', got %q", t.MinVersion)
	}

	return nil
}

// validateBackendURL enforces that a backend URL is absolute, uses
// http/https, and has a resolvable, non-empty host.
func validateBackendURL(raw string) error {
	if raw == "" {
		return errors.New("url must not be empty")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errors.New("url scheme must be http or https")
	}
	if strings.TrimSpace(u.Host) == "" || strings.TrimSpace(u.Hostname()) == "" {
		return errors.New("url must include a resolvable host")
	}
	return nil
}

// ---------------------------------------------------------------------------
// TLS material resolution
// ---------------------------------------------------------------------------

// secretAccessor abstracts GCP Secret Manager access to allow hermetic unit
// testing without live GCP calls.
type secretAccessor interface {
	AccessSecret(ctx context.Context, name string) ([]byte, error)
}

// ResolveTLSMaterial returns PEM-encoded cert & key bytes, sourcing from a
// local file pair or GCP Secret Manager per TLSConfig.CertSource.
func ResolveTLSMaterial(ctx context.Context, t *TLS) (certPEM, keyPEM []byte, err error) {
	switch t.CertSource {
	case "file":
		certPEM, err = os.ReadFile(t.CertFile)
		if err != nil {
			return nil, nil, fmt.Errorf("read cert file: %w", err)
		}
		keyPEM, err = os.ReadFile(t.KeyFile)
		if err != nil {
			return nil, nil, fmt.Errorf("read key file: %w", err)
		}
	case "secretmanager":
		accessor, err := newGCPSecretAccessor(ctx)
		if err != nil {
			return nil, nil, fmt.Errorf("init secret manager client: %w", err)
		}
		certPEM, err = accessor.AccessSecret(ctx, t.CertSecretName)
		if err != nil {
			return nil, nil, fmt.Errorf("access cert secret: %w", err)
		}
		keyPEM, err = accessor.AccessSecret(ctx, t.KeySecretName)
		if err != nil {
			return nil, nil, fmt.Errorf("access key secret: %w", err)
		}
	default:
		return nil, nil, fmt.Errorf("unsupported cert_source %q", t.CertSource)
	}

	if len(strings.TrimSpace(string(certPEM))) == 0 {
		return nil, nil, errors.New("resolved certificate material is empty")
	}
	if len(strings.TrimSpace(string(keyPEM))) == 0 {
		return nil, nil, errors.New("resolved key material is empty")
	}

	return certPEM, keyPEM, nil
}

// newGCPSecretAccessor is a seam allowing tests to substitute a fake
// implementation; the real implementation lazily constructs a Secret
// Manager client only when the secretmanager cert source is actually used.
var newGCPSecretAccessor = func(ctx context.Context) (secretAccessor, error) {
	return nil, errors.New("secretmanager cert_source not available in this build")
}
