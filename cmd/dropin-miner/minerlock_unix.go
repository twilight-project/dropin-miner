//go:build !windows

package main

import (
	"errors"
	"io/fs"
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

// tryFlushLock is tryLockFile with the flush's read-only fallback (see
// flushlock.go): a permission-denied read-write open retries read-only on the
// existing file and takes the same exclusive flock.
func tryFlushLock(path string) (*os.File, bool, flushLockMode, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600) // #nosec G304 -- the configured flush lock
	mode := flushLockReadWrite
	if err != nil {
		if !errors.Is(err, syscall.EACCES) && !errors.Is(err, syscall.EPERM) {
			return nil, false, mode, err
		}
		mode = flushLockReadOnly
		f, err = os.Open(path) // #nosec G304 -- the configured flush lock
		if errors.Is(err, fs.ErrNotExist) {
			return nil, false, mode, errFlushLockAbsent
		}
		if err != nil {
			return nil, false, mode, err
		}
	}
	switch err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); {
	case err == nil:
		return f, true, mode, nil
	case errors.Is(err, syscall.EWOULDBLOCK):
		_ = f.Close()
		return nil, false, mode, nil
	default:
		_ = f.Close()
		return nil, false, mode, err
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
