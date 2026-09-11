// Package fsx publishes complete files with explicit durability semantics.
package fsx

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// ErrDirectorySyncUnsupported identifies a platform without directory fsync.
// Windows publication instead uses MOVEFILE_WRITE_THROUGH. Other directory
// sync failures are never converted into successful publication.
var ErrDirectorySyncUnsupported = errors.New("directory sync unsupported")

// StageError reports where a durable operation failed. Published means that
// the final pathname may already contain the complete new bytes.
type StageError struct {
	Stage     string
	Published bool
	Err       error
}

func (e *StageError) Error() string { return fmt.Sprintf("fsx: %s: %v", e.Stage, e.Err) }
func (e *StageError) Unwrap() error { return e.Err }

type stagedFile interface {
	Name() string
	Chmod(fs.FileMode) error
	Write([]byte) (int, error)
	Sync() error
	Close() error
}
type operations struct {
	create  func(string, string) (stagedFile, error)
	publish func(string, string, bool) error
	syncDir func(string) error
	remove  func(string) error
}

func defaultOperations() operations {
	return operations{
		create:  func(dir, pattern string) (stagedFile, error) { return os.CreateTemp(dir, pattern) },
		publish: publish, syncDir: syncDirectory, remove: os.Remove,
	}
}

// WriteFileAtomic durably replaces name with complete data.
func WriteFileAtomic(dir, name string, data []byte, mode fs.FileMode) error {
	return writeFile(dir, name, data, mode, false, defaultOperations())
}

// WriteFileExclusive publishes name only if absent; an existing winner is
// preserved and the error matches fs.ErrExist. There is no check-then-rename.
func WriteFileExclusive(dir, name string, data []byte, mode fs.FileMode) error {
	return writeFile(dir, name, data, mode, true, defaultOperations())
}

func writeFile(dir, name string, data []byte, mode fs.FileMode, exclusive bool, ops operations) error {
	if name == "." || name == "" || filepath.Base(name) != name {
		return &StageError{Stage: "name", Err: fs.ErrInvalid}
	}
	f, err := ops.create(dir, ".tmp-*")
	if err != nil {
		return &StageError{Stage: "create", Err: err}
	}
	defer func() { _ = ops.remove(f.Name()) }()
	closed := false
	defer func() {
		if !closed {
			_ = f.Close()
		}
	}()
	if err := f.Chmod(mode); err != nil {
		return &StageError{Stage: "chmod", Err: err}
	}
	n, err := f.Write(data)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	if err != nil {
		return &StageError{Stage: "write", Err: err}
	}
	if err := f.Sync(); err != nil {
		return &StageError{Stage: "file sync", Err: err}
	}
	err = f.Close()
	closed = true
	if err != nil {
		return &StageError{Stage: "close", Err: err}
	}
	if err := ops.publish(f.Name(), filepath.Join(dir, name), exclusive); err != nil {
		return &StageError{Stage: "publication", Err: err}
	}
	if err := ops.syncDir(dir); err != nil {
		// Only the platform backend's explicit unavailability is allowed.
		// Windows has already completed write-through publication.
		if publicationSyncsDirectory && errors.Is(err, ErrDirectorySyncUnsupported) {
			return nil
		}
		return &StageError{Stage: "directory sync", Published: true, Err: err}
	}
	return nil
}

// SyncDirectory makes a directory mutation durable. On Windows callers get
// ErrDirectorySyncUnsupported rather than a fabricated fsync success.
func SyncDirectory(dir string) error { return syncDirectory(dir) }
