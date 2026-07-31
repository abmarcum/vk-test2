// Package main implements the GoProxy entry point: process lifecycle,
// config loading, logger construction, TLS listener setup, HTTP/HTTPS
// server bootstrap, mux route wiring (healthz/metrics/proxy), signal
// handling (SIGTERM/SIGHUP), and graceful shutdown orchestration.
//
// Requires Go 1.21+ at build time for the standard-library "log/slog"
// package (see go.mod; the Docker build stage uses golang:1.22-bookworm).
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

// healthCheckerAdapter satisfies the healthChecker interface (declared in
