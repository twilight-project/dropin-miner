//go:build !windows

package main

import (
	"os"
	"sync"
	"testing"
)

// sandboxEmulationUnavailable says why this process cannot emulate a sandbox
// that denies writes to the miner root, or "" when it can.
func sandboxEmulationUnavailable() string {
	if os.Geteuid() == 0 {
		return "running as root: file modes do not deny root, so a write-denied miner root cannot be emulated"
	}
	return ""
}

// denyWritesKeepReads is the POSIX emulation of Codex's write-denying sandbox:
// dir 0500, each file 0400, everything still readable. restore puts back 0700
// and 0600 and is safe to call more than once; it also runs at cleanup.
func denyWritesKeepReads(t *testing.T, dir string, files ...string) (restore func()) {
	t.Helper()
	for _, f := range files {
		if err := os.Chmod(f, 0o400); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(dir, 0o500); err != nil { // #nosec G302 -- deliberately unwritable, to force a real permission denial
		t.Fatal(err)
	}
	var once sync.Once
	restore = func() {
		once.Do(func() {
			_ = os.Chmod(dir, 0o700) // #nosec G302 -- restore so t.TempDir cleanup can remove it
			for _, f := range files {
				_ = os.Chmod(f, 0o600)
			}
		})
	}
	t.Cleanup(restore)
	return restore
}
