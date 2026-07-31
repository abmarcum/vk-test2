// schema validation, defaulting, and TLS material resolution (from local
// file or GCP Secret Manager). It does not own HTTP handling, routing, or
// load-balancing/health-check internals (balancer.go).
package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// ---------------------------------------------------------------------------
// Config structs
// ---------------------------------------------------------------------------

// Config is the top-level configuration tree, unmarshaled from YAML.
type Config struct {
	Server ServerConfig   `yaml:"server"`
	Routes []RouteConfig  `yaml:"routes"`
	Pools  []PoolConfig   `yaml:"pools"`
}

// ServerConfig holds top-level server/listener settings.
type ServerConfig struct {
	HTTPAddr             string        `yaml:"http_addr"`
	HTTPSAddr            string        `yaml:"https_addr"`
	EnableTLS            bool          `yaml:"enable_tls"`
	TLS                  TLS           `yaml:"tls"`
	Timeouts             Timeouts      `yaml:"timeouts"`
	ShutdownGraceSeconds int           `yaml:"shutdown_grace_seconds"`
}

// TLS holds TLS certificate sourcing configuration and, once resolved, the
// in-memory certificate/key material. CertPEM/KeyPEM are populated once at
// config-load time and are never serialized back out.
type TLS struct {
	CertSource     string `yaml:"cert_source"` // "file" | "secretmanager"
	CertFile       string `yaml:"cert_file"`
	KeyFile        string `yaml:"key_file"`
	CertSecretName string `yaml:"cert_secret_name"`
	KeySecretName  string `yaml:"key_secret_name"`
	MinVersion     string `yaml:"min_version"` // "1.2" | "1.3"

	CertPEM []byte `yaml:"-"`
	KeyPEM  []byte `yaml:"-"`
}

// Timeouts holds duration-string tunables plus their parsed equivalents.
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

// RouteConfig maps a path prefix to a named pool.
type RouteConfig struct {
	PathPrefix string `yaml:"path_prefix"`
	Pool       string `yaml:"pool"`
}

// PoolConfig defines a named group of backends sharing a strategy and
// health-check policy.
type PoolConfig struct {
	Name        string              `yaml:"name"`
	Strategy    string              `yaml:"strategy"`
	HealthCheck HealthCheckSettings `yaml:"health_check"`
	Backends    []BackendConfig     `yaml:"backends"`
}
