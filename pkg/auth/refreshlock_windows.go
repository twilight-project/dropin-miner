//go:build windows

package auth

// The Windows half of the refresh lock.
//
// The obvious call is LockFileEx, and it is deliberately not used. Its
// lpOverlapped argument has to reach the kernel as a raw pointer, and
// producing one in Go needs the unsafe package — banned outright in this
// repository and enforced by the linter (AGENTS.md invariant 8). The
// standard syscall package does not wrap LockFileEx, and the wrapper in
// golang.org/x/sys/windows would be a new module dependency, which the
// dependency budget admits only under an accepted ADR.
//
// Opening the lockfile with dwShareMode = 0 buys the same property from
// the file system instead: while one handle is open, every other
// CreateFile on that path fails with ERROR_SHARING_VIOLATION — between
// processes and between handles inside a single process alike, which is
// the same symmetry flock gives on Unix. The kernel closes the handle
// when the holder exits, so a crashed holder cannot leave the lock
// stuck. The one behavior flock does not share is that this exclusion is
// mandatory rather than advisory, so an indexer or backup agent that
// opens the lockfile can cause a spurious refusal; the retry loop and
// the bounded wait absorb that, and nothing but this file ever opens the
// path.

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// Neither code is exported by the standard syscall package. The same gap
// is why notlistening_windows.go spells WSAECONNREFUSED out by number.
const (
	errorSharingViolation = syscall.Errno(32) // ERROR_SHARING_VIOLATION
	errorLockViolation    = syscall.Errno(33) // ERROR_LOCK_VIOLATION
)

// tryLockRefreshFile makes one non-blocking attempt to become the sole
// holder of the lockfile. A lock held elsewhere is reported as
// (nil, false, nil) — not an error, because the caller's whole job is to
// keep trying until its deadline.
func tryLockRefreshFile(path string) (*os.File, bool, error) {
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, false, fmt.Errorf("auth: %s path: %w", refreshLockFile, err)
	}
	h, err := syscall.CreateFile(
		name,
		syscall.GENERIC_READ|syscall.GENERIC_WRITE,
		0, // dwShareMode = 0 — this open IS the lock
		nil,
		syscall.OPEN_ALWAYS,
		syscall.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		if errors.Is(err, errorSharingViolation) || errors.Is(err, errorLockViolation) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("auth: open %s: %w", refreshLockFile, err)
	}
	return os.NewFile(uintptr(h), path), true, nil
}

// unlockRefreshFile releases the lock, which on this platform is exactly
// closing the handle.
func unlockRefreshFile(f *os.File) error {
	if err := f.Close(); err != nil {
		return fmt.Errorf("auth: close %s: %w", refreshLockFile, err)
	}
	return nil
}
