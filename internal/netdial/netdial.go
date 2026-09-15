// Package netdial is the one dial seam every http.Transport this module
// builds in production code goes through, instead of each package building
// its own net.Dialer (or, worse, several different ones with quietly
// different timeouts). Before this package existed, half of this module's
// clients (the search client, the login probe, pkg/platform's client,
// internal/selfupdate's source) relied on http.DefaultTransport's own
// dialer implicitly, by leaving Transport nil, while the other half
// (pkg/auth's discovery and DPoP transports, wallet_tx.go's RPC client)
// built their own *http.Transport with no DialContext at all, which
// net/http resolves to a bare net.Dialer{} — no explicit connect timeout,
// the platform's default keep-alive — silently different from
// DefaultTransport's own 30s/30s. Neither difference was a deliberate
// choice; it was just what a plain composite literal happened to do.
//
// DialContext's default reproduces http.DefaultTransport's own dialer
// exactly (30s connect timeout, 30s keep-alive — see net/http's own
// DefaultTransport literal): every production client that used to rely on
// DefaultTransport implicitly gets byte-for-byte the same dialer by naming
// this var explicitly instead, and every client that used to build its own
// Proxy: nil Transport with no DialContext gets an explicit, bounded connect
// timeout where none existed before — a real difference at the net.Dialer
// level, but not an observable one: every client in this module already
// bounds its whole request with an http.Client.Timeout of 30 seconds or
// more (netdial_test.go's TestDefaultMatchesEveryClientsOwnOuterTimeout
// checks this), so the raw TCP connect phase was never the constraint that
// actually governed how long a caller could wait.
//
// A test — in this package or any other — reassigns DialContext to
// intercept every dial the whole module attempts, from any package, without
// needing a seam per client. internal/networkfence is that seam's one
// consumer: it is what TestMain in every package that builds a network
// client sets DialContext to.
package netdial

import (
	"context"
	"net"
	"time"
)

// DefaultTimeout and DefaultKeepAlive are http.DefaultTransport's own
// dialer values (net/http's DefaultTransport literal), named here so a test
// can assert against them directly rather than against a magic 30 that
// could silently drift from what net/http actually does.
const (
	DefaultTimeout   = 30 * time.Second
	DefaultKeepAlive = 30 * time.Second
)

// defaultDialer is unexported and never reassigned: it exists so a test can
// restore DialContext to production behavior by name (DialContext =
// defaultDialer.DialContext) without having to reconstruct the same values
// itself.
var defaultDialer = &net.Dialer{Timeout: DefaultTimeout, KeepAlive: DefaultKeepAlive}

// DialContext is the seam a test reassigns. Production code never names it
// directly — see Dial.
var DialContext func(ctx context.Context, network, addr string) (net.Conn, error) = defaultDialer.DialContext

// Dial is what every http.Transport this module builds in non-test code
// sets its own DialContext field to — boundary_test.go's
// TestEveryHTTPTransportDialsThroughTheSharedSeam is what proves that
// holds module-wide, and a future client that instead builds its own
// net.Dialer trips that sweep rather than silently opening a hole no test
// can see.
//
// Dial exists, rather than every consumer naming the DialContext variable
// itself, because several of this module's Transports are package-level
// vars (constructed once, at package init, so repeated requests share one
// connection pool) — and DialContext: DialContext in that shape would copy
// whatever function value the variable held AT INIT TIME into the
// Transport's own field permanently, before any test's TestMain ever ran
// to reassign it. Dial is a stable function value that looks up the
// current DialContext on every call instead, so reassigning DialContext
// changes what every already-constructed Transport does on its next dial,
// not just the ones built after the reassignment.
func Dial(ctx context.Context, network, addr string) (net.Conn, error) {
	return DialContext(ctx, network, addr)
}
