// Package netdial is the one dial seam every http.Transport this module
// builds in production code goes through — not by sharing one dialer, but
// by sharing one hook every transport's own dialer checks first.
//
// D1c's first attempt shared a single default dialer (30s timeout, 30s
// keep-alive, matching http.DefaultTransport) and pointed every Transport
// at it. That changed production behavior in two directions at once: the
// four clients that used to rely on http.DefaultTransport implicitly
// (leaving Transport nil) got a bare &http.Transport{} in its place,
// losing DefaultTransport's ForceAttemptHTTP2, TLSHandshakeTimeout,
// IdleConnTimeout, MaxIdleConns and ExpectContinueTimeout — a custom
// DialContext with none of those set does not behave like
// DefaultTransport just because the dial timing matches. And the three
// clients that built their own Proxy: nil Transport with no DialContext at
// all (net/http's zero net.Dialer: no connect timeout, a 15s keep-alive)
// got the 30s/30s values instead, a real change this module's own review
// ruled out.
//
// This package fixes both by not choosing a dialer at all. For takes the
// *net.Dialer a Transport would have used — a fresh zero-value one for a
// client that built its own Transport, or one built from DefaultTimeout/
// DefaultKeepAlive for a client cloning http.DefaultTransport — and
// returns a DialContext function that calls it directly, unchanged from
// what naming that dialer's own DialContext method would already do. Hook
// is the only thing that changes that: nil in production, and reassigned
// to internal/networkfence's fence for the duration of a test run, which
// then intercepts every Transport built this way at once, regardless of
// which dialer each would otherwise use.
package netdial

import (
	"context"
	"net"
	"time"
)

// DefaultTimeout and DefaultKeepAlive are http.DefaultTransport's own
// dialer values (net/http's DefaultTransport literal), named here so a
// client cloning DefaultTransport can build its own equivalent *net.Dialer
// from named constants rather than a repeated magic 30 — see each such
// client's own dialer var for why it needs one at all instead of just
// reading DefaultTransport's.
const (
	DefaultTimeout   = 30 * time.Second
	DefaultKeepAlive = 30 * time.Second
)

// Hook, when non-nil, replaces every dial made by a Transport built with
// For — internal/networkfence's Install is the only thing that ever
// assigns it, for the duration of a test run. nil in production.
//
// Checked at call time inside the function For returns, never copied at
// construction: a Transport built as a package-level var, before any
// TestMain ever runs, still observes a later reassignment of Hook, because
// the closure For returns reads this variable fresh on every dial rather
// than capturing whatever it held when the Transport was built. D1c found
// the alternative the hard way — a seam named directly (DialContext:
// DialContext, this variable) instead of through an indirecting function
// copies the function value it held at that moment permanently into the
// struct field, and a later reassignment has no effect on a Transport
// already built that way. TestForObservesAHookInstalledAfterConstruction
// guards the mechanism.
var Hook func(ctx context.Context, network, addr string) (net.Conn, error)

// For returns a DialContext function for a Transport that would otherwise
// dial with base directly. In production (Hook nil) it calls
// base.DialContext for every dial — exactly what naming base.DialContext
// itself would do, so a client's own choice of dialer (a zero net.Dialer,
// or one matching DefaultTransport's) is preserved byte for byte. A test
// installing Hook is the only thing that changes behavior, and it reaches
// every Transport built this way, not just the ones built after the
// installation.
func For(base *net.Dialer) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		if Hook != nil {
			return Hook(ctx, network, addr)
		}
		return base.DialContext(ctx, network, addr)
	}
}
