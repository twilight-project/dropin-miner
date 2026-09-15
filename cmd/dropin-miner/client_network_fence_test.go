package main

// One guard per production http.Client this binary builds that talks to a
// real host (network_fence_test.go's own doc comment explains the fence
// itself). Each test drives the client through its REAL production
// construction path, at its REAL default hostname — never a stand-in — and
// proves the fence refused the dial before anything reached the network:
// the whole point is that a regression here would otherwise reach
// production silently, exactly as D1's own connect-refusal guard did until
// this fence existed.
//
// Every client below either leaves its Transport nil (so a global
// http.DefaultTransport swap reaches it directly: the login probe, the
// search client via searchTransport, pkg/platform's client, and
// internal/selfupdate's source) or is in this same package and was given a
// dial-context seam for exactly this purpose (wallet_tx.go's
// rpcClientDialContext, the same idiom as searchTransport itself).
//
// pkg/auth's discovery and DPoP/credential transports are neither: each
// constructs its own *http.Transport with Proxy: nil and no DialContext of
// its own, which a global swap cannot reach, and pkg/auth is out of scope
// for this commit (no seam is added there — see the package doc on
// TestPkgAuthTransportsAreNotYetCoveredByTheNetworkFence below, which
// proves that gap structurally rather than assuming it, and does not try
// to close it).

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/twilight-project/dropin-miner/internal/selfupdate"
	"github.com/twilight-project/dropin-miner/pkg/config"
	"github.com/twilight-project/dropin-miner/pkg/platform"
)

// fencedCallTimeout is generous only so a genuine bug (the fence failing to
// refuse) fails as a clear timeout rather than hanging the suite — a
// working fence returns in microseconds, refusing before any dial.
const fencedCallTimeout = 5 * time.Second

// assertOnlyRefusalsRecorded drains the fence and fails the test unless at
// least one refusal was recorded and every recorded address is exactly one
// of want (order-independent, since two attempts in one call — search's
// retry, for instance — can both be recorded).
func assertOnlyRefusalsRecorded(t *testing.T, want ...string) {
	t.Helper()
	got := networkFenceExpect()
	if len(got) == 0 {
		t.Fatalf("the fence recorded no refusal at all — this client reached (or hung trying to reach) the network")
	}
	wantSet := map[string]bool{}
	for _, w := range want {
		wantSet[w] = true
	}
	for _, r := range got {
		if !wantSet[r.Addr] {
			t.Errorf("fence refused an unexpected address %q; wanted one of %v", r.Addr, want)
		}
	}
}

func TestNetworkFenceCoversTheLoginProbe(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), fencedCallTimeout)
	defer cancel()
	_, _, err := probeKey(ctx, config.DefaultRouterURL, "canary-not-a-real-key") // #nosec G101 -- a syntactically valid placeholder, never sent past the fence
	if err == nil {
		t.Fatal("probeKey against the real router host returned no error — did it actually reach the network?")
	}
	assertOnlyRefusalsRecorded(t, "router-api.nyks.dev:443")
}

func TestNetworkFenceCoversTheSearchClient(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), fencedCallTimeout)
	defer cancel()
	call := searchCall{
		Endpoint: config.DefaultRouterURL + "/v1/search",
		Key:      "canary-not-a-real-key", // #nosec G101 -- a syntactically valid placeholder, never sent past the fence
		Query:    "network fence guard — this must never leave the machine",
	}
	out := performSearch(ctx, time.Now, call)
	if out.Err == nil {
		t.Fatal("performSearch against the real router host returned no error — did it actually reach the network?")
	}
	assertOnlyRefusalsRecorded(t, "router-api.nyks.dev:443")
}

func TestNetworkFenceCoversThePlatformClient(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), fencedCallTimeout)
	defer cancel()
	client := platform.New(config.DefaultAgentsAPIURL, config.DefaultPlatformBaseURL)
	_, err := client.Register(ctx, "network-fence-guard", []string{"credits"})
	if err == nil {
		t.Fatal("Register against the real platform host returned no error — did it actually reach the network?")
	}
	assertOnlyRefusalsRecorded(t, "agents-v1.nyks.dev:443")
}

func TestNetworkFenceCoversTheSelfUpdateSource(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), fencedCallTimeout)
	defer cancel()
	src := selfupdate.NewHTTPSource(nil) // nil: exactly NewHTTPClient(), the production default
	_, err := src.Release(ctx, nil)
	if err == nil {
		t.Fatal("Release against the real GitHub API host returned no error — did it actually reach the network?")
	}
	assertOnlyRefusalsRecorded(t, "api.github.com:443")
}

// TestNetworkFenceCoversTheWalletRPCClient proves rpcClientDialContext
// (wallet_tx.go) actually reaches the client newRPCClient builds: this is
// the one production Transport in this package that does not default to
// http.DefaultTransport, so it needed its own seam rather than being
// reachable by the global swap alone. Production leaves the seam nil
// (newRPCClient's own Transport.DialContext stays unset, unchanged
// behavior); only this test arms it.
func TestNetworkFenceCoversTheWalletRPCClient(t *testing.T) {
	prev := rpcClientDialContext
	rpcClientDialContext = networkFenceDialContext
	t.Cleanup(func() { rpcClientDialContext = prev })

	node := config.DefaultWalletNodes[config.DefaultChainID]
	client := newRPCClient(node)
	ctx, cancel := context.WithTimeout(context.Background(), fencedCallTimeout)
	defer cancel()
	var out any
	err := client.get(ctx, "/status", nil, &out)
	if err == nil {
		t.Fatal("a call against the real wallet node host returned no error — did it actually reach the network?")
	}
	assertOnlyRefusalsRecorded(t, "rpc.nyks.dev:443")
}

// TestPkgAuthTransportsAreNotYetCoveredByTheNetworkFence is not a guard —
// it is the honest opposite of one, and it is why this commit's report
// says pkg/auth needs its own ruling rather than claiming it is already
// covered. pkg/auth's discovery and DPoP/credential clients (discovery.go,
// transport.go) each build a fresh *http.Transport{Proxy: nil} with no
// DialContext of its own, deliberately (so no environment proxy can
// interpose on AS identity — see those files' own comments), which means
// neither the global http.DefaultTransport swap nor the HTTP_PROXY
// fail-safe reaches them, and pkg/auth is out of scope for a seam in this
// commit. Actually driving one of those clients at a real AS hostname to
// prove that gap would be the one way to reach the real network by
// accident that this whole commit exists to prevent — so this test proves
// the gap by reading the source instead (AGENTS.md's own "a source-reading
// test" pattern, boundary_test.go's idiom), and is itself the record that
// the gap was checked, not assumed.
//
// If this test ever goes red, pkg/auth's Transport construction changed:
// either it now sets its own DialContext (in which case this test's job is
// done and it should be deleted, not fixed) or it now reads the proxy
// environment (in which case the HTTP_PROXY fail-safe would reach it and
// this test's assumption needs revisiting either way).
func TestPkgAuthTransportsAreNotYetCoveredByTheNetworkFence(t *testing.T) {
	root := moduleRoot(t)
	for _, c := range []struct {
		file   string
		lookIn string
	}{
		{"pkg/auth/discovery.go", "NewDiscoverer"},
		{"pkg/auth/transport.go", "newDPoPTransport"},
	} {
		t.Run(c.file, func(t *testing.T) {
			assertTransportHasNoDialContextSeam(t, root, c.file, c.lookIn)
		})
	}
}

// assertTransportHasNoDialContextSeam is deliberately a plain substring
// check on the source, not an AST walk: boundary_test.go's AST sweep
// already proves every http.Client{} sets CheckRedirect, a different
// property. This test asserts something narrower and more literal — that
// the named function's body contains an http.Transport{Proxy: nil} literal
// and no DialContext/DialTLSContext field anywhere in the file — which is
// exactly the textual shape a future PR adding a seam (closing this gap)
// or a proxy-reading change (opening a different one) would have to alter,
// so either change trips this test rather than passing it unnoticed.
func assertTransportHasNoDialContextSeam(t *testing.T, root, relPath, fn string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, relPath)) // #nosec G304 -- a fixed path under this module's own root
	if err != nil {
		t.Fatalf("read %s: %v", relPath, err)
	}
	src := string(data)
	if !strings.Contains(src, fn) {
		t.Fatalf("%s no longer defines %s — update this test's target", relPath, fn)
	}
	if !strings.Contains(src, "Proxy: nil") {
		t.Fatalf("%s no longer has a Transport{Proxy: nil} literal — it may now read the proxy environment, "+
			"which the fence's HTTP_PROXY fail-safe would then reach; re-examine before trusting either way", relPath)
	}
	if strings.Contains(src, "DialContext") || strings.Contains(src, "DialTLSContext") {
		t.Fatalf("%s now sets a DialContext/DialTLSContext — the network fence gap this test records may be "+
			"closed; if so, delete this test rather than fix it", relPath)
	}
}
