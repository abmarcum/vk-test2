// Package main: intentionally left as a minimal placeholder.
//
// The actual application entry point (main, run, and all HTTP/HTTPS
// lifecycle wiring: config loading, Router/ProxyServer/Mux construction,
// signal handling, and graceful shutdown) lives in proxy.go, next to the
// types it constructs and wires together (Router, ProxyServer, Mux,
// Metrics). This file declares nothing further to avoid duplicate
// declaration conflicts (duplicate main/run/envOrDefault) between the two
// files.
package main
