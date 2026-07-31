package main

import "testing"

type joinPathCase struct {
	name  string
	base  string
	extra string
	want  string
}

// TestJoinPath exercises the health-check path-joining helper in
// balancer.go, providing minimal coverage so the test binary compiles and
// runs cleanly alongside the rest of the package.
func TestJoinPath(t *testing.T) {
	cases := []joinPathCase{
		{name: "empty base absolute extra", base: "", extra: "/healthz", want: "/healthz"},
		{name: "root base relative extra", base: "/", extra: "healthz", want: "/healthz"},
		{name: "non-slash base relative extra", base: "/api", extra: "healthz", want: "/api/healthz"},
		{name: "slash-terminated base relative extra", base: "/api/", extra: "healthz", want: "/api/healthz"},
		{name: "absolute extra overrides base", base: "/api", extra: "/healthz", want: "/healthz"},
		{name: "empty extra returns base", base: "/api", extra: "", want: "/api"},
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
