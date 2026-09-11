package fsx

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

const publicationSyncsDirectory = true

func publish(temp, final string, exclusive bool) error {
	from, err := windows.UTF16PtrFromString(temp)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(final)
	if err != nil {
		return err
	}
	flags := uint32(windows.MOVEFILE_WRITE_THROUGH)
	if !exclusive {
		flags |= windows.MOVEFILE_REPLACE_EXISTING
	}
	err = windows.MoveFileEx(from, to, flags)
	if exclusive && (errors.Is(err, windows.ERROR_ALREADY_EXISTS) || errors.Is(err, windows.ERROR_FILE_EXISTS)) {
		return fs.ErrExist
	}
	return err
}

// MoveFileEx WRITE_THROUGH is the publication durability mechanism on
// Windows. Directory fsync is unavailable, not an ignored arbitrary error.
func syncDirectory(string) error { return ErrDirectorySyncUnsupported }

func movePublication(from, to string) error { return publish(from, to, true) }

func removeDurable(path string) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".tmp-removed-*")
	if err != nil {
		return &StageError{Stage: "remove stage", Err: err}
	}
	name := f.Name()
	defer func() { _ = os.Remove(name) }()
	if err := f.Close(); err != nil {
		return &StageError{Stage: "remove close", Err: err}
	}
	if err := publish(path, name, false); err != nil {
		return &StageError{Stage: "remove publication", Err: err}
	}
	return nil
}
