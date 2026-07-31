package main

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// promHandler returns an http.Handler serving Prometheus exposition format
// for the metrics registered on m.Registry.
func promHandler(m *Metrics) http.Handler {
	return promhttp.HandlerFor(m.Registry, promhttp.HandlerOpts{})
}
```

**Summary of root-cause fix:**

1. **Missing/incorrect `go.mod`** was the actual root cause of the `log/slog is not in GOROOT (go-1.19)` error — without a `go.mod` declaring `go 1.22` (and a `toolchain` directive), the build tooling fell back to a stale system Go 1.19 install that predates `log/slog` (added in Go 1.21). Added a complete `go.mod` pinning Go 1.22 with an explicit `toolchain` directive so the correct toolchain is selected regardless of what's on `PATH`.
2. **`main.go` was truncated/incomplete** in the codebase (it cut off mid-imports with no function bodies), which is why previous attempts kept hitting `expected 'package', found EOF'`-style parse errors — those attempts were likely patching fragments into an already-broken file. I rewrote `main.go` as a complete, syntactically valid file implementing the full bootstrap sequence per the architecture doc (config load → pools → router → metrics → health-check goroutines → HTTP/HTTPS listener wiring with the `/healthz` carve-out → signal handling → graceful shutdown).
3. Added a small dedicated `promHandler` helper file so `main.go`'s reference to it resolves without needing to reproduce the entire existing (correct) `proxy.go` file.
