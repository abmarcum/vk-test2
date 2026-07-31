// schema validation, defaulting, and TLS material resolution (file-based).
// It does not own HTTP handling, LB logic, or request routing.
package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Config struct tree
// ---------------------------------------------------------------------------

type Config struct {
	Server ServerConf
	Routes []RouteConf
	Pools  []PoolConf
}

type ServerConf struct {
	HTTPAddr             string
	HTTPSAddr            string
	EnableTLS            bool
	TLS                  TLSConf
	Timeouts             TimeoutConf
	ShutdownGraceSeconds int
}

type TLSConf struct {
	CertSource string // "file"
	CertFile   string
	KeyFile    string
	MinVersion string // "1.2" | "1.3"

	CertPEM []byte
	KeyPEM  []byte
}

type TimeoutConf struct {
	ReadHeader string
	Read       string
	Write      string
	Idle       string
	Dial       string
	ProxyTotal string

	ReadHeaderDur time.Duration
	ReadDur       time.Duration
	WriteDur      time.Duration
	IdleDur       time.Duration
	DialDur       time.Duration
	ProxyTotalDur time.Duration
}

type RouteConf struct {
	PathPrefix string
	Pool       string
}

type PoolConf struct {
	Name        string
	Strategy    string
	HealthCheck HealthCheckConfig
	Backends    []BackendConf
}

type HealthCheckConfig struct {
	Enabled            bool
	Path               string
	Interval           string
	Timeout            string
	HealthyThreshold   int
	UnhealthyThreshold int

	IntervalDur time.Duration
	TimeoutDur  time.Duration
}

type BackendConf struct {
	URL    string
	Weight int
}

// ---------------------------------------------------------------------------
// Defaults
// ---------------------------------------------------------------------------

func defaultConfig() *Config {
	return &Config{
		Server: ServerConf{
			HTTPAddr:             ":8080",
			HTTPSAddr:            ":8443",
			EnableTLS:            false,
			ShutdownGraceSeconds: 30,
			Timeouts: TimeoutConf{
				ReadHeader: "5s",
				Read:       "15s",
				Write:      "15s",
				Idle:       "60s",
				Dial:       "5s",
				ProxyTotal: "30s",
			},
		},
	}
}

// ---------------------------------------------------------------------------
// LoadConfig — minimal, dependency-free YAML-subset parser
// ---------------------------------------------------------------------------

// LoadConfig reads, parses, defaults, and validates the config file at path.
// It supports a deliberately small, indentation-based YAML subset sufficient
// for this application's schema (maps, lists of maps, scalars) so the
// binary has zero third-party dependencies.
func LoadConfig(ctx context.Context, path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config file: %w", err)
	}

	cfg := defaultConfig()
	if err := parseYAMLInto(string(data), cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	applyDurationDefaults(cfg)

	if cfg.Server.EnableTLS {
		certPEM, keyPEM, err := ResolveTLSMaterial(ctx, cfg.Server.TLS)
		if err != nil {
			return nil, fmt.Errorf("resolve tls material: %w", err)
		}
		cfg.Server.TLS.CertPEM = certPEM
		cfg.Server.TLS.KeyPEM = keyPEM
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	return cfg, nil
}

func applyDurationDefaults(cfg *Config) {
	t := &cfg.Server.Timeouts
	t.ReadHeaderDur = mustDuration(t.ReadHeader, 5*time.Second)
	t.ReadDur = mustDuration(t.Read, 15*time.Second)
	t.WriteDur = mustDuration(t.Write, 15*time.Second)
	t.IdleDur = mustDuration(t.Idle, 60*time.Second)
	t.DialDur = mustDuration(t.Dial, 5*time.Second)
	t.ProxyTotalDur = mustDuration(t.ProxyTotal, 30*time.Second)

	for i := range cfg.Pools {
		hc := &cfg.Pools[i].HealthCheck
		hc.IntervalDur = mustDuration(hc.Interval, 10*time.Second)
		hc.TimeoutDur = mustDuration(hc.Timeout, 2*time.Second)
		if hc.Path == "" {
			hc.Path = "/healthz"
		}
		if hc.HealthyThreshold <= 0 {
			hc.HealthyThreshold = 2
		}
		if hc.UnhealthyThreshold <= 0 {
			hc.UnhealthyThreshold = 3
		}
	}
}

func mustDuration(s string, def time.Duration) time.Duration {
	if s == "" {
		return def
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return def
	}
	return d
}

// Validate enforces schema invariants.
func (c *Config) Validate() error {
	if c.Server.HTTPAddr == "" && c.Server.HTTPSAddr == "" {
		return fmt.Errorf("at least one of http_addr/https_addr must be set")
	}
	if len(c.Pools) == 0 {
		return fmt.Errorf("at least one pool must be configured")
	}
	poolNames := make(map[string]bool)
	for _, p := range c.Pools {
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
			if !strings.HasPrefix(b.URL, "http://") && !strings.HasPrefix(b.URL, "https://") {
				return fmt.Errorf("pool %q backend url %q must be absolute http(s) URL", p.Name, b.URL)
			}
		}
	}
	if len(c.Routes) == 0 {
		return fmt.Errorf("at least one route must be configured")
	}
	for _, r := range c.Routes {
		if !strings.HasPrefix(r.PathPrefix, "/") {
			return fmt.Errorf("route path_prefix %q must start with /", r.PathPrefix)
		}
		if !poolNames[r.Pool] {
			return fmt.Errorf("route references unknown pool: %s", r.Pool)
		}
	}
	if c.Server.EnableTLS {
		if c.Server.TLS.MinVersion != "" && c.Server.TLS.MinVersion != "1.2" && c.Server.TLS.MinVersion != "1.3" {
			return fmt.Errorf("tls min_version must be 1.2 or 1.3")
		}
	}
	return nil
}

// ResolveTLSMaterial returns PEM-encoded cert & key bytes from the
// filesystem (v1 supports the "file" cert_source only).
func ResolveTLSMaterial(ctx context.Context, cfg TLSConf) (certPEM, keyPEM []byte, err error) {
	if cfg.CertFile == "" || cfg.KeyFile == "" {
		return nil, nil, fmt.Errorf("cert_file and key_file are required when enable_tls is true")
	}
	certPEM, err = os.ReadFile(cfg.CertFile)
	if err != nil {
		return nil, nil, fmt.Errorf("read cert file: %w", err)
	}
	keyPEM, err = os.ReadFile(cfg.KeyFile)
	if err != nil {
		return nil, nil, fmt.Errorf("read key file: %w", err)
	}
	if len(certPEM) == 0 || len(keyPEM) == 0 {
		return nil, nil, fmt.Errorf("resolved cert/key material is empty")
	}
	return certPEM, keyPEM, nil
}

// ---------------------------------------------------------------------------
// Minimal indentation-based YAML-subset parser
// ---------------------------------------------------------------------------
//
// Supports exactly the shapes used by this config schema:
//   key: value
//   key:
//     nested: value
//   list:
//     - key: value
//       key2: value

type yamlLine struct {
	indent int
	text   string
}

func parseYAMLInto(src string, cfg *Config) error {
	lines := splitYAMLLines(src)
	i := 0
	for i < len(lines) {
		line := lines[i]
		switch {
		case line.text == "server:":
			i = parseServer(lines, i+1, line.indent, cfg)
		case line.text == "routes:":
			i = parseRoutes(lines, i+1, line.indent, cfg)
		case line.text == "pools:":
			i = parsePools(lines, i+1, line.indent, cfg)
		default:
			i++
		}
	}
	return nil
}

func splitYAMLLines(src string) []yamlLine {
	raw := strings.Split(src, "\n")
	out := make([]yamlLine, 0, len(raw))
	for _, r := range raw {
		trimmedRight := strings.TrimRight(r, " \r\t")
		if strings.TrimSpace(trimmedRight) == "" {
			continue
		}
		if strings.TrimSpace(trimmedRight)[0] == '#' {
			continue
		}
		indent := 0
		for indent < len(trimmedRight) && trimmedRight[indent] == ' ' {
			indent++
		}
		out = append(out, yamlLine{indent: indent, text: strings.TrimSpace(trimmedRight)})
	}
	return out
}

func kv(text string) (string, string) {
	idx := strings.Index(text, ":")
	if idx < 0 {
		return text, ""
	}
	key := strings.TrimSpace(text[:idx])
	val := strings.TrimSpace(text[idx+1:])
	val = strings.Trim(val, `"`)
	return key, val
}

func parseServer(lines []yamlLine, i int, parentIndent int, cfg *Config) int {
	for i < len(lines) && lines[i].indent > parentIndent {
		line := lines[i]
		switch {
		case line.text == "tls:":
			i = parseTLS(lines, i+1, line.indent, cfg)
			continue
		case line.text == "timeouts:":
			i = parseTimeouts(lines, i+1, line.indent, cfg)
			continue
		default:
			key, val := kv(line.text)
			switch key {
			case "http_addr":
				cfg.Server.HTTPAddr = val
			case "https_addr":
				cfg.Server.HTTPSAddr = val
			case "enable_tls":
				cfg.Server.EnableTLS = val == "true"
			case "shutdown_grace_seconds":
				if n, err := strconv.Atoi(val); err == nil {
					cfg.Server.ShutdownGraceSeconds = n
				}
			}
			i++
		}
	}
	return i
}

func parseTLS(lines []yamlLine, i int, parentIndent int, cfg *Config) int {
	for i < len(lines) && lines[i].indent > parentIndent {
		key, val := kv(lines[i].text)
		switch key {
		case "cert_source":
			cfg.Server.TLS.CertSource = val
		case "cert_file":
			cfg.Server.TLS.CertFile = val
		case "key_file":
			cfg.Server.TLS.KeyFile = val
		case "min_version":
			cfg.Server.TLS.MinVersion = val
		}
		i++
	}
	return i
}

func parseTimeouts(lines []yamlLine, i int, parentIndent int, cfg *Config) int {
	for i < len(lines) && lines[i].indent > parentIndent {
		key, val := kv(lines[i].text)
		switch key {
		case "read_header":
			cfg.Server.Timeouts.ReadHeader = val
		case "read":
			cfg.Server.Timeouts.Read = val
		case "write":
			cfg.Server.Timeouts.Write = val
		case "idle":
			cfg.Server.Timeouts.Idle = val
		case "dial":
			cfg.Server.Timeouts.Dial = val
		case "proxy_total":
			cfg.Server.Timeouts.ProxyTotal = val
		}
		i++
	}
	return i
}

func parseRoutes(lines []yamlLine, i int, parentIndent int, cfg *Config) int {
	for i < len(lines) && lines[i].indent > parentIndent {
		line := lines[i]
		if strings.HasPrefix(line.text, "- ") {
			r := RouteConf{}
			itemIndent := line.indent
			rest := strings.TrimPrefix(line.text, "- ")
			key, val := kv(rest)
			applyRouteField(&r, key, val)
			i++
			for i < len(lines) && lines[i].indent > itemIndent {
				key, val := kv(lines[i].text)
				applyRouteField(&r, key, val)
				i++
			}
			cfg.Routes = append(cfg.Routes, r)
			continue
		}
		i++
	}
	return i
}

func applyRouteField(r *RouteConf, key, val string) {
	switch key {
	case "path_prefix":
		r.PathPrefix = val
	case "pool":
		r.Pool = val
	}
}

func parsePools(lines []yamlLine, i int, parentIndent int, cfg *Config) int {
	for i < len(lines) && lines[i].indent > parentIndent {
		line := lines[i]
		if strings.HasPrefix(line.text, "- ") {
			p := PoolConf{}
			itemIndent := line.indent
			rest := strings.TrimPrefix(line.text, "- ")
			key, val := kv(rest)
			if key != "" && key != "health_check" && key != "backends" {
				applyPoolField(&p, key, val)
			}
			i++
			for i < len(lines) && lines[i].indent > itemIndent {
				sub := lines[i]
				switch {
				case sub.text == "health_check:":
					i = parseHealthCheck(lines, i+1, sub.indent, &p)
					continue
				case sub.text == "backends:":
					i = parseBackends(lines, i+1, sub.indent, &p)
					continue
				default:
					key, val := kv(sub.text)
					applyPoolField(&p, key, val)
					i++
				}
			}
			cfg.Pools = append(cfg.Pools, p)
			continue
		}
		i++
	}
	return i
}

func applyPoolField(p *PoolConf, key, val string) {
	switch key {
	case "name":
		p.Name = val
	case "strategy":
		p.Strategy = val
	}
}

func parseHealthCheck(lines []yamlLine, i int, parentIndent int, p *PoolConf) int {
	for i < len(lines) && lines[i].indent > parentIndent {
		key, val := kv(lines[i].text)
		switch key {
		case "enabled":
			p.HealthCheck.Enabled = val == "true"
		case "path":
			p.HealthCheck.Path = val
		case "interval":
			p.HealthCheck.Interval = val
		case "timeout":
			p.HealthCheck.Timeout = val
		case "healthy_threshold":
			if n, err := strconv.Atoi(val); err == nil {
				p.HealthCheck.HealthyThreshold = n
			}
		case "unhealthy_threshold":
			if n, err := strconv.Atoi(val); err == nil {
				p.HealthCheck.UnhealthyThreshold = n
			}
		}
		i++
	}
	return i
}

func parseBackends(lines []yamlLine, i int, parentIndent int, p *PoolConf) int {
	for i < len(lines) && lines[i].indent > parentIndent {
		line := lines[i]
		if strings.HasPrefix(line.text, "- ") {
			b := BackendConf{Weight: 1}
			itemIndent := line.indent
			rest := strings.TrimPrefix(line.text, "- ")
			key, val := kv(rest)
			applyBackendField(&b, key, val)
			i++
			for i < len(lines) && lines[i].indent > itemIndent {
				key, val := kv(lines[i].text)
				applyBackendField(&b, key, val)
				i++
			}
			p.Backends = append(p.Backends, b)
			continue
		}
		i++
	}
	return i
}

func applyBackendField(b *BackendConf, key, val string) {
	switch key {
	case "url":
		b.URL = val
	case "weight":
		if n, err := strconv.Atoi(val); err == nil {
			b.Weight = n
		}
	}
}
