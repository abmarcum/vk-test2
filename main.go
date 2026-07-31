// Package main implements the GoProxy entry point: process lifecycle,
// config loading, logger construction, TLS listener setup, HTTP/HTTPS
// server bootstrap, mux route wiring (healthz/metrics/proxy), signal
// handling (SIGTERM/SIGHUP), and graceful shutdown orchestration.
//
// Requires Go 1.21+ at build time for the standard-library "log/slog"
