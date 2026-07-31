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

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got := joinPath(tc.base, tc.extra)
			if got != tc.want {
				t.Errorf("joinPath(%q, %q) = %q, want %q", tc.base, tc.extra, got, tc.want)
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

// TestNewStrategyKnownKinds verifies each recognized strategy name resolves
// to its corresponding concrete Strategy implementation.
func TestNewStrategyKnownKinds(t *testing.T) {
	if _, ok := newStrategy("round_robin").(*RoundRobinStrategy); !ok {
		t.Errorf("newStrategy(%q) did not return *RoundRobinStrategy", "round_robin")
	}
	if _, ok := newStrategy("least_connections").(*LeastConnStrategy); !ok {
		t.Errorf("newStrategy(%q) did not return *LeastConnStrategy", "least_connections")
	}
	if _, ok := newStrategy("random").(*RandomStrategy); !ok {
		t.Errorf("newStrategy(%q) did not return *RandomStrategy", "random")
	}
}
