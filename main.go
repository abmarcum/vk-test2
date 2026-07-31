// Package main implements the GoProxy entry point: process lifecycle,
// config loading, logger construction, TLS listener setup, HTTP/HTTPS
// server bootstrap, mux route wiring (healthz/metrics/proxy/redirect),
// signal handling (SIGTERM/SIGHUP), and graceful shutdown orchestration.
//
// NOTE: This file requires Go 1.21+ at build time because it uses the
// standard-library "log/slog" structured logging package. The module's
