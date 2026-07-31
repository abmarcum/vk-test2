// invocation, TLS listener construction, HTTP server bootstrap,
// signal handling (SIGTERM/SIGHUP), graceful shutdown/drain, and
// top-level wiring of Router <-> Pool <-> HealthChecker.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)
