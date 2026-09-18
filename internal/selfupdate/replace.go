package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/twilight-project/dropin-miner/pkg/fsx"
)

// PreviousPath is the one-level rollback slot beside executable. It is the
// binary the most recent upgrade or rollback displaced, kept locally; nothing
// authenticates it beyond the version check a rollback runs on a copy of it.
func PreviousPath(executable string) string { return executable + ".previous" }

// replaceOps are the transaction's hard failure points, injectable so every
// transition can be tested; the order and the recovery are not.
type replaceOps struct {
	snapshot      func(executable string) (string, error) // POSIX: a durable same-directory copy
	reserve       func(dir string) (string, error)        // Windows: a unique absent name
	renameReplace func(from, to string) error             // replaces an existing to
	renameNew     func(from, to string) error             // fails when to exists
	syncDir       func(dir string) error
	remove        func(path string) error
	validate      func(ctx context.Context, path string, want Version) error
	inUse         func(err error) bool // the target is an image a process still runs
	// transient reports a move-aside failure that a momentary holder of the
	// file explains (Windows only; never true elsewhere), and pause waits
	// between attempts. See moveAside.
	transient func(err error) bool
	pause     func(time.Duration)
}

// The move-aside is attempted at most moveAsideAttempts times, moveAsideDelay
// apart: five attempts and four waits, one second in all.
const (
	moveAsideAttempts = 5
	moveAsideDelay    = 250 * time.Millisecond
)

// moveAside renames the installed binary to its displaced name, which is the
// first change the Windows sequence makes.
//
// Seen once on main's Windows runner (#78): this rename failed with a sharing
// violation while nothing of ours held the file — the only child ran from
// .previous, and Windows lets a running image be renamed. What was left, by
// elimination, was a scanner or indexer reading a binary the step before had
// just put in place. A moment later the same rename succeeds, and a
// participant's desktop has the same scanners.
//
// So the rename is retried, for about a second, and ONLY for the errors
// Windows returns for a file something else is holding right now
// (transientlyHeld). Every other error is returned at once, on the first
// attempt: a missing file will not appear, and waiting for it would only
// delay the message. A directory this user cannot write never gets here —
// reserve has just created and removed a file in it. When the attempts run
// out the last error is returned and the caller reports exactly what it
// always did; nothing has been changed, so there is nothing to undo.
//
// An access-denied that is a real, permanent refusal (an ACL that forbids
// renaming this one file) costs that second and then fails as before. That is
// the price of not being able to tell the two apart from the code alone, and
// it is paid once, on a path that was going to fail anyway.
func moveAside(ctx context.Context, executable, displaced string, ops replaceOps) error {
	for attempt := 1; ; attempt++ {
		err := ops.renameNew(executable, displaced)
		if err == nil || attempt == moveAsideAttempts || !ops.transient(err) || ctx.Err() != nil {
			return err
		}
		ops.pause(moveAsideDelay)
	}
}

func defaultReplaceOps(runner CommandRunner) replaceOps {
	return replaceOpsWithin(runner, CandidateTimeout)
}

func replaceOpsWithin(runner CommandRunner, budget time.Duration) replaceOps {
	return replaceOps{
		snapshot:      durableSnapshot,
		reserve:       reservePath,
		renameReplace: platformRenameReplace,
		renameNew:     platformRenameNew,
		syncDir: func(dir string) error {
			if err := fsx.SyncDirectory(dir); err != nil && !errors.Is(err, fsx.ErrDirectorySyncUnsupported) {
				return err
			}
			return nil
		},
		remove: os.Remove,
		validate: func(ctx context.Context, path string, want Version) error {
			return validateCandidate(ctx, runner, path, want, budget)
		},
		inUse:     fileInUse,
		transient: transientlyHeld,
		pause:     time.Sleep,
	}
}

// Install replaces p.Executable with p.Candidate through the platform's
// transaction. Nothing is committed as .previous until the canonical path
// itself has run and reported p.To.
func (u Updater) Install(ctx context.Context, p Prepared) error {
	if p.NoChange || p.Candidate == "" {
		return failure(KindReplacementFailed, errors.New("nothing was prepared to install"))
	}
	return replaceWith(ctx, u.onWindows(), p.Executable, p.Candidate, p.To, u.replaceOps())
}

func (u Updater) onWindows() bool {
	if u.windows != nil {
		return *u.windows
	}
	return runtime.GOOS == "windows"
}

func (u Updater) replaceOps() replaceOps {
	if u.ops != nil {
		return *u.ops
	}
	return replaceOpsWithin(u.Runner, u.budget())
}

func replaceWith(ctx context.Context, windows bool, executable, candidate string, target Version, ops replaceOps) error {
	if filepath.Dir(executable) != filepath.Dir(candidate) {
		return failure(KindReplacementFailed, fmt.Errorf("the candidate %s is not beside %s", candidate, executable))
	}
	if windows {
		return replaceWindows(ctx, executable, candidate, target, ops)
	}
	return replacePOSIX(ctx, executable, candidate, target, ops)
}

// replacePOSIX: S is a durable copy of C; N renames over C, so the canonical
// name is never absent; the directory is synced; C runs and reports target;
// only then does S become .previous, replacing the old one. Any failure
// before that validation puts S back over C and leaves .previous untouched.
func replacePOSIX(ctx context.Context, executable, candidate string, target Version, ops replaceOps) error {
	dir, previous := filepath.Dir(executable), PreviousPath(executable)
	snapshot, err := ops.snapshot(executable)
	if err != nil {
		return failure(KindReplacementFailed, fmt.Errorf("keep a copy of the installed binary: %w", err))
	}
	if err := ops.renameReplace(candidate, executable); err != nil {
		_ = ops.remove(snapshot)
		return failure(KindReplacementFailed, fmt.Errorf("install the candidate: %w", err))
	}
	// From here the snapshot is the only copy of the old binary.
	if err := ops.syncDir(dir); err != nil {
		return restorePOSIX(dir, executable, snapshot, fmt.Errorf("sync the installation: %w", err), ops)
	}
	if err := ops.validate(ctx, executable, target); err != nil {
		return restorePOSIX(dir, executable, snapshot, fmt.Errorf("the installed binary did not validate: %w", err), ops)
	}
	if err := ops.renameReplace(snapshot, previous); err != nil {
		return &Error{Kind: KindIncomplete, Paths: []string{executable, snapshot, previous},
			Err: fmt.Errorf("%s is installed and validated, but the binary it replaced could not be kept as %s (%w); that binary is at %s, and %s is unchanged", target, previous, err, snapshot, previous)}
	}
	if err := ops.syncDir(dir); err != nil {
		return &Error{Kind: KindIncomplete, Paths: []string{executable, previous},
			Err: fmt.Errorf("%s is installed and the binary it replaced is %s, but the directory could not be synced (%w), so a crash now could lose either change", target, previous, err)}
	}
	return nil
}

func restorePOSIX(dir, executable, snapshot string, cause error, ops replaceOps) error {
	if err := ops.renameReplace(snapshot, executable); err != nil {
		return &Error{Kind: KindManualIntervention, Paths: []string{executable, snapshot},
			Err: fmt.Errorf("%v; putting the old binary back failed too (%w): the old binary is at %s and %s holds the unvalidated candidate", cause, err, snapshot, executable)}
	}
	if err := ops.syncDir(dir); err != nil {
		return &Error{Kind: KindIncomplete, Paths: []string{executable},
			Err: fmt.Errorf("%v; the old binary was put back at %s but the directory could not be synced (%w)", cause, executable, err)}
	}
	return failure(KindReplacementFailed, fmt.Errorf("%w; the old binary was put back and .previous is untouched", cause))
}

// replaceWindows is the sequence the Windows probe qualified on a running
// image: C moves aside to a unique D, N moves into C, C runs and reports
// target, then D replaces .previous. MoveFileEx with write-through is the
// durability; there is no directory sync. A failure at any step moves C back
// out (to N's name) where needed and D back into C, leaving .previous
// untouched. When D cannot replace .previous because a process still runs
// from it, the error is previous_in_use.
func replaceWindows(ctx context.Context, executable, candidate string, target Version, ops replaceOps) error {
	previous := PreviousPath(executable)
	displaced, err := ops.reserve(filepath.Dir(executable))
	if err != nil {
		return failure(KindReplacementFailed, fmt.Errorf("reserve a name for the installed binary: %w", err))
	}
	if err := moveAside(ctx, executable, displaced, ops); err != nil {
		return failure(KindReplacementFailed, fmt.Errorf("move the installed binary aside: %w", err))
	}
	if err := ops.renameNew(candidate, executable); err != nil {
		return restoreWindows(executable, displaced, "", fmt.Errorf("install the candidate: %w", err), KindReplacementFailed, ops)
	}
	if err := ops.validate(ctx, executable, target); err != nil {
		return restoreWindows(executable, displaced, candidate, fmt.Errorf("the installed binary did not validate: %w", err), KindReplacementFailed, ops)
	}
	if err := ops.renameReplace(displaced, previous); err != nil {
		if ops.inUse(err) {
			return restoreWindows(executable, displaced, candidate, fmt.Errorf("%s (%w)", PreviousInUseMessage, err), KindPreviousInUse, ops)
		}
		return restoreWindows(executable, displaced, candidate, fmt.Errorf("keep the installed binary as %s: %w", previous, err), KindReplacementFailed, ops)
	}
	return nil
}

// restoreWindows moves what sits at executable out to moveOutTo, when given,
// then the displaced binary back to executable.
func restoreWindows(executable, displaced, moveOutTo string, cause error, kind Kind, ops replaceOps) error {
	if moveOutTo != "" {
		if err := ops.renameNew(executable, moveOutTo); err != nil {
			return &Error{Kind: KindManualIntervention, Paths: []string{executable, displaced},
				Err: fmt.Errorf("%v; moving the new binary out of the way failed (%w): the old binary is at %s and %s holds the new one", cause, err, displaced, executable)}
		}
	}
	if err := ops.renameNew(displaced, executable); err != nil {
		paths := []string{displaced}
		if moveOutTo != "" {
			paths = append(paths, moveOutTo)
		}
		return &Error{Kind: KindManualIntervention, Paths: paths,
			Err: fmt.Errorf("%v; putting the old binary back failed (%w): %s is absent and the old binary is at %s", cause, err, executable, displaced)}
	}
	return &Error{Kind: kind, Err: fmt.Errorf("%w; the old binary was put back and .previous is untouched", cause)}
}

// durableSnapshot copies source to a fresh file in its directory, with its
// mode, and syncs it. A copy, never a link: it must survive the name it was
// copied from being replaced.
func durableSnapshot(source string) (string, error) {
	in, err := os.Open(source) // #nosec G304 G703 -- the resolved installed binary
	if err != nil {
		return "", err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > MaxExecutableBytes {
		return "", fmt.Errorf("the installed binary's size %d is outside 1..%d bytes", info.Size(), MaxExecutableBytes)
	}
	out, err := os.CreateTemp(filepath.Dir(source), ".dropin-miner.snapshot-*")
	if err != nil {
		return "", err
	}
	name := out.Name()
	ok := false
	defer func() {
		_ = out.Close()
		if !ok {
			_ = os.Remove(name) // #nosec G703 -- a snapshot this function created beside it
		}
	}()
	if err := out.Chmod(info.Mode().Perm()); err != nil && runtime.GOOS != "windows" {
		return "", err
	}
	n, err := io.Copy(out, io.LimitReader(in, MaxExecutableBytes+1))
	if err == nil && n != info.Size() {
		err = io.ErrUnexpectedEOF
	}
	if err != nil {
		return "", err
	}
	if err := out.Sync(); err != nil {
		return "", err
	}
	if err := out.Close(); err != nil {
		return "", err
	}
	if err := fsx.SyncDirectory(filepath.Dir(source)); err != nil && !errors.Is(err, fsx.ErrDirectorySyncUnsupported) {
		return "", err
	}
	ok = true
	return name, nil
}

// reservePath returns a unique name in dir that does not exist.
func reservePath(dir string) (string, error) {
	f, err := os.CreateTemp(dir, ".dropin-miner.displaced-*")
	if err != nil {
		return "", err
	}
	name := f.Name()
	if err := f.Close(); err != nil {
		return "", err
	}
	if err := os.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) { // #nosec G703 -- a name this function created and reserved beside it
		return "", err
	}
	return name, nil
}

// stagingPrefixes are the only names this package creates beside an
// executable: staged candidates, POSIX snapshots and Windows displaced
// binaries.
var stagingPrefixes = []string{".dropin-miner.candidate-", ".dropin-miner.snapshot-", ".dropin-miner.displaced-"}

// StagingLeftovers lists the regular files in dir that this package named and
// an interrupted operation may have left, and nothing else in dir.
func StagingLeftovers(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		for _, prefix := range stagingPrefixes {
			if strings.HasPrefix(e.Name(), prefix) && len(e.Name()) > len(prefix) && e.Type().IsRegular() {
				out = append(out, filepath.Join(dir, e.Name()))
				break
			}
		}
	}
	return out, nil
}

// MoveAside renames executable to a fresh displaced name in its directory
// with the same no-replace rename the Windows transaction uses, and returns
// that name. On Windows it is what can be done to a running binary: the probe
// showed a running image can be renamed but not deleted, so the file stays,
// under the returned name, until nothing runs it.
func MoveAside(executable string) (string, error) {
	aside, err := reservePath(filepath.Dir(executable))
	if err != nil {
		return "", err
	}
	if err := platformRenameNew(executable, aside); err != nil {
		return "", err
	}
	return aside, nil
}
