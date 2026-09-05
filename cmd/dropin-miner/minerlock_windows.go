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

func unlockFile(f *os.File) error { return f.Close() }
