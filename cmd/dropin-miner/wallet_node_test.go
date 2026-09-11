package main

// §4.5: walletNode's resolution order, and the installers' literals
// matching the Go defaults table.

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/twilight-project/dropin-miner/pkg/config"
)

// TestWalletNodeResolutionOrder: flag beats env beats table; a
// configured chain id with no table row refuses with the message above
// and the stub node records zero requests.
func TestWalletNodeResolutionOrder(t *testing.T) {
	t.Run("flag wins over everything", func(t *testing.T) {
		got, err := walletNode("https://flag.example", envOf(map[string]string{walletNodeEnv: "https://env.example"}), config.DefaultChainID)
		if err != nil || got != "https://flag.example" {
			t.Fatalf("got %q, %v", got, err)
		}
	})
	t.Run("env wins over the table", func(t *testing.T) {
		got, err := walletNode("", envOf(map[string]string{walletNodeEnv: "https://env.example"}), config.DefaultChainID)
		if err != nil || got != "https://env.example" {
			t.Fatalf("got %q, %v", got, err)
		}
	})
	t.Run("table when neither flag nor env is set", func(t *testing.T) {
		got, err := walletNode("", noEnv, config.DefaultChainID)
		if err != nil || got != config.DefaultWalletNodes[config.DefaultChainID] {
			t.Fatalf("got %q, %v, want the table row for %q", got, err, config.DefaultChainID)
		}
	})
	t.Run("unknown chain id refuses with no request ever sent", func(t *testing.T) {
		node := newFakeNode(t, nodeConfig{chainID: "irrelevant"})
		_, err := walletNode("", noEnv, "some-chain-nobody-configured")
		if err == nil {
			t.Fatal("expected a refusal")
		}
		if !strings.Contains(err.Error(), "no default RPC node is known for chain") {
			t.Errorf("error should name the refusal: %v", err)
		}
		node.mu.Lock()
		bc, tq := node.broadcastCount, node.txQueries
		node.mu.Unlock()
		if bc != 0 || tq != 0 {
			t.Fatalf("no request should ever reach the node: broadcast=%d tx=%d", bc, tq)
		}
	})
}

// TestInstallerDefaultsMatchGo: reads scripts/setup.sh and
// scripts/install.ps1 off disk and asserts their literal defaults for
// AS, router, chain, platform base, and agents API equal the Go value.
// A source-reading test — its injection check is a disk edit to a
// script, not an overlay.
func TestInstallerDefaultsMatchGo(t *testing.T) {
	root := moduleRoot(t)

	setupSh, err := os.ReadFile(filepath.Join(root, "scripts", "setup.sh")) // #nosec G304 -- fixed relative path under the module root this test resolves itself
	if err != nil {
		t.Fatal(err)
	}
	checkShellDefault(t, "scripts/setup.sh", string(setupSh), "TOKENDROP_CHAIN", config.DefaultChainID)
	checkShellDefault(t, "scripts/setup.sh", string(setupSh), "TOKENDROP_AS_URL", config.DefaultASBaseURL)
	checkShellDefault(t, "scripts/setup.sh", string(setupSh), "TOKENDROP_ROUTER_URL", config.DefaultRouterURL)
	checkShellDefault(t, "scripts/setup.sh", string(setupSh), "TOKENDROP_PLATFORM_URL", config.DefaultPlatformBaseURL)
	checkShellDefault(t, "scripts/setup.sh", string(setupSh), "TOKENDROP_AGENTS_API_URL", config.DefaultAgentsAPIURL)
	checkShellSlotDefault(t, "scripts/setup.sh", string(setupSh))

	installPs1, err := os.ReadFile(filepath.Join(root, "scripts", "install.ps1")) // #nosec G304 -- fixed relative path under the module root this test resolves itself
	if err != nil {
		t.Fatal(err)
	}
	checkPowerShellDefault(t, "scripts/install.ps1", string(installPs1), "TOKENDROP_CHAIN", "chain", config.DefaultChainID)
	checkPowerShellDefault(t, "scripts/install.ps1", string(installPs1), "TOKENDROP_AS_URL", "asUrl", config.DefaultASBaseURL)
	checkPowerShellDefault(t, "scripts/install.ps1", string(installPs1), "TOKENDROP_ROUTER_URL", "router", config.DefaultRouterURL)
	checkPowerShellDefault(t, "scripts/install.ps1", string(installPs1), "TOKENDROP_PLATFORM_URL", "platformUrl", config.DefaultPlatformBaseURL)
	checkPowerShellDefault(t, "scripts/install.ps1", string(installPs1), "TOKENDROP_AGENTS_API_URL", "agentsApiUrl", config.DefaultAgentsAPIURL)
	checkPowerShellSlotDefault(t, "scripts/install.ps1", string(installPs1))
}

// checkShellDefault finds `NAME="${ENV_VAR:-literal}"` and asserts
// literal == want.
func checkShellDefault(t *testing.T, file, content, envVar, want string) {
	t.Helper()
	re := regexp.MustCompile(regexp.QuoteMeta(envVar) + `:-([^}]+)\}`)
	m := re.FindStringSubmatch(content)
	if m == nil {
		t.Fatalf("%s: could not find a default for %s", file, envVar)
	}
	if m[1] != want {
		t.Errorf("%s: %s defaults to %q, Go has %q", file, envVar, m[1], want)
	}
}

func checkShellSlotDefault(t *testing.T, file, content string) {
	t.Helper()
	re := regexp.MustCompile(`TOKENDROP_SLOT:-(\d+)\}`)
	m := re.FindStringSubmatch(content)
	if m == nil {
		t.Fatalf("%s: could not find the slot default", file)
	}
	got, err := strconv.Atoi(m[1])
	if err != nil || got != config.DefaultSlotID {
		t.Errorf("%s: slot defaults to %q, Go has %d", file, m[1], config.DefaultSlotID)
	}
}

// checkPowerShellDefault finds
// `$var = if ($env:ENV_VAR) { ... } else { "literal" }`.
func checkPowerShellDefault(t *testing.T, file, content, envVar, psVar, want string) {
	t.Helper()
	re := regexp.MustCompile(`\$` + regexp.QuoteMeta(psVar) + `\s*=.*else\s*\{\s*"([^"]+)"`)
	m := re.FindStringSubmatch(content)
	if m == nil {
		t.Fatalf("%s: could not find a default for $%s (env %s)", file, psVar, envVar)
	}
	if m[1] != want {
		t.Errorf("%s: $%s defaults to %q, Go has %q", file, psVar, m[1], want)
	}
}

func checkPowerShellSlotDefault(t *testing.T, file, content string) {
	t.Helper()
	re := regexp.MustCompile(`\$slot\s*=.*else\s*\{\s*"(\d+)"`)
	m := re.FindStringSubmatch(content)
	if m == nil {
		t.Fatalf("%s: could not find the slot default", file)
	}
	got, err := strconv.Atoi(m[1])
	if err != nil || got != config.DefaultSlotID {
		t.Errorf("%s: slot defaults to %q, Go has %d", file, m[1], config.DefaultSlotID)
	}
}
