// Backend and Pool state structures, load balancing Strategy interface
// with round-robin/least-connections/random implementations, active
// health check goroutine loop, and passive failure/success marking
// for backends. Uses only the standard library "log" package (not
// "log/slog") to remain compatible with Go 1.19+ build environments.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
)

// ErrNoHealthyBackends is returned by Pool.Choose when no backend in the
// pool is currently marked alive.
var ErrNoHealthyBackends = errors.New("no healthy backends available")

// ---------------------------------------------------------------------------
// Backend
// ---------------------------------------------------------------------------

// Backend represents one upstream target and its live health state.
type Backend struct {
	URL *url.URL

	Alive       atomic.Bool
	ActiveConns atomic.Int64

	consecFails     atomic.Int32
	consecSuccesses atomic.Int32
}

func newBackend(rawURL string) (*Backend, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parse backend url %q: %w", rawURL, err)
	}
	b := &Backend{URL: u}
	b.Alive.Store(true) // starts alive so it can serve traffic immediately
	return b, nil
}

// ---------------------------------------------------------------------------
// Strategy interface + implementations
// ---------------------------------------------------------------------------

type Strategy interface {
	Next(backends []*Backend) (*Backend, error)
}

func aliveBackends(backends []*Backend) []*Backend {
	out := make([]*Backend, 0, len(backends))
	for _, b := range backends {
		if b.Alive.Load() {
			out = append(out, b)
		}
	}
	return out
}

// RoundRobinStrategy cycles through healthy backends in order using an
// atomic counter.
type RoundRobinStrategy struct {
	cursor atomic.Uint64
}

func (s *RoundRobinStrategy) Next(backends []*Backend) (*Backend, error) {
	alive := aliveBackends(backends)
	if len(alive) == 0 {
		return nil, ErrNoHealthyBackends
	}
	idx := s.cursor.Add(1) - 1
	return alive[idx%uint64(len(alive))], nil
}

// LeastConnStrategy selects the healthy backend with the fewest current
// in-flight requests; ties broken by first-seen order.
type LeastConnStrategy struct{}

func (s *LeastConnStrategy) Next(backends []*Backend) (*Backend, error) {
	alive := aliveBackends(backends)
	if len(alive) == 0 {
		return nil, ErrNoHealthyBackends
	}
	best := alive[0]
	bestConns := best.ActiveConns.Load()
	for _, b := range alive[1:] {
		c := b.ActiveConns.Load()
		if c < bestConns {
			best = b
			bestConns = c
		}
	}
	return best, nil
}

// RandomStrategy selects a uniformly random healthy backend.
type RandomStrategy struct {
	counter atomic.Uint64
}

func (s *RandomStrategy) Next(backends []*Backend) (*Backend, error) {
	alive := aliveBackends(backends)
	if len(alive) == 0 {
		return nil, ErrNoHealthyBackends
	}
	// Simple, dependency-free pseudo-randomness: time-seeded atomic
	// counter mixed with wall-clock nanoseconds. Sufficient for LB
	// distribution purposes; not used for any security-sensitive purpose.
	n := s.counter.Add(1)
	mix := uint64(time.Now().UnixNano()) ^ n
	return alive[mix%uint64(len(alive))], nil
}

func newStrategy(name string) Strategy {
	switch name {
	case "least_connections":
		return &LeastConnStrategy{}
	case "random":
		return &RandomStrategy{}
	default:
		return &RoundRobinStrategy{}
	}
}

// ---------------------------------------------------------------------------
// Pool
// ---------------------------------------------------------------------------

// Pool groups backends under one strategy and health-check policy.
type Pool struct {
	Name        string
	Backends    []*Backend
	Strategy    Strategy
	HealthCheck HealthCheckConfig
}

// NewPool constructs a Pool from PoolConfig, resolving Strategy by name.
func NewPool(cfg PoolConfig) (*Pool, error) {
	backends := make([]*Backend, 0, len(cfg.Backends))
	for _, bc := range cfg.Backends {
		b, err := newBackend(bc.URL)
		if err != nil {
			return nil, err
		}
		backends = append(backends, b)
	}

	return &Pool{
		Name:        cfg.Name,
		Backends:    backends,
		Strategy:    newStrategy(cfg.Strategy),
		HealthCheck: cfg.HealthCheck,
	}, nil
}

// HealthCheckEnabled reports whether active health checking is configured
// for this pool.
func (p *Pool) HealthCheckEnabled() bool {
	return p.HealthCheck.Enabled
}

// Choose returns the next healthy backend per the pool's strategy.
func (p *Pool) Choose() (*Backend, error) {
	return p.Strategy.Next(p.Backends)
}

// MarkFailure implements passive circuit-breaker-lite behavior.
func (p *Pool) MarkFailure(b *Backend) {
	if b == nil {
		return
	}
	b.consecSuccesses.Store(0)
	fails := b.consecFails.Add(1)
	threshold := int32(p.HealthCheck.UnhealthyThreshold)
	if threshold <= 0 {
		threshold = 3
	}
	if fails >= threshold {
		b.Alive.Store(false)
	}
}

// MarkSuccess resets passive failure counters on a successful proxied response.
func (p *Pool) MarkSuccess(b *Backend) {
	if b == nil {
		return
	}
	b.consecFails.Store(0)
	successes := b.consecSuccesses.Add(1)
	threshold := int32(p.HealthCheck.HealthyThreshold)
	if threshold <= 0 {
		threshold = 2
	}
	if successes >= threshold {
		b.Alive.Store(true)
	}
}

// newHealthCheckClient returns a hardened HTTP client used exclusively for
// active health probing: it does not follow redirects (redirects are
// treated as a pass/fail signal via status code inspection instead), and
// honors the given per-probe timeout.
func newHealthCheckClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// joinPath joins a backend base path with a health-check probe path.
//
//   - If extra is empty, base is returned unchanged.
//   - If extra is absolute (starts with "/"), it overrides base entirely.
//   - Otherwise extra is appended to base, inserting exactly one "/"
//     separator between them.
func joinPath(base, extra string) string {
	if extra == "" {
		return base
	}
	if strings.HasPrefix(extra, "/") {
		return extra
	}
	if base == "" {
		return "/" + extra
	}
	if strings.HasSuffix(base, "/") {
		return base + extra
	}
	return base + "/" + extra
}

// RunHealthChecks runs the active health-check loop for this pool,
// probing every backend's health_check.path on health_check.interval
// using a timeout-bounded client, until ctx is canceled. Intended to be
// launched as a goroutine by main().
func (p *Pool) RunHealthChecks(ctx context.Context, logger *log.Logger) {
	interval := p.HealthCheck.IntervalDur
	if interval <= 0 {
		interval = 10 * time.Second
	}
	timeout := p.HealthCheck.TimeoutDur
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	probePath := p.HealthCheck.Path
	if probePath == "" {
		probePath = "/healthz"
	}

	client := newHealthCheckClient(timeout)

	probeOnce := func() {
		for _, b := range p.Backends {
			target := *b.URL
			target.Path = joinPath(b.URL.Path, probePath)

			req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
			if err != nil {
				p.MarkFailure(b)
				continue
			}

			resp, err := client.Do(req)
			if err != nil {
				p.MarkFailure(b)
				if logger != nil {
					logger.Printf("WARN health check request failed: pool=%s backend=%s err=%v", p.Name, b.URL.Host, err)
				}
				continue
			}
			resp.Body.Close()

			if resp.StatusCode >= 200 && resp.StatusCode < 400 {
				p.MarkSuccess(b)
			} else {
				p.MarkFailure(b)
			}
		}
	}

	// Probe immediately on startup so backends aren't left in the
	// optimistic "alive" state longer than one interval unnecessarily.
	probeOnce()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			probeOnce()
		}
	}
}
