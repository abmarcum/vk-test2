// strategy implementations (round robin, least connections, random),
// active health-check scheduling, and passive failure/success marking.
// It does not own HTTP routing, TLS, logging middleware, or metrics
// emission.
package main

import (
	"context"
	"errors"
	"log/slog"
	"math/rand"
	"net/http"
	"net/url"
	"sync/atomic"
	"time"
)

// ErrNoHealthyBackends is returned by Pool.Choose when no backend in the
// pool is currently eligible for selection.
var ErrNoHealthyBackends = errors.New("no healthy backends available")

// Backend represents one upstream target and its live health state.
type Backend struct {
	URL *url.URL

	Alive       atomic.Bool
	ActiveConns atomic.Int64

	consecFails     atomic.Int32
	consecSuccesses atomic.Int32
}

// Strategy selects the next backend from a candidate slice of already
// health-filtered backends.
type Strategy interface {
	Next(backends []*Backend) (*Backend, error)
}

// RoundRobinStrategy cycles through healthy backends using an atomic counter.
type RoundRobinStrategy struct {
	cursor atomic.Uint64
}

func (s *RoundRobinStrategy) Next(backends []*Backend) (*Backend, error) {
	if len(backends) == 0 {
		return nil, ErrNoHealthyBackends
	}
	idx := s.cursor.Add(1)
	return backends[idx%uint64(len(backends))], nil
}

// LeastConnStrategy selects the healthy backend with fewest active conns.
type LeastConnStrategy struct{}

func (s *LeastConnStrategy) Next(backends []*Backend) (*Backend, error) {
	if len(backends) == 0 {
		return nil, ErrNoHealthyBackends
	}
	best := backends[0]
	for _, b := range backends[1:] {
		if b.ActiveConns.Load() < best.ActiveConns.Load() {
			best = b
		}
	}
	return best, nil
}

// RandomStrategy selects a uniformly random healthy backend.
type RandomStrategy struct{}

func (s *RandomStrategy) Next(backends []*Backend) (*Backend, error) {
	if len(backends) == 0 {
		return nil, ErrNoHealthyBackends
	}
	return backends[rand.Intn(len(backends))], nil
}

// Pool groups backends under one strategy and health-check policy.
type Pool struct {
	Name        string
	Backends    []*Backend
	strategy    Strategy
	HealthCheck HealthCheckConfig
}

// NewPool constructs a Pool from PoolConfig, resolving Strategy by name and
// parsing/validating each backend URL. Every Backend starts Alive so it can
// serve traffic immediately at startup, before the first health-check pass.
func NewPool(cfg PoolConfig) (*Pool, error) {
	p := &Pool{
		Name:        cfg.Name,
		HealthCheck: cfg.HealthCheck,
	}

	switch cfg.Strategy {
	case "least_connections":
		p.strategy = &LeastConnStrategy{}
	case "random":
		p.strategy = &RandomStrategy{}
	default:
		p.strategy = &RoundRobinStrategy{}
	}

	for _, bc := range cfg.Backends {
		u, err := url.Parse(bc.URL)
		if err != nil {
			return nil, err
		}
		b := &Backend{URL: u}
		b.Alive.Store(true)
		p.Backends = append(p.Backends, b)
	}

	return p, nil
}

// HealthCheckEnabled reports whether active probing should run for this pool.
func (p *Pool) HealthCheckEnabled() bool {
	return p.HealthCheck.Enabled
}

// Choose returns the next healthy backend per the pool's strategy.
func (p *Pool) Choose() (*Backend, error) {
	alive := make([]*Backend, 0, len(p.Backends))
	for _, b := range p.Backends {
		if b.Alive.Load() {
			alive = append(alive, b)
		}
	}
	if len(alive) == 0 {
		return nil, ErrNoHealthyBackends
	}
	return p.strategy.Next(alive)
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

// RunHealthChecks starts a blocking loop probing each backend on Interval,
// applying healthy/unhealthy thresholds with hysteresis. Exits cleanly when
// ctx is canceled. logger is required and is used to emit a structured log
// line on every Alive state transition.
func (p *Pool) RunHealthChecks(ctx context.Context, logger *slog.Logger) {
	interval := p.HealthCheck.IntervalDur
	if interval <= 0 {
		interval = 10 * time.Second
	}
	timeout := p.HealthCheck.TimeoutDur
	if timeout <= 0 {
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
	target.Path = path

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		p.recordProbeFailure(b, logger)
		return
	}

	resp, err := client.Do(req)
	if err != nil {
		p.recordProbeFailure(b, logger)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 400 {
		p.recordProbeSuccess(b, logger)
	} else {
		p.recordProbeFailure(b, logger)
	}
}

func (p *Pool) recordProbeFailure(b *Backend, logger *slog.Logger) {
	wasAlive := b.Alive.Load()
	p.MarkFailure(b)
	nowAlive := b.Alive.Load()
	if wasAlive != nowAlive {
		logger.Info("backend health transition",
			"pool", p.Name, "backend", b.URL.String(),
			"from", wasAlive, "to", nowAlive)
	}
}

func (p *Pool) recordProbeSuccess(b *Backend, logger *slog.Logger) {
	wasAlive := b.Alive.Load()
	p.MarkSuccess(b)
	nowAlive := b.Alive.Load()
	if wasAlive != nowAlive {
		logger.Info("backend health transition",
			"pool", p.Name, "backend", b.URL.String(),
			"from", wasAlive, "to", nowAlive)
	}
}
