//go:build !windows

package main

// The shell profile block, held to the same ASCII rule as every other file
// this client writes into (generated_config_ascii_test.go). Its own build
// tag rather than a runtime skip, because profileEnvLines does not exist on
// Windows — setup_env_unix.go owns it, and the Windows equivalent is the
// user environment, which is registry values rather than a file a
// participant opens.

import (
	"path/filepath"
	"testing"
)

func TestTheGeneratedShellProfileBlockIsASCII(t *testing.T) {
	home := filepath.Join(t.TempDir(), "tokendrop")
	lines := profileEnvLines(
		filepath.Join(home, "bin"),
		filepath.Join(home, setupConfigFile),
		filepath.Join(home, "wallet"),
	)
	asciiOrFail(t, "profileBlock", []byte(profileBlock(lines)))
	// Without a wallet directory the block is one line shorter; both shapes
	// are what setup writes, so both are held to the rule.
	asciiOrFail(t, "profileBlock (no wallet)", []byte(profileBlock(
		profileEnvLines(filepath.Join(home, "bin"), filepath.Join(home, setupConfigFile), ""))))
}
