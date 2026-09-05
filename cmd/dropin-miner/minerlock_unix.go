//go:build !windows

package main

import (
	"errors"
	"os"
	"syscall"
)

// tryLockFile makes one non-blocking attempt to hold path exclusively.
// A lock held elsewhere is (nil, false, nil): the caller decides whether
// that means "wait" or "someone else is already doing this". Mirrors the
// refresh-token lock in internal/auth, which is the same mechanism for
// the same reason — a flock belongs to the open description, so two
// goroutines contend exactly as two processes do.
func tryLockFile(path string) (*os.File, bool, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600) // #nosec G304 -- our own state dir
	if err != nil {
		return nil, false, err
	}
	switch err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); {
	case err == nil:
		return f, true, nil
	case errors.Is(err, syscall.EWOULDBLOCK):
		_ = f.Close()
		return nil, false, nil
	default:
		_ = f.Close()
		return nil, false, err
	}
}

func unlockFile(f *os.File) error {
	unlockErr := syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	closeErr := f.Close()
	if unlockErr != nil {
		return unlockErr
	}
	return closeErr
}
