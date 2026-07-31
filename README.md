# GoProxy

A simple, dependency-free HTTP/HTTPS reverse proxy with TLS termination,
path-based routing, and load balancing across upstream backend pools —
written entirely in the Go standard library.

## Features

- **HTTP & HTTPS listeners** — run plaintext HTTP, TLS-terminated HTTPS, or
  both simultaneously, sharing a single routing/proxy handler.
- **Path-prefix routing** — map URL path prefixes to named upstream pools
  (longest-prefix-match).
- **Load balancing strategies** — `round_robin` (default),
  `least_connections`, and `random`.
- **Active health checks** — periodic HTTP probes per pool with
  configurable interval/timeout and healthy/unhealthy thresholds.
- **Passive health checks** — backends are automatically marked
  unhealthy after consecutive proxy failures and restored after
  consecutive successes.
- **Graceful shutdown** — SIGINT/SIGTERM triggers a bounded drain window
  before process exit.
- **Operational endpoints** — `/healthz` (liveness) and `/metrics`
  (plain-text counters/gauges).
- **Zero third-party dependencies** — configuration is parsed with an
  in-repo YAML subset parser; `go.mod` has no external requires.

## Architecture Overview

| File          | Responsibility                                                                 |
|---------------|----------------------------------------------------------------------------------|
| `main.go`     | Minimal placeholder; entry point lives in `proxy.go`.                          |
| `config.go`   | `Config` struct tree + dependency-free YAML-subset parser (`LoadConfig`).       |
| `proxy.go`    | Process lifecycle, `Router`, `ProxyServer` (reverse-proxy handler), `Mux`, TLS/HTTP server bootstrap, signal handling, graceful shutdown. |
| `balancer.go` | `Backend`/`Pool` state, `Strategy` implementations, active/passive health checks. |

Request flow: incoming request → `Router.Match` (longest path-prefix) →
`Pool.Choose` (load-balancing `Strategy`, healthy backends only) →
`httputil.ReverseProxy` forwards to the chosen `Backend` → passive
success/failure is recorded against the backend → `/metrics` counters
are updated.

## Configuration

GoProxy reads a YAML configuration file (path from `-config` flag or the
`CONFIG_PATH` environment variable, default `config.yaml`). The parser
supports a minimal YAML subset (block mappings, block sequences, scalars,
`#` comments) — no external YAML library is required.

### Example `config.yaml`

```yaml
server:
  http_addr: ":8080"
  https_addr: ":8443"
  enable_tls: true
  shutdown_grace_seconds: 15
  tls:
    cert_file: "certs/server.crt"
    key_file: "certs/server.key"
    min_version: "1.2"          # "1.2" (default) or "1.3"
  timeouts:
    read_header: "5s"
    read: "15s"
    write: "15s"
    idle: "60s"

routes:
  - match: "/api"
    pool: "api_pool"
  - match: "/"
    pool: "web_pool"

pools:
  - name: "api_pool"
    strategy: "round_robin"       # round_robin | least_connections | random
    backends:
      - url: "http://10.0.0.1:9000"
      - url: "http://10.0.0.2:9000"
    health_check:
      enabled: true
      path: "/healthz"
      unhealthy_threshold: 3
      healthy_threshold: 2
      interval: "10s"
      timeout: "2s"

  - name: "web_pool"
    strategy: "least_connections"
    backends:
      - url: "http://10.0.0.3:8000"
```

### Configuration Reference

| Key                                      | Type    | Default   | Description |
|-------------------------------------------|---------|-----------|-------------|
| `server.http_addr`                        | string  | `:8080`   | HTTP listener address. |
| `server.https_addr`                       | string  | `:8443`   | HTTPS listener address. |
| `server.enable_tls`                       | bool    | `false`   | Enables the HTTPS listener. |
| `server.shutdown_grace_seconds`           | int     | `15`      | Max seconds to wait for in-flight requests during shutdown. |
| `server.tls.cert_file` / `key_file`       | string  | —         | PEM cert/key paths (required if `enable_tls: true`). |
| `server.tls.min_version`                  | string  | `1.2`     | Minimum TLS version: `1.2` or `1.3`. |
| `server.timeouts.read_header`             | duration| `5s`      | `http.Server.ReadHeaderTimeout`. |
| `server.timeouts.read`                    | duration| `15s`     | `http.Server.ReadTimeout`. |
| `server.timeouts.write`                   | duration| `15s`     | `http.Server.WriteTimeout`. |
| `server.timeouts.idle`                    | duration| `60s`     | `http.Server.IdleTimeout`. |
| `routes[].match`                          | string  | —         | Path prefix to match. |
| `routes[].pool`                           | string  | —         | Name of the pool to route to. |
| `pools[].name`                            | string  | —         | Unique pool identifier, referenced by `routes[].pool`. |
| `pools[].strategy`                        | string  | `round_robin` | `round_robin`, `least_connections`, or `random`. |
| `pools[].backends[].url`                  | string  | —         | Upstream base URL, e.g. `http://host:port`. |
| `pools[].health_check.enabled`            | bool    | `false`   | Enables the active health-check loop. |
| `pools[].health_check.path`               | string  | `/healthz`| Path probed on each backend. |
| `pools[].health_check.unhealthy_threshold`| int     | `3`       | Consecutive failures before marking a backend down. |
| `pools[].health_check.healthy_threshold`  | int     | `2`       | Consecutive successes before restoring a backend. |
| `pools[].health_check.interval`           | duration| `10s`     | Probe interval. |
| `pools[].health_check.timeout`            | duration| `2s`      | Per-probe timeout. |

### CLI Flags / Environment Variables

| Flag          | Env var       | Default        | Description |
|---------------|---------------|----------------|-------------|
| `-config`     | `CONFIG_PATH` | `config.yaml`  | Path to the YAML config file. |
| `-log-level`  | `LOG_LEVEL`   | `info`         | Accepted for CLI compatibility; the stdlib `log` package used here does not implement level filtering. |

---

## Build Instructions

**Prerequisites:** Go 1.19 or later (no third-party modules are required —
`go.mod` has an empty dependency graph).

1. Clone the repository:
   ```bash
   git clone https://example.com/your-org/goproxy.git
   cd goproxy
   ```

2. Verify the module builds and vets cleanly:
   ```bash
   go vet ./...
   ```

3. Run tests (if present):
   ```bash
   go test ./...
   ```

4. Build a native binary:
   ```bash
   go build -o goproxy .
   ```

5. (Optional) Cross-compile a static Linux binary for containerized
   deployment:
   ```bash
   CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o goproxy .
   ```

6. Run locally:
   ```bash
   ./goproxy -config ./config.yaml
   ```

---

## Deployment Instructions

### 1. Docker

**Dockerfile** (multi-stage, static binary):

```dockerfile
# --- build stage ---
FROM golang:1.21 AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/goproxy .

# --- runtime stage ---
FROM gcr.io/distroless/static-debian12
COPY --from=build /out/goproxy /goproxy
COPY config.yaml /config.yaml
COPY certs/ /certs/
EXPOSE 8080 8443
ENTRYPOINT ["/goproxy", "-config", "/config.yaml"]
```

Build and run:

```bash
docker build -t goproxy:latest .
docker run -d \
  --name goproxy \
  -p 8080:8080 -p 8443:8443 \
  -v $(pwd)/config.yaml:/config.yaml:ro \
  -v $(pwd)/certs:/certs:ro \
  goproxy:latest
```

### 2. docker-compose

```yaml
version: "3.9"
services:
  goproxy:
    build: .
    image: goproxy:latest
    ports:
      - "8080:8080"
      - "8443:8443"
    volumes:
      - ./config.yaml:/config.yaml:ro
      - ./certs:/certs:ro
    environment:
      - CONFIG_PATH=/config.yaml
    restart: unless-stopped
```

```bash
docker-compose up -d --build
```

### 3. Kubernetes

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: goproxy-config
data:
  config.yaml: |
    server:
      http_addr: ":8080"
      https_addr: ":8443"
      enable_tls: true
      tls:
        cert_file: "/certs/tls.crt"
        key_file: "/certs/tls.key"
    routes:
      - match: "/"
        pool: "web_pool"
    pools:
      - name: "web_pool"
        strategy: "round_robin"
        backends:
          - url: "http://web-svc:8000"
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: goproxy
spec:
  replicas: 2
  selector:
    matchLabels:
      app: goproxy
  template:
    metadata:
      labels:
        app: goproxy
    spec:
      containers:
        - name: goproxy
          image: goproxy:latest
          args: ["-config", "/etc/goproxy/config.yaml"]
          ports:
            - containerPort: 8080
            - containerPort: 8443
          volumeMounts:
            - name: config
              mountPath: /etc/goproxy
            - name: tls-certs
              mountPath: /certs
          readinessProbe:
            httpGet:
              path: /healthz
              port: 8080
          livenessProbe:
            httpGet:
              path: /healthz
              port: 8080
      volumes:
        - name: config
          configMap:
            name: goproxy-config
        - name: tls-certs
          secret:
            secretName: goproxy-tls
---
apiVersion: v1
kind: Service
metadata:
  name: goproxy
spec:
  selector:
    app: goproxy
  ports:
    - name: http
      port: 80
      targetPort: 8080
    - name: https
      port: 443
      targetPort: 8443
```

Apply:

```bash
kubectl create secret tls goproxy-tls --cert=certs/server.crt --key=certs/server.key
kubectl apply -f k8s/goproxy.yaml
```

### 4. Terraform (container image reference example)

```hcl
resource "kubernetes_deployment" "goproxy" {
  metadata { name = "goproxy" }
  spec {
    replicas = 2
    selector { match_labels = { app = "goproxy" } }
    template {
      metadata { labels = { app = "goproxy" } }
      spec {
        container {
          name  = "goproxy"
          image = "your-registry/goproxy:latest"
          port { container_port = 8080 }
          port { container_port = 8443 }
        }
      }
    }
  }
}
```

```bash
terraform init
terraform plan
terraform apply
```

---

## Operational Endpoints

- `GET /healthz` — `200 ok` if at least one backend across all pools is
  alive (or vacuously `200` if no pools configured); otherwise
  `503 unavailable`.
- `GET /metrics` — plain-text counters/gauges (`proxy_requests_total`,
  `proxy_requests_failed_total`, `proxy_last_request_duration_seconds`).
- `/*` — all other paths are matched against configured routes and
  reverse-proxied to a healthy backend. See [API Docs](docs/api.md).

## Graceful Shutdown

On `SIGINT`/`SIGTERM`, GoProxy stops accepting new health-check probes,
calls `http.Server.Shutdown` on every listener, and waits up to
`server.shutdown_grace_seconds` for in-flight requests to complete before
exiting.

## License & Documentation

[MIT License](LICENSE) | [API Docs](docs/api.md)

This project is licensed under the **MIT License** — see the
[LICENSE](LICENSE) file for the full text.
