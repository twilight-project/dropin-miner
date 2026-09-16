package auth

// D1c's second review, applied to this package's two clients: naming
// internal/netdial's shared seam must not change what discoveryDialer and
// dpopDialer actually dial with. Both built a bare Transport{Proxy: nil}
// with no DialContext before this — net/http's zero net.Dialer, no
// explicit connect timeout, a 15s default keep-alive — and must still,
// against a fresh zero-value *http.Transport (except DialContext).

import (
	"net"
	"net/http"
	"testing"

	"github.com/twilight-project/dropin-miner/internal/networkfence"
)

func TestProductionTransportsMatchTheirPreD1cShapeExactly(t *testing.T) {
	t.Run("discovery client (was a bare Transport)", func(t *testing.T) {
		d, err := NewDiscoverer(DiscoveryConfig{BaseURL: "https://fence-probe.invalid", ChainID: "x", SlotID: 1})
		if err != nil {
			t.Fatalf("NewDiscoverer: %v", err)
		}
		transport, ok := d.client.Transport.(*http.Transport)
		if !ok {
			t.Fatalf("Discoverer's client Transport is a %T, not *http.Transport", d.client.Transport)
		}
		networkfence.AssertTransportFieldsMatch(t, transport, &http.Transport{})
		assertZeroValueDialer(t, discoveryDialer)
	})
	t.Run("DPoP base transport (was a bare Transport)", func(t *testing.T) {
		dt := newDPoPTransport(nil)
		transport, ok := dt.base.(*http.Transport)
		if !ok {
			t.Fatalf("dpopTransport's base is a %T, not *http.Transport", dt.base)
		}
		networkfence.AssertTransportFieldsMatch(t, transport, &http.Transport{})
		assertZeroValueDialer(t, dpopDialer)
	})
}

// assertZeroValueDialer checks d has never had an explicit Timeout or
// KeepAlive set — the exact net.Dialer{} zero value this package's two
// Transports always used before this commit gave them a named seam.
func assertZeroValueDialer(t *testing.T, d *net.Dialer) {
	t.Helper()
	if d.Timeout != 0 {
		t.Errorf("dialer Timeout = %v, want 0 (unchanged from before this commit)", d.Timeout)
	}
	if d.KeepAlive != 0 {
		t.Errorf("dialer KeepAlive = %v, want 0 (unchanged from before this commit)", d.KeepAlive)
	}
}
