// placeholder to avoid duplicate type declarations across files.
//
// Configuration is parsed as JSON (encoding/json, stdlib-only — no
// third-party YAML dependency required, keeping the module
// dependency-free). Set CONFIG_PATH to a JSON document matching this
// schema. The TLS_CERT_PATH / TLS_KEY_PATH environment variables, when
// set, override server.tls.cert_file / server.tls.key_file.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"
)

// ---------------------------------------------------------------------
// Config schema
// ---------------------------------------------------------------------

// Config is the top-level configuration object loaded from CONFIG_PATH.
type Config struct {
	Server ServerConfig  `json:"server"`
	Routes []RouteConfig `json:"routes"`
	Pools  []PoolConfig  `json:"pools"`
}

// ServerConfig holds listener addresses, TLS, and timeout settings.
type ServerConfig struct {
	HTTPAddr             string         `json:"http_addr"`
	HTTPSAddr            string         `json:"https_addr"`
	EnableTLS            bool           `json:"enable_tls"`
	TLS                  TLSConfig      `json:"tls"`
	Timeouts             TimeoutsConfig `json:"timeouts"`
	ShutdownGraceSeconds int            `json:"shutdown_grace_seconds"`
}

// TLSConfig describes how the HTTPS listener's certificate/key material
// is located on disk and which minimum protocol version to enforce.
// CertPEM/KeyPEM are populated at load time by LoadConfig and consumed by
