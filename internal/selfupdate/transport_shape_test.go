package selfupdate

// D1c's second review, applied to this package's client: naming
// internal/netdial's shared seam must not change what selfupdateDialer
// actually dials with — http.DefaultTransport's own dialer, 30s timeout,
// 30s keep-alive, since this client used to rely on http.DefaultTransport
// implicitly (Transport left nil).

import (
	"net/http"
	"testing"
	"time"

	"github.com/twilight-project/dropin-miner/internal/networkfence"
)

func TestProductionTransportMatchesItsPreD1cShapeExactly(t *testing.T) {
	dt, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		t.Fatalf("http.DefaultTransport is a %T, not *http.Transport", http.DefaultTransport)
	}
	networkfence.AssertTransportFieldsMatch(t, selfupdateTransport, dt.Clone())
	if selfupdateDialer.Timeout != 30*time.Second {
		t.Errorf("selfupdateDialer.Timeout = %v, want 30s (http.DefaultTransport's own dialer)", selfupdateDialer.Timeout)
	}
	if selfupdateDialer.KeepAlive != 30*time.Second {
		t.Errorf("selfupdateDialer.KeepAlive = %v, want 30s (http.DefaultTransport's own dialer)", selfupdateDialer.KeepAlive)
	}
}
