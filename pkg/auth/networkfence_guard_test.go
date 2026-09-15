package auth

// D1c closes the gap D1b left open: pkg/auth's discovery and DPoP/credential
// transports each build their own *http.Transport with Proxy: nil, which
// neither a global http.DefaultTransport swap nor an environment-proxy
// fail-safe can reach — only naming internal/netdial's shared seam
// explicitly, as discovery.go and transport.go now do, closes it. This file
// is the guard-side proof: TestMain installs the same test fence
// cmd/dropin-miner uses, and each test below drives the real, unexported
// production client at an address that can never resolve or route
// (fence-probe.invalid, RFC 6761; 192.0.2.1, TEST-NET-1 under RFC 5737) —
// never a real AS hostname, so a regression in the fence fails on a DNS or
// routing error here, not by reaching a real service.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/twilight-project/dropin-miner/internal/networkfence"
)

func TestMain(m *testing.M) {
	os.Exit(networkfence.Guard(m))
}

func assertRefused(t *testing.T, err error, wantAddr string) {
	t.Helper()
	if err == nil {
		t.Fatal("the call returned no error at all — did it actually reach the network?")
	}
	refusal, ok := networkfence.AsRefusal(err)
	if !ok {
		t.Fatalf("error is not the fence's typed refusal (got %T: %v)", err, err)
	}
	if refusal.Addr != wantAddr {
		t.Errorf("fence refused %q, want %q", refusal.Addr, wantAddr)
	}
	if left := networkfence.Expect(); len(left) == 0 {
		t.Fatal("the fence recorded no refusal at all, despite a typed refusal error coming back")
	}
}

// TestNetworkFenceCoversDiscovery drives NewDiscoverer's real client — the
// exported constructor every production caller uses — at a hostname that
// can never resolve.
func TestNetworkFenceCoversDiscovery(t *testing.T) {
	d, err := NewDiscoverer(DiscoveryConfig{
		BaseURL: "https://fence-probe.invalid",
		ChainID: "fence-probe",
		SlotID:  1,
	})
	if err != nil {
		t.Fatalf("NewDiscoverer: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = d.Document(ctx)
	assertRefused(t, err, "fence-probe.invalid:443")
}

// TestNetworkFenceCoversDPoPCredentialClient drives newDPoPTransport wrapped
// in newCredentialClient — the exact pair every OAuth/mining-plane call in
// this package goes through (oauthclient.go's httpCtx, submit.go,
// joinclient.go, capability.go) — at a TEST-NET-1 address, covering the
// IP-literal path through the fence rather than a second hostname case.
func TestNetworkFenceCoversDPoPCredentialClient(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	proofer, err := NewProofer(key)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		"https://192.0.2.1/observations", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "DPoP canary-not-a-real-token") // #nosec G101 -- a syntactically valid placeholder, never sent anywhere
	_, err = newCredentialClient(newDPoPTransport(proofer)).Do(req)
	assertRefused(t, err, "192.0.2.1:443")
}
