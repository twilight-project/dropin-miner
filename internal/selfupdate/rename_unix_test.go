//go:build !windows

package selfupdate

import (
	"io/fs"
	"syscall"
	"testing"
)

// POSIX behavior is untouched by #78, asserted: no error a rename can return
// here is a passing holder, so nothing is ever retried off Windows.
func TestNothingIsTransientlyHeldOffWindows(t *testing.T) {
	for _, err := range []error{nil, syscall.EACCES, syscall.EPERM, syscall.EBUSY, syscall.ETXTBSY, syscall.ENOENT, syscall.EROFS, fs.ErrPermission, fs.ErrNotExist} {
		if transientlyHeld(err) {
			t.Errorf("%v is classified as transient on a POSIX system", err)
		}
	}
}
