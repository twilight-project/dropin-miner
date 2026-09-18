//go:build !windows

package selfupdate

import (
	"errors"
	"io/fs"
	"os"
)

// platformRenameReplace is rename(2): atomic, replacing to.
func platformRenameReplace(from, to string) error { return os.Rename(from, to) } // #nosec G703 -- the installed binary and names this package created beside it

// platformRenameNew refuses an existing to. Production POSIX replacement never
// uses it; the Windows sequence's tests do.
func platformRenameNew(from, to string) error {
	if _, err := os.Lstat(to); err == nil { // #nosec G703 -- a name this package created beside the installed binary
		return fs.ErrExist
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return os.Rename(from, to) // #nosec G703 -- the installed binary and names this package created beside it
}

// fileInUse: a POSIX rename is never refused because a process runs the
// target.
func fileInUse(error) bool { return false }

// transientlyHeld: never. The move-aside belongs to the Windows sequence; a
// POSIX rename is not refused because another process has the file open, so
// there is no transient holder to wait for and nothing here is ever retried.
func transientlyHeld(error) bool { return false }
