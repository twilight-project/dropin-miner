package main

// The agent-command helpers. Every test injects getenv — a developer's
// real TOKENDROP_LISTEN or TOKENDROP_CONFIG must never leak into a test,
// because env beats the config file in config.Load's precedence.

import (
	"os"
	"path/filepath"
	"testing"
)

// noEnv is the getenv of a hermetic shell.
func noEnv(string) string { return "" }

// envOf builds a getenv from a literal map.
func envOf(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// writeListenConfig writes a minimal config naming addr as the listener
// and returns its path.
func writeListenConfig(t *testing.T, addr string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "tokendrop.toml")
	if err := os.WriteFile(p, []byte("[proxy]\nlisten = \""+addr+"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// The env-only half of key resolution; the credentials file's place in the
// order is covered in credentials_test.go.
func TestAPIKeyPrefersTheTokendropKeyOverTheOpenAIKey(t *testing.T) {
	m := emptyMiner(t)
	got, src, err := resolveAPIKey(envOf(map[string]string{ // #nosec G101 -- env-var names and synthetic canaries, not credentials
		"TOKENDROP_API_KEY": "canary-tenant-key",
		"OPENAI_API_KEY":    "canary-personal-key",
	}), m)
	if err != nil || got != "canary-tenant-key" || src != keyFromEnv {
		t.Errorf("a personal OpenAI key must not shadow the tenant key: got %q from %q (%v)", got, src, err)
	}
	if k, src, _ := resolveAPIKey(envOf(map[string]string{"OPENAI_API_KEY": "canary-personal-key"}), m); k != "canary-personal-key" || src != keyFromOpenAI {
		t.Errorf("fallback: %q from %q", k, src)
	}
	if k, src, err := resolveAPIKey(noEnv, m); k != "" || src != keyFromNone || err != nil {
		t.Errorf("no key set must mean no key: %q %q %v", k, src, err)
	}
}
