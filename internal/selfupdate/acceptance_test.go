package selfupdate

// The acceptance test for replacement and rollback with real processes: the
// binary being replaced is running, from the very pathname the transaction
// moves. The "binary" is this test executable, copied with a trailer naming
// the release version it reports. TestMain gives copies two roles: `version`
// prints the trailer's version exactly as a release does, and the helper
// argument runs one transaction from inside the copy. On Windows this is the
// qualification of the C→D, N→C, validate, D→P sequence on a running image,
// and of previous_in_use against a .previous that a live process runs.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/twilight-project/dropin-miner/internal/networkfence"
)

const (
	acceptanceHelper = "__selfupdate-acceptance-helper"
	trailerMagic     = "SELFUPDATE-TEST-VERSION:"
	trailerLen       = 48
)

func TestMain(m *testing.M) {
	if len(os.Args) == 2 && os.Args[1] == "version" {
		os.Exit(printTrailerVersion())
	}
	if len(os.Args) >= 2 && os.Args[1] == acceptanceHelper {
		os.Exit(runAcceptanceHelper(os.Args[2:]))
	}
	if os.Getenv("DROPIN_MINER_LIVE_RELEASE") == "1" {
		// TestLiveReleaseVerification (live_test.go) is the one test in this
		// package deliberately opted into reaching the real GitHub release
		// API; the fence would refuse it exactly as it would any other real
		// host, so this one env-gated path skips installing it.
		os.Exit(m.Run())
	}
	os.Exit(networkfence.Guard(m))
}

func trailerVersion(path string) (string, error) {
	f, err := os.Open(path) // #nosec G304 G703 -- this process's own image or a fixture
	if err != nil {
		return "", err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.Size() < trailerLen {
		return "", fmt.Errorf("no trailer in %s", path)
	}
	buf := make([]byte, trailerLen)
	if _, err := f.ReadAt(buf, info.Size()-trailerLen); err != nil {
		return "", err
	}
	s := strings.TrimRight(string(buf), " \n")
	if !strings.HasPrefix(s, trailerMagic) {
		return "", fmt.Errorf("no trailer in %s", path)
	}
	return strings.TrimPrefix(s, trailerMagic), nil
}

func printTrailerVersion() int {
	exe, err := os.Executable()
	if err == nil {
		var v string
		if v, err = trailerVersion(exe); err == nil {
			fmt.Printf("dropin-miner %s\n", v)
			return 0
		}
	}
	fmt.Fprintln(os.Stderr, err)
	return 3
}

type helperResult struct {
	Kind  Kind     `json:"kind"`
	Error string   `json:"error,omitempty"`
	Paths []string `json:"paths,omitempty"`
	Self  string   `json:"self"`
}

// runAcceptanceHelper runs inside a copy: replace <exe> <candidate> <version>
// [inject], rollback <exe> <current>, or sleep <seconds>.
func runAcceptanceHelper(args []string) int {
	if len(args) == 0 {
		return 2
	}
	self, _ := os.Executable()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	var err error
	switch args[0] {
	case "sleep":
		fmt.Println("ready")
		time.Sleep(time.Minute)
		return 0
	case "replace":
		exe, candidate := args[1], args[2]
		target, perr := ParseVersion(args[3])
		if perr != nil {
			return 2
		}
		ops := defaultReplaceOps(ExecRunner{})
		if len(args) > 4 {
			inject := args[4]
			realReplace, realNew, realValidate := ops.renameReplace, ops.renameNew, ops.validate
			fail := func(from, to string) bool { return inject == "fail-install" && from == candidate && to == exe }
			ops.renameReplace = func(from, to string) error {
				if fail(from, to) {
					return errInjected
				}
				return realReplace(from, to)
			}
			ops.renameNew = func(from, to string) error {
				if fail(from, to) {
					return errInjected
				}
				return realNew(from, to)
			}
			ops.validate = func(ctx context.Context, path string, want Version) error {
				if inject == "fail-validate" {
					if err := realValidate(ctx, path, want); err != nil {
						return err
					}
					return errInjected
				}
				return realValidate(ctx, path, want)
			}
		}
		err = replaceWith(ctx, runtime.GOOS == "windows", exe, candidate, target, ops)
	case "rollback":
		_, err = Updater{Runner: ExecRunner{}}.Rollback(ctx, args[1], args[2])
	default:
		return 2
	}
	res := helperResult{Kind: KindOf(err), Self: self}
	if err != nil {
		res.Error = err.Error()
		if typed, ok := err.(*Error); ok {
			res.Paths = typed.Paths
		}
	}
	_ = json.NewEncoder(os.Stdout).Encode(res)
	return 0
}

// ── the test ────────────────────────────────────────────────────────────

type acceptance struct {
	t        *testing.T
	self     []byte
	dir      string
	exe      string
	prev     string
	serial   int
	sleepers []*exec.Cmd
}

func exeName() string {
	if runtime.GOOS == "windows" {
		return "dropin-miner.exe"
	}
	return "dropin-miner"
}

func (a *acceptance) copyAs(path, version string) {
	a.t.Helper()
	tag := trailerMagic + version
	tag += strings.Repeat(" ", trailerLen-1-len(tag)) + "\n"
	if err := os.WriteFile(path, append(append([]byte(nil), a.self...), tag...), 0o700); err != nil { // #nosec G306 -- an executable fixture
		a.t.Fatal(err)
	}
}

func (a *acceptance) candidate(version string) string {
	a.serial++
	path := filepath.Join(a.dir, fmt.Sprintf(".dropin-miner.candidate-%d%s", a.serial, filepath.Ext(a.exe)))
	a.copyAs(path, version)
	return path
}

// reports is what the file at path says when it is run, through the
// updater's own validator.
func (a *acceptance) reports(path string) string {
	a.t.Helper()
	v, err := CandidateVersion(context.Background(), ExecRunner{}, path)
	if err != nil {
		return "<" + err.Error() + ">"
	}
	return v.String()
}

// holds is the version a file carries, read from its trailer without running
// it: executing .previous would map the very image the next commit replaces.
func (a *acceptance) holds(path string) string {
	v, err := trailerVersion(path)
	if err != nil {
		return "<" + err.Error() + ">"
	}
	return v
}

func (a *acceptance) sum(path string) string {
	b, err := os.ReadFile(path) // #nosec G304 -- fixture
	if err != nil {
		return "<absent>"
	}
	return fmt.Sprintf("%x", sha256.Sum256(b))
}

// helper runs one transaction inside the process started from a.exe — the
// running image is the canonical pathname itself.
func (a *acceptance) helper(args ...string) helperResult {
	a.t.Helper()
	cmd := exec.Command(a.exe, append([]string{acceptanceHelper}, args...)...) // #nosec G204 -- the fixture copy of this test binary
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		a.t.Fatalf("helper %v: %v\n%s", args, err, stderr.String())
	}
	var res helperResult
	if err := json.Unmarshal(stdout.Bytes(), &res); err != nil {
		a.t.Fatalf("helper %v output %q: %v", args, stdout.String(), err)
	}
	if !strings.EqualFold(filepath.Clean(res.Self), filepath.Clean(a.exe)) && res.Self != "" {
		if resolved, _ := filepath.EvalSymlinks(a.exe); !strings.EqualFold(res.Self, resolved) {
			a.t.Fatalf("the transaction ran from %s, not from the canonical path %s", res.Self, a.exe)
		}
	}
	return res
}

func (a *acceptance) noTransactionLeftovers(step string) {
	a.t.Helper()
	entries, _ := os.ReadDir(a.dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), "snapshot") || strings.Contains(e.Name(), "displaced") {
			a.t.Errorf("%s: transaction file left behind: %s", step, e.Name())
		}
	}
}

func TestReplacementAcceptanceWithTheRunningImage(t *testing.T) {
	if testing.Short() {
		t.Skip("runs real processes")
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	image, err := os.ReadFile(self) // #nosec G304 -- this test binary
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	a := &acceptance{t: t, self: image, dir: dir, exe: filepath.Join(dir, exeName())}
	a.prev = PreviousPath(a.exe)
	defer func() {
		for _, s := range a.sleepers {
			_ = s.Process.Kill()
			_ = s.Wait()
		}
	}()
	a.copyAs(a.exe, "0.3.0")
	a.copyAs(a.prev, "0.2.9")

	// 1. First upgrade, over an existing .previous, from the running 0.3.0.
	if res := a.helper("replace", a.exe, a.candidate("0.3.1"), "0.3.1"); res.Kind != "" {
		t.Fatalf("first upgrade: %s %s", res.Kind, res.Error)
	}
	if a.reports(a.exe) != "0.3.1" || a.holds(a.prev) != "0.3.0" {
		t.Fatalf("first upgrade: canonical %s, previous %s", a.reports(a.exe), a.holds(a.prev))
	}
	a.noTransactionLeftovers("first upgrade")

	// 2. Another upgrade, from the running 0.3.1.
	if res := a.helper("replace", a.exe, a.candidate("0.3.2"), "0.3.2"); res.Kind != "" {
		t.Fatalf("second upgrade: %s %s", res.Kind, res.Error)
	}
	if a.reports(a.exe) != "0.3.2" || a.holds(a.prev) != "0.3.1" {
		t.Fatalf("second upgrade: canonical %s, previous %s", a.reports(a.exe), a.holds(a.prev))
	}

	// 3. A failed third upgrade — its canonical validation fails after the
	// candidate is installed — restores the running binary and leaves the
	// existing .previous byte-identical.
	exeBefore, prevBefore := a.sum(a.exe), a.sum(a.prev)
	cand := a.candidate("0.3.3")
	res := a.helper("replace", a.exe, cand, "0.3.3", "fail-validate")
	if res.Kind != KindReplacementFailed || a.sum(a.exe) != exeBefore || a.sum(a.prev) != prevBefore {
		t.Fatalf("failed upgrade: kind %q (%s), canonical %s, previous %s", res.Kind, res.Error, a.reports(a.exe), a.holds(a.prev))
	}
	_ = os.Remove(cand)
	a.noTransactionLeftovers("failed upgrade")

	// 4. An injected failure of the move into the canonical name restores it.
	cand = a.candidate("0.3.3")
	res = a.helper("replace", a.exe, cand, "0.3.3", "fail-install")
	if res.Kind != KindReplacementFailed || a.sum(a.exe) != exeBefore || a.sum(a.prev) != prevBefore {
		t.Fatalf("failed install: kind %q (%s), canonical %s, previous %s", res.Kind, res.Error, a.reports(a.exe), a.holds(a.prev))
	}
	_ = os.Remove(cand)
	a.noTransactionLeftovers("failed install")

	// 5. Rollback from the running 0.3.2, then back again.
	if res := a.helper("rollback", a.exe, "0.3.2"); res.Kind != "" {
		t.Fatalf("rollback: %s %s", res.Kind, res.Error)
	}
	if a.reports(a.exe) != "0.3.1" || a.holds(a.prev) != "0.3.2" {
		t.Fatalf("rollback: canonical %s, previous %s", a.reports(a.exe), a.holds(a.prev))
	}
	if res := a.helper("rollback", a.exe, "0.3.1"); res.Kind != "" {
		t.Fatalf("second rollback: %s %s", res.Kind, res.Error)
	}
	if a.reports(a.exe) != "0.3.2" || a.holds(a.prev) != "0.3.1" {
		t.Fatalf("second rollback: canonical %s, previous %s", a.reports(a.exe), a.holds(a.prev))
	}
	a.noTransactionLeftovers("rollback")

	// 6. Windows: a process still runs from .previous, so the committing move
	// cannot replace it: previous_in_use, the running binary restored,
	// .previous untouched, the candidate back at its staging name.
	if runtime.GOOS != "windows" {
		return
	}
	sleeper := exec.Command(a.prev, acceptanceHelper, "sleep") // #nosec G204 -- the fixture .previous
	out, err := sleeper.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := sleeper.Start(); err != nil {
		t.Fatalf("start a process from .previous: %v", err)
	}
	a.sleepers = append(a.sleepers, sleeper)
	line := make([]byte, 6)
	if _, err := io.ReadFull(out, line); err != nil || !strings.HasPrefix(string(line), "ready") {
		t.Fatalf("the process running from .previous did not start: %q %v", line, err)
	}
	exeBefore, prevBefore = a.sum(a.exe), a.sum(a.prev)
	cand = a.candidate("0.3.3")
	candBefore := a.sum(cand)
	res = a.helper("replace", a.exe, cand, "0.3.3")
	if res.Kind != KindPreviousInUse || !strings.Contains(res.Error, PreviousInUseMessage) {
		t.Fatalf("mapped .previous: want previous_in_use, got %q: %s", res.Kind, res.Error)
	}
	if a.sum(a.exe) != exeBefore || a.sum(a.prev) != prevBefore || a.sum(cand) != candBefore {
		t.Errorf("previous_in_use: canonical %s, previous %s, candidate %s; want the running binary restored, .previous untouched and the candidate back",
			a.reports(a.exe), a.sum(a.prev), a.sum(cand))
	}
	a.noTransactionLeftovers("previous_in_use")
}
