//go:build windows

package selfupdate

import (
	"errors"
	"io/fs"

	"golang.org/x/sys/windows"
)

// platformRenameReplace is MoveFileEx with REPLACE_EXISTING and WRITE_THROUGH.
func platformRenameReplace(from, to string) error {
	return moveFileEx(from, to, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
}

// platformRenameNew is MoveFileEx with WRITE_THROUGH, refusing an existing to.
// The probe showed it moves the pathname of a running image.
func platformRenameNew(from, to string) error {
	err := moveFileEx(from, to, windows.MOVEFILE_WRITE_THROUGH)
	if errors.Is(err, windows.ERROR_ALREADY_EXISTS) || errors.Is(err, windows.ERROR_FILE_EXISTS) {
		return fs.ErrExist
	}
	return err
}

func moveFileEx(from, to string, flags uint32) error {
	f, err := windows.UTF16PtrFromString(from)
	if err != nil {
		return err
	}
	t, err := windows.UTF16PtrFromString(to)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(f, t, flags)
}

// fileInUse: replacing a file a process still runs fails with access denied
// (the probe's result for a mapped .previous) or a sharing violation.
func fileInUse(err error) bool {
	return errors.Is(err, windows.ERROR_ACCESS_DENIED) || errors.Is(err, windows.ERROR_SHARING_VIOLATION)
}

// transientlyHeld: the move-aside failed because something else holds the
// file at this moment — a sharing violation (a handle opened without delete
// sharing: a scanner, an indexer) or access denied (what Windows answers
// while such a holder's delete or scan is pending). These are the same two
// codes fileInUse reads, and they mean something different here: there the
// holder is a process running from .previous and will not let go, so the
// answer is previous_in_use; here nothing of ours holds the installed binary,
// so the holder is passing and the answer is to try again shortly. A missing
// file, a missing path, an existing target and a write-protected volume are
// none of them transient, and none is retried.
func transientlyHeld(err error) bool {
	return errors.Is(err, windows.ERROR_SHARING_VIOLATION) || errors.Is(err, windows.ERROR_ACCESS_DENIED)
}
