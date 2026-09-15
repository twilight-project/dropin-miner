//go:build windows

package main

import (
	"errors"
	"os"
	"syscall"
)

var (
	errSharingViolation = syscall.Errno(32)
	errLockViolation    = syscall.Errno(33)
)

// tryLockFile on Windows: an exclusive open (share mode 0) IS the lock,
// and a sharing violation means another flush holds it.
func tryLockFile(path string) (*os.File, bool, error) {
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, false, err
	}
	h, err := syscall.CreateFile(
		name,
		syscall.GENERIC_READ|syscall.GENERIC_WRITE,
		0,
		nil,
		syscall.OPEN_ALWAYS,
		syscall.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		if errors.Is(err, errSharingViolation) || errors.Is(err, errLockViolation) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return os.NewFile(uintptr(h), path), true, nil
}

// tryFlushLock is tryLockFile with the flush's read-only fallback (see
// flushlock.go): an access-denied read-write open retries as a share-mode-0
// read handle on the existing file, which is the same exclusive lock.
func tryFlushLock(path string) (*os.File, bool, flushLockMode, error) {
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, false, flushLockReadWrite, err
	}
	mode := flushLockReadWrite
	h, err := syscall.CreateFile(name, syscall.GENERIC_READ|syscall.GENERIC_WRITE, 0, nil,
		syscall.OPEN_ALWAYS, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if errors.Is(err, syscall.ERROR_ACCESS_DENIED) {
		mode = flushLockReadOnly
		h, err = syscall.CreateFile(name, syscall.GENERIC_READ, 0, nil,
			syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0)
		if errors.Is(err, syscall.ERROR_FILE_NOT_FOUND) {
			return nil, false, mode, errFlushLockAbsent
		}
	}
	if err != nil {
		if errors.Is(err, errSharingViolation) || errors.Is(err, errLockViolation) {
			return nil, false, mode, nil
		}
		return nil, false, mode, err
	}
	return os.NewFile(uintptr(h), path), true, mode, nil
}

func unlockFile(f *os.File) error { return f.Close() }
