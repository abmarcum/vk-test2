// Package main: Config struct definitions (Server, TLS, Timeouts, Routes,
// Pools, HealthCheck, Backend) and LoadConfig, which parses the on-disk
// YAML configuration file referenced by main's -config flag / CONFIG_PATH.
//
// This implements a minimal, dependency-free YAML subset parser (block
// mappings, block sequences, scalars) sufficient for this project's own
// configuration schema (see README.md / docs/prd.md examples), so that no
// third-party module (e.g. gopkg.in/yaml.v3) needs to be added to go.mod,
// keeping the module dependency graph empty.
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
// Config structs
// ---------------------------------------------------------------------------

// Config is the root configuration document.
type Config struct {
	Server ServerConfig
	Routes []RouteConfig
	Pools  []PoolConfig
}

// ServerConfig configures the HTTP/HTTPS listeners and TLS/timeouts.
type ServerConfig struct {
	HTTPAddr             string
	HTTPSAddr            string
	EnableTLS            bool
	ShutdownGraceSeconds int
	TLS                  TLSConfig
	Timeouts             TimeoutsConfig
}

// TLSConfig describes the TLS certificate/key material and minimum
// protocol version for the HTTPS listener.
type TLSConfig struct {
	CertFile   string
	KeyFile    string
	MinVersion string // "1.2" (default) or "1.3"

	CertPEM []byte
	KeyPEM  []byte
}

// TimeoutsConfig holds parsed HTTP server timeouts.
type TimeoutsConfig struct {
	ReadHeaderDur time.Duration
	ReadDur       time.Duration
	WriteDur      time.Duration
	IdleDur       time.Duration
}

// RouteConfig maps a path-prefix match to a named pool.
type RouteConfig struct {
	Match string
	Pool  string
}

// PoolConfig describes an upstream pool: its backends, LB strategy, and
// active health-check policy.
type PoolConfig struct {
	Name        string
	Strategy    string
	Backends    []BackendConfig
	HealthCheck HealthCheckConfig
}

// BackendConfig is a single upstream target URL.
type BackendConfig struct {
	URL string
}

// HealthCheckConfig configures active health probing for a pool.
type HealthCheckConfig struct {
	Enabled            bool
	Path               string
	UnhealthyThreshold int
	HealthyThreshold   int

	IntervalDur time.Duration
	TimeoutDur  time.Duration
}

// ---------------------------------------------------------------------------
// LoadConfig
// ---------------------------------------------------------------------------

// LoadConfig reads and parses the YAML config file at path into a Config.
func LoadConfig(ctx context.Context, path string) (*Config, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config file %q: %w", path, err)
	}

	lines := tokenizeYAML(data)
	if len(lines) == 0 {
		return nil, fmt.Errorf("config file %q is empty", path)
	}

	pos := 0
	root := parseMap(lines, &pos, lines[0].indent)

	cfg := &Config{}

	serverMap := asMap(root["server"])
	cfg.Server.HTTPAddr = asString(serverMap["http_addr"])
	if cfg.Server.HTTPAddr == "" {
		cfg.Server.HTTPAddr = ":8080"
	}
	cfg.Server.HTTPSAddr = asString(serverMap["https_addr"])
	if cfg.Server.HTTPSAddr == "" {
		cfg.Server.HTTPSAddr = ":8443"
	}
	cfg.Server.EnableTLS = asBool(serverMap["enable_tls"])
	cfg.Server.ShutdownGraceSeconds = asInt(serverMap["shutdown_grace_seconds"])
	if cfg.Server.ShutdownGraceSeconds <= 0 {
		cfg.Server.ShutdownGraceSeconds = 15
	}

	tlsMap := asMap(serverMap["tls"])
	cfg.Server.TLS.CertFile = asString(tlsMap["cert_file"])
	cfg.Server.TLS.KeyFile = asString(tlsMap["key_file"])
	cfg.Server.TLS.MinVersion = asString(tlsMap["min_version"])

	if cfg.Server.EnableTLS {
		if cfg.Server.TLS.CertFile != "" {
			certPEM, err := os.ReadFile(cfg.Server.TLS.CertFile)
			if err != nil {
				return nil, fmt.Errorf("read tls cert file %q: %w", cfg.Server.TLS.CertFile, err)
			}
			cfg.Server.TLS.CertPEM = certPEM
		}
		if cfg.Server.TLS.KeyFile != "" {
			keyPEM, err := os.ReadFile(cfg.Server.TLS.KeyFile)
			if err != nil {
				return nil, fmt.Errorf("read tls key file %q: %w", cfg.Server.TLS.KeyFile, err)
			}
			cfg.Server.TLS.KeyPEM = keyPEM
		}
	}

	timeoutsMap := asMap(serverMap["timeouts"])
	cfg.Server.Timeouts.ReadHeaderDur = parseDurationOr(asString(timeoutsMap["read_header"]), 5*time.Second)
	cfg.Server.Timeouts.ReadDur = parseDurationOr(asString(timeoutsMap["read"]), 15*time.Second)
	cfg.Server.Timeouts.WriteDur = parseDurationOr(asString(timeoutsMap["write"]), 15*time.Second)
	cfg.Server.Timeouts.IdleDur = parseDurationOr(asString(timeoutsMap["idle"]), 60*time.Second)

	for _, rv := range asList(root["routes"]) {
		rm := asMap(rv)
		cfg.Routes = append(cfg.Routes, RouteConfig{
			Match: asString(rm["match"]),
			Pool:  asString(rm["pool"]),
		})
	}

	for _, pv := range asList(root["pools"]) {
		pm := asMap(pv)
		pc := PoolConfig{
			Name:     asString(pm["name"]),
			Strategy: asString(pm["strategy"]),
		}
		for _, bv := range asList(pm["backends"]) {
			bm := asMap(bv)
			pc.Backends = append(pc.Backends, BackendConfig{URL: asString(bm["url"])})
		}
		hcMap := asMap(pm["health_check"])
		pc.HealthCheck = HealthCheckConfig{
			Enabled:            asBool(hcMap["enabled"]),
			Path:               asString(hcMap["path"]),
			UnhealthyThreshold: asInt(hcMap["unhealthy_threshold"]),
			HealthyThreshold:   asInt(hcMap["healthy_threshold"]),
		}
		pc.HealthCheck.IntervalDur = parseDurationOr(asString(hcMap["interval"]), 10*time.Second)
		pc.HealthCheck.TimeoutDur = parseDurationOr(asString(hcMap["timeout"]), 2*time.Second)
		cfg.Pools = append(cfg.Pools, pc)
	}

	return cfg, nil
}

func parseDurationOr(s string, fallback time.Duration) time.Duration {
	if s == "" {
		return fallback
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return fallback
	}
	return d
}

// ---------------------------------------------------------------------------
// Minimal YAML-subset tokenizer / parser
// ---------------------------------------------------------------------------

type yamlLine struct {
	indent  int
	content string
}

func tokenizeYAML(data []byte) []yamlLine {
	rawLines := strings.Split(string(data), "\n")
	out := make([]yamlLine, 0, len(rawLines))
	for _, l := range rawLines {
		line := strings.TrimRight(l, "\r")
		trimmed := strings.TrimLeft(line, " ")
		indent := len(line) - len(trimmed)
		trimmed = stripYAMLComment(trimmed)
		trimmed = strings.TrimRight(trimmed, " \t")
		if trimmed == "" || trimmed == "---" {
			continue
		}
		out = append(out, yamlLine{indent: indent, content: trimmed})
	}
	return out
}

func stripYAMLComment(s string) string {
	inSingle, inDouble := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '\'':
			if !inDouble {
				inSingle = !inSingle
			}
		case '"':
			if !inSingle {
				inDouble = !inDouble
			}
		case '#':
			if !inSingle && !inDouble {
				return strings.TrimRight(s[:i], " ")
			}
		}
	}
	return s
}

func findColon(s string) int {
	for i := 0; i < len(s); i++ {
		if s[i] == ':' && (i+1 >= len(s) || s[i+1] == ' ') {
			return i
		}
	}
	return -1
}

func parseScalar(s string) interface{} {
	s = strings.TrimSpace(s)
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	switch s {
	case "true":
		return true
	case "false":
		return false
	case "null", "~", "":
