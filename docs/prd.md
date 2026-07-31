# Product Requirements Document (PRD)

## 1. Product Overview

**Product Name:** GoProxy — Simple SSL-Enabled HTTP Reverse Proxy & Load Balancer

**Summary:** A lightweight, production-grade HTTP server written in Go that terminates SSL/TLS, reverse-proxies HTTP requests, and load-balances traffic across multiple upstream backend servers. Designed for deployment on Google Cloud Platform (GCP), with a minimal, tightly-scoped codebase (2–4 core files).

**Problem Statement:** Teams often need a small, auditable, dependency-light reverse proxy/load balancer for internal services or edge routing without adopting a full-scale solution (e.g., Envoy, Nginx, HAProxy). This product fills that gap with a single static Go binary.

**Target Users:** Backend/platform engineers, DevOps teams, small-to-mid scale service operators on GCP.

---

## 2. Goals & Non-Goals

### Goals
- Provide SSL/TLS termination for incoming HTTPS traffic.
- Reverse proxy HTTP(S) requests to one or more backend upstream servers.
- Load balance requests across upstreams using a configurable strategy (round-robin, least-connections).
- Perform active health checks on upstreams; remove unhealthy nodes from rotation automatically.
- Be configurable via a single YAML/JSON config file (no code changes needed to add backends).
- Run as a single static binary in a Docker container, deployable on GCP (GCE, GKE, or Cloud Run with caveats for TLS).
- Expose basic observability: structured logs, `/healthz`, `/metrics` (Prometheus format).

### Non-Goals
- No support for gRPC-specific load balancing (HTTP/1.1 and HTTP/2 over TLS only).
- No built-in service discovery (e.g., Consul, etcd integration) — backends are statically configured (v1).
- No WAF / advanced security filtering.
- No UI/dashboard — configuration and observability via files and standard endpoints only.
- No automatic certificate provisioning via ACME in v1 (certs supplied via GCP Secret Manager or mounted files); ACME/Let's Encrypt is a stated future enhancement.

---

## 3. Key Features & Requirements

### 3.1 SSL/TLS Termination
- Load TLS certificate/key pairs from local files or GCP Secret Manager (env-var-driven paths).
- Support TLS 1.2 and 1.3 only; disable weak ciphers.
- Support SNI-based multi-domain certs (optional, single cert acceptable for v1).
- Graceful fallback: if TLS config absent, server can run HTTP-only (for local dev/testing).

### 3.2 Reverse Proxy
- Forward incoming requests to backend upstreams using Go's `net/http/httputil.ReverseProxy`.
- Preserve/modify standard proxy headers: `X-Forwarded-For`, `X-Forwarded-Proto`, `X-Forwarded-Host`.
- Configurable request timeout and idle connection settings.
- Support path-based routing rules (optional, simple prefix match) mapping to upstream pools.

### 3.3 Load Balancing
- Strategies (configurable per route/pool):
  - Round Robin (default)
  - Least Connections
  - Random (stretch goal)
- Active health checks: periodic HTTP GET to configurable health path per backend; mark unhealthy after N consecutive failures; reintroduce after M consecutive successes.
- Passive detection: mark backend as temporarily down after connection failure during proxying (circuit-breaker-lite).

### 3.4 Configuration
- Single YAML config file defining:
  - Listen address/port (HTTP + HTTPS)
  - TLS cert/key paths
  - Upstream pools (name, backend URLs, LB strategy, health check settings)
  - Routing rules (path prefix → pool)
  - Timeouts
- Config loaded at startup; SIGHUP triggers safe reload (optional stretch goal, else restart-only in v1).

### 3.5 Observability
- Structured JSON logging (request method, path, upstream chosen, latency, status code).
- `/healthz` endpoint for the proxy itself (liveness/readiness).
- `/metrics` endpoint exposing Prometheus metrics: request count, latency histogram, upstream health status, active connections per upstream.

### 3.6 Deployment (GCP)
- Ship as a Docker container image (multi-stage build, distroless/alpine base).
- Deployable to:
  - **GKE** (recommended): Deployment + Service + Ingress or direct LoadBalancer Service for TLS passthrough or termination at pod.
  - **GCE**: VM running the container via Container-Optimized OS.
- Use GCP Secret Manager for TLS cert/key retrieval at startup (via mounted CSI driver in GKE, or fetched via API using Workload Identity).
- Compatible with GCP Cloud Logging (stdout JSON logs auto-ingested) and Cloud Monitoring (via Prometheus sidecar or Managed Service for Prometheus).

---

## 4. Architecture Overview

```
                        ┌─────────────────────────┐
   Client (HTTPS) ─────▶│   GoProxy (this app)    │
                        │  - TLS Termination      │
                        │  - Reverse Proxy        │
                        │  - Load Balancer        │
                        │  - Health Checker       │
                        │  - Metrics/Logging      │
                        └──────────┬──────────────┘
                                   │ HTTP(S)
                     ┌─────────────┼──────────────┐
                     ▼             ▼              ▼
               Backend A      Backend B      Backend C
              (upstream)     (upstream)     (upstream)
```

**Runtime flow:**
1. Config loaded at startup → builds upstream pools + router.
2. TLS listener starts (and optional HTTP listener for redirect/health).
3. Health checker goroutine runs on interval, updates shared pool state.
4. Incoming request → router matches path → selects pool → LB strategy picks healthy backend → `httputil.ReverseProxy` forwards → response streamed back → metrics/log recorded.

---

## 5. Code File Manifest (STRICT: 2–4 core files)

To meet the strict scope constraint, the application is organized into **4 core Go source files**, each with a single clear responsibility. No additional source files are permitted without a PRD revision.

| # | File | Responsibility |
|---|------|-----------------|
| 1 | `main.go` | Entry point: config loading, TLS setup, HTTP server bootstrap, graceful shutdown, wiring of router/proxy/balancer/health checker. |
| 2 | `config.go` | Config struct definitions, YAML parsing/validation, GCP Secret Manager integration for TLS material. |
| 3 | `proxy.go` | Reverse proxy logic, path-based routing, request/response header manipulation, Prometheus metrics + structured logging middleware. |
| 4 | `balancer.go` | Load balancer strategies (round robin, least connections), upstream pool state management, active/passive health checking. |

**Supporting non-code files (excluded from the 2–4 constraint):**
- `config.yaml.example` — sample configuration
- `Dockerfile` — multi-stage build
- `go.mod` / `go.sum` — dependency manifest
- `README.md` — usage documentation
- `k8s/*.yaml` — GKE deployment manifests (Deployment, Service, ConfigMap, SecretProviderClass)
- `Makefile` — build/test/lint targets

**Explicitly out of scope for code files:** No separate packages/directories (e.g., `/internal/router`), no test-only helper files beyond standard `_test.go` counterparts of the 4 files above (test files are exempt from the core-file count since they are validation artifacts, not core app logic).

---

## 6. Technology Stack

| Layer | Choice |
|---|---|
| Language | Go 1.22+ |
| HTTP/Proxy | Standard library `net/http`, `net/http/httputil` |
| Config parsing | `gopkg.in/yaml.v3` |
| Metrics | `github.com/prometheus/client_golang` |
| Logging | Standard library `log/slog` (structured JSON) |
| Secrets | `cloud.google.com/go/secretmanager` (GCP SDK) |
| Container | Distroless or Alpine base image |
| Orchestration | GKE (primary), GCE (alternative) |

---

## 7. Configuration Schema (Example)

```yaml
server:
  http_addr: ":8080"
  https_addr: ":8443"
  tls:
    cert_source: "secretmanager"   # or "file"
    cert_secret: "projects/my-proj/secrets/tls-cert/versions/latest"
    key_secret: "projects/my-proj/secrets/tls-key/versions/latest"
  timeouts:
    read_timeout: "10s"
    write_timeout: "30s"
    idle_timeout: "120s"

routes:
  - path_prefix: "/"
    pool: "default-pool"

pools:
  - name: "default-pool"
    strategy: "round_robin"   # round_robin | least_conn
    health_check:
      path: "/healthz"
      interval: "5s"
      timeout: "2s"
      unhealthy_threshold: 3
      healthy_threshold: 2
    backends:
      - url: "http://10.0.1.10:8081"
      - url: "http://10.0.1.11:8081"
      - url: "http://10.0.1.12:8081"
```

---

## 8. Non-Functional Requirements

| Category | Requirement |
|---|---|
| Performance | Handle ≥5,000 req/s on a 4-vCPU GCE/GKE node with p99 added latency < 5ms over direct backend call. |
| Reliability | Zero dropped requests during single backend failure (auto-failover via health check within configured interval). |
| Security | TLS 1.2+ only; no secrets in logs; least-privilege GCP service account (Secret Manager Accessor role only). |
| Scalability | Stateless design — horizontally scalable behind GCP Load Balancer/GKE Service. |
| Observability | 100% of requests logged with latency/status; metrics scrape endpoint available. |
| Portability | Single static binary; runs identically in Docker locally and on GCP. |
| Graceful Shutdown | SIGTERM triggers connection draining (configurable grace period, default 15s). |

---

## 9. Success Metrics

- **Correctness:** 0 critical bugs in health-check/failover logic during load test with simulated backend failures.
- **Performance:** Meets ≥5,000 req/s target with p99 latency overhead <5ms.
- **Operability:** Time-to-deploy on fresh GKE cluster ≤ 15 minutes following README.
- **Code Scope Compliance:** Exactly 4 core `.go` files maintained; enforced via CI lint check (fails build if a 5th core file is added without PRD sign-off).

---

## 10. Milestones

| Phase | Deliverable |
|---|---|
| M1 | `config.go` + config schema finalized; unit tests for parsing/validation |
| M2 | `balancer.go` — round robin + health checking functional |
| M3 | `proxy.go` — reverse proxy + metrics/logging integrated |
| M4 | `main.go` — TLS bootstrap, graceful shutdown, full wiring |
| M5 | Dockerfile + GKE manifests; deploy to GCP staging project |
| M6 | Load testing, docs finalization, GA release |

---

## 11. Risks & Mitigations

| Risk | Mitigation |
|---|---|
| Scope creep beyond 4 files | Enforce via CI file-count lint check on PR |
| TLS cert rotation downtime | Support hot-reload via SIGHUP or periodic Secret Manager poll (stretch) |
| Single point of failure (proxy itself) | Deploy multiple replicas behind GCP HTTP(S) Load Balancer or GKE Service with multiple pods |
| Backend flapping causing thrash | Configurable thresholds + minimum recovery interval |

---

## 12. Future Enhancements (Out of v1 Scope)
- ACME/Let's Encrypt auto-provisioning
- Dynamic config reload without restart
- Service discovery integration (GCP Service Directory)
- gRPC/HTTP2 load balancing awareness
- WebSocket proxying hardening
- Rate limiting / circuit breaker tuning UI

--- Cohere AI Quality Audit ---
# Product: GoProxy - HTTP Reverse Proxy & Load Balancer

## Technical Specification:

**Functionality:**
- SSL/TLS termination for HTTPS traffic.
- Reverse proxy HTTP requests to multiple backend servers.
- Load balancing with strategies: round-robin, least-connections, random.
- Health checks: active (periodic GET) and passive (connection failure).
- Configuration via YAML/JSON file.
- Observability: structured logs, health, and metrics endpoints.

**Interfaces:**
- Client: HTTPS requests to GoProxy.
- GoProxy: terminates TLS, proxies to backends, load balances.
- Backends: upstream servers receiving proxied requests.

**API Endpoints:**
- `/healthz`: liveness/readiness check.
- `/metrics`: Prometheus metrics exposure.

**Database Schema (Prisma/DDL):**
N/A

**Data Structures:**
- `Config`: YAML/JSON config with server, routes, pools, and timeouts.
- `Pool`: upstream pool with strategy, health check settings, and backends.
- `Route`: path prefix to pool mapping.

**Core Requirements:**
- Load TLS certs from files or GCP Secret Manager.
- Support TLS 1.2/1.3, SNI multi-domain certs (optional).
- Reverse proxy with Go's `net/http/httputil.ReverseProxy`.
- Configure request timeouts, idle connections.
- Load balance with strategies, active/passive health checks.
- YAML config for server, routes, pools, and timeouts.
- Structured JSON logging.
- Docker container deployment on GCP (GKE, GCE).
- Integrate with GCP Secret Manager for TLS certs.
- Meet performance, reliability, security, and scalability goals.

## Architecture:
GoProxy app with 4 core Go files:
1. `main.go`: entry point, config loading, TLS setup, server bootstrap.
2. `config.go`: config parsing, GCP Secret Manager integration.
3. `proxy.go`: reverse proxy, path routing, logging middleware.
4. `balancer.go`: load balancing strategies, health checking.

## Technology Stack:
- Language: Go 1.22+
- HTTP/Proxy: `net/http`, `net/http/httputil`
- Config: `gopkg.in/yaml.v3`
- Metrics: `github.com/prometheus/client_golang`
- Logging: `log/slog` (structured JSON)
- Secrets: `cloud.google.com/go/secretmanager` (GCP SDK)
- Container: Distroless/Alpine
- Orchestration: GKE/GCE

## Configuration Example:
```yaml
server:
  ...
routes:
  ...
pools:
  ...
```

## Non-Functional Requirements:
- Performance: ≥5,000 req/s, p99 latency < 5ms.
- Reliability: zero dropped requests during backend failure.
- Security: TLS 1.2+, least-privilege GCP access.
- Scalability: stateless, horizontally scalable.
- Observability: log all requests, expose metrics.
- Portability: single static binary, Docker.
- Graceful shutdown: SIGTERM with connection draining.

## Success Metrics:
- Correct health-check/failover logic.
- Performance targets met.
- Quick GKE deployment.
- Maintain 4 core `.go` files.

## Milestones:
- M1: Config finalization, unit tests.
- M2: Load balancing, health checking.
- M3: Reverse proxy, metrics/logging.
- M4: TLS setup, graceful shutdown.
- M5: Docker, GKE deployment.
- M6: Load testing, GA release.

## Risks & Mitigations:
- Scope creep: CI lint check on PRs.
- Cert rotation: hot-reload or periodic Secret Manager poll.
- Single point of failure: multiple replicas behind load balancer.
- Backend flapping: configurable thresholds.
