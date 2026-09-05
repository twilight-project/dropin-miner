package auth

// GATE-0 live legs: the same AUTH-020/021 assertions the package tests
// prove against httptest, run against a REAL Authorization Server.
// Skipped unless TOKENDROP_LIVE_AS_URL is set (the gate runner sets it;
// CI and laptops skip).

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"
)

func liveConfig(t *testing.T) DiscoveryConfig {
	t.Helper()
	base := os.Getenv("TOKENDROP_LIVE_AS_URL")
	if base == "" {
		t.Skip("TOKENDROP_LIVE_AS_URL not set (GATE-0 live leg)")
	}
	chainID := os.Getenv("TOKENDROP_LIVE_CHAIN_ID")
	if chainID == "" {
		chainID = "twilight-1"
	}
	slot := uint64(7)
	if raw := os.Getenv("TOKENDROP_LIVE_SLOT_ID"); raw != "" {
		v, err := strconv.ParseUint(raw, 10, 64)
		if err != nil {
			t.Fatalf("TOKENDROP_LIVE_SLOT_ID: %v", err)
		}
		slot = v
	}
	return DiscoveryConfig{BaseURL: base, ChainID: chainID, SlotID: slot}
}

// AUTH-020 (live): discovery against the real AS validates and matches
// the configured identity.
func TestLiveDiscoveryAgainstRealAS(t *testing.T) {
	cfg := liveConfig(t)
	d, err := NewDiscoverer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	doc, err := d.Document(ctx)
	if err != nil {
		t.Fatalf("live discovery failed: %v", err)
	}
	if doc.ObservationsEndpoint == "" || len(doc.SourceProfiles) == 0 {
		t.Fatalf("live document incomplete: %+v", doc)
	}
	t.Logf("live AS discovered: chain=%s slot=%s profiles=%v", doc.ChainID, doc.SlotID, doc.SourceProfiles)
}

// AUTH-020/021 (live): a proxy configured for a different chain or slot
// refuses the same real AS, fail-closed.
func TestLiveDiscoveryMismatchFailsClosed(t *testing.T) {
	base := liveConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	wrongChain := base
	wrongChain.ChainID = "twilight-other"
	d, err := NewDiscoverer(wrongChain)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Document(ctx); err == nil {
		t.Fatal("live AS accepted under a mismatched chain_id; §19 requires fail-closed")
	}

	wrongSlot := base
	wrongSlot.SlotID = base.SlotID + 1
	d, err = NewDiscoverer(wrongSlot)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Document(ctx); err == nil {
		t.Fatal("live AS accepted under a mismatched slot_id; §19 requires fail-closed")
	}
}
