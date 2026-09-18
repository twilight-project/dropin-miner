package selfupdate

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// A replacement fixture: files whose content is the version they "report".
type txn struct {
	t                   *testing.T
	dir, exe, prev, can string
}

func newTxn(t *testing.T, current, previous, candidate string) *txn {
	t.Helper()
	// Resolved, as Rollback resolves the executable: step injections compare
	// paths, and a temp directory behind a symlink (macOS /var) would
	// otherwise never match.
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	x := &txn{t: t, dir: dir, exe: filepath.Join(dir, "dropin-miner"), can: filepath.Join(dir, ".dropin-miner.candidate-1")}
	x.prev = PreviousPath(x.exe)
	x.write(x.exe, current)
	if previous != "" {
		x.write(x.prev, previous)
	}
	if candidate != "" {
		x.write(x.can, candidate)
	}
	return x
}

func (x *txn) write(path, version string) {
	x.t.Helper()
	if err := os.WriteFile(path, []byte(version+"\n"), 0o700); err != nil { // #nosec G306 -- an executable fixture
		x.t.Fatal(err)
	}
}

func (x *txn) version(path string) string {
	x.t.Helper()
	b, err := os.ReadFile(path) // #nosec G304 G703 -- fixture path
	if err != nil {
		return "<" + err.Error() + ">"
	}
	return strings.TrimSpace(string(b))
}

func (x *txn) sum(path string) [32]byte {
	b, _ := os.ReadFile(path) // #nosec G304 -- fixture path
	return sha256.Sum256(b)
}

func (x *txn) names() []string {
	entries, _ := os.ReadDir(x.dir)
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out
}

func mustVersion(t *testing.T, raw string) Version {
	t.Helper()
	v, err := ParseVersion(raw)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

var errInjected = errors.New("injected failure")
var errInjectedInUse = errors.New("injected: the file is in use")

// testOps are the real file operations with a marker validation (content is
// the version) and a failure injected at each named step (comma-separated). Steps: snapshot,
// install, sync-installed, validate, restore, commit, sync-committed, aside,
// move-out.
func (x *txn) ops(injectSteps string, injectErr error) replaceOps {
	if injectErr == nil {
		injectErr = errInjected
	}
	steps := map[string]bool{}
	for _, s := range strings.Split(injectSteps, ",") {
		steps[s] = true
	}
	inject := func(step string) bool { return steps[step] }
	syncs := 0
	var snapshot, displaced string
	return replaceOps{
		// No error is transient and no pause is real unless a test says so:
		// every transaction test below sees exactly one move-aside attempt.
		transient: func(error) bool { return false },
		pause:     func(time.Duration) {},
		snapshot: func(exe string) (string, error) {
			if inject("snapshot") {
				return "", injectErr
			}
			s, err := durableSnapshot(exe)
			snapshot = s
			return s, err
		},
		reserve: func(dir string) (string, error) {
			d, err := reservePath(dir)
			displaced = d
			return d, err
		},
		renameReplace: func(from, to string) error {
			switch {
			case inject("install") && from == x.can && to == x.exe,
				inject("restore") && from == snapshot && to == x.exe,
				inject("commit") && to == x.prev:
				return injectErr
			}
			return os.Rename(from, to) // #nosec G703 -- a fixture in the test's own directory
		},
		renameNew: func(from, to string) error {
			switch {
			case inject("aside") && from == x.exe,
				inject("install") && from == x.can && to == x.exe,
				inject("move-out") && from == x.exe && to == x.can,
				inject("restore") && from == displaced && to == x.exe:
				return injectErr
			}
			return platformRenameNew(from, to)
		},
		syncDir: func(string) error {
			syncs++
			if (inject("sync-installed") && syncs == 1) || (inject("sync-committed") && syncs == 2) {
				return injectErr
			}
			return nil
		},
		remove: os.Remove,
		validate: func(_ context.Context, path string, want Version) error {
			if inject("validate") {
				return injectErr
			}
			if got := x.version(path); got != want.String() {
				return fmt.Errorf("reports %s, want %s", got, want)
			}
			return nil
		},
		inUse: func(err error) bool { return errors.Is(err, errInjectedInUse) },
	}
}

func strategies() map[string]bool { return map[string]bool{"posix": false, "windows": true} }

func TestReplacementInstallsValidatesThenCommitsPrevious(t *testing.T) {
	for name, windows := range strategies() {
		x := newTxn(t, "0.3.0", "0.2.9", "0.3.1")
		if err := replaceWith(context.Background(), windows, x.exe, x.can, mustVersion(t, "0.3.1"), x.ops("", nil)); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if x.version(x.exe) != "0.3.1" || x.version(x.prev) != "0.3.0" {
			t.Errorf("%s: installed %s, previous %s; want 0.3.1 and the displaced 0.3.0", name, x.version(x.exe), x.version(x.prev))
		}
		if names := x.names(); len(names) != 2 {
			t.Errorf("%s: a clean replacement leaves only the binary and .previous: %v", name, names)
		}
		// A second upgrade rotates the one-level slot.
		x.write(x.can, "0.3.2")
		if err := replaceWith(context.Background(), windows, x.exe, x.can, mustVersion(t, "0.3.2"), x.ops("", nil)); err != nil {
			t.Fatalf("%s second: %v", name, err)
		}
		if x.version(x.exe) != "0.3.2" || x.version(x.prev) != "0.3.1" {
			t.Errorf("%s: second upgrade: installed %s, previous %s", name, x.version(x.exe), x.version(x.prev))
		}
	}
}

// Every failure before the canonical path validates leaves the installed
// binary and the existing .previous exactly as they were.
func TestReplacementFailuresBeforeValidationChangeNothing(t *testing.T) {
	cases := map[string][]string{
		"posix":   {"snapshot", "install", "sync-installed", "validate"},
		"windows": {"aside", "install", "validate"},
	}
	for name, windows := range strategies() {
		for _, step := range cases[name] {
			x := newTxn(t, "0.3.0", "0.2.9", "0.3.1")
			exeSum, prevSum := x.sum(x.exe), x.sum(x.prev)
			err := replaceWith(context.Background(), windows, x.exe, x.can, mustVersion(t, "0.3.1"), x.ops(step, nil))
			if KindOf(err) != KindReplacementFailed || !KindReplacementFailed.Retryable() {
				t.Errorf("%s/%s: want replacement_failed, got %v", name, step, err)
			}
			if x.sum(x.exe) != exeSum || x.sum(x.prev) != prevSum {
				t.Errorf("%s/%s: the installed binary is %s and .previous %s; both must be byte-identical to before", name, step, x.version(x.exe), x.version(x.prev))
			}
			for _, n := range x.names() {
				if strings.Contains(n, "snapshot") || strings.Contains(n, "displaced") {
					t.Errorf("%s/%s: a recovered failure leaves no transaction file: %v", name, step, x.names())
				}
			}
		}
	}
}

// §11.4: an existing valid .previous survives a second upgrade that fails
// before its canonical path validates, byte for byte.
func TestAFailedSecondUpgradeLeavesPreviousByteIdentical(t *testing.T) {
	for name, windows := range strategies() {
		x := newTxn(t, "0.3.0", "0.2.9", "0.3.1")
		if err := replaceWith(context.Background(), windows, x.exe, x.can, mustVersion(t, "0.3.1"), x.ops("", nil)); err != nil {
			t.Fatal(err)
		}
		exeSum, prevSum := x.sum(x.exe), x.sum(x.prev)
		// The second candidate installs, runs, and reports the wrong version.
		x.write(x.can, "0.3.9")
		err := replaceWith(context.Background(), windows, x.exe, x.can, mustVersion(t, "0.3.2"), x.ops("", nil))
		if KindOf(err) != KindReplacementFailed {
			t.Errorf("%s: want replacement_failed, got %v", name, err)
		}
		if x.sum(x.prev) != prevSum || x.version(x.prev) != "0.3.0" {
			t.Errorf("%s: .previous changed to %s; a failed second upgrade must leave it byte-identical", name, x.version(x.prev))
		}
		if x.sum(x.exe) != exeSum {
			t.Errorf("%s: the installed binary is %s; want the original 0.3.1 restored", name, x.version(x.exe))
		}
	}
}

func TestReplacementRestorationFailureIsManualAndNamesEveryCopy(t *testing.T) {
	for name, windows := range strategies() {
		x := newTxn(t, "0.3.0", "0.2.9", "0.3.9")
		prevSum := x.sum(x.prev)
		// The candidate does not validate, and putting the old binary back fails.
		var typed *Error
		err := replaceWith(context.Background(), windows, x.exe, x.can, mustVersion(t, "0.3.1"), x.ops("restore", nil))
		if KindOf(err) != KindManualIntervention || !errors.As(err, &typed) || len(typed.Paths) == 0 {
			t.Fatalf("%s: want manual_intervention with paths, got %v", name, err)
		}
		found := false
		for _, p := range typed.Paths {
			if x.version(p) == "0.3.0" {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: the paths %v must name where the old binary survives", name, typed.Paths)
		}
		if x.sum(x.prev) != prevSum {
			t.Errorf("%s: .previous must be untouched even when recovery fails", name)
		}
		if KindManualIntervention.Retryable() {
			t.Error("manual intervention is not retryable")
		}
	}
}

func TestPOSIXFailuresAfterValidationAreIncompleteNotSuccess(t *testing.T) {
	x := newTxn(t, "0.3.0", "0.2.9", "0.3.1")
	var typed *Error
	err := replaceWith(context.Background(), false, x.exe, x.can, mustVersion(t, "0.3.1"), x.ops("commit", nil))
	if KindOf(err) != KindIncomplete || !errors.As(err, &typed) {
		t.Fatalf("commit failure: want incomplete, got %v", err)
	}
	if x.version(x.exe) != "0.3.1" || x.version(x.prev) != "0.2.9" {
		t.Errorf("commit failure: installed %s, previous %s; the validated install stays and .previous is unchanged", x.version(x.exe), x.version(x.prev))
	}
	snapshotNamed := false
	for _, p := range typed.Paths {
		snapshotNamed = snapshotNamed || x.version(p) == "0.3.0"
	}
	if !snapshotNamed {
		t.Errorf("commit failure: the displaced binary's path must be reported: %v", typed.Paths)
	}

	x = newTxn(t, "0.3.0", "0.2.9", "0.3.1")
	err = replaceWith(context.Background(), false, x.exe, x.can, mustVersion(t, "0.3.1"), x.ops("sync-committed", nil))
	if KindOf(err) != KindIncomplete || x.version(x.exe) != "0.3.1" || x.version(x.prev) != "0.3.0" {
		t.Errorf("final sync failure: want incomplete with both renames visible, got %v (%s, %s)", err, x.version(x.exe), x.version(x.prev))
	}
}

func TestWindowsPreviousInUseRestoresTheDisplacedBinary(t *testing.T) {
	x := newTxn(t, "0.3.0", "0.2.9", "0.3.1")
	exeSum, prevSum, canSum := x.sum(x.exe), x.sum(x.prev), x.sum(x.can)
	err := replaceWith(context.Background(), true, x.exe, x.can, mustVersion(t, "0.3.1"), x.ops("commit", errInjectedInUse))
	if KindOf(err) != KindPreviousInUse || !KindPreviousInUse.Retryable() || !strings.Contains(err.Error(), PreviousInUseMessage) {
		t.Fatalf("want previous_in_use carrying its message, got %v", err)
	}
	if x.sum(x.exe) != exeSum || x.sum(x.prev) != prevSum || x.sum(x.can) != canSum {
		t.Errorf("previous_in_use: installed %s, previous %s, candidate %s; the displaced binary is restored, .previous untouched and the candidate back at its staging name",
			x.version(x.exe), x.version(x.prev), x.version(x.can))
	}
	if names := x.names(); len(names) != 3 {
		t.Errorf("previous_in_use leaves only the binary, .previous and the candidate: %v", names)
	}

	x = newTxn(t, "0.3.0", "0.2.9", "0.3.1")
	if err := replaceWith(context.Background(), true, x.exe, x.can, mustVersion(t, "0.3.1"), x.ops("commit", nil)); KindOf(err) != KindReplacementFailed {
		t.Errorf("a commit failure that is not in-use is replacement_failed, got %v", err)
	}
	x = newTxn(t, "0.3.0", "0.2.9", "0.3.9")
	err = replaceWith(context.Background(), true, x.exe, x.can, mustVersion(t, "0.3.1"), x.ops("move-out", nil))
	if KindOf(err) != KindManualIntervention {
		t.Errorf("failing to move a bad candidate out is manual intervention, got %v", err)
	}
}

func TestReplacementRefusesACandidateElsewhere(t *testing.T) {
	x := newTxn(t, "0.3.0", "", "")
	other := filepath.Join(t.TempDir(), "candidate")
	if err := replaceWith(context.Background(), false, x.exe, other, mustVersion(t, "0.3.1"), x.ops("", nil)); KindOf(err) != KindReplacementFailed {
		t.Errorf("a candidate in another directory cannot be renamed atomically: %v", err)
	}
}

// ── rollback ────────────────────────────────────────────────────────────

type forbiddenSource struct{ t *testing.T }

func (s forbiddenSource) Release(context.Context, *Version) (ReleaseInfo, error) {
	s.t.Error("rollback contacted the release source")
	return ReleaseInfo{}, errors.New("no network")
}

func (s forbiddenSource) DownloadAssets(context.Context, ReleaseInfo, []AssetRequirement) (map[string][]byte, error) {
	s.t.Error("rollback contacted the release source")
	return nil, errors.New("no network")
}

// recordingRunner is markerRunner that remembers every path it ran.
func recordingRunner(ran *[]string) CommandRunner {
	inner := markerRunner()
	return runnerFunc(func(ctx context.Context, path string, args, env []string) ([]byte, []byte, error) {
		*ran = append(*ran, path)
		return inner.Run(ctx, path, args, env)
	})
}

func rollbackUpdater(t *testing.T, x *txn, windows bool, ran *[]string) Updater {
	ops := x.ops("", nil)
	return Updater{Source: forbiddenSource{t}, Runner: recordingRunner(ran), ops: &ops, windows: &windows}
}

func TestRollbackSwapsOneLevelWithoutTheNetworkAndNeverRunsPreviousInPlace(t *testing.T) {
	for name, windows := range strategies() {
		x := newTxn(t, "0.3.1", "0.3.0", "")
		var ran []string
		r, err := rollbackUpdater(t, x, windows, &ran).Rollback(context.Background(), x.exe, "0.3.1")
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if r.To.String() != "0.3.0" || x.version(x.exe) != "0.3.0" || x.version(x.prev) != "0.3.1" {
			t.Errorf("%s: rollback installed %s with previous %s; want 0.3.0 and the displaced 0.3.1", name, x.version(x.exe), x.version(x.prev))
		}
		for _, p := range ran {
			if p == x.prev {
				t.Errorf("%s: rollback ran .previous in place", name)
			}
		}
		// A second rollback swaps back: .previous may be newer.
		ran = nil
		if _, err := rollbackUpdater(t, x, windows, &ran).Rollback(context.Background(), x.exe, "0.3.0"); err != nil {
			t.Fatalf("%s second: %v", name, err)
		}
		if x.version(x.exe) != "0.3.1" || x.version(x.prev) != "0.3.0" {
			t.Errorf("%s: second rollback: installed %s, previous %s", name, x.version(x.exe), x.version(x.prev))
		}
		if names := x.names(); len(names) != 2 {
			t.Errorf("%s: rollback leaves only the binary and .previous: %v", name, names)
		}
	}
}

func TestRollbackRefusesAMissingInvalidOrSameVersionPrevious(t *testing.T) {
	cases := map[string]struct {
		setup func(x *txn)
		kind  Kind
	}{
		"missing":      {func(x *txn) {}, KindNoPrevious},
		"same version": {func(x *txn) { x.write(x.prev, "0.3.1") }, KindPreviousInvalid},
		"not a release": {func(x *txn) {
			x.write(x.prev, "garbage")
		}, KindPreviousInvalid},
		"empty": {func(x *txn) {
			if err := os.WriteFile(x.prev, nil, 0o700); err != nil { // #nosec G306 -- fixture
				x.t.Fatal(err)
			}
		}, KindPreviousInvalid},
		"a directory": {func(x *txn) {
			if err := os.Mkdir(x.prev, 0o700); err != nil {
				x.t.Fatal(err)
			}
		}, KindPreviousInvalid},
		"a symlink": {func(x *txn) {
			other := filepath.Join(x.dir, "elsewhere")
			x.write(other, "0.3.0")
			if err := os.Symlink(other, x.prev); err != nil {
				x.t.Skip("symlinks unavailable")
			}
		}, KindPreviousInvalid},
	}
	for name, tc := range cases {
		x := newTxn(t, "0.3.1", "", "")
		tc.setup(x)
		before := x.names()
		exeSum := x.sum(x.exe)
		var ran []string
		_, err := rollbackUpdater(t, x, false, &ran).Rollback(context.Background(), x.exe, "0.3.1")
		if KindOf(err) != tc.kind {
			t.Errorf("%s: want %s, got %v", name, tc.kind, err)
		}
		if x.sum(x.exe) != exeSum || fmt.Sprint(x.names()) != fmt.Sprint(before) {
			t.Errorf("%s: a refused rollback changes nothing and leaves no staged copy: %v -> %v", name, before, x.names())
		}
	}
}

// A Windows rollback whose validation fails and whose restoration then fails
// reports the displaced binary and the staged copy as the surviving copies;
// every one of them must still be there to find.
func TestRollbackPreservesEveryNamedRecoveryPathWhenRestorationFails(t *testing.T) {
	x := newTxn(t, "0.3.1", "0.3.0", "")
	prevSum := x.sum(x.prev)
	ops := x.ops("validate,restore", nil)
	windows := true
	var ran []string
	u := Updater{Source: forbiddenSource{t}, Runner: recordingRunner(&ran), ops: &ops, windows: &windows}
	_, err := u.Rollback(context.Background(), x.exe, "0.3.1")
	var typed *Error
	if KindOf(err) != KindManualIntervention || !errors.As(err, &typed) || len(typed.Paths) == 0 {
		t.Fatalf("want manual_intervention with paths, got %v", err)
	}
	var displaced, staged string
	for _, p := range typed.Paths {
		if _, statErr := os.Lstat(p); statErr != nil {
			t.Errorf("the error names %s as surviving, but it does not exist: %v", p, statErr)
			continue
		}
		switch {
		case strings.Contains(filepath.Base(p), "displaced"):
			displaced = p
		case strings.Contains(filepath.Base(p), "candidate"):
			staged = p
		}
	}
	if displaced == "" || x.version(displaced) != "0.3.1" {
		t.Errorf("the old binary must be at the reported displaced path %q (holds %s)", displaced, x.version(displaced))
	}
	if staged == "" || x.version(staged) != "0.3.0" {
		t.Errorf("the staged copy of .previous must be at its reported path %q (holds %s)", staged, x.version(staged))
	}
	if x.sum(x.prev) != prevSum {
		t.Error(".previous must be byte-identical")
	}
}

func TestDiscardAfterInstallKeepsWhatAnErrorNames(t *testing.T) {
	dir := t.TempDir()
	candidate := filepath.Join(dir, ".dropin-miner.candidate-1")
	write := func() {
		if err := os.WriteFile(candidate, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	p := Prepared{Candidate: candidate}
	write()
	p.DiscardAfterInstall(&Error{Kind: KindManualIntervention, Err: errInjected, Paths: []string{candidate}})
	if _, err := os.Lstat(candidate); err != nil {
		t.Error("a candidate the error names as surviving must be kept")
	}
	p.DiscardAfterInstall(&Error{Kind: KindReplacementFailed, Err: errInjected, Paths: []string{filepath.Join(dir, "other")}})
	if _, err := os.Lstat(candidate); err == nil {
		t.Error("a candidate the error does not name must be removed")
	}
	write()
	p.DiscardAfterInstall(errInjected)
	if _, err := os.Lstat(candidate); err == nil {
		t.Error("an untyped error names nothing; the candidate must be removed")
	}
	p.DiscardAfterInstall(nil) // after a successful install the name is gone; nothing to do
}

func TestInstallReplacesThePreparedCandidate(t *testing.T) {
	x := newTxn(t, "0.3.0", "", "0.3.1")
	ops := x.ops("", nil)
	windows := false
	u := Updater{ops: &ops, windows: &windows}
	if err := u.Install(context.Background(), Prepared{To: mustVersion(t, "0.3.1"), Executable: x.exe, Candidate: x.can}); err != nil {
		t.Fatal(err)
	}
	if x.version(x.exe) != "0.3.1" || x.version(x.prev) != "0.3.0" {
		t.Errorf("Install: installed %s, previous %s", x.version(x.exe), x.version(x.prev))
	}
	if err := u.Install(context.Background(), Prepared{NoChange: true}); KindOf(err) != KindReplacementFailed {
		t.Errorf("nothing prepared is nothing to install: %v", err)
	}
}
