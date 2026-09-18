//go:build windows

package selfupdate

// #78 on the operating system it happens on. These tests do not inject an
// error: they make Windows produce it, by holding the installed binary open
// the way a scanner does — for reading, WITHOUT delete sharing — and then run
// the real operations (MoveFileEx, transientlyHeld, time.Sleep).
//
// They run only on Windows, and a skipped test reads as a pass, so each logs
// a line beginning "K6-WINDOWS" that a verbose run can count.

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// holdLikeAScanner opens path for reading and refuses delete sharing, which
// is what makes a rename of it fail with ERROR_SHARING_VIOLATION.
func holdLikeAScanner(t *testing.T, path string) windows.Handle {
	t.Helper()
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	h, err := windows.CreateFile(p, windows.GENERIC_READ, windows.FILE_SHARE_READ, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatalf("could not hold %s open: %v", path, err)
	}
	return h
}

// realOps are the production operations, observed: every move-aside error is
// recorded, and onPause runs before each real wait.
func realOps(x *txn, asideErrs *[]error, onPause func()) replaceOps {
	ops := defaultReplaceOps(markerRunner())
	rename := ops.renameNew
	ops.renameNew = func(from, to string) error {
		err := rename(from, to)
		if from == x.exe && strings.Contains(to, "displaced") {
			*asideErrs = append(*asideErrs, err)
		}
		return err
	}
	pause := ops.pause
	ops.pause = func(d time.Duration) {
		if onPause != nil {
			onPause()
		}
		pause(d)
	}
	return ops
}

func TestOnWindowsARealSharingViolationIsRetriedUntilTheHolderLetsGo(t *testing.T) {
	x := newTxn(t, "0.3.0", "0.2.9", "0.3.1")
	h := holdLikeAScanner(t, x.exe)
	released := false
	t.Cleanup(func() {
		if !released {
			_ = windows.CloseHandle(h)
		}
	})
	var asideErrs []error
	pauses := 0
	ops := realOps(x, &asideErrs, func() {
		pauses++
		if !released { // the scanner finishes during the first wait
			_ = windows.CloseHandle(h)
			released = true
		}
	})
	if err := replaceWith(context.Background(), true, x.exe, x.can, mustVersion(t, "0.3.1"), ops); err != nil {
		t.Fatalf("the upgrade failed although the holder let go: %v (move-aside errors: %v)", err, asideErrs)
	}
	if len(asideErrs) != 2 || !errors.Is(asideErrs[0], windows.ERROR_SHARING_VIOLATION) || asideErrs[1] != nil {
		t.Fatalf("move-aside errors %v; want one ERROR_SHARING_VIOLATION and then success — if the first is nil, the hold did not reproduce #78 and this test proved nothing", asideErrs)
	}
	if pauses != 1 {
		t.Errorf("%d pauses, want 1", pauses)
	}
	if x.version(x.exe) != "0.3.1" || x.version(x.prev) != "0.3.0" {
		t.Errorf("installed %s, previous %s", x.version(x.exe), x.version(x.prev))
	}
	t.Logf("K6-WINDOWS transient: ran; first move-aside error %v, then success after %d pause(s) and %d attempt(s)", asideErrs[0], pauses, len(asideErrs))
}

func TestOnWindowsAHolderThatNeverLetsGoFailsAfterAboutASecondWithNothingChanged(t *testing.T) {
	x := newTxn(t, "0.3.0", "0.2.9", "0.3.1")
	exeSum, prevSum := x.sum(x.exe), x.sum(x.prev)
	h := holdLikeAScanner(t, x.exe)
	var asideErrs []error
	start := time.Now()
	err := replaceWith(context.Background(), true, x.exe, x.can, mustVersion(t, "0.3.1"), realOps(x, &asideErrs, nil))
	elapsed := time.Since(start)
	_ = windows.CloseHandle(h)

	if KindOf(err) != KindReplacementFailed || !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
		t.Fatalf("got %v; want the existing replacement_failed carrying the sharing violation", err)
	}
	if len(asideErrs) != moveAsideAttempts {
		t.Errorf("%d attempts, want %d", len(asideErrs), moveAsideAttempts)
	}
	if elapsed < 900*time.Millisecond || elapsed > 5*time.Second {
		t.Errorf("gave up after %s; the bound is about a second", elapsed)
	}
	if x.sum(x.exe) != exeSum || x.sum(x.prev) != prevSum {
		t.Error("the installed binary or .previous changed")
	}
	for _, n := range x.names() {
		if strings.Contains(n, "displaced") {
			t.Errorf("a transaction file was left: %v", x.names())
		}
	}
	t.Logf("K6-WINDOWS held-for-good: ran; %d attempts in %s", len(asideErrs), elapsed)
}

func TestOnWindowsAMissingInstalledBinaryIsNotWaitedFor(t *testing.T) {
	x := newTxn(t, "0.3.0", "0.2.9", "0.3.1")
	if err := os.Remove(x.exe); err != nil {
		t.Fatal(err)
	}
	var asideErrs []error
	pauses := 0
	start := time.Now()
	err := replaceWith(context.Background(), true, x.exe, x.can, mustVersion(t, "0.3.1"), realOps(x, &asideErrs, func() { pauses++ }))
	elapsed := time.Since(start)

	if KindOf(err) != KindReplacementFailed || !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("got %v; want replacement_failed for a file that is not there", err)
	}
	if len(asideErrs) != 1 || pauses != 0 {
		t.Errorf("%d attempts and %d pauses for a missing file, want 1 and 0", len(asideErrs), pauses)
	}
	if elapsed > 900*time.Millisecond {
		t.Errorf("took %s: a missing file was waited for", elapsed)
	}
	t.Logf("K6-WINDOWS missing-file: ran; error %v, %d attempt(s), %d pause(s), %s", asideErrs[0], len(asideErrs), pauses, elapsed)
}

func TestOnWindowsOnlyAHeldFileIsTransient(t *testing.T) {
	for err, want := range map[error]bool{
		windows.ERROR_SHARING_VIOLATION: true,
		windows.ERROR_ACCESS_DENIED:     true,
		windows.ERROR_FILE_NOT_FOUND:    false,
		windows.ERROR_PATH_NOT_FOUND:    false,
		windows.ERROR_ALREADY_EXISTS:    false,
		windows.ERROR_FILE_EXISTS:       false,
		windows.ERROR_WRITE_PROTECT:     false,
		windows.ERROR_DISK_FULL:         false,
		windows.ERROR_NOT_SAME_DEVICE:   false,
		fs.ErrExist:                     false,
		fs.ErrNotExist:                  false,
	} {
		if got := transientlyHeld(err); got != want {
			t.Errorf("transientlyHeld(%v) = %v, want %v", err, got, want)
		}
	}
	if transientlyHeld(nil) {
		t.Error("no error is not a held file")
	}
	// fileInUse keeps reading the same two codes for the .previous step,
	// where they mean previous_in_use (#78's own requirement).
	if !fileInUse(windows.ERROR_SHARING_VIOLATION) || !fileInUse(windows.ERROR_ACCESS_DENIED) || fileInUse(windows.ERROR_FILE_NOT_FOUND) {
		t.Error("fileInUse changed")
	}
	t.Log("K6-WINDOWS classification: ran")
}
