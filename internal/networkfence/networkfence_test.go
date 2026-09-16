package networkfence

import (
	"context"
	"errors"
	"net"
	"os"
	"testing"
)

func TestDecideAllowsLoopbackAndRefusesEverythingElse(t *testing.T) {
	cases := []struct {
		addr    string
		refused bool
	}{
		{"127.0.0.1:8080", false},
		{"127.0.0.2:1", false}, // any 127.0.0.0/8 literal is loopback
		{"[::1]:443", false},
		{"localhost:80", false},
		{"LOCALHOST:80", false}, // case-insensitive
		{"192.0.2.1:443", true}, // TEST-NET-1 (RFC 5737), never routes anywhere real
		{"fence-probe.invalid:443", true},
		{"example.com:443", true},
		{"192.168.1.1:443", true}, // private, but not loopback
	}
	for _, c := range cases {
		got := Decide("tcp", c.addr)
		if (got != nil) != c.refused {
			t.Errorf("Decide(%q): refused=%v, want %v (err=%v)", c.addr, got != nil, c.refused, got)
		}
		if got != nil && got.Addr != c.addr {
			t.Errorf("Decide(%q).Addr = %q, want %q", c.addr, got.Addr, c.addr)
		}
	}
}

func TestExpectDrainsAndRemainingPeeks(t *testing.T) {
	// Isolated from any parallel state: this package's tests run
	// sequentially (none call t.Parallel()), so the shared recorder is
	// safe to exercise directly here.
	Expect() // clear anything a prior test in this file left behind

	_, err := dialContext(context.Background(), "tcp", "example.invalid:443")
	if err == nil {
		t.Fatal("dialContext allowed a non-loopback address through")
	}
	var refusal *RefusedError
	if !errors.As(err, &refusal) {
		t.Fatalf("dialContext's error is not a *RefusedError: %v (%T)", err, err)
	}
	if refusal.Addr != "example.invalid:443" {
		t.Errorf("refusal.Addr = %q, want %q", refusal.Addr, "example.invalid:443")
	}

	if left := Remaining(); len(left) != 1 {
		t.Fatalf("Remaining() = %d entries, want 1", len(left))
	}
	drained := Expect()
	if len(drained) != 1 || drained[0].Addr != "example.invalid:443" {
		t.Fatalf("Expect() = %v, want one entry for example.invalid:443", drained)
	}
	if left := Remaining(); len(left) != 0 {
		t.Fatalf("Remaining() after Expect() = %d entries, want 0", len(left))
	}
}

// TestClearProxyEnvUnsetsEveryVariable proves the mechanical half of the
// fix directly: every variable clearProxyEnv names is gone afterward. It
// cannot, by itself, prove the ordering guarantee that actually matters —
// that this runs before Go's own ProxyFromEnvironment caches a value for
// the rest of the process, which is a once-per-process fact this package's
// own test binary cannot reset — cmd/dropin-miner's subprocess test proves
// that half, by re-executing a whole fresh process with the environment
// already poisoned before its own TestMain ever runs.
func TestClearProxyEnvUnsetsEveryVariable(t *testing.T) {
	for _, key := range proxyEnvVars {
		t.Setenv(key, "http://127.0.0.1:1")
	}
	clearProxyEnv()
	for _, key := range proxyEnvVars {
		if v, ok := os.LookupEnv(key); ok {
			t.Errorf("%s is still set to %q after clearProxyEnv", key, v)
		}
	}
}

func TestDialContextAllowsLoopbackForReal(t *testing.T) {
	// The one case that must actually reach the network in this package's
	// own tests: proving the fence does not refuse the loopback listeners
	// every httptest.Server in this module depends on.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		c, err := ln.Accept()
		if err == nil {
			_ = c.Close()
		}
	}()
	conn, err := dialContext(context.Background(), "tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dialContext refused a loopback address: %v", err)
	}
	_ = conn.Close()
	if left := Expect(); len(left) != 0 {
		t.Fatalf("a loopback dial was recorded as refused: %v", left)
	}
}
