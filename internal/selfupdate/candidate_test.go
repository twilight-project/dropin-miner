package selfupdate

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

type runnerFunc func(context.Context, string, []string, []string) ([]byte, []byte, error)

func (f runnerFunc) Run(ctx context.Context, path string, args, env []string) ([]byte, []byte, error) {
	return f(ctx, path, args, env)
}

func outputs(stdout, stderr string) CommandRunner {
	return runnerFunc(func(context.Context, string, []string, []string) ([]byte, []byte, error) {
		return []byte(stdout), []byte(stderr), nil
	})
}

func TestValidateCandidateRunsOnlyVersionWithAScrubbedEnvironment(t *testing.T) {
	t.Setenv("TOKENDROP_API_KEY", "sr-secret")
	t.Setenv("HTTPS_PROXY", "http://proxy.invalid")
	v, _ := ParseVersion("0.3.0")
	runner := runnerFunc(func(_ context.Context, path string, args, env []string) ([]byte, []byte, error) {
		if path != "/candidate" || len(args) != 1 || args[0] != "version" {
			t.Fatalf("unexpected invocation %q %v", path, args)
		}
		for _, item := range env {
			if strings.HasPrefix(item, "TOKENDROP_") || strings.Contains(item, "PROXY") || strings.HasPrefix(item, "HOME=") {
				t.Errorf("the participant's environment reached the candidate: %q", item)
			}
		}
		return []byte("dropin-miner 0.3.0\n"), nil, nil
	})
	if err := ValidateCandidate(context.Background(), runner, "/candidate", v); err != nil {
		t.Fatal(err)
	}
}

func TestValidateCandidateAcceptsOnlyTheExactOutputContract(t *testing.T) {
	v, _ := ParseVersion("0.3.0")
	for _, out := range []string{
		"dropin-miner v0.3.0\n", "dropin-miner 0.3.1\n", "dropin-miner 0.3.0", "dropin-miner 0.3.0\r\n",
		"extra\ndropin-miner 0.3.0\n", "dropin-miner 0.3.0\n\n", "dropin-miner  0.3.0\n",
		"dropin-miner dev (abc)\n", "Dropin-miner 0.3.0\n", "dropin-miner 00.3.0\n", "",
	} {
		if err := ValidateCandidate(context.Background(), outputs(out, ""), "/candidate", v); err == nil {
			t.Errorf("accepted candidate output %q", out)
		}
	}
	if err := ValidateCandidate(context.Background(), outputs("dropin-miner 0.3.0\n", "warning\n"), "/candidate", v); err == nil ||
		!strings.Contains(err.Error(), "stderr") {
		t.Errorf("a candidate writing to stderr must be refused: %v", err)
	}
}

func TestValidateCandidateReportsExecutionFailureAndTimeout(t *testing.T) {
	v, _ := ParseVersion("0.3.0")
	err := ValidateCandidate(context.Background(), runnerFunc(func(context.Context, string, []string, []string) ([]byte, []byte, error) {
		return nil, nil, errors.New("exec format error")
	}), "/candidate", v)
	if err == nil || !strings.Contains(err.Error(), "exec format error") {
		t.Errorf("execution failure: %v", err)
	}
	var limit time.Duration
	parent, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	short, cancelShort := context.WithTimeout(parent, 20*time.Millisecond)
	defer cancelShort()
	_ = ValidateCandidate(parent, runnerFunc(func(ctx context.Context, _ string, _ []string, _ []string) ([]byte, []byte, error) {
		dl, _ := ctx.Deadline()
		limit = time.Until(dl)
		return []byte("dropin-miner 0.3.0\n"), nil, nil
	}), "/candidate", v)
	if limit <= 0 || limit > CandidateTimeout {
		t.Errorf("the candidate runs under its own %s deadline even inside a longer operation, got %s", CandidateTimeout, limit)
	}
	err = ValidateCandidate(short, runnerFunc(func(ctx context.Context, _ string, _ []string, _ []string) ([]byte, []byte, error) {
		<-ctx.Done()
		return nil, nil, ctx.Err()
	}), "/candidate", v)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Errorf("a candidate that never returns must time out: %v", err)
	}
}

func TestResolveExecutableAndStageBesideTheResolvedFile(t *testing.T) {
	real := filepath.Join(t.TempDir(), "install", "bin", "dropin-miner")
	if err := os.MkdirAll(filepath.Dir(real), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(real, []byte("installed"), 0o750); err != nil { // #nosec G306 -- an executable fixture
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "dropin-miner")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	resolved, err := ResolveExecutable(link)
	if err != nil {
		t.Fatal(err)
	}
	wantReal, _ := filepath.EvalSymlinks(real)
	if resolved != wantReal {
		t.Fatalf("ResolveExecutable(%s) = %s, want the link's target %s", link, resolved, wantReal)
	}
	candidate, err := StageCandidate(resolved, []byte("candidate bytes"))
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(candidate)
	if filepath.Dir(candidate) != filepath.Dir(wantReal) {
		t.Errorf("the candidate was staged in %s, not beside the resolved binary in %s", filepath.Dir(candidate), filepath.Dir(wantReal))
	}
	if b, _ := os.ReadFile(candidate); !bytes.Equal(b, []byte("candidate bytes")) { // #nosec G304 -- test path
		t.Error("the staged candidate does not hold the given bytes")
	}
	if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Error("staging must not touch the launcher link")
	}
	if b, _ := os.ReadFile(real); string(b) != "installed" { // #nosec G304 -- test path
		t.Error("staging must not touch the installed binary")
	}
	if runtime.GOOS != "windows" {
		if info, _ := os.Stat(candidate); info.Mode().Perm() != 0o750 {
			t.Errorf("the candidate's mode is %v, want the installed binary's 0750", info.Mode().Perm())
		}
	}
	if _, err := ResolveExecutable(filepath.Dir(real)); err == nil {
		t.Error("a directory is not an executable")
	}
}

func TestStageCandidateFailsWhereItCannotWrite(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("directory write permission is not enforced here")
	}
	dir := filepath.Join(t.TempDir(), "readonly")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(dir, "dropin-miner")
	if err := os.WriteFile(exe, []byte("x"), 0o700); err != nil { // #nosec G306 -- fixture
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil { // #nosec G302 -- deliberately unwritable fixture
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(dir, 0o700) }() // #nosec G302 -- restore for cleanup
	if _, err := StageCandidate(exe, []byte("candidate")); err == nil {
		t.Error("staging into an unwritable directory must fail by trying, not succeed")
	}
}

func TestValidationEnvironmentIsFixed(t *testing.T) {
	env := validationEnvironment([]string{"HOME=/home/me", "TOKENDROP_CONFIG=/x", "SystemRoot=C:\\Windows", "PATH=/bin"})
	joined := strings.Join(env, "\n")
	if !strings.Contains(joined, "LANG=C") || strings.Contains(joined, "HOME=") || strings.Contains(joined, "TOKENDROP_") || strings.Contains(joined, "PATH=/bin") {
		t.Errorf("validation environment %v", env)
	}
	if (runtime.GOOS == "windows") != strings.Contains(joined, "SystemRoot=") {
		t.Errorf("SystemRoot is kept on Windows only: %v", env)
	}
}
