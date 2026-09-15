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

// proxyEnvVars are every environment variable a Transport with
// Proxy: http.ProxyFromEnvironment might read — the four Clone()-based
// production transports this module builds (the login probe, the search
// client, pkg/platform's client, internal/selfupdate's source) all
// deliberately preserve that Proxy setting, the same as
// http.DefaultTransport itself, so the participant's own proxy
// configuration still applies in production. net/http's own
// ProxyFromEnvironment reads only HTTP_PROXY/HTTPS_PROXY/NO_PROXY (and
// lowercase); ALL_PROXY is cleared too as a second layer, since it is a
// convention some other HTTP tooling honors even though this module's own
// clients do not read it.
var proxyEnvVars = []string{
	"HTTP_PROXY", "http_proxy",
	"HTTPS_PROXY", "https_proxy",
	"ALL_PROXY", "all_proxy",
}

// clearProxyEnv unsets every variable in proxyEnvVars.
//
// This is not optional, and the order Install calls it in is not
// incidental: a developer's own machine routinely already has one of these
// set — a corporate proxy agent, a VPN client, a local debugging proxy —
// entirely independently of anything this fence does, and Go's own
// http.ProxyFromEnvironment reads the environment exactly ONCE per process
// and caches the result forever after (a sync.Once inside net/http itself,
// triggered by the first Transport that actually tries to route a
// request). Clearing these variables after that first read has already
// happened does nothing at all — the cached decision stands for the rest
// of the process. That is exactly the gap a review of this package found:
// with HTTPS_PROXY already pointed at a loopback listener when a test
// began, a Clone()-based client's CONNECT went to that listener — which a
// real corporate proxy or VPN client would then have forwarded to the real
// production host — while netdial's DialContext saw only the (loopback,
// so allowed) proxy address and never learned what the real target even
// was. Clearing the environment is therefore the very first thing Install
// does, before netdial.Hook is touched or anything else runs, and Guard
// calls Install before m.Run() so this happens before any test — and
// before any package-level Transport var's first real dial — gets the
// chance to cache a poisoned value.
func clearProxyEnv() {
	for _, key := range proxyEnvVars {
		_ = os.Unsetenv(key)
	}
}

// Install clears the proxy environment (clearProxyEnv, first) and
// reassigns netdial.Hook to the fence. Called once, by Guard, before
// m.Run(). Every Transport this module builds in production code dials via
// netdial.For(itsOwnDialer), which checks Hook first and its own dialer
// only when Hook is nil — so this one reassignment reaches every client at
// once, whatever dialer each would otherwise use, without this package
// needing to know what any of them are.
func Install() error {
	clearProxyEnv()
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
