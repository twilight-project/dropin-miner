//go:build darwin || freebsd || linux || netbsd || openbsd

package auth

// flock(2) is the Unix half of the refresh lock. The build list is
// explicit rather than this repository's usual `!windows` because
// syscall.Flock is absent on aix, solaris and dragonfly; naming the
// platforms that have it keeps the failure at compile time on a port
// nobody has done rather than at runtime on a lock that silently is not
// one. The release matrix is darwin, linux and windows (`make cross`),
// and android and ios satisfy the linux and darwin constraints.

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// tryLockRefreshFile makes one non-blocking attempt at an exclusive
// flock. A lock held elsewhere is reported as (nil, false, nil) — not an
// error, because the caller's whole job is to keep trying until its
// deadline.
//
// The lockfile is opened afresh on every attempt on purpose. A flock
// belongs to the open file description rather than to the process, so
// two goroutines in one process that each open the file contend exactly
// as two processes do. That is what makes one mechanism enough: there is
// no second, in-process lock that could disagree with this one.
func tryLockRefreshFile(path string) (*os.File, bool, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600) // #nosec G304 G703 -- store-dir + fixed name
	if err != nil {
		return nil, false, fmt.Errorf("auth: open %s: %w", refreshLockFile, err)
	}
	switch err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); {
	case err == nil:
		return f, true, nil
	case errors.Is(err, syscall.EWOULDBLOCK):
		_ = f.Close()
		return nil, false, nil
	default:
		_ = f.Close()
		return nil, false, fmt.Errorf("auth: lock %s: %w", refreshLockFile, err)
	}
}

// unlockRefreshFile releases the lock and closes the descriptor. Closing
// alone would release it; unlocking first means a close that fails still
// leaves the lock demonstrably dropped rather than held until the
// descriptor is collected.
func unlockRefreshFile(f *os.File) error {
	unlockErr := syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	closeErr := f.Close()
	if unlockErr != nil {
		return fmt.Errorf("auth: unlock %s: %w", refreshLockFile, unlockErr)
	}
	if closeErr != nil {
		return fmt.Errorf("auth: close %s: %w", refreshLockFile, closeErr)
	}
	return nil
}
