// Package main implements the GoProxy reverse-proxy data plane: Router
// construction and path matching, the ProxyServer reverse-proxy handler
// (Director/ErrorHandler hooks, forwarded-header rewriting, passive
// health/metrics recording), and the Mux wiring /healthz, /metrics, and
// the proxy data plane onto a single http.Handler shared by both the
// HTTP and HTTPS listeners.
//
// Uses only the standard library "log" package (not "log/slog") to
// remain compatible with Go 1.19+ build environments, and only stdlib
// packages overall (no third-party dependencies).
package main

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// health
