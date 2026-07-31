// connections, random), active health-check scheduling, and passive
// failure/success marking. It does not own HTTP routing, TLS, logging
// middleware, or metrics emission.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"sync/atomic"
	"time"
)

// ErrNoHealthyBackends is returned by Pool.Choose when no backend in the
// pool is currently marked alive.
var ErrNoHealthyBackends = errors.New("no healthy backends available")

// ---------------------------------------------------------------------------
// Backend
// ---------------------------------------------------------------------------

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
	if u.Host == "" {
		return nil, fmt.Errorf("backend url %q missing host", rawURL)
	}
	b := &Backend{URL: u}
	b.Alive.Store(true)
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

type RandomStrategy struct {
	counter atomic.Uint64
}

func (s *RandomStrategy) Next(backends []*Backend) (*Backend, error) {
	alive := aliveBackends(backends)
	if len(alive) == 0 {
		return nil, ErrNoHealthyBackends
	}
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

type Pool struct {
	Name        string
	Backends    []*Backend
	Strategy    Strategy
	HealthCheck HealthCheckConfig
}

// NewPool constructs a Pool from PoolConf, resolving Strategy by name.
func NewPool(cfg PoolConf) (*Pool, error) {
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

func (p *Pool) HealthCheckEnabled() bool {
	return p.HealthCheck.Enabled
}

func (p *Pool) Choose() (*Backend, error) {
	return p.Strategy.Next(p.Backends)
}

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

// RunHealthChecks starts a blocking loop that periodically GETs
// HealthCheck.Path on each backend, applying threshold hysteresis to flip
// Alive state. Exits cleanly when ctx is canceled.
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
	target.Path = joinPath(b.URL.Path, path)

	reqCtx, cancel := context.WithTimeout(ctx, client.Timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, target.String(), nil)
	if err != nil {
		p.recordProbeFailure(b, logger)
		return
	}

	resp, err := client.Do(req)
	wasAlive := b.Alive.Load()

	if err != nil || resp.StatusCode < 200 || resp.StatusCode >= 400 {
		if resp != nil {
			resp.Body.Close()
		}
		p.recordProbeFailure(b, logger)
	} else {
		resp.Body.Close()
		p.recordProbeSuccess(b, logger)
	}

	nowAlive := b.Alive.Load()
	if wasAlive != nowAlive && logger != nil {
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

func (p *Pool) recordProbeFailure(b *Backend, _ *slog.Logger) {
	p.MarkFailure(b)
}

func (p *Pool) recordProbeSuccess(b *Backend, _ *slog.Logger) {
	p.MarkSuccess(b)
}

func joinPath(base, extra string) string {
	if base == "" {
		return extra
	}
	if extra == "" {
		return base
	}
	if base[len(base)-1] == '/' && extra[0] == '/' {
		return base + extra[1:]
	}
	if base[len(base)-1] != '/' && extra[0] != '/' {
		return base + "/" + extra
	}
	return base + extra
}
