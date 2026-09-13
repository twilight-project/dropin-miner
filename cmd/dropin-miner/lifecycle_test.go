package main

// Lifecycle coordination (lifecycle.go): the gate's place, how every command
// finds it, and the one guarantee it exists for — an operation that destroys
// or replaces an installation cannot be crossed by a setup, connect or flush
// that starts after its check.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// ── helpers ─────────────────────────────────────────────────────────────

func setForegroundWait(t *testing.T, d time.Duration) {
	t.Helper()
	orig := lifecycleForegroundWait
	lifecycleForegroundWait = d
	t.Cleanup(func() { lifecycleForegroundWait = orig })
}

// lifecycleEvents is what the admission hook observed. locked fires once an
// operation holds its own lock. With handshake, the first two gate attempts
// are each handed to the test before they are made: receiving the second
// proves the first was refused, because a successful attempt never loops.
// Without it, attempts are only counted, for code under test that runs on
// the test's own goroutine.
type lifecycleEvents struct {
	attempts chan int
	locked   chan struct{}
	count    atomic.Int32
}

func watchLifecycle(t *testing.T, handshake bool) *lifecycleEvents {
	t.Helper()
	ev := &lifecycleEvents{attempts: make(chan int), locked: make(chan struct{}, 1)}
	lifecycleTestHook = func(event string) {
		if event == "operation locked" {
			select {
			case ev.locked <- struct{}{}:
			default:
			}
			return
		}
		if n := ev.count.Add(1); handshake && n <= 2 {
			ev.attempts <- int(n)
		}
	}
	t.Cleanup(func() { lifecycleTestHook = nil })
	return ev
}

// awaitRefusedAtGate returns once the operation started in the background
// has attempted the gate twice — proof it was refused the first time — and
// fails if it took its operation lock or exited first.
func awaitRefusedAtGate(t *testing.T, ev *lifecycleEvents, done <-chan int, what string) {
	t.Helper()
	for want := 1; want <= 2; want++ {
		select {
		case <-ev.attempts:
		case <-ev.locked:
			t.Fatalf("%s took its operation lock while a destructive operation held the gate", what)
		case code := <-done:
			t.Fatalf("%s ran to exit %d while a destructive operation held the gate", what, code)
		}
	}
}

func mustAcquireGate(t *testing.T, path string) *lifecycleLock {
	t.Helper()
	gate, err := acquireLifecycleGate(path, 0)
	if err != nil {
		t.Fatalf("acquire %s: %v", path, err)
	}
	t.Cleanup(gate.release)
	return gate
}

func holdLockFile(t *testing.T, path string) func() {
	t.Helper()
	f, held, err := tryLockFile(path)
	if err != nil || !held {
		t.Fatalf("hold %s: held=%v err=%v", path, held, err)
	}
	released := false
	release := func() {
		if !released {
			released = true
			_ = unlockFile(f)
		}
	}
	t.Cleanup(release)
	return release
}

func lockIsFree(t *testing.T, path string) bool {
	t.Helper()
	f, held, err := tryLockFile(path)
	if err != nil {
		t.Fatalf("probe %s: %v", path, err)
	}
	if held {
		_ = unlockFile(f)
	}
	return held
}

func activeOperation(err error) string {
	var active *lifecycleActiveError
	if errors.As(err, &active) {
		return active.Operation
	}
	return ""
}

// minerConfigTemplate is a mining installation's config with its state,
// intake and sessions directories filled in. The AS is never contacted: no
// mining decision is on file.
const minerConfigTemplate = "[mining]\nas_url = \"https://rewards.invalid.test\"\nchain_id = \"twilight-testnet-1\"\nslot_id = 3\nstate_dir = %q\n\n[miner]\nenabled = true\nintake_dir = %q\nsessions_dir = %q\n"

func writeFileT(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil { // #nosec G703 -- a path inside the test's own t.TempDir()
		t.Fatal(err)
	}
}

// ── where the gate is ───────────────────────────────────────────────────

func TestLifecycleGateIsASiblingOutsideTheInstallation(t *testing.T) {
	root := t.TempDir()
	sep := string(filepath.Separator)
	for _, spelled := range []string{
		filepath.Join(root, ".tokendrop"),
		filepath.Join(root, "opt", "me", "dropin"),
		filepath.Join(root, "opt", "me", "dropin") + sep,
		filepath.Join(root, "opt", "me", "x") + sep + ".." + sep + "dropin",
	} {
		home, err := lifecycleIdentity(spelled)
		if err != nil {
			t.Fatal(err)
		}
		gate := lifecycleGatePath(home)
		want := filepath.Join(filepath.Dir(home), filepath.Base(home)+".lifecycle.lock")
		if gate != want {
			t.Errorf("gate of %s = %s, want the sibling %s", spelled, gate, want)
		}
		if within(gate, home) {
			t.Errorf("gate %s is inside the installation %s, which a purge removes", gate, home)
		}

		// The three ways a command reaches this installation name one gate.
		fromConfig, err := configGatePath(filepath.Join(spelled, setupConfigFile), noEnv)
		if err != nil {
			t.Fatal(err)
		}
		resolved, err := resolveInstallationHome(spelled, noEnv, "", "")
		if err != nil {
			t.Fatal(err)
		}
		if fromConfig != want || lifecycleGatePath(resolved) != want {
			t.Errorf("%s: config-keyed gate %s and home-keyed gate %s must both be %s", spelled, fromConfig, lifecycleGatePath(resolved), want)
		}
	}
}

func TestResolveInstallationHomeOrder(t *testing.T) {
	root := t.TempDir()
	user := filepath.Join(root, "user")
	flagHome := filepath.Join(root, "flag")
	envHome := filepath.Join(root, "envhome")
	cfgDir := filepath.Join(root, "cfgdir")
	layout := filepath.Join(root, "layout")
	writeFileT(t, filepath.Join(layout, setupConfigFile), "")
	layoutExe := filepath.Join(layout, "bin", "dropin-miner")
	noToml := filepath.Join(root, "notoml", "bin", "dropin-miner")
	notBin := filepath.Join(root, "layout", "tools", "dropin-miner")

	cases := []struct {
		name string
		flag string
		env  map[string]string
		exe  string
		want string
	}{
		{"-home wins over everything", flagHome, map[string]string{"TOKENDROP_HOME": envHome, "TOKENDROP_CONFIG": filepath.Join(cfgDir, "x.toml")}, layoutExe, flagHome},
		{"TOKENDROP_HOME over TOKENDROP_CONFIG and layout", "", map[string]string{"TOKENDROP_HOME": envHome, "TOKENDROP_CONFIG": filepath.Join(cfgDir, "x.toml")}, layoutExe, envHome},
		{"the directory of TOKENDROP_CONFIG named tokendrop.toml over layout", "", map[string]string{"TOKENDROP_CONFIG": filepath.Join(cfgDir, setupConfigFile)}, layoutExe, cfgDir},
		{"TOKENDROP_CONFIG with another name falls through to the layout", "", map[string]string{"TOKENDROP_CONFIG": filepath.Join(cfgDir, "custom.toml")}, layoutExe, layout},
		{"TOKENDROP_CONFIG with another name falls through to the default", "", map[string]string{"TOKENDROP_CONFIG": filepath.Join(cfgDir, "tokendrop.toml.bak")}, "", filepath.Join(user, ".tokendrop")},
		{"native layout", "", nil, layoutExe, layout},
		{"bin without tokendrop.toml beside it is not a layout", "", nil, noToml, filepath.Join(user, ".tokendrop")},
		{"tokendrop.toml beside a directory not named bin is not a layout", "", nil, notBin, filepath.Join(user, ".tokendrop")},
		{"default", "", nil, "", filepath.Join(user, ".tokendrop")},
	}
	for _, tc := range cases {
		got, err := resolveInstallationHome(tc.flag, envOf(tc.env), user, tc.exe)
		if err != nil || got != tc.want {
			t.Errorf("%s: got %q (%v), want %q", tc.name, got, err, tc.want)
		}
	}
	if _, err := resolveInstallationHome("", noEnv, "", ""); err == nil {
		t.Error("no -home, no environment, no layout and no user home must be an error, not a guess")
	}

	// Lexical: a symlinked home keeps its own spelling, so its gate is
	// beside the link every command names, never beside the link's target.
	target := filepath.Join(root, "real-home")
	link := filepath.Join(root, "linked-home")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable here: %v", err)
	}
	if got, _ := resolveInstallationHome(link, noEnv, user, ""); got != link {
		t.Errorf("a symlinked home resolved to %q; lifecycle identities are lexical and must stay %q", got, link)
	}
}

func TestConfigGatePathFollowsTheConfigLoadConfigWouldChoose(t *testing.T) {
	root := t.TempDir()
	a := filepath.Join(root, "a", setupConfigFile)
	b := filepath.Join(root, "b", setupConfigFile)
	c := filepath.Join(root, "c")
	env := map[string]string{"TOKENDROP_CONFIG": b, "TOKENDROP_HOME": c}

	if got, _ := configGatePath(a, envOf(env)); got != lifecycleGatePath(filepath.Dir(a)) {
		t.Errorf("-config must key the gate even with TOKENDROP_CONFIG and TOKENDROP_HOME set: got %s", got)
	}
	if got, _ := configGatePath("", envOf(env)); got != lifecycleGatePath(filepath.Dir(b)) {
		t.Errorf("TOKENDROP_CONFIG must key the gate over TOKENDROP_HOME: got %s", got)
	}

	cwd := filepath.Join(root, "cwd")
	writeFileT(t, filepath.Join(cwd, setupConfigFile), "")
	t.Chdir(cwd)
	wd, _ := os.Getwd() // the platform's own spelling of cwd (macOS /private)
	if got, _ := configGatePath("", envOf(map[string]string{"TOKENDROP_HOME": c})); got != lifecycleGatePath(wd) {
		t.Errorf("./tokendrop.toml must key the gate like loadConfig picks it up: got %s, want %s", got, lifecycleGatePath(wd))
	}

	t.Chdir(root)
	if got, _ := configGatePath("", envOf(map[string]string{"TOKENDROP_HOME": c})); got != lifecycleGatePath(c) {
		t.Errorf("with no config file, TOKENDROP_HOME keys the gate: got %s", got)
	}
	userHome, _ := os.UserHomeDir()
	if got, _ := configGatePath("", noEnv); got != lifecycleGatePath(filepath.Join(userHome, ".tokendrop")) {
		t.Errorf("with no config file and no TOKENDROP_HOME, the default installation keys the gate: got %s", got)
	}
}

// ── the gate itself ─────────────────────────────────────────────────────

func TestLifecycleGateContention(t *testing.T) {
	gatePath := lifecycleGatePath(filepath.Join(t.TempDir(), "home"))
	first, err := acquireLifecycleGate(gatePath, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := acquireLifecycleGate(gatePath, 0); !errors.Is(err, errLifecycleBusy) {
		t.Fatalf("a held gate must be busy to a second holder, got %v", err)
	}

	setForegroundWait(t, 150*time.Millisecond)
	start := time.Now()
	if _, err := admitOrdinary(gatePath, admitForeground); !errors.Is(err, errLifecycleBusy) {
		t.Fatalf("foreground admission past a held gate: %v", err)
	}
	if waited := time.Since(start); waited < 150*time.Millisecond {
		t.Errorf("foreground admission gave up after %s; it waits the whole bound", waited)
	}
	if _, err := admitOrdinary(gatePath, admitDetached); !errors.Is(err, errLifecycleBusy) {
		t.Fatalf("detached admission past a held gate: %v", err)
	}
	if gate, err := admitOrdinary(gatePath, admitAlreadyAdmitted); gate != nil || err != nil {
		t.Fatalf("an already-admitted caller takes no gate: %v %v", gate, err)
	}

	first.release()
	first.release() // idempotent
	again, err := acquireLifecycleGate(gatePath, 0)
	if err != nil {
		t.Fatalf("a released gate must be free: %v", err)
	}
	again.release()
}

func TestAnUnopenableGateAdmitsOrdinaryOperationsButRefusesDestructiveOnes(t *testing.T) {
	home := filepath.Join(t.TempDir(), "missing-parent", "home")
	gate, err := admitOrdinary(lifecycleGatePath(home), admitForeground)
	if gate != nil || err != nil {
		t.Errorf("an ordinary operation whose gate cannot be opened proceeds without it: %v %v", gate, err)
	}
	ex, err := excludeLifecycle(home, noEnv, 0)
	if err == nil || errors.Is(err, errLifecycleBusy) || activeOperation(err) != "" {
		ex.release()
		t.Errorf("a destructive operation whose gate cannot be opened must refuse on that error, got %v", err)
	}
}

// ── destructive exclusion ───────────────────────────────────────────────

func TestDestructiveExclusionRefusesEachRunningOperation(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	// State and intake outside home: the operation locks are found through
	// the config, not assumed beside it.
	stateDir := filepath.Join(root, "elsewhere", "state")
	intakeDir := filepath.Join(root, "evidence", "intake")
	writeFileT(t, filepath.Join(home, setupConfigFile), fmt.Sprintf(minerConfigTemplate,
		stateDir, intakeDir, filepath.Join(root, "evidence", "sessions")))
	for _, dir := range []string{stateDir, intakeDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	locks := map[string]string{
		"setup":   filepath.Join(home, setupLockFile),
		"connect": filepath.Join(stateDir, "connect.lock"),
		"flush":   filepath.Join(root, "evidence", "flush.lock"),
	}
	gatePath := lifecycleGatePath(home)

	for _, op := range []string{"setup", "connect", "flush"} {
		release := holdLockFile(t, locks[op])
		ex, err := excludeLifecycle(home, noEnv, 0)
		if got := activeOperation(err); got != op {
			ex.release()
			t.Fatalf("with %s running, the exclusion must refuse naming it; got %v", op, err)
		}
		if !lockIsFree(t, gatePath) {
			t.Errorf("a refused exclusion must not keep the gate (%s running)", op)
		}
		for other, path := range locks {
			if other != op && !lockIsFree(t, path) {
				t.Errorf("a refused exclusion must not keep %s.lock (%s running)", other, op)
			}
		}
		release()
	}

	ex, err := excludeLifecycle(home, noEnv, 0)
	if err != nil {
		t.Fatalf("nothing running: %v", err)
	}
	if lockIsFree(t, gatePath) {
		t.Error("an exclusion holds the gate for its whole operation")
	}
	for op, path := range locks {
		if lockIsFree(t, path) {
			t.Errorf("an exclusion holds %s.lock for its whole operation, so a binary that predates the gate is excluded too", op)
		}
	}
	ex.releaseOperation(locks["connect"])
	if !lockIsFree(t, locks["connect"]) {
		t.Error("releaseOperation lets go of exactly that lock, for Windows to delete its file")
	}
	if lockIsFree(t, gatePath) {
		t.Error("releaseOperation must leave the gate held")
	}
	ex.release()
	for name, path := range map[string]string{"gate": gatePath, "setup": locks["setup"], "flush": locks["flush"]} {
		if !lockIsFree(t, path) {
			t.Errorf("release must let go of the %s lock", name)
		}
	}
}

func TestDestructiveExclusionWithoutAConfigProbesTheInstallerLayout(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	if err := os.MkdirAll(filepath.Join(home, "state"), 0o700); err != nil {
		t.Fatal(err)
	}
	for op, path := range map[string]string{
		"connect": filepath.Join(home, "state", "connect.lock"),
		"flush":   filepath.Join(home, "flush.lock"),
	} {
		release := holdLockFile(t, path)
		ex, err := excludeLifecycle(home, noEnv, 0)
		if activeOperation(err) != op {
			ex.release()
			t.Errorf("no config: %s.lock at the installer layout must be probed; got %v", op, err)
		}
		release()
	}
}

func TestDestructiveExclusionTakesTheGateBeforeReadingTheConfig(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	writeFileT(t, filepath.Join(home, setupConfigFile), "this is = [not toml\n")

	// Held elsewhere: the answer is busy, because the config must not even be
	// read before the gate is held.
	held := mustAcquireGate(t, lifecycleGatePath(home))
	if _, err := excludeLifecycle(home, noEnv, 0); !errors.Is(err, errLifecycleBusy) {
		t.Fatalf("with the gate held elsewhere the exclusion must be busy before it reads the config; got %v", err)
	}
	held.release()

	_, err := excludeLifecycle(home, noEnv, 0)
	var cfgErr *lifecycleConfigError
	if !errors.As(err, &cfgErr) {
		t.Fatalf("a config that exists but will not load must refuse the exclusion; got %v", err)
	}
	if !lockIsFree(t, lifecycleGatePath(home)) || !lockIsFree(t, filepath.Join(home, setupLockFile)) {
		t.Error("the refusal must release the gate and setup.lock it held while deciding")
	}
}

// ── admission cannot cross a held exclusion ─────────────────────────────

// connectInstallation is an installation whose connect talks to a stub
// platform: enough for connect to register and poll, then exit.
func connectInstallation(t *testing.T) (home, cfgPath, stateDir string) {
	t.Helper()
	withShortConnectTimings(t)
	platform := newStubPlatform(t)
	home = filepath.Join(t.TempDir(), "home")
	stateDir = filepath.Join(home, "state")
	cfgPath = filepath.Join(home, setupConfigFile)
	// mining.enabled is explicit, so connect -json has a decision to act on
	// and reaches admission instead of answering human_decision_required.
	writeFileT(t, cfgPath, fmt.Sprintf("[platform]\nbase_url = %q\nagents_api_url = %q\n\n[mining]\nenabled = false\nstate_dir = %q\n",
		platform.srv.URL, platform.srv.URL, stateDir))
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	orig := connectInteractive
	connectInteractive = func(io.Reader, io.Writer) bool { return false }
	t.Cleanup(func() { connectInteractive = orig })
	return home, cfgPath, stateDir
}

// The start-after-check race, closed: a destructive operation holds its
// exclusion and has let go of connect.lock to delete it (as it must on
// Windows); a connect that starts now meets the gate before it reads the
// config or creates anything, and runs only after the exclusion ends.
func TestConnectCannotStartUnderAHeldExclusion(t *testing.T) {
	home, cfgPath, stateDir := connectInstallation(t)
	ex, err := excludeLifecycle(home, noEnv, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer ex.release()
	ex.releaseOperation(connectLockPath(stateDir))
	if err := os.RemoveAll(stateDir); err != nil {
		t.Fatal(err)
	}

	setForegroundWait(t, 30*time.Second)
	ev := watchLifecycle(t, true)
	done := make(chan int, 1)
	go func() {
		done <- cmdConnect([]string{"-config", cfgPath}, strings.NewReader(""), io.Discard, io.Discard, noEnv)
	}()
	awaitRefusedAtGate(t, ev, done, "connect")
	if lexists(stateDir) {
		t.Fatal("connect created its state directory before passing the gate")
	}

	ex.release()
	select {
	case <-ev.locked:
	case code := <-done:
		t.Fatalf("once the exclusion ended connect must pass the gate and take connect.lock; it exited %d", code)
	}
	<-done
}

func TestDetachedAndMachineConnectUnderAHeldGate(t *testing.T) {
	home, cfgPath, stateDir := connectInstallation(t)
	mustAcquireGate(t, lifecycleGatePath(home))
	if err := os.RemoveAll(stateDir); err != nil {
		t.Fatal(err)
	}
	setForegroundWait(t, 100*time.Millisecond)
	ev := watchLifecycle(t, false)

	var stderr bytes.Buffer
	if code := cmdConnect([]string{"-config", cfgPath, "-resume"}, strings.NewReader(""), io.Discard, &stderr, noEnv); code != exitOK {
		t.Errorf("connect -resume under a held gate exits 0, got %d", code)
	}
	if lexists(stateDir) {
		t.Error("connect -resume under a held gate must record nothing: no resume stamp, no health")
	}

	var out bytes.Buffer
	code := cmdConnect([]string{"-config", cfgPath, "-json"}, strings.NewReader(""), &out, io.Discard, noEnv)
	var env struct {
		OK           bool   `json:"ok"`
		ExitCode     int    `json:"exit_code"`
		Status       string `json:"status"`
		Code         string `json:"code"`
		Retryable    bool   `json:"retryable"`
		Action       string `json:"action"`
		RetryAfterMS *int64 `json:"retry_after_ms"`
	}
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("connect -json must emit one envelope: %v\n%s", err, out.String())
	}
	if code != exitTransport || env.ExitCode != exitTransport || env.OK || env.Status != statusTransport ||
		env.Code != "lifecycle_busy" || !env.Retryable || env.Action != actionRetry ||
		env.RetryAfterMS == nil || *env.RetryAfterMS != lifecycleBusyRetryAfter.Milliseconds() {
		t.Errorf("connect -json under a held gate: exit %d, envelope %+v", code, env)
	}
	if lexists(stateDir) {
		t.Error("connect -json under a held gate must not open the state directory")
	}

	var textErr bytes.Buffer
	if code := cmdConnect([]string{"-config", cfgPath}, strings.NewReader(""), io.Discard, &textErr, noEnv); code != exitTransport ||
		!strings.Contains(textErr.String(), errLifecycleBusy.Error()) {
		t.Errorf("foreground connect under a held gate refuses non-zero and says why: exit %d, %q", code, textErr.String())
	}
	select {
	case <-ev.locked:
		t.Error("no connect may take connect.lock while the gate is held")
	default:
	}
}

func flushInstallation(t *testing.T) (home, cfgPath string) {
	t.Helper()
	home = filepath.Join(t.TempDir(), "home")
	cfgPath = filepath.Join(home, setupConfigFile)
	writeFileT(t, cfgPath, fmt.Sprintf(minerConfigTemplate,
		filepath.Join(home, "state"), filepath.Join(home, "intake"), filepath.Join(home, "sessions")))
	return home, cfgPath
}

func TestFlushCannotStartUnderAHeldExclusion(t *testing.T) {
	home, cfgPath := flushInstallation(t)
	ex, err := excludeLifecycle(home, noEnv, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer ex.release()
	ex.releaseOperation(filepath.Join(home, "flush.lock"))
	if err := os.Remove(filepath.Join(home, "flush.lock")); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}

	setForegroundWait(t, 30*time.Second)
	ev := watchLifecycle(t, true)
	done := make(chan int, 1)
	go func() {
		done <- cmdFlush([]string{"-config", cfgPath}, io.Discard, io.Discard, noEnv)
	}()
	awaitRefusedAtGate(t, ev, done, "flush")
	if lexists(filepath.Join(home, "flush.lock")) {
		t.Fatal("flush touched its lock before passing the gate")
	}

	ex.release()
	select {
	case <-ev.locked:
	case code := <-done:
		t.Fatalf("once the exclusion ended flush must pass the gate and take flush.lock; it exited %d", code)
	}
	<-done
}

func TestDetachedAndManualFlushUnderAHeldGate(t *testing.T) {
	home, cfgPath := flushInstallation(t)
	mustAcquireGate(t, lifecycleGatePath(home))
	setForegroundWait(t, 100*time.Millisecond)
	ev := watchLifecycle(t, false)

	detached := envOf(map[string]string{detachedChildEnv: "1"})
	start := time.Now()
	if code := cmdFlush([]string{"-config", cfgPath}, io.Discard, io.Discard, detached); code != exitOK {
		t.Errorf("a detached flush under a held gate exits 0, got %d", code)
	}
	if waited := time.Since(start); waited >= lifecycleForegroundWait {
		t.Errorf("a detached flush must not wait for the gate; it took %s", waited)
	}
	if entries, _ := os.ReadDir(home); len(entries) != 1 {
		t.Errorf("a detached flush under a held gate records nothing: home holds %v", entries)
	}

	var stderr bytes.Buffer
	if code := cmdFlush([]string{"-config", cfgPath}, io.Discard, &stderr, noEnv); code != exitTransport ||
		!strings.Contains(stderr.String(), errLifecycleBusy.Error()) {
		t.Errorf("a manual flush under a held gate refuses non-zero and says why: exit %d, %q", code, stderr.String())
	}
	select {
	case <-ev.locked:
		t.Error("no flush may take flush.lock while the gate is held")
	default:
	}
}

// ── setup ───────────────────────────────────────────────────────────────

// probeWriter runs fn the first time a write contains needle, before the
// write itself.
type probeWriter struct {
	w      io.Writer
	needle string
	fn     func()
	fired  bool
}

func (p *probeWriter) Write(b []byte) (int, error) {
	if !p.fired && bytes.Contains(b, []byte(p.needle)) {
		p.fired = true
		p.fn()
	}
	return p.w.Write(b)
}

func TestSetupHoldsSetupLockThroughItsWholeRun(t *testing.T) {
	s := newSetupSandbox(t)
	orig := connectInteractive
	connectInteractive = func(io.Reader, io.Writer) bool { return false }
	defer func() { connectInteractive = orig }()

	probed := 0
	probe := func(when string) {
		probed++
		ex, err := excludeLifecycle(s.home, s.getenv, 0)
		if activeOperation(err) != "setup" {
			ex.release()
			t.Errorf("%s: a destructive operation must be refused by the running setup, got %v", when, err)
		}
		gate, err := acquireLifecycleGate(lifecycleGatePath(s.home), 0)
		if err != nil {
			t.Errorf("%s: setup must hold setup.lock, not the gate: %v", when, err)
			return
		}
		gate.release()
	}

	var out, errOut bytes.Buffer
	d := s.deps(&ttyReader{}, &out, &errOut, false)
	inner := d.connect
	d.connect = func(args []string, stdin io.Reader, stdout, stderr io.Writer, getenv func(string) string) int {
		probe("during connect")
		return inner(args, stdin, stdout, stderr, getenv)
	}
	closing := &probeWriter{w: &out, needle: "Setup complete", fn: func() { probe("at the closing message") }}
	d.stdout = closing
	if code := setupMain(d, nil); code != exitOK {
		t.Fatalf("setup: exit %d\n%s\n%s", code, out.String(), errOut.String())
	}
	if probed != 2 {
		t.Fatalf("both probes must run (during connect and at the closing message); ran %d", probed)
	}
	if !lockIsFree(t, filepath.Join(s.home, setupLockFile)) || !lockIsFree(t, lifecycleGatePath(s.home)) {
		t.Error("setup must release setup.lock and hold no gate once it returns")
	}
}

func TestSetupRefusesWhileTheGateIsHeld(t *testing.T) {
	s := newSetupSandbox(t)
	mustAcquireGate(t, lifecycleGatePath(s.home))
	setForegroundWait(t, 100*time.Millisecond)

	code, _, stderr := s.run(nil, false)
	if code != exitTransport || !strings.Contains(stderr, errLifecycleBusy.Error()) {
		t.Errorf("setup under a held gate refuses non-zero and says why: exit %d, %q", code, stderr)
	}
	if lexists(s.home) || s.connectCalls != 0 {
		t.Errorf("setup under a held gate changes nothing: home exists=%v, connect calls=%d", lexists(s.home), s.connectCalls)
	}

	if code, _, stderr := s.run(nil, false, "-dry-run"); code != exitOK {
		t.Errorf("a dry run writes nothing and takes no lock, so a held gate does not stop it: exit %d, %q", code, stderr)
	}
}

func TestSetupConnectsUnderItsOwnAdmission(t *testing.T) {
	d := systemSetupDeps(strings.NewReader(""), io.Discard, io.Discard, noEnv)
	if reflect.ValueOf(d.connect).Pointer() != reflect.ValueOf(connectAdmitted).Pointer() {
		t.Error("setup's in-process connect must be connectAdmitted: taking the gate while holding setup.lock is the reverse lock order")
	}

	// connectAdmitted makes no attempt on the gate; cmdConnect does.
	_, cfgPath, _ := connectInstallation(t)
	setForegroundWait(t, 50*time.Millisecond)
	ev := watchLifecycle(t, false)
	connectAdmitted([]string{"-config", cfgPath}, strings.NewReader(""), io.Discard, io.Discard, noEnv)
	if n := ev.count.Load(); n != 0 {
		t.Errorf("connectAdmitted attempted the gate %d time(s)", n)
	}
	cmdConnect([]string{"-config", cfgPath}, strings.NewReader(""), io.Discard, io.Discard, noEnv)
	if ev.count.Load() == 0 {
		t.Error("cmdConnect must attempt the gate")
	}
}

func TestTheSetAsideScanNeverOffersTheLifecycleGate(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, ".tokendrop")
	// The gate is a file in practice; a directory holding a marker is what it
	// would take for the scan to mistake it for an installation.
	writeFileT(t, filepath.Join(lifecycleGatePath(home), credentialsFile), "{}")
	if got := setAsideInstallation(home); got != "" {
		t.Errorf("the lifecycle gate must never be offered as a set-aside installation, got %q", got)
	}
	aside := filepath.Join(root, ".tokendrop.bak-1")
	writeFileT(t, filepath.Join(aside, credentialsFile), "{}")
	if got := setAsideInstallation(home); got != aside {
		t.Errorf("a real set-aside installation is still found: got %q, want %q", got, aside)
	}
}

// ── the detached marker ─────────────────────────────────────────────────

func TestDetachedChildrenCarryTheMarker(t *testing.T) {
	parent := []string{"PATH=/bin", detachedChildEnv + "=0"}
	child := detachedEnvironment(parent)
	if child[len(child)-1] != detachedChildEnv+"=1" || len(parent) != 2 || parent[1] != detachedChildEnv+"=0" {
		t.Errorf("the child gets the marker last, so it wins over an inherited value, and the parent's slice is untouched: %v / %v", child, parent)
	}
	for value, want := range map[string]bool{"1": true, "": false, "0": false, "true": false} {
		if got := detachedChild(envOf(map[string]string{detachedChildEnv: value})); got != want {
			t.Errorf("detachedChild(%q) = %v, want %v", value, got, want)
		}
	}

	// spawnDetached is the single choke point for every detached child, on
	// both platforms, whichever this test runs on.
	for _, file := range []string{"detach_unix.go", "detach_windows.go"} {
		f, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		marked := false
		ast.Inspect(f, func(n ast.Node) bool {
			fn, ok := n.(*ast.FuncDecl)
			if !ok || fn.Name.Name != "spawnDetached" {
				return true
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				as, ok := n.(*ast.AssignStmt)
				if !ok || len(as.Lhs) != 1 || len(as.Rhs) != 1 {
					return true
				}
				sel, ok := as.Lhs[0].(*ast.SelectorExpr)
				call, isCall := as.Rhs[0].(*ast.CallExpr)
				if ok && sel.Sel.Name == "Env" && isCall {
					if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "detachedEnvironment" {
						marked = true
					}
				}
				return true
			})
			return false
		})
		if !marked {
			t.Errorf("%s: spawnDetached must set cmd.Env = detachedEnvironment(...) so every detached child is marked", file)
		}
	}
}
