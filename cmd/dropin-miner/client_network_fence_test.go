package main

// One guard per production http.Client this binary builds that talks to a
// real host in production. Each test drives the client through its REAL
// production construction path — never a stand-in for the client itself —
// but at an address that can never resolve or route anywhere at all:
// fenceProbeHost (a hostname under the .invalid TLD, reserved by RFC 6761
// to never resolve) or fenceProbeIP (192.0.2.1, TEST-NET-1 under RFC 5737,
// reserved for documentation and never routed). With a working fence,
// DialContext refuses either before any DNS lookup or socket ever opens.
// With a broken one, the test still contacts nothing — .invalid fails DNS
// resolution and TEST-NET-1 fails routing, by the same RFCs that reserve
// them — so the failure mode of a regression here is a client-side error,
// never a request that actually reaches a real service. That is the whole
// point: a target under *.nyks.dev or api.github.com would make a fence
// regression reach production, which is exactly what this file exists to
// prevent.
//
// TestFenceClassifiesProductionHostnamesAsRefused separately proves the
// module's real default hostnames — router-api.nyks.dev, agents-v1.nyks.dev,
// rpc.nyks.dev, api.github.com, rewards.nyks.dev — are classified as refused
// by the fence's decision function, as plain strings, with no dial and no
// network of any kind. That is the half of "prove it is covered" this file
// can do safely for a client (internal/selfupdate's) whose target host is a
// compiled-in constant with no override seam.

import (
	"context"
	neturl "net/url"
	"testing"
	"time"

	"github.com/twilight-project/dropin-miner/internal/networkfence"
	"github.com/twilight-project/dropin-miner/pkg/config"
	"github.com/twilight-project/dropin-miner/pkg/platform"
)

// fenceProbeHost and fenceProbeIP are the two forms rule 1 (D1c's review)
// asks for: a hostname under .invalid, and a TEST-NET-1 address, so both
// the hostname-refusal and IP-literal-refusal paths through the fence get
// exercised, not just one.
const (
	fenceProbeHost = "fence-probe.invalid"
	fenceProbeIP   = "192.0.2.1"
)

// fencedCallTimeout is generous only so a genuine bug (the fence failing to
// refuse, and .invalid or TEST-NET-1 instead hanging on some resolver or
// routing edge case) fails as a clear timeout rather than hanging the
// suite — a working fence returns in microseconds.
const fencedCallTimeout = 5 * time.Second

// assertRefused drains the fence and requires exactly one refusal, of the
// fence's own typed shape, naming wantAddr — never merely "an error", which
// a real DNS failure or a coincidental unrelated error could also produce.
func assertRefused(t *testing.T, err error, wantAddr string) {
	t.Helper()
	if err == nil {
		t.Fatal("the call returned no error at all — did it actually reach the network?")
	}
	refusal, ok := networkfence.AsRefusal(err)
	if !ok {
		t.Fatalf("error is not the fence's typed refusal (got %T: %v) — "+
			"a plain error here could mean this ran into a real network problem instead of being refused", err, err)
	}
	if refusal.Addr != wantAddr {
		t.Errorf("fence refused %q, want %q", refusal.Addr, wantAddr)
	}
	if left := networkfence.Expect(); len(left) == 0 {
		t.Fatal("the fence recorded no refusal at all, despite a typed refusal error coming back — inconsistent state")
	}
}

func TestNetworkFenceCoversTheLoginProbe(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), fencedCallTimeout)
	defer cancel()
	_, _, err := probeKey(ctx, "https://"+fenceProbeHost, "canary-not-a-real-key") // #nosec G101 -- a syntactically valid placeholder, never sent anywhere
	assertRefused(t, err, fenceProbeHost+":443")
}

func TestNetworkFenceCoversTheSearchClient(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), fencedCallTimeout)
	defer cancel()
	call := searchCall{
		Endpoint: "https://" + fenceProbeHost + "/v1/search",
		Key:      "canary-not-a-real-key", // #nosec G101 -- a syntactically valid placeholder, never sent anywhere
		Query:    "network fence guard — this must never leave the machine",
	}
	out := performSearch(ctx, time.Now, call)
	assertRefused(t, out.Err, fenceProbeHost+":443")
}

func TestNetworkFenceCoversThePlatformClient(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), fencedCallTimeout)
	defer cancel()
	client := platform.New("https://"+fenceProbeHost, "https://"+fenceProbeHost)
	_, err := client.Register(ctx, "network-fence-guard", []string{"credits"})
	assertRefused(t, err, fenceProbeHost+":443")
}

// TestNetworkFenceCoversTheWalletRPCClient drives newRPCClient at the
// TEST-NET-1 address rather than a hostname, covering the IP-literal path
// through the fence — and, unlike D1b, needs no per-test seam-arming: the
// Transport wallet_tx.go builds now names netdial.DialContext directly, so
// TestMain's Guard() covers it for every test in this package, not just
// this one.
func TestNetworkFenceCoversTheWalletRPCClient(t *testing.T) {
	client := newRPCClient("https://" + fenceProbeIP)
	ctx, cancel := context.WithTimeout(context.Background(), fencedCallTimeout)
	defer cancel()
	var out any
	err := client.get(ctx, "/status", nil, &out)
	assertRefused(t, err, fenceProbeIP+":443")
}

// TestFenceClassifiesProductionHostnamesAsRefused is the pure, no-dial half
// of proving coverage: every real default hostname this module's clients
// point at, checked as a plain string against the fence's own decision
// function. internal/selfupdate's source has no test here that actually
// dials — NewHTTPSource's apiBase/downloadBase are compiled-in constants
// with no override seam, so a live guard test for it would have no target
// but the real api.github.com, which rule 1 forbids — but its production
// Transport does dial through the same shared seam as everything else
// (internal/selfupdate/source.go's selfupdateTransport), and this line
// proves the fence would refuse that real hostname if it ever tried.
func TestFenceClassifiesProductionHostnamesAsRefused(t *testing.T) {
	hosts := []string{
		config.DefaultRouterURL,                          // login probe, search client
		config.DefaultAgentsAPIURL,                       // pkg/platform
		config.DefaultASBaseURL,                          // pkg/auth discovery + DPoP/credential
		config.DefaultWalletNodes[config.DefaultChainID], // wallet_tx.go
		"https://api.github.com",                         // internal/selfupdate (compiled-in, no override seam)
	}
	for _, raw := range hosts {
		host := mustHost(t, raw)
		t.Run(host, func(t *testing.T) {
			refusal := networkfence.Decide("tcp", host+":443")
			if refusal == nil {
				t.Fatalf("the fence would ALLOW a dial to %s:443 — a production hostname must never classify as loopback", host)
			}
		})
	}
}

func mustHost(t *testing.T, rawURL string) string {
	t.Helper()
	u, err := neturl.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse %q: %v", rawURL, err)
	}
	if u.Hostname() == "" {
		t.Fatalf("%q has no hostname", rawURL)
	}
	return u.Hostname()
}
