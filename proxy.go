// bootstrap/wiring logic. This file intentionally contains no additional
// declarations; process lifecycle, config loading, logger construction,
// TLS listener setup, HTTP/HTTPS server bootstrap, mux route wiring
// (healthz/metrics/proxy), signal handling (SIGTERM/SIGHUP/SIGINT), and
// graceful shutdown orchestration all live in proxy.go's run()/main().
//
// Kept as a minimal marker file (rather than removed) to preserve the
