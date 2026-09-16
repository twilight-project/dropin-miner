package main

// Every test in this package runs with the participant's own directories out
// of reach. pkg/config defaults mining.state_dir to os.UserConfigDir(), the
// wallet defaults beside it, and agents install writes under the home
// directory: a test that leaves one of those out of its config would
// otherwise read or write the real machine's installation. That happened
// once, from a setup test whose config named no state_dir; the per-test
// sandbox fixed that test, and this makes it true of the whole package.
//
// The guard is a refusal, not a best effort: if the user config directory or
// the home directory does not resolve under the test root after the
// redirect, no test runs.
//
// The same test run also gets a network fence (internal/networkfence): no
// test in this package may reach a non-loopback host. See that package for
// the mechanism; client_network_fence_test.go is this package's own set of
// guard tests proving every client it builds is actually covered, and
// proxy_fence_subprocess_test.go proves the fence still holds when the
// process inherits an already-poisoned proxy environment — a real process
// re-exec, the same acceptanceHelper idiom internal/selfupdate's own
// TestMain uses, because that class of bug can only be reproduced in a
// fresh process whose own TestMain has not run networkfence.Install yet.

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/twilight-project/dropin-miner/internal/networkfence"
)

func TestMain(m *testing.M) {
	if len(os.Args) >= 2 && os.Args[1] == proxyFenceHelperArg {
		os.Exit(runProxyFenceHelper())
	}
	os.Exit(runWithUserDirsUnderTestRoot(m))
}

func runWithUserDirsUnderTestRoot(m *testing.M) int {
	// Go's own caches and settings default under the directories redirected
	// below; pin them where they already are, so a test that runs `go build`
	// neither starts from a cold cache nor loses the developer's GOENV.
	pin := func(key, value string) {
		if os.Getenv(key) == "" && value != "" {
			_ = os.Setenv(key, value)
		}
	}
	if cache, err := os.UserCacheDir(); err == nil {
		pin("GOCACHE", filepath.Join(cache, "go-build"))
	}
	if cfg, err := os.UserConfigDir(); err == nil {
		pin("GOENV", filepath.Join(cfg, "go", "env"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		pin("GOPATH", filepath.Join(home, "go"))
	}
	if gopath := filepath.SplitList(os.Getenv("GOPATH")); len(gopath) > 0 {
		pin("GOMODCACHE", filepath.Join(gopath[0], "pkg", "mod"))
	}

	root, err := os.MkdirTemp("", "dropin-miner-test-root-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "TestMain: create the test root:", err)
		return 2
	}
	defer func() { _ = os.RemoveAll(root) }()
	user := filepath.Join(root, "user")
	for key, value := range map[string]string{
		"HOME":            user,
		"USERPROFILE":     user,
		"XDG_CONFIG_HOME": filepath.Join(user, ".config"),
		"AppData":         filepath.Join(user, "AppData", "Roaming"),
		"LOCALAPPDATA":    filepath.Join(user, "AppData", "Local"),
	} {
		if err := os.MkdirAll(value, 0o700); err != nil {
			fmt.Fprintln(os.Stderr, "TestMain:", err)
			return 2
		}
		_ = os.Setenv(key, value)
	}

	cfg, cfgErr := os.UserConfigDir()
	home, homeErr := os.UserHomeDir()
	if cfgErr != nil || homeErr != nil || !within(cfg, root) || !within(home, root) {
		fmt.Fprintf(os.Stderr, "TestMain: refusing to run: the user config directory (%q, %v) and home (%q, %v) must resolve under the test root %q\n",
			cfg, cfgErr, home, homeErr, root)
		return 2
	}

	return networkfence.Guard(m)
}
