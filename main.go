// Package main implements the GoProxy entry point: process lifecycle,
// config loading, logger construction, TLS listener setup, HTTP/HTTPS
// server bootstrap, mux route wiring (healthz/metrics/proxy/redirect),
// signal handling (SIGTERM/SIGHUP), and graceful shutdown orchestration.
//
// This file requires Go 1.22+ at build time because it uses the
// standard-library "log/slog" structured logging package (available since
// Go 1.21) and generics-adjacent stdlib APIs used elsewhere in the module.
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
	"syscall"
	"time"
)

// ---------------------------------------------------------------------------
// Entry point
// ---------------------------------------------------------------------------

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// healthCheckerAdapter satisfies the healthChecker interface (declared in
