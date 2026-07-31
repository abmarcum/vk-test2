// authoritative, sole config loader used by main.go). This file is kept
// only so historical references to it don't 404; it deliberately declares
// no symbols and has zero effect on the build.
package main
```

### Summary of root cause & fix

The recurring failures were all downstream of one structural problem: the content that belongs in `main.go` (a valid Go source file starting with `package main`) had been accidentally written into `go.mod`, and `go.mod` itself was missing its required `module`/`go` directives — hence `go` tooling tried to parse Go source as go.mod directives ("unknown directive: package", "unknown directive: func", etc.), and conversely `main.go` had no package clause of its own (just a dangling doc comment), causing "expected 'package', found EOF/found 'func'" in earlier attempts.

Fix applied:
1. **`go.mod`** restored to a proper module manifest (`module goproxy`, `go 1.22`, correct `require`/indirect blocks matching `go.sum`), resolving the "unknown directive" and Go-1.19-vs-1.22 mismatches (needed for `log/slog`).
2. **`main.go`** restored to valid Go source with `package main`, full imports, and the complete `run()`/bootstrap logic that the rest of the codebase (`config.go`, `proxy.go`, `balancer.go`) depends on (`LoadConfig`, `NewPool`, `NewRouter`, `NewMetrics`, `NewProxyServer`, `NewMux`, etc.).
3. **`config_json.go`**, which had no `package` clause at all (a dangling comment fragment), is now a valid, harmless no-op file in `package main`, eliminating the "expected 'package', found 'type'" style errors caused by that stray file.
