package main

// The flush lock is one file, <miner root>/flush.lock, for every binary that
// can run a flush — 0.2.9 and later, inside or outside an agent's sandbox.
// Two flushes over the same intake and spool must never overlap: a 0.2.9
// flush that overlaps another re-spools records it already read and damages
// them in code no later binary can patch, so the lock is the only safety and
// it cannot be split across paths or generations.
//
// A sandbox that denies writes to the miner root (Codex's workspace-write
// block never grants it) denies a read-write open of flush.lock too. The
// flush then opens the existing file read-only and takes the same exclusive
// lock: flock(LOCK_EX) on a read-only descriptor on POSIX, and on Windows a
// share-mode-0 read handle, which conflicts with a share-mode-0 read-write
// handle in both directions. Only a permission denial falls back; any other
// open error is a failure. A read-only open cannot create the file, so an
// absent, uncreatable lock is its own error: that flush does not run. It
// hides no overlap, because every flush that can write — 0.2.9's included —
// creates the file before it promotes anything.
//
// Only the flush uses the fallback. Setup, connect, the lifecycle gate and
// the destructive exclusion keep tryLockFile: they run outside any sandbox,
// and an uninstall or upgrade that cannot open a lock read-write must refuse
// rather than proceed on a lock over a file it could not remove.

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// flushLockMode says how a flush lock was opened.
type flushLockMode int

const (
	flushLockReadWrite flushLockMode = iota
	flushLockReadOnly
)

func (m flushLockMode) String() string {
	if m == flushLockReadOnly {
		return "read-only"
	}
	return "read-write"
}

// flushOpenOutcome is what one failed open of the flush lock means, decided
// per platform by classifyFlushLockOpenError from the error alone.
type flushOpenOutcome int

const (
	flushOpenFailed   flushOpenOutcome = iota // stop the flush with the error
	flushOpenFallBack                         // permission denied read-write: retry read-only
	flushOpenBusy                             // another flush holds the lock
	flushOpenAbsent                           // the read-only retry found no file
)

// errFlushLockAbsent is a lock file that does not exist and cannot be
// created by this process.
var errFlushLockAbsent = errors.New("the flush lock does not exist and this process cannot create it")

// ensureFlushLockFile creates the flush lock file when it is absent and its
// directory exists, without opening an existing one: a held lock is never
// touched. It reports whether it created the file.
func ensureFlushLockFile(path string) (bool, error) {
	if path == "" {
		return false, nil
	}
	if _, err := os.Lstat(path); err == nil {
		return false, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	dir, err := os.Stat(filepath.Dir(path))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !dir.IsDir() {
		return false, fmt.Errorf("%s is not a directory", filepath.Dir(path))
	}
	f, err := os.OpenFile(path, os.O_RDONLY|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 -- the configured flush lock
	if errors.Is(err, fs.ErrExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, f.Close()
}
