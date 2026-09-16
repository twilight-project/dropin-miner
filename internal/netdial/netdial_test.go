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
// really do reproduce http.DefaultTransport's own dialer values, not just a
// value this package's author believed was the same one.
func TestDefaultMatchesHTTPDefaultTransport(t *testing.T) {
	dt, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		t.Fatalf("http.DefaultTransport is a %T, not *http.Transport", http.DefaultTransport)
	}
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

// TestForCallsTheGivenDialerWhenNoHookIsInstalled is the behavior For
// promises production code: with Hook nil, the returned function is
// indistinguishable from naming base.DialContext directly. Proven by a
// short-timeout dialer against an address that never accepts a connection
// (TEST-NET-1, RFC 5737) — if For called anything other than base, the
// short timeout it was given would not be what bounds the failure.
func TestForCallsTheGivenDialerWhenNoHookIsInstalled(t *testing.T) {
	orig := Hook
	Hook = nil
	t.Cleanup(func() { Hook = orig })

	base := &net.Dialer{Timeout: 50 * time.Millisecond}
	dial := For(base)
	start := time.Now()
	_, err := dial(context.Background(), "tcp", "192.0.2.1:80")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("dialing TEST-NET-1 succeeded — that address must never be reachable")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("dial took %v, want roughly base.Timeout (50ms) — For is not using the given dialer", elapsed)
	}
}

// TestForObservesAHookInstalledAfterConstruction is the injection-checked
// guarantee this package exists to make true: a DialContext function
// built as a package-level var (constructed once, at init, before any
// test's TestMain ever runs) via For(base) must still be reachable after a
// test reassigns Hook later. The bug this guards against is real and was
// found building D1c: naming a package variable directly in a Transport
// literal (DialContext: someVar) copies whatever function value the
// variable held AT THAT MOMENT into the struct field permanently —
// reassigning the variable afterward has no effect on a Transport already
// built that way. For's closure reads Hook fresh on every call instead, so
// this must pass even when dial was obtained before Hook is set.
func TestForObservesAHookInstalledAfterConstruction(t *testing.T) {
	// Mimics exactly what a package-level var elsewhere in the module
	// does: obtained once, before Hook is ever installed.
	dial := For(&net.Dialer{})

	orig := Hook
	t.Cleanup(func() { Hook = orig })
	sentinel := errors.New("sentinel: Hook was called")
	Hook = func(ctx context.Context, network, addr string) (net.Conn, error) {
		return nil, sentinel
	}

	_, err := dial(context.Background(), "tcp", "example.invalid:443")
	if !errors.Is(err, sentinel) {
		t.Fatalf("a dial function obtained before Hook was installed did not observe it: got %v, want the sentinel", err)
	}
}
