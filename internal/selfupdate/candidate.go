package selfupdate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/twilight-project/dropin-miner/pkg/fsx"
)

// ResolveExecutable is the file an upgrade acts on: path with every symlink
// resolved, so a launcher link is never mistaken for the binary, and it must
// be a regular file. A candidate is staged beside what this returns.
func ResolveExecutable(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", path, err)
	}
	if resolved, err = filepath.Abs(resolved); err != nil {
		return "", err
	}
	info, err := os.Lstat(resolved)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a regular file", resolved)
	}
	return resolved, nil
}

// StageCandidate writes binary beside executable (already resolved) under a
// fresh name, durably, with executable permissions, so a later rename stays
// on one filesystem. Whether the directory is writable is learned by trying:
// the temporary file's creation is the permission check. The name keeps the
// executable's extension, so a Windows candidate is still an .exe.
func StageCandidate(executable string, binary []byte) (string, error) {
	dir := filepath.Dir(executable)
	mode := os.FileMode(0o755)
	if info, err := os.Stat(executable); err == nil && info.Mode().Perm()&0o100 != 0 { // #nosec G703 -- the resolved installed binary
		mode = info.Mode().Perm()
	}
	f, err := os.CreateTemp(dir, ".dropin-miner.candidate-*"+filepath.Ext(executable))
	if err != nil {
		return "", fmt.Errorf("stage the candidate beside %s: %w", executable, err)
	}
	name := f.Name()
	keep := false
	defer func() {
		if !keep {
			_ = f.Close()
			_ = os.Remove(name) // #nosec G703 -- a temporary file this function created beside it
		}
	}()
	if err := f.Chmod(mode); err != nil && runtime.GOOS != "windows" {
		return "", fmt.Errorf("stage the candidate: %w", err)
	}
	if n, err := f.Write(binary); err != nil || n != len(binary) {
		if err == nil {
			err = errors.New("short write")
		}
		return "", fmt.Errorf("write the candidate: %w", err)
	}
	if err := f.Sync(); err != nil {
		return "", fmt.Errorf("sync the candidate: %w", err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("close the candidate: %w", err)
	}
	if err := fsx.SyncDirectory(dir); err != nil && !errors.Is(err, fsx.ErrDirectorySyncUnsupported) {
		return "", fmt.Errorf("sync %s: %w", dir, err)
	}
	keep = true
	return name, nil
}

// CommandRunner runs one command and returns its output.
type CommandRunner interface {
	Run(ctx context.Context, path string, args, env []string) (stdout, stderr []byte, err error)
}

// ExecRunner runs a real process.
type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, path string, args, env []string) ([]byte, []byte, error) {
	cmd := exec.CommandContext(ctx, path, args...) // #nosec G204 G702 -- a verified staged candidate, one fixed argument
	cmd.Env = env
	cmd.Stdin = nil
	// A child that leaves a grandchild holding its pipes must not outlive
	// the deadline by keeping Wait from returning.
	cmd.WaitDelay = time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}

// ErrCandidateTimeout marks a candidate that did not answer `version` within
// its budget. It is absence of evidence, and is kept apart from every other
// failure for that reason: see CandidateVersion.
var ErrCandidateTimeout = errors.New("the candidate's version command timed out")

// CandidateVersion runs `path version` under CandidateTimeout with a stripped
// environment and accepts only the release output contract: exactly
// "dropin-miner X.Y.Z\n" with X.Y.Z canonical, and nothing on stderr.
//
// A candidate that has not answered is not a bad candidate (#95). The check is
// the first execution of a freshly written binary, which is exactly when a
// real-time scanner inspects it, and under load a process that prints one
// line has missed five seconds twice on a development machine. A timeout is
// therefore retried ONCE. Anything the candidate actually said — a wrong
// version, a malformed line, a byte on stderr, a failure to start — is
// evidence, and is refused on the first call with no second one: a retry
// there could only turn a bad binary's second, luckier answer into an
// acceptance. The check is read-only, so asking twice changes nothing.
//
// The budget is not raised. CandidateTimeout is a frozen bound, and a larger
// number would be a guess at how slow a scan can be; one retry is bounded at
// twice the budget and says something a bigger number cannot — that the
// candidate was asked again and still did not answer. Two timeouts return an
// error that is ErrCandidateTimeout, which callers report as retryable.
func CandidateVersion(ctx context.Context, runner CommandRunner, path string) (Version, error) {
	return candidateVersion(ctx, runner, path, CandidateTimeout)
}

func candidateVersion(ctx context.Context, runner CommandRunner, path string, budget time.Duration) (Version, error) {
	v, err := candidateVersionOnce(ctx, runner, path, budget)
	// Only a timeout, and only while the operation itself still has time: a
	// parent deadline that has passed is the operation's end, not a slow
	// candidate, and a second attempt under it could not run at all.
	if !errors.Is(err, ErrCandidateTimeout) || ctx.Err() != nil {
		return v, err
	}
	if v, err = candidateVersionOnce(ctx, runner, path, budget); errors.Is(err, ErrCandidateTimeout) {
		return Version{}, fmt.Errorf("%w twice (limit %s each); it may be slow to start rather than broken, so try again", ErrCandidateTimeout, budget)
	}
	return v, err
}

func candidateVersionOnce(ctx context.Context, runner CommandRunner, path string, budget time.Duration) (Version, error) {
	if runner == nil {
		runner = ExecRunner{}
	}
	vctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	stdout, stderr, err := runner.Run(vctx, path, []string{"version"}, validationEnvironment(os.Environ()))
	if err != nil {
		if errors.Is(vctx.Err(), context.DeadlineExceeded) {
			return Version{}, fmt.Errorf("%w (limit %s)", ErrCandidateTimeout, budget)
		}
		return Version{}, fmt.Errorf("the candidate's version command failed: %w", err)
	}
	if len(stderr) != 0 {
		return Version{}, fmt.Errorf("the candidate's version command wrote to stderr: %q", stderr)
	}
	line := string(stdout)
	if !strings.HasPrefix(line, ProjectName+" ") || !strings.HasSuffix(line, "\n") || strings.Count(line, "\n") != 1 {
		return Version{}, fmt.Errorf("the candidate's version output %q is not exactly %q", stdout, ProjectName+" X.Y.Z\n")
	}
	raw := strings.TrimSuffix(strings.TrimPrefix(line, ProjectName+" "), "\n")
	v, err := ParseVersion(raw)
	if err != nil || raw != v.String() {
		return Version{}, fmt.Errorf("the candidate's version output %q is not a canonical release version", stdout)
	}
	return v, nil
}

// ValidateCandidate requires the candidate to report exactly expected.
func ValidateCandidate(ctx context.Context, runner CommandRunner, path string, expected Version) error {
	return validateCandidate(ctx, runner, path, expected, CandidateTimeout)
}

func validateCandidate(ctx context.Context, runner CommandRunner, path string, expected Version, budget time.Duration) error {
	got, err := candidateVersion(ctx, runner, path, budget)
	if err != nil {
		return err
	}
	if got.Compare(expected) != 0 {
		return fmt.Errorf("the candidate reports %s, want %s", got, expected)
	}
	return nil
}

// candidateFailure classifies a failed version check made BEFORE anything was
// replaced. A candidate that answered wrongly is kind; one that never answered
// is KindReplacementFailed, whose meaning is exactly this state — replacement
// stopped before it was committed, the installed binary and .previous are as
// they were, safe to retry — and which a participant is told to retry. The
// same check made after the swap already ends there, through the restore.
func candidateFailure(kind Kind, err error) error {
	if errors.Is(err, ErrCandidateTimeout) {
		return failure(KindReplacementFailed, err)
	}
	return failure(kind, err)
}

// validationEnvironment is what a candidate runs with: a fixed locale and,
// on Windows, only the variables a process needs to start. Nothing of the
// participant's — no TOKENDROP_*, no key, no proxy — reaches it.
func validationEnvironment(source []string) []string {
	out := []string{"LANG=C", "LC_ALL=C"}
	if runtime.GOOS != "windows" {
		return out
	}
	allowed := map[string]bool{"SYSTEMROOT": true, "WINDIR": true, "COMSPEC": true, "PATHEXT": true}
	for _, item := range source {
		if key, _, ok := strings.Cut(item, "="); ok && allowed[strings.ToUpper(key)] {
			out = append(out, item)
		}
	}
	return out
}
