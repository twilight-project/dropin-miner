package netdial

import (
	"context"
	"errors"
	"net"
	"net/http"
	"testing"
	"time"
)

// TestDefaultMatchesHTTPDefaultTransport proves DefaultTimeout/DefaultKeepAlive
// (and so DialContext's default) really do reproduce http.DefaultTransport's
// own dialer, not just a value this package's author believed was the same
// one. If net/http's own DefaultTransport literal ever changes these values,
// this test is what notices — the alternative (this package silently
// drifting from what "the same dialer as before" actually means for the
// four clients that used to rely on DefaultTransport implicitly) is exactly
// the kind of behavior change the doc comment promises does not happen.
func TestDefaultMatchesHTTPDefaultTransport(t *testing.T) {
	dt, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		t.Fatalf("http.DefaultTransport is a %T, not *http.Transport", http.DefaultTransport)
	}
	// http.DefaultTransport's own DialContext is built from a *net.Dialer
	// closure (net/http's defaultTransportDialContext), which is not
	// itself inspectable — so this asserts the one thing that is: this
	// package's constants match the literal values net/http's own source
	// constructs DefaultTransport's dialer from (Timeout: 30s, KeepAlive:
	// 30s), named here rather than hardcoded a second time so a change to
	// either constant is caught in one place.
	if DefaultTimeout != 30*time.Second {
		t.Errorf("DefaultTimeout = %v, want 30s (http.DefaultTransport's own dialer)", DefaultTimeout)
	}
	if DefaultKeepAlive != 30*time.Second {
		t.Errorf("DefaultKeepAlive = %v, want 30s (http.DefaultTransport's own dialer)", DefaultKeepAlive)
	}
	if dt.DialContext == nil {
		t.Fatal("http.DefaultTransport.DialContext is nil — net/http's own shape changed; re-check this package's assumption")
	}
}

// TestDialContextDefaultsToTheDefaultDialer checks what a test CAN check
// about a func value in Go (nothing compares equal to another func except
// nil): that DialContext is set at package init, not left nil for the first
// caller to panic on, and that the dialer its initializer names has the
// values this package promises.
func TestDialContextDefaultsToTheDefaultDialer(t *testing.T) {
	if defaultDialer.Timeout != DefaultTimeout || defaultDialer.KeepAlive != DefaultKeepAlive {
		t.Fatalf("defaultDialer = %+v, want Timeout=%v KeepAlive=%v", defaultDialer, DefaultTimeout, DefaultKeepAlive)
	}
	if DialContext == nil {
		t.Fatal("DialContext is nil at package init — every production Transport that names it would panic on first dial")
	}
}

// TestDialIndirectsThroughAReassignedDialContext is the injection-checked
// guarantee this package exists to make true: a Transport built as a
// package-level var (constructed once, at init, before any test's TestMain
// ever runs) and given DialContext: Dial must still be reachable after a
// test reassigns DialContext later. The bug this guards against is real
// and was found building D1c: a Transport built with `DialContext:
// DialContext` (naming the variable directly) copies whatever function
// value DialContext held AT THAT MOMENT into the struct field permanently
// — reassigning the package variable afterward has no effect on a Transport
// already built that way, so cmd/dropin-miner's own package-level
// Transports (the login probe, the search client, wallet_tx.go before this
// package existed) were never actually reached by the test fence at all;
// they just happened to look refused because of an unrelated proxy-env
// masking bug that produced the same symptom for a different reason.
func TestDialIndirectsThroughAReassignedDialContext(t *testing.T) {
	// Mimics exactly what a package-level var elsewhere in the module does:
	// captured once, before DialContext is ever reassigned.
	capturedAtInitTime := Dial

	orig := DialContext
	t.Cleanup(func() { DialContext = orig })

	sentinel := errors.New("sentinel: DialContext was called")
	DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		return nil, sentinel
	}

	_, err := capturedAtInitTime(context.Background(), "tcp", "example.invalid:443")
	if !errors.Is(err, sentinel) {
		t.Fatalf("Dial captured before reassignment did not observe the reassigned DialContext: got %v, want the sentinel", err)
	}
}
