package main

// A network fence for the whole test binary, installed by TestMain before
// any test runs (testmain_test.go). TestMain already walls off HOME and the
// config directories so a test cannot read or write the participant's real
// installation; it had no equivalent wall for the network, and D1's own
// connect-refusal guard showed why that gap is not hypothetical — with the
// guard it proves removed, `connect` did not just misreport, it registered
// a real agent against the production platform. A regression in that guard,
// or in any future one, must fail loudly in this package's own test run
// rather than reach a real host.
//
// The fence works at two independent levels. http.DefaultTransport — what
// every `&http.Client{}` this binary or its dependencies build uses unless
// it sets its own Transport — is replaced with a clone whose DialContext
// refuses every address that is not loopback, before any DNS lookup:
// refusing by inspecting the address string never calls the resolver, so a
// hostname never gets as far as a lookup either. That clone's own Proxy is
// cleared (installNetworkFence's comment on Clone() explains why): DialContext
// must see the client's real destination, not a proxy's address, or a
// refusal can never be attributed to the host that earned it.
//
// Separately, HTTP_PROXY/HTTPS_PROXY are pointed at a closed loopback port
// and NO_PROXY is set to the loopback spellings, so a client this module (or
// a future dependency) builds with its own Transport that still honors
// http.ProxyFromEnvironment also fails closed. This covers a different gap
// than DialContext's: every custom Transport this module currently builds
// (pkg/auth's, wallet_tx.go's) sets Proxy: nil deliberately and would not be
// helped by this layer either way — see client_network_fence_test.go's own
// pkg/auth test for why those need a seam, not a proxy fail-safe, to be
// covered at all.
//
// Every refusal is recorded. A guard test that deliberately drives a real
// client at a real hostname to prove the fence covers it calls
// networkFenceExpect right after, which drains and returns what was
// recorded so far — consuming it so the end-of-run check below does not
// also fail on a refusal a test already examined. Anything left over when
// m.Run() returns is a dial nothing in the suite accounted for: exactly the
// shape of an accidental leak, and testmain_test.go fails the run for it
// and lists every address.

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
)

// networkFenceRefusal is one dial the fence turned away.
type networkFenceRefusal struct {
	Network string
	Addr    string
}

func (r networkFenceRefusal) String() string { return r.Network + " " + r.Addr }

var networkFenceState struct {
	mu      sync.Mutex
	refused []networkFenceRefusal
}

// networkFenceRecord appends one refusal. Safe for concurrent dials: two
// tests reaching the fence at once each get their own recorded entry, never
// a lost update.
func networkFenceRecord(network, addr string) {
	networkFenceState.mu.Lock()
	networkFenceState.refused = append(networkFenceState.refused, networkFenceRefusal{Network: network, Addr: addr})
	networkFenceState.mu.Unlock()
}

// networkFenceExpect drains and returns every refusal recorded so far. A
// guard test calls this immediately after the one call it expects the fence
// to refuse, so that expected refusal is consumed rather than left for
// testmain_test.go's end-of-run check to trip over. It is deliberately not
// scoped to one address: two clients built one after another inside a
// single test (as the enumeration guard in client_network_fence_test.go
// does) drain together, and the test asserts on the addresses it got back.
func networkFenceExpect() []networkFenceRefusal {
	networkFenceState.mu.Lock()
	defer networkFenceState.mu.Unlock()
	got := networkFenceState.refused
	networkFenceState.refused = nil
	return got
}

// networkFenceRemaining reports every refusal no guard test has consumed,
// without clearing them — testmain_test.go calls this once, after m.Run(),
// purely to decide whether to fail the run; nothing before that point
// should ever call it instead of networkFenceExpect, or a real guard test's
// own drain would come back empty.
func networkFenceRemaining() []networkFenceRefusal {
	networkFenceState.mu.Lock()
	defer networkFenceState.mu.Unlock()
	return append([]networkFenceRefusal(nil), networkFenceState.refused...)
}

// networkFenceAllowedHost reports whether host (already split from any
// port) is loopback: a literal loopback IP in either family, or the name
// "localhost" however it is cased. httptest.Server binds to a loopback IP
// literal, never the name, but a client is free to be pointed at
// "localhost" instead, so both are allowed.
func networkFenceAllowedHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// networkFenceDialContext is installed as http.DefaultTransport's
// DialContext. It decides purely from the address string — never touching
// the network or the resolver — so a refusal never performs the DNS lookup
// the real dial would have needed.
func networkFenceDialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr // a bare host with no port; SplitHostPort's own error is not the interesting one here
	}
	if !networkFenceAllowedHost(host) {
		networkFenceRecord(network, addr)
		return nil, fmt.Errorf("network fence: refused a %s dial to %q — cmd/dropin-miner tests must not reach a "+
			"non-loopback host (see network_fence_test.go)", network, addr)
	}
	var d net.Dialer
	return d.DialContext(ctx, network, addr)
}

// installNetworkFence replaces http.DefaultTransport with a fenced clone
// and points the proxy environment at a closed loopback port. Called once,
// by TestMain, before m.Run().
func installNetworkFence() error {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return fmt.Errorf("network fence: http.DefaultTransport is a %T, not *http.Transport — nothing to clone", http.DefaultTransport)
	}
	fenced := base.Clone()
	fenced.DialContext = networkFenceDialContext
	// A Transport that already resolved a proxy CONNECT dial through its
	// own DialContext is covered by the line above; DialTLSContext is a
	// second seam some transports use to skip DialContext entirely for TLS.
	// Clearing it forces even a TLS dial back through DialContext instead of
	// around it.
	fenced.DialTLSContext = nil
	// Clone() carries over Proxy: http.ProxyFromEnvironment, the default
	// transport's own setting. Left in place, the proxy env vars set below
	// would make every DefaultTransport-based client dial the closed proxy
	// address INSTEAD of the real destination — DialContext would then only
	// ever see the (loopback, so allowed) proxy address, never record the
	// real host, and a client that should have been refused for reaching
	// production would instead just see a plain connection-refused error
	// with nothing to attribute it to. Clearing Proxy here is what makes
	// DialContext the client's real destination, not the proxy's.
	fenced.Proxy = nil
	http.DefaultTransport = fenced

	closed, err := closedLoopbackAddr()
	if err != nil {
		return fmt.Errorf("network fence: reserve a closed loopback port: %w", err)
	}
	proxyURL := "http://" + closed
	for _, key := range []string{"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy"} {
		if err := os.Setenv(key, proxyURL); err != nil {
			return fmt.Errorf("network fence: set %s: %w", key, err)
		}
	}
	for _, key := range []string{"NO_PROXY", "no_proxy"} {
		if err := os.Setenv(key, "localhost,127.0.0.1,::1"); err != nil {
			return fmt.Errorf("network fence: set %s: %w", key, err)
		}
	}
	return nil
}

// closedLoopbackAddr reserves a loopback TCP port and immediately releases
// it, so the returned address is (barring another process racing to bind
// the same ephemeral port at the exact instant this returns, which the
// kernel avoids by not reusing a just-freed ephemeral port immediately) one
// nothing is listening on: a client sent to it fails to connect rather than
// hanging, and rather than the fixed "port 9" guess some code uses for the
// same purpose, which is not reliably closed on every machine or CI image.
func closedLoopbackAddr() (string, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		return "", err
	}
	return addr, nil
}
