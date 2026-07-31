// Package main implements the GoProxy entry point: process lifecycle,
// config loading, logger construction, TLS listener setup, HTTP/HTTPS
// server bootstrap, mux route wiring (healthz/metrics/proxy/redirect),
// signal handling (SIGTERM/SIGHUP), and graceful shutdown orchestration.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

// healthCheckerAdapter bridges balancer.go's Pool state to proxy.go's
