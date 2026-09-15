//go:build !windows

package main

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"syscall"
	"testing"
	"time"
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

// TestPOSIXFlushLockOpenErrorDecision enumerates the POSIX fallback decision:
// a read-write open denied with EACCES (file modes) or EPERM (Codex's macOS
// sandbox) retries read-only; nothing else does; on the read-only retry only
// a missing file is errFlushLockAbsent.
func TestPOSIXFlushLockOpenErrorDecision(t *testing.T) {
	pathErr := func(errno syscall.Errno) error {
		return &fs.PathError{Op: "open", Path: "flush.lock", Err: errno}
	}
	cases := []struct {
		name     string
		err      error
		readOnly bool
		want     flushOpenOutcome
	}{
		{"EACCES falls back", pathErr(syscall.EACCES), false, flushOpenFallBack},
		{"EPERM falls back", pathErr(syscall.EPERM), false, flushOpenFallBack},
		{"ENOENT does not fall back", pathErr(syscall.ENOENT), false, flushOpenFailed},
		{"EISDIR does not fall back", pathErr(syscall.EISDIR), false, flushOpenFailed},
		{"ELOOP does not fall back", pathErr(syscall.ELOOP), false, flushOpenFailed},
		{"a wrapped non-permission error does not fall back", fmt.Errorf("open flush.lock: %w", pathErr(syscall.EIO)), false, flushOpenFailed},
		{"an unwrapped non-errno error does not fall back", errors.New("permission denied"), false, flushOpenFailed},
		{"ENOENT on the read-only retry is absent", pathErr(syscall.ENOENT), true, flushOpenAbsent},
		{"EACCES on the read-only retry fails", pathErr(syscall.EACCES), true, flushOpenFailed},
		{"EPERM on the read-only retry fails", pathErr(syscall.EPERM), true, flushOpenFailed},
	}
	for _, c := range cases {
		if got := classifyFlushLockOpenError(c.err, c.readOnly); got != c.want {
			t.Errorf("%s: got %d, want %d", c.name, got, c.want)
		}
	}
}

// setUserImmutable sets `chflags uchg` on each path and clears it at cleanup,
// before the temp directory holding them is removed.
func setUserImmutable(t *testing.T, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if out, err := exec.Command("chflags", "uchg", p).CombinedOutput(); err != nil { // #nosec G204 -- fixed command, test-owned path
			skipPermissionTest(t, fmt.Sprintf("chflags uchg %s: %v %s", p, err, bytes.TrimSpace(out)))
			return
		}
		t.Cleanup(func() { _ = exec.Command("chflags", "nouchg", p).Run() }) // #nosec G204 -- fixed command, test-owned path
	}
}

// proveReadWriteOpenIsEPERM is the EPERM fixture's own self-proof: the lock
// refuses a read-write open with EPERM specifically — what Codex's macOS
// sandbox returns — and still takes LOCK_EX through a read-only descriptor.
func proveReadWriteOpenIsEPERM(lock string) error {
	if f, err := os.OpenFile(lock, os.O_RDWR|os.O_CREATE, 0o600); err == nil { // #nosec G304 G703 -- test-owned path
		_ = f.Close()
		return errors.New("a read-write open of the immutable lock succeeded")
	} else if !errors.Is(err, syscall.EPERM) {
		return fmt.Errorf("a read-write open of the immutable lock failed with %v, not EPERM", err)
	}
	f, err := os.Open(lock) // #nosec G304 G703 -- test-owned path
	if err != nil {
		return fmt.Errorf("a read-only open of the immutable lock failed: %w", err)
	}
	defer func() { _ = f.Close() }()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fmt.Errorf("LOCK_EX on a read-only descriptor of the immutable lock failed: %w", err)
	}
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}

// TestSandboxedFlushFallsBackOnEPERM is the sandbox emulation with the errno
// Codex's real macOS sandbox produces. The miner root is 0500, but the lock
// and the legacy stamp keep 0600 and are made immutable instead, so a
// read-write open of the lock fails with EPERM, never EACCES.
func TestSandboxedFlushFallsBackOnEPERM(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skipf("EPERM is emulated with macOS `chflags uchg`; %s is covered by the EACCES emulation and the decision table", runtime.GOOS)
	}
	requireSandboxEmulation(t)
	x := newFlushLockFixture(t)
	if _, err := ensureFlushLockFile(x.lock); err != nil {
		t.Fatal(err)
	}
	writeStampT(t, x.legacy, flushStamp{SlotID: testSlotID, TargetEpoch: 7, LastAS: time.Now().Add(-time.Hour)})
	legacyBefore, err := os.ReadFile(x.legacy)
	if err != nil {
		t.Fatal(err)
	}
	denyWritesKeepReads(t, x.root)
	setUserImmutable(t, x.lock, x.legacy)
	if err := proveWriteDeniedMinerRoot(x.root, x.lock, x.state); err != nil {
		t.Fatalf("the fixture does not emulate the sandbox: %v", err)
	}
	if err := proveReadWriteOpenIsEPERM(x.lock); err != nil {
		t.Fatalf("the fixture does not produce EPERM: %v", err)
	}

	events := recordFlushEvents(t)
	rep, code, out := runFlushT(t, x.cfg, x.cfgPath, false)
	if code != exitOK || !contains(events(), "locked read-only") {
		t.Fatalf("flush exit %d, events %v; want the lock taken read-only after EPERM\n%s", code, events(), out)
	}
	if credited, _, _ := x.ledger.totals(lockFixtureRequest); rep.Delivered != 1 || credited != 1 || x.intakeCount(t) != 0 {
		t.Fatalf("delivered %d, credited %d, intake %d; want 1, 1, 0\n%s", rep.Delivered, credited, x.intakeCount(t), out)
	}
	if st := readFlushStamp(x.stamp); st.TargetEpoch != 10 {
		t.Errorf("new stamp %+v, want target 10", st)
	}
	if after, err := os.ReadFile(x.legacy); err != nil || !bytes.Equal(after, legacyBefore) {
		t.Errorf("the legacy stamp changed (err %v)", err)
	}
}
