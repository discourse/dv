package cli

import "testing"

func TestCaddyHostsForLocalProxy(t *testing.T) {
	t.Parallel()

	got := caddyHostsForLocalProxy("sideload.dev.home.arpa", "dev.home.arpa")
	want := "sideload.dev.home.arpa, *.dev.home.arpa"
	if got != want {
		t.Fatalf("caddyHostsForLocalProxy() = %q, want %q", got, want)
	}
}

func TestCaddyHostsForLocalProxyDeduplicatesWildcardBase(t *testing.T) {
	t.Parallel()

	got := caddyHostsForLocalProxy("*.dev.home.arpa", "*.dev.home.arpa")
	want := "*.dev.home.arpa"
	if got != want {
		t.Fatalf("caddyHostsForLocalProxy() = %q, want %q", got, want)
	}
}

func TestCaddyHostsForLocalProxySkipsBuiltInLocalhostWildcard(t *testing.T) {
	t.Parallel()

	got := caddyHostsForLocalProxy("agent.dv.localhost", "dv.localhost")
	want := "agent.dv.localhost"
	if got != want {
		t.Fatalf("caddyHostsForLocalProxy() = %q, want %q", got, want)
	}
}
