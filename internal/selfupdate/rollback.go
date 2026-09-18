package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
)

// Rolled is a completed rollback.
type Rolled struct {
	From       Version
	To         Version
	Executable string
}

// Rollback restores the one-level .previous beside executable, with no
// network: .previous is read, bounded, into a fresh copy staged beside the
// executable; that copy — never .previous where it sits — runs and must
// report a release version other than the current one; then the ordinary
// replacement transaction installs it, and the binary it displaces becomes
// the new .previous. A second rollback therefore swaps back. Rollback
// imposes no direction: .previous may be newer after an earlier rollback.
func (u Updater) Rollback(ctx context.Context, executable, currentBuild string) (Rolled, error) {
	current, err := ParseVersion(currentBuild)
	if err != nil || currentBuild != current.String() {
		return Rolled{}, failure(KindNotRelease, fmt.Errorf("this binary reports %q, not a release version", currentBuild))
	}
	resolved, err := ResolveExecutable(executable)
	if err != nil {
		return Rolled{}, failure(KindStaging, err)
	}
	previous := PreviousPath(resolved)
	binary, err := readPrevious(previous)
	if err != nil {
		return Rolled{}, err
	}
	candidate, err := StageCandidate(resolved, binary)
	if err != nil {
		return Rolled{}, failure(KindStaging, err)
	}
	discard := func() { _ = os.Remove(candidate) } // #nosec G703 -- the staged copy this function just created
	target, err := candidateVersion(ctx, u.Runner, candidate, u.budget())
	if err != nil {
		discard()
		return Rolled{}, candidateFailure(KindPreviousInvalid, fmt.Errorf("%s does not run as a release: %w", previous, err))
	}
	if target.Compare(current) == 0 {
		discard()
		return Rolled{}, failure(KindPreviousInvalid, fmt.Errorf("%s is also %s; a rollback would change nothing", previous, current))
	}
	if err := replaceWith(ctx, u.onWindows(), resolved, candidate, target, u.replaceOps()); err != nil {
		// A recovery that failed may have moved a binary back out to the
		// staging name and reported it; that copy is kept.
		if !errorPreservesPath(err, candidate) {
			discard()
		}
		return Rolled{}, err
	}
	return Rolled{From: current, To: target, Executable: resolved}, nil
}

// readPrevious reads .previous whole, requiring a regular file within the
// executable bound.
func readPrevious(path string) ([]byte, error) {
	info, err := os.Lstat(path) // #nosec G703 -- the rollback slot beside the resolved executable
	if errors.Is(err, fs.ErrNotExist) {
		return nil, failure(KindNoPrevious, fmt.Errorf("there is no previous binary at %s to roll back to", path))
	}
	if err != nil {
		return nil, failure(KindPreviousInvalid, err)
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > MaxExecutableBytes {
		return nil, failure(KindPreviousInvalid, fmt.Errorf("%s is not a regular file of 1..%d bytes", path, MaxExecutableBytes))
	}
	f, err := os.Open(path) // #nosec G304 G703 -- the rollback slot beside the resolved executable
	if err != nil {
		return nil, failure(KindPreviousInvalid, err)
	}
	defer f.Close()
	data, err := readBounded(f, MaxExecutableBytes)
	if err == nil && int64(len(data)) != info.Size() {
		err = io.ErrUnexpectedEOF
	}
	if err != nil {
		return nil, failure(KindPreviousInvalid, fmt.Errorf("read %s: %w", path, err))
	}
	return data, nil
}
