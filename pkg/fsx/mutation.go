package fsx

import (
	"errors"
	"path/filepath"
)

// MoveFileDurable atomically moves a complete file. Published on an error
// means the destination is visible but its durability is uncertain.
func MoveFileDurable(from, to string) error {
	return moveFile(from, to, movePublication, syncDirectory)
}

func moveFile(from, to string, move func(string, string) error, syncDir func(string) error) error {
	if err := move(from, to); err != nil {
		return &StageError{Stage: "move", Err: err}
	}
	return confirmMove(from, to, syncDir)
}

func confirmMove(from, to string, syncDir func(string) error) error {
	for _, dir := range []string{filepath.Dir(to), filepath.Dir(from)} {
		if err := syncDir(dir); err != nil {
			if publicationSyncsDirectory && errors.Is(err, ErrDirectorySyncUnsupported) {
				continue
			}
			return &StageError{Stage: "move directory sync", Published: true, Err: err}
		}
	}
	return nil
}

// ConfirmMove retries the metadata barrier after a previously visible move.
// Windows moves completed their write-through barrier before returning.
func ConfirmMove(from, to string) error { return confirmMove(from, to, syncDirectory) }

// RemoveFileDurable removes a file from the active namespace. On Windows a
// write-through move to an ignored staging name durably removes the original
// name; deleting that inactive staging file is best-effort cleanup.
func RemoveFileDurable(path string) error { return removeDurable(path) }
