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
