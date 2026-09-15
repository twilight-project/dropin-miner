// Package networkfence is test-only infrastructure: no production code
// anywhere in this module imports it. Every package whose tests build a
// network client (cmd/dropin-miner, pkg/auth, pkg/platform,
// internal/selfupdate) calls Guard from its own TestMain instead of calling
// m.Run() directly, and every such client is wired (in its own package's
// production code) to internal/netdial's shared DialContext seam — this
// package's whole job is reassigning that one variable for the duration of
// the test run, so it has to live somewhere every one of those packages can
// import, which internal/netdial's own directory (production code) is not
// the right place for.
//
// D1's own connect-refusal guard test is why this exists: it drove connect
// against built-in defaults, which point at the real production platform,
// and TestMain had no wall for the network the way it already had one for
// HOME and the config directories. A regression in that guard would have
// made the test register a real agent. Guard closes that class of gap for
// every package that adopts it, not just the one that found it.
package networkfence

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/twilight-project/dropin-miner/internal/netdial"
)

// RefusedError is the fence's own typed refusal. A guard test asserts
// errors.As against this, not merely that some error came back — a real
// DNS failure, a real connection refusal from some unrelated cause, or a
// misconfigured target must never be mistaken for the fence doing its job,
// which is exactly why rule 1 of this package's own review says a guard
// test targets an address that cannot resolve or route at all (a .invalid
// hostname, or 192.0.2.1/TEST-NET-1) rather than a real service: with a
// broken fence, such a test then fails on that DNS or routing error and
// still contacts nothing, instead of silently reaching production.
type RefusedError struct {
	Network string
	Addr    string
}

func (e *RefusedError) Error() string {
	return fmt.Sprintf("network fence: refused a %s dial to %q — this test binary must not reach a non-loopback host", e.Network, e.Addr)
}

var state struct {
	mu      sync.Mutex
	refused []*RefusedError
}

func record(e *RefusedError) {
	state.mu.Lock()
	state.refused = append(state.refused, e)
	state.mu.Unlock()
}

// Expect drains and returns every refusal recorded so far. A guard test
// calls this immediately after the one call it expects the fence to
// refuse, consuming it so Guard's own end-of-run check does not also fail
// on a refusal a test already examined.
func Expect() []*RefusedError {
	state.mu.Lock()
	defer state.mu.Unlock()
	got := state.refused
	state.refused = nil
	return got
}

// Remaining reports every refusal no test has consumed, without clearing
// it. Guard is the only caller; a guard test wanting its own refusal calls
// Expect instead.
func Remaining() []*RefusedError {
	state.mu.Lock()
	defer state.mu.Unlock()
	return append([]*RefusedError(nil), state.refused...)
}

// AllowedHost reports whether host (already split from any port, or a bare
// host with none) is loopback: a literal loopback IP in either family, or
// the name "localhost" however it is cased. httptest.Server binds to a
// loopback IP literal, never the name, but a client is free to be pointed
// at "localhost" instead, so both are allowed.
func AllowedHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// Decide is the fence's decision function with no dial attached: given
// network and addr exactly as a Transport would pass them to DialContext,
// it reports the *RefusedError a live dial would produce, or nil when addr
// would be allowed through. A guard test uses this to prove a production
// hostname (agents-v1.nyks.dev, api.github.com, and the like) is classified
// as refused without ever dialing it — the string-level half of "prove it
// is covered" that costs nothing and touches no network at all.
func Decide(network, addr string) *RefusedError {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	if AllowedHost(host) {
		return nil
	}
	return &RefusedError{Network: network, Addr: addr}
}

// dialContext is installed as netdial.Hook. It decides purely from
// the address string, via Decide — never touching the network or the
// resolver — so a refusal never performs the DNS lookup the real dial
// would have needed.
func dialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	if refusal := Decide(network, addr); refusal != nil {
		record(refusal)
		return nil, refusal
	}
	var d net.Dialer
	return d.DialContext(ctx, network, addr)
}

// Install reassigns netdial.Hook to the fence. Called once, by Guard,
// before m.Run(). Every Transport this module builds in production code
// dials via netdial.For(itsOwnDialer), which checks Hook first and its own
// dialer only when Hook is nil — so this one reassignment reaches every
// client at once, whatever dialer each would otherwise use, without this
// package needing to know what any of them are.
//
// It does NOT also point HTTP_PROXY/HTTPS_PROXY at a closed port, which an
// earlier revision of this fence (cmd/dropin-miner's own, before this
// shared seam existed) did as a second, independent layer. That layer is
// not just redundant now, it is actively wrong: several production
// transports deliberately preserve Proxy: http.ProxyFromEnvironment (the
// login probe, the search client, pkg/platform's client,
// internal/selfupdate's source — see each one's own comment), because that
// is what http.DefaultTransport already did for them and this seam's whole
// promise is not changing that. With HTTP_PROXY pointed at a closed
// LOOPBACK port, those transports would resolve a proxy address that
// DialContext allows through (it is loopback), and DialContext would never
// see the real destination at all — a live guard test would then get a
// plain "connection refused" from the fake proxy instead of this package's
// typed refusal naming the host actually being reached, exactly the
// masking bug D1b found and fixed for the http.DefaultTransport swap this
// seam replaces. Now that boundary_test.go (cmd/dropin-miner's
// TestEveryHTTPTransportDialsThroughTheSharedSeam) structurally guarantees
// every Transport this module builds names netdial.For explicitly, there
// is no remaining category of client this second layer would have caught
// that Hook does not already cover on its own — and this module's one
// third-party HTTP-capable dependency, golang.org/x/oauth2, is handed
// pkg/auth's own already-seamed client via the oauth2.HTTPClient context
// value (oauthclient.go) rather than building one of its own, so there is
// nothing left outside this module's control to defend against either.
func Install() error {
	netdial.Hook = dialContext
	return nil
}

// Guard installs the fence, runs m, and fails the run if any refusal went
// unexamined by the end — the exact shape of an accidental leak, or a
// regression in a guard that used to need one. Every package whose
// TestMain builds a network client calls this in place of m.Run().
func Guard(m *testing.M) int {
	if err := Install(); err != nil {
		fmt.Fprintln(os.Stderr, "TestMain: refusing to run:", err)
		return 2
	}
	code := m.Run()
	if left := Remaining(); len(left) > 0 {
		fmt.Fprintf(os.Stderr, "TestMain: the network fence refused %d dial(s) no test examined:\n", len(left))
		for _, r := range left {
			fmt.Fprintln(os.Stderr, " ", r)
		}
		if code == 0 {
			code = 1
		}
	}
	return code
}

// AsRefusal is errors.As for RefusedError, spelled out once so a guard test
// reads as an assertion rather than a type-switch.
func AsRefusal(err error) (*RefusedError, bool) {
	var r *RefusedError
	ok := errors.As(err, &r)
	return r, ok
}

// transportFieldsSkippedInComparison are the exported *http.Transport
// fields AssertTransportFieldsMatch does not compare directly:
//
//   - DialContext: the one field this module's seam legitimately sets to a
//     netdial.For closure, never equal to whatever reference dialer want
//     was built with.
//   - Proxy: compared separately, by underlying function pointer, since
//     reflect.DeepEqual treats any two non-nil func values as unequal even
//     when they are the same function.
//   - TLSClientConfig, TLSNextProto: not construction-time configuration
//     at all. net/http's HTTP/2 auto-configuration (onceSetNextProtoDefaults)
//     writes to both the first time a Transport is actually used, if unset
//     — so a production Transport this module has already dialed a real
//     request through (which every package-level one here has, by the time
//     its own package's other tests run) compares unequal to a never-used
//     reference clone on these two alone, for a reason that has nothing to
//     do with how either Transport was built.
var transportFieldsSkippedInComparison = map[string]bool{
	"DialContext":     true,
	"Proxy":           true,
	"TLSClientConfig": true,
	"TLSNextProto":    true,
}

// AssertTransportFieldsMatch fails t unless got and want are the same in
// every exported field except those named in
// transportFieldsSkippedInComparison. This is D1c's review, made
// permanent: routing every Transport through one seam must not silently
// change what ForceAttemptHTTP2, TLSHandshakeTimeout, IdleConnTimeout,
// MaxIdleConns or ExpectContinueTimeout it carries, for the four clients
// that clone http.DefaultTransport, or invent a Proxy the three that build
// a bare Transport never had.
//
// Iterating exported fields by name (rather than a whole-struct
// reflect.DeepEqual with a couple of fields zeroed first) is deliberate:
// http.Transport carries unexported connection-pool state that starts
// identical (empty) in two never-used values but diverges the moment
// either handles a real request, which every production Transport this
// function is asked to check has, by the time its own package's tests
// reach this assertion. A field-by-field walk restricted to exported
// fields never touches that state at all, and a future Go release adding
// a new exported field is compared automatically rather than silently
// skipped, which a hardcoded allowlist would have done instead.
func AssertTransportFieldsMatch(t *testing.T, got, want *http.Transport) {
	t.Helper()
	if !sameFuncOrBothNil(got.Proxy, want.Proxy) {
		t.Errorf("Transport.Proxy differs from expected")
	}
	gv, wv := reflect.ValueOf(got).Elem(), reflect.ValueOf(want).Elem()
	typ := gv.Type()
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if !f.IsExported() || transportFieldsSkippedInComparison[f.Name] {
			continue
		}
		gf, wf := gv.Field(i).Interface(), wv.Field(i).Interface()
		if !reflect.DeepEqual(gf, wf) {
			t.Errorf("Transport.%s = %#v, want %#v", f.Name, gf, wf)
		}
	}
}

// sameFuncOrBothNil compares two func(*http.Request) (*url.URL, error)
// values (http.Transport.Proxy's type) by their underlying code pointer —
// the only way two independent copies of the same top-level function value
// (http.ProxyFromEnvironment, referenced by several of this module's
// Transports) compare equal, since reflect.DeepEqual never considers two
// non-nil funcs deeply equal at all.
func sameFuncOrBothNil(a, b func(*http.Request) (*url.URL, error)) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return reflect.ValueOf(a).Pointer() == reflect.ValueOf(b).Pointer()
}
