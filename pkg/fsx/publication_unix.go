//go:build !windows

package fsx

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

const publicationSyncsDirectory = false

func publish(temp, final string, exclusive bool) error {
	if exclusive {
		// Linking the fully synced inode publishes without replacing a winner.
		// The staged link is cleaned by the caller; the final link is synced.
		return os.Link(temp, final)
	}
	return os.Rename(temp, final)
}

func syncDirectory(dir string) error {
	f, err := os.Open(dir) // #nosec G304 -- caller-owned publication directory.
	if err != nil {
		return err
	}
	err = f.Sync()
	closeErr := f.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func movePublication(from, to string) error { return os.Rename(from, to) }

func removeDurable(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return &StageError{Stage: "remove", Err: err}
	}
	if err := syncDirectory(filepath.Dir(path)); err != nil {
		return &StageError{Stage: "remove directory sync", Published: true, Err: err}
	}
	return nil
}
