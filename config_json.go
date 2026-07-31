// only as an empty placeholder to preserve the original file listing).
//
// Configuration is parsed as JSON (encoding/json, stdlib-only — no
// third-party YAML dependency is required to keep the module
// dependency-free). Set CONFIG_PATH to a JSON document matching this
// schema.
package main

import (
	"encoding/json"
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
	HTTPAddr             string         `json:"http_addr"`
	HTTPSAddr            string         `json:"https_addr"`
	EnableTLS            bool           `json:"enable_tls"`
	TLS                  TLSConfig      `json:"tls"`
	Timeouts             TimeoutsConfig `json:"timeouts"`
	ShutdownGraceSeconds int            `json:"shutdown_grace_seconds"`
}

// TLSConfig describes how
