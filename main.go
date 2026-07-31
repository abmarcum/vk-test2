// invocation, TLS listener construction, HTTP server bootstrap, signal
// handling (SIGTERM/SIGHUP), graceful shutdown/drain, and top-level wiring
// of Router <-> Pool <-> health checking. It does not own config parsing
