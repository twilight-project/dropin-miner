package platform

// D1c: the same test fence cmd/dropin-miner and pkg/auth install, so this
// package's own tests are walled off from the network the same way. See
// cmd/dropin-miner/network_fence_test.go's D1b history and
// internal/networkfence's package doc for the mechanism and why it exists.

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/twilight-project/dropin-miner/internal/networkfence"
)

func TestMain(m *testing.M) {
	os.Exit(networkfence.Guard(m))
}

// TestNetworkFenceCoversPlatformClient drives New's real client — the
// exported constructor every production caller uses — at a hostname that
// can never resolve (fence-probe.invalid, RFC 6761), never a real platform
// host, so a regression in the fence fails on a DNS error here rather than
// reaching production.
func TestNetworkFenceCoversPlatformClient(t *testing.T) {
	client := New("https://fence-probe.invalid", "https://fence-probe.invalid")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := client.Register(ctx, "network-fence-guard", []string{"credits"})
	if err == nil {
		t.Fatal("Register returned no error at all — did it actually reach the network?")
	}
	refusal, ok := networkfence.AsRefusal(err)
	if !ok {
		t.Fatalf("error is not the fence's typed refusal (got %T: %v)", err, err)
	}
	if refusal.Addr != "fence-probe.invalid:443" {
		t.Errorf("fence refused %q, want %q", refusal.Addr, "fence-probe.invalid:443")
	}
	if left := networkfence.Expect(); len(left) == 0 {
		t.Fatal("the fence recorded no refusal at all, despite a typed refusal error coming back")
	}
}
