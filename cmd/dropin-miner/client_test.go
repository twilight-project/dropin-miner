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

func TestAPIKeyPrefersTheTokendropKeyOverTheOpenAIKey(t *testing.T) {
	got := apiKey(envOf(map[string]string{ // #nosec G101 -- env-var names and synthetic canaries, not credentials
		"TOKENDROP_API_KEY": "canary-tenant-key",
		"OPENAI_API_KEY":    "canary-personal-key",
	}))
	if got != "canary-tenant-key" {
		t.Errorf("a personal OpenAI key must not shadow the tenant key: got %q", got)
	}
	if k := apiKey(envOf(map[string]string{"OPENAI_API_KEY": "canary-personal-key"})); k != "canary-personal-key" {
		t.Errorf("fallback: %q", k)
	}
	if k := apiKey(noEnv); k != "" {
		t.Errorf("no key set must mean no key: %q", k)
	}
}
