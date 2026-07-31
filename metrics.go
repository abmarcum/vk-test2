// registry used in place of a third-party Prometheus client library, to
// keep the module's dependency graph empty (avoids go.sum/module
// resolution issues entirely). It exposes counters and gauges sufficient
// to satisfy the /metrics endpoint in a Prometheus-compatible exposition
// format, without pulling in any external packages.
package main

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

// Metrics is a small, thread-safe collection of named counters and gauges,
// each optionally keyed by a label set, rendered in Prometheus text
// exposition format by WriteTo.
type Metrics struct {
	mu       sync.Mutex
	counters map[string]float64
	gauges   map[string]float64
}

// NewMetrics constructs an empty Metrics registry.
func NewMetrics() *Metrics {
	return &Metrics{
		counters: make(map[string]float64),
		gauges:   make(map[string]float64),
	}
}

// key builds a stable series key from a metric name and label set.
func key(name string, labels map[string]string) string {
	if len(labels) == 0 {
		return name
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	sb.WriteString(name)
	sb.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(fmt.Sprintf("%s=%q", k, labels[k]))
	}
	sb.WriteByte('}')
	return sb.String()
}

// IncCounter increments a named counter (keyed by labels) by 1.
func (m *Metrics) IncCounter(name string, labels map[string]string) {
	m.AddCounter(name, labels, 1)
}

// AddCounter adds delta to a named counter (keyed by labels).
func (m *Metrics) AddCounter(name string, labels map[string]string, delta float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.counters[key(name, labels)] += delta
}

// SetGauge sets a named gauge (keyed by labels) to value.
func (m *Metrics) SetGauge(name string, labels map[string]string, value float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.gauges[key(name, labels)] = value
}

// WriteTo renders all series in Prometheus text exposition format.
func (m *Metrics) WriteTo(sb *strings.Builder) {
	m.mu.Lock()
	defer m.mu.Unlock()

	names := make([]string, 0, len(m.counters))
	for k := range m.counters {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		fmt.Fprintf(sb, "%s %g\n", k, m.counters[k])
	}

	gnames := make([]string, 0, len(m.gauges))
	for k := range m.gauges {
		gnames = append(gnames, k)
	}
	sort.Strings(gnames)
	for _, k := range gnames {
		fmt.Fprintf(sb, "%s %g\n", k, m.gauges[k])
	}
}
