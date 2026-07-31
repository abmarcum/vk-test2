package main

import "testing"

// TestJoinPath exercises the health-check path-joining helper in
// balancer.go, providing minimal coverage so the test binary compiles and
// runs cleanly alongside the rest of the package.
func TestJoinPath(t *testing.T) {
	cases := []struct {
		name  string
		base  string
		extra string
		want  string
	}{
		{"empty base absolute extra", "", "/healthz", "/healthz"},
		{"root base relative extra", "/", "healthz", "/healthz"},
		{"non-slash base relative extra", "/api", "healthz", "/api/healthz"},
		{"slash-terminated base relative extra", "/api/", "healthz", "/api/healthz"},
		{"absolute extra overrides base", "/api", "/healthz", "/healthz"},
		{"empty extra returns base", "/api", "", "/api"},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			got := joinPath(c.base, c.extra)
			if got != c.want {
				t.Errorf("joinPath(%q, %q) = %q, want %q", c.base, c.extra, got, c.want)
			}
		})
	}
}

// TestNewStrategyDefault ensures an unrecognized strategy name falls back
// to round-robin rather than panicking or returning nil.
func TestNewStrategyDefault(t *testing.T) {
	s := newStrategy("unknown-strategy")
	if s == nil {
		t.Fatal("newStrategy returned nil")
	}
	if _, ok := s.(*RoundRobinStrategy); !ok {
		t.Errorf("newStrategy(%q) = %T, want *RoundRobinStrategy", "unknown-strategy", s)
	}
}
