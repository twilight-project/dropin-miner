package config

// §4.5: one defaults table, and the per-chain wallet node default.

import (
	"net/url"
	"strings"
	"testing"
)

// TestDefaultsAreTestnet: every scalar default is the testnet value and
// every URL is https; DefaultWalletNodes has exactly one row,
// twilight-testnet-1, its URL is https, and no default anywhere
// contains "devnet", an IP literal, or plain-http :26657.
func TestDefaultsAreTestnet(t *testing.T) {
	scalars := map[string]string{
		"DefaultChainID":         DefaultChainID,
		"DefaultASBaseURL":       DefaultASBaseURL,
		"DefaultRouterURL":       DefaultRouterURL,
		"DefaultPlatformBaseURL": DefaultPlatformBaseURL,
		"DefaultAgentsAPIURL":    DefaultAgentsAPIURL,
		"DefaultWalletDenom":     DefaultWalletDenom,
		"DefaultBech32HRP":       DefaultBech32HRP,
	}
	if DefaultChainID != "twilight-testnet-1" {
		t.Errorf("DefaultChainID = %q, want twilight-testnet-1", DefaultChainID)
	}
	if DefaultSlotID != 3 {
		t.Errorf("DefaultSlotID = %d, want 3", DefaultSlotID)
	}
	if DefaultWalletDenom != "utwlt" {
		t.Errorf("DefaultWalletDenom = %q, want utwlt", DefaultWalletDenom)
	}
	if DefaultBech32HRP != "twilight" {
		t.Errorf("DefaultBech32HRP = %q, want twilight", DefaultBech32HRP)
	}

	urlFields := map[string]string{
		"DefaultASBaseURL":       DefaultASBaseURL,
		"DefaultRouterURL":       DefaultRouterURL,
		"DefaultPlatformBaseURL": DefaultPlatformBaseURL,
		"DefaultAgentsAPIURL":    DefaultAgentsAPIURL,
	}
	for name, raw := range urlFields {
		u, err := url.Parse(raw)
		if err != nil || u.Scheme != "https" {
			t.Errorf("%s = %q is not https", name, raw)
		}
	}

	if len(DefaultWalletNodes) != 1 {
		t.Fatalf("DefaultWalletNodes has %d rows, want exactly 1", len(DefaultWalletNodes))
	}
	node, ok := DefaultWalletNodes["twilight-testnet-1"]
	if !ok {
		t.Fatal(`DefaultWalletNodes is missing the "twilight-testnet-1" row`)
	}
	if u, err := url.Parse(node); err != nil || u.Scheme != "https" {
		t.Errorf("DefaultWalletNodes[twilight-testnet-1] = %q is not https", node)
	}

	all := make(map[string]string, len(scalars)+1)
	for k, v := range scalars {
		all[k] = v
	}
	all["DefaultWalletNodes[twilight-testnet-1]"] = node
	for name, v := range all {
		lower := strings.ToLower(v)
		if strings.Contains(lower, "devnet") {
			t.Errorf("%s = %q contains \"devnet\"", name, v)
		}
		if strings.Contains(v, ":26657") && strings.HasPrefix(v, "http://") {
			t.Errorf("%s = %q is a plain-http :26657 node URL", name, v)
		}
	}
	// No default anywhere is a bare IP literal (the deleted
	// defaultWalletNodeURL was "http://54.179.101.3:26657").
	if strings.Contains(node, "54.179.101.3") {
		t.Error("DefaultWalletNodes still contains the deleted devnet IP literal")
	}
}
