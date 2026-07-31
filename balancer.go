// implementations (round robin, least connections, random), active
// health-check scheduling, and passive failure/success marking. It
// does not own HTTP routing, TLS, logging middleware, or metrics emission.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"net/http"
	"net/url"
	"sync/atomic"
	"time"
)

// ErrNoHealthyBackends is returned by Pool.Choose when no backend is alive.
var ErrNoHealthyBackends = errors.New("no healthy backends available")

// Backend represents one upstream target and its live health state.
type Backend struct {
	URL *url.URL

	Alive       atomic.Bool
	ActiveConns atomic.Int64

	consecFails     atomic.Int32
	consecSuccesses atomic.Int32
}

// NewBackend constructs a Backend that starts alive so it can serve
// traffic immediately at startup, before the first probe cycle completes.
func NewBackend(u *url.URL) *Backend {
	b := &Backend{URL: u}
	b.Alive.Store(true)
	return b
}

// Strategy selects the next backend from a candidate slice.
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

// RoundRobinStrategy cycles through healthy backends using an atomic counter.
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

// LeastConnStrategy selects the healthy backend with the fewest in-flight
// requests; ties broken by first-seen order.
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
type RandomStrategy struct{}

func (s *RandomStrategy) Next(backends []*Backend) (*Backend, error) {
	alive := aliveBackends(backends)
	if len(alive) == 0 {
		return nil, ErrNoHealthyBackends
	}
	return alive[rand.Intn(len(alive))], nil
}

// Pool groups backends under one strategy and health-check policy.
type Pool struct {
	Name        string
	Backends    []*Backend
	Strategy    Strategy
	HealthCheck HealthCheckConf
}

// NewPool constructs a Pool from PoolConf, resolving Strategy by name.
func NewPool(cfg PoolConf) (*Pool, error) {
	backends := make([]*Backend, 0, len(cfg.Backends))
	for _, bc := range cfg.Backends {
		u, err := url.Parse(bc.URL)
		if err != nil {
			return nil, fmt.Errorf("pool %q: invalid backend url %q: %w", cfg.Name, bc.URL, err)
		}
		backends = append(backends, NewBackend(u))
	}

	var strategy Strategy
	switch cfg.Strategy {
	case "least_connections":
		strategy = &LeastConnStrategy{}
	case "random":
		strategy = &RandomStrategy{}
	case "round_robin", "":
		strategy = &RoundRobinStrategy{}
	default:
		strategy = &RoundRobinStrategy{}
	}

	return &Pool{
		Name:        cfg.Name,
		Backends:    backends,
		Strategy:    strategy,
		HealthCheck: cfg.HealthCheck,
	}, nil
}

// Choose returns the next healthy backend per the pool's strategy.
func (p *Pool) Choose() (*Backend, error) {
	return p.Strategy.Next(p.Backends)
}

// MarkFailure implements passive circuit-breaker-lite behavior.
func (p *Pool) MarkFailure(b *Backend) {
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

// RunHealthChecks starts a blocking loop that periodically GETs
// HealthCheck.Path on each backend, applying threshold hysteresis to flip
// Alive state. Exits cleanly when ctx is canceled.
func (p *Pool) RunHealthChecks(ctx context.Context, logger *slog.Logger) {
	if !p.HealthCheck.Enabled {
		return
	}

	interval, err := time.ParseDuration(p.HealthCheck.Interval)
	if err != nil || interval <= 0 {
		interval = 10 * time.Second
	}
	timeout, err := time.ParseDuration(p.HealthCheck.Timeout)
	if err != nil || timeout <= 0 {
		timeout = 2 * time.Second
	}

	client := &http.Client{
		Timeout: timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for _, b := range p.Backends {
				p.probeOnce(ctx, client, b, logger)
			}
		}
	}
}

func (p *Pool) probeOnce(ctx context.Context, client *http.Client, b *Backend, logger *slog.Logger) {
	path := p.HealthCheck.Path
	if path == "" {
		path = "/healthz"
	}
	target := *b.URL
	target.Path = joinPath(b.URL.Path, path)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	wasAlive := b.Alive.Load()

	healthy := false
	if err == nil {
		resp, doErr := client.Do(req)
		if doErr == nil {
			healthy = resp.StatusCode >= 200 && resp.StatusCode < 400
			resp.Body.Close()
		}
	}

	if healthy {
		b.consecFails.Store(0)
		successes := b.consecSuccesses.Add(1)
		threshold := int32(p.HealthCheck.HealthyThreshold)
		if threshold <= 0 {
			threshold = 2
		}
		if successes >= threshold {
			b.Alive.Store(true)
		}
	} else {
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

	nowAlive := b.Alive.Load()
	if nowAlive != wasAlive && logger != nil {
		logger.Info("backend health transition",
			"pool", p.Name,
			"backend", b.URL.String(),
			"from", wasAlive,
			"to", nowAlive,
			"consec_fails", b.consecFails.Load(),
			"consec_successes", b.consecSuccesses.Load(),
		)
	}
}

func joinPath(base, suffix string) string {
	if base == "" {
		return suffix
	}
	if len(base) > 0 && base[len(base)-1] == '/' && len(suffix) > 0 && suffix[0] == '/' {
		return base + suffix[1:]
	}
	if len(base) > 0 && base[len(base)-1] != '/' && len(suffix) > 0 && suffix[0] != '/' {
		return base + "/" + suffix
	}
	return base + suffix
}
