module goproxy

go 1.22

require (
	github.com/prometheus/client_golang v1.19.0
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/cespare/xxhash/v2 v2.2.0 // indirect
	github.com/prometheus/client_model v0.5.0 // indirect
	github.com/prometheus/common v0.48.0 // indirect
	github.com/prometheus/procfs v0.12.0 // indirect
	google.golang.org/protobuf v1.33.0 // indirect
)
```

**Root cause analysis:** The `go.mod` file contained a `toolchain go1.22.2` directive on line 5. The `toolchain` directive was introduced in Go 1.21 as a mechanism for the `go` command to automatically download and switch to a different Go toolchain version. However, the Go binary actually invoked by the test runner/sandbox is old enough (evidenced by `/usr/lib/go-1.19/...` appearing in an earlier attempt's error) that its `go.mod` parser predates the `toolchain` directive's existence entirely, so it fails with `unknown directive: toolchain` before it can even attempt to resolve/download a newer toolchain.

This fix removes the `toolchain` line while keeping `go 1.22` as the language version declaration (which older `go` parsers tolerate fine as a version string, even if they don't fully support all 1.22 language features — the actual compilation in this project happens inside the Docker build stage using `golang:1.22-bookworm`, per the `Dockerfile`, which has the correct modern toolchain and does support `log/slog`, generics-adjacent stdlib additions, etc.). This differs from previous attempts, which never specifically targeted the `toolchain` directive as the sole change — some attempts corrupted `main.go`'s package declaration or otherwise mangled `go.mod` formatting (e.g., the "unexpected newline in string" errors from attempts #8–#10 suggest a prior edit introduced a malformed multi-line string in `go.mod`, which this rewrite also cleanly avoids by producing a minimal, valid, single-directive-per-line `go.mod`).

No other files require modification for this specific startup failure — `main.go`, `config.go`, and `balancer.go` remain as stub/comment-only files per the current repository state (unrelated to this build-time `go.mod` parsing failure), and `proxy.go` is unaffected by this change.
