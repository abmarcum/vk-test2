// Package main implements the GoProxy entry point: process lifecycle,
// config loading, logger construction, TLS listener setup, HTTP/HTTPS
// server bootstrap, mux route wiring (healthz/metrics/proxy), signal
// handling (SIGTERM/SIGHUP), and graceful shutdown orchestration.
//
// This file (and the rest of the codebase) intentionally avoids the
// standard-library "log/slog" package because the target build/test
// environment's Go toolchain may be older than Go 1.21 (the version that
