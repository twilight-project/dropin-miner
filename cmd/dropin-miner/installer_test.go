package main

// dropin-miner setup, driven in-process against a sandbox: a fake user home,
// the installation directory inside it, the onboarding tests' stub platform
// and AS, connect run for real (cmdConnect, not a stub), and an agentOps
// whose PATH lookup finds exactly the agents a case says are installed.
//
// Every case snapshots the whole sandbox before and after, and asserts that
// nothing changed outside the ownership set for that case: the installation
// directory, the accepted adoption source, the chosen profile file, and the
// host paths of the targets selected for that run. That is a stronger claim
// than "nothing outside home" — the agents' own directories are inside the
// fake home too, and a case that installs one agent must not touch another.
//
// A registration is pre-claimed on the stub before setup runs, so connect's
// first poll finds it claimed and a case never waits out a poll budget.
// Those pieces of connect's behavior are covered at length elsewhere; what
// these cases prove is the order setup runs things in, what it writes, and
// what it refuses.

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/twilight-project/dropin-miner/pkg/auth"
	"github.com/twilight-project/dropin-miner/pkg/config"
	"github.com/twilight-project/dropin-miner/pkg/fsx"
)

// ── the sandbox ───────────────────────────────────────────────────────────

type setupSandbox struct {
	t        *testing.T
	root     string
	userHome string
	home     string // the installation directory
	exe      string
	platform *stubPlatform
	as       *stubOnboardingAS
	env      map[string]string
	onPath   map[string]bool
	userEnv  *fakeUserEnv
	// connectCalls counts every time setup invoked connect, whatever
	// connect then did.
	connectCalls int
	// move and restrict, when set, replace adoption's file operations so a
	// case can make one of them fail.
	move     func(from, to string) error
	restrict func(path string, dir bool) error
	// agentPlanObserver, when set, is threaded through to setupDeps: a case
	// wanting the plan agentsStep actually built sets this before calling
	// run.
	agentPlanObserver func(agentPlan)
}

func newSetupSandbox(t *testing.T) *setupSandbox {
	t.Helper()
	withShortConnectTimings(t)
	root := t.TempDir()
	s := &setupSandbox{
		t:        t,
		root:     root,
		userHome: filepath.Join(root, "user"),
		exe:      filepath.Join(root, "bin", "dropin-miner"),
		platform: newStubPlatform(t),
		as:       newStubAS(t),
		onPath:   map[string]bool{},
		userEnv:  newFakeUserEnv(),
	}
	s.home = filepath.Join(s.userHome, ".tokendrop")
	// Every default that reads the real environment — os.UserHomeDir,
	// os.UserConfigDir (pkg/config's default state_dir, the default wallet
	// directory) — resolves inside the sandbox, so a config that leaves a
	// directory out cannot reach the participant's own machine.
	t.Setenv("HOME", s.userHome)
	t.Setenv("USERPROFILE", s.userHome)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(s.userHome, ".config"))
	t.Setenv("AppData", filepath.Join(s.userHome, "AppData", "Roaming"))
	t.Setenv("LOCALAPPDATA", filepath.Join(s.userHome, "AppData", "Local"))
	for _, dir := range []string{s.userHome, filepath.Dir(s.exe)} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(s.exe, []byte("not run by these tests"), 0o700); err != nil { // #nosec G306 -- a placeholder executable in the test's own sandbox
		t.Fatal(err)
	}
	s.env = map[string]string{ // #nosec G101 -- env var names and stub URLs, no credential
		"TOKENDROP_HOME":           s.home,
		"TOKENDROP_AS_URL":         s.as.srv.URL,
		"TOKENDROP_ROUTER_URL":     "https://router.invalid.test",
		"TOKENDROP_PLATFORM_URL":   s.platform.srv.URL,
		"TOKENDROP_AGENTS_API_URL": s.platform.srv.URL,
		"SHELL":                    "/bin/zsh",
	}
	return s
}

func (s *setupSandbox) getenv(k string) string { return s.env[k] }

func (s *setupSandbox) agentOps(interactive bool) agentOps {
	ops := realAgentOps()
	ops.home = s.userHome
	ops.lookPath = func(name string) (string, error) {
		if s.onPath[name] {
			return filepath.Join(s.root, "path", name), nil
		}
		return "", exec.ErrNotFound
	}
	ops.executable = func() (string, error) { return s.exe, nil }
	ops.isTerminal = func() bool { return interactive }
	return ops
}

func (s *setupSandbox) deps(stdin io.Reader, stdout, stderr io.Writer, interactive bool) setupDeps {
	return setupDeps{
		stdin:       stdin,
		stdout:      stdout,
		stderr:      stderr,
		getenv:      s.getenv,
		userHome:    s.userHome,
		executable:  func() (string, error) { return s.exe, nil },
		interactive: interactive,
		agents:      s.agentOps(interactive),
		connect: func(args []string, stdin io.Reader, stdout, stderr io.Writer, getenv func(string) string) int {
			s.connectCalls++
			return connectAdmitted(args, stdin, stdout, stderr, getenv)
		},
		userEnv:           s.userEnv,
		now:               fixedSetupClock,
		move:              s.move,
		restrict:          s.restrict,
		agentPlanObserver: s.agentPlanObserver,
	}
}

// run executes setup. interactive also forces connect's terminal check, so
// an interactive case is interactive all the way through.
func (s *setupSandbox) run(stdin io.Reader, interactive bool, args ...string) (code int, stdout, stderr string) {
	s.t.Helper()
	if stdin == nil {
		stdin = &ttyReader{}
	}
	orig := connectInteractive
	connectInteractive = func(io.Reader, io.Writer) bool { return interactive }
	defer func() { connectInteractive = orig }()
	var out, errOut bytes.Buffer
	code = setupMain(s.deps(stdin, &out, &errOut, interactive), args)
	return code, out.String(), errOut.String()
}

func (s *setupSandbox) cfgPath() string { return filepath.Join(s.home, setupConfigFile) }

func (s *setupSandbox) readFile(path string) []byte {
	s.t.Helper()
	b, err := os.ReadFile(path) // #nosec G304 -- a path inside this test's own sandbox
	if err != nil {
		s.t.Fatal(err)
	}
	return b
}

func (s *setupSandbox) paths() agentPaths { return s.agentOps(false).paths(s.getenv) }

// ttyReader models a terminal: each Read returns at most one line, the way
// a line-disciplined tty delivers input, so connect's buffered reader and
// setup's byte-at-a-time reader take turns on one stdin exactly as they do
// at a real terminal.
type ttyReader struct{ lines []string }

func tty(lines ...string) *ttyReader {
	r := &ttyReader{}
	for _, l := range lines {
		r.lines = append(r.lines, l+"\n")
	}
	return r
}

func (r *ttyReader) Read(p []byte) (int, error) {
	if len(r.lines) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.lines[0])
	if n < len(r.lines[0]) {
		r.lines[0] = r.lines[0][n:]
	} else {
		r.lines = r.lines[1:]
	}
	return n, nil
}

// fakeUserEnv is the User environment as a map.
type fakeUserEnv struct {
	values     map[string]string
	broadcasts int
}

func newFakeUserEnv() *fakeUserEnv { return &fakeUserEnv{values: map[string]string{}} }

func (e *fakeUserEnv) Delete(name string) error {
	delete(e.values, name)
	return nil
}

func (e *fakeUserEnv) Get(name string) (string, bool, error) {
	v, ok := e.values[name]
	return v, ok, nil
}

func (e *fakeUserEnv) Set(name, value string) error { e.values[name] = value; return nil }
func (e *fakeUserEnv) Broadcast()                   { e.broadcasts++ }

// fixedSetupClock names the set-aside directory deterministically.
func fixedSetupClock() time.Time { return time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC) }

// ── the ownership set ─────────────────────────────────────────────────────

type fileSig struct {
	dir  bool
	link string
	mode fs.FileMode
	sum  [32]byte
}

func snapshotTree(t *testing.T, root string) map[string]fileSig {
	t.Helper()
	out := map[string]fileSig{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		sig := fileSig{mode: info.Mode()}
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			sig.link, _ = os.Readlink(path)
		case info.IsDir():
			sig.dir = true
		default:
			b, err := os.ReadFile(path) // #nosec G304 G122 -- walking this test's own sandbox, which nothing else mutates
			if err != nil {
				return err
			}
			sig.sum = sha256.Sum256(b)
		}
		out[path] = sig
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func within(path, owned string) bool {
	rel, err := filepath.Rel(owned, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// assertOwnership fails for every path that changed between before and after
// and is not inside an owned path. A directory created as an ancestor of an
// owned path is allowed, because creating the owned path creates it. An owned
// directory's lifecycle gate (lifecycle.go) is owned with it: the gate is a
// sibling by design, so the directory's removal never removes it.
func assertOwnership(t *testing.T, before, after map[string]fileSig, owned ...string) {
	t.Helper()
	allowed := func(path string, created bool, sig fileSig) bool {
		for _, o := range owned {
			if within(path, o) || path == lifecycleGatePath(o) {
				return true
			}
			if created && sig.dir && within(o, path) {
				return true
			}
		}
		return false
	}
	for path, sig := range after {
		prev, existed := before[path]
		if existed && prev == sig {
			continue
		}
		if !allowed(path, !existed, sig) {
			t.Errorf("setup changed %s, outside the ownership set %v", path, owned)
		}
	}
	for path := range before {
		if _, ok := after[path]; !ok && !allowed(path, false, fileSig{}) {
			t.Errorf("setup removed %s, outside the ownership set %v", path, owned)
		}
	}
}

// targetOwnedPaths is where a host target writes, from the same agentPaths
// the installer plans against.
func targetOwnedPaths(p agentPaths, id string) []string {
	switch id {
	case "claude":
		return []string{filepath.Dir(p.claudeSkill), p.claudeSettings}
	case "codex":
		return []string{filepath.Dir(p.codexSkill), p.codexConfig}
	case "cursor":
		return []string{filepath.Dir(p.cursorSkill), p.cursorHooks}
	case "opencode":
		return []string{p.opencodePlugin}
	case "pi":
		return []string{filepath.Dir(p.piSkill), p.piExtension}
	case "hermes":
		return []string{filepath.Dir(p.hermesSkill), p.hermesConfig}
	}
	return nil
}

// profilePath is the profile a POSIX run owns under SHELL=/bin/zsh, and
// nothing on Windows.
func (s *setupSandbox) profilePath() string {
	if runtime.GOOS == "windows" {
		return ""
	}
	return filepath.Join(s.userHome, ".zshrc")
}

func ownedSet(paths ...string) []string {
	var out []string
	for _, p := range paths {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// ── helpers for asserting outcomes ────────────────────────────────────────

func miningDecisionOf(t *testing.T, stateDir string) auth.MiningDecisionState {
	t.Helper()
	store, err := auth.OpenStoreExisting(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	return store.ReadMiningDecision().State
}

func loadSetupConfig(t *testing.T, path string) *config.Config {
	t.Helper()
	cfg, _, err := config.Load([]string{"-config", path}, func(string) string { return "" })
	if err != nil {
		t.Fatalf("config %s does not load: %v", path, err)
	}
	return cfg
}

func profileBlockCount(b []byte) int { return bytes.Count(b, []byte(profileMarkerStart)) }

// ── the cases ─────────────────────────────────────────────────────────────

// A fresh install with no terminal and no TOKENDROP_MINING: connect runs and
// persists "off" from the file's silence, [mining] carries no enabled key,
// and neither a profile nor an agent is touched.
func TestSetupFreshNonInteractiveWritesNoEnabledAndEndsStopped(t *testing.T) {
	s := newSetupSandbox(t)
	s.platform.claim("credits")
	s.onPath["claude"] = true
	before := snapshotTree(t, s.root)

	code, out, errOut := s.run(nil, false)
	if code != exitOK {
		t.Fatalf("setup exited %d\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
	}
	cfg := loadSetupConfig(t, s.cfgPath())
	if cfg.MiningEnabledExplicit {
		t.Errorf("[mining] enabled was written with no TOKENDROP_MINING opt-in:\n%s", s.readFile(s.cfgPath()))
	}
	if !cfg.Miner.Enabled || cfg.Platform.BaseURL != s.platform.srv.URL {
		t.Errorf("config is missing [miner] or [platform]: %+v %+v", cfg.Miner, cfg.Platform)
	}
	if reg, _, _ := s.platform.counts(); reg != 1 {
		t.Fatalf("register calls = %d, want 1: connect did not run exactly once", reg)
	}
	if got := miningDecisionOf(t, filepath.Join(s.home, "state")); got != auth.MiningDisabled {
		t.Fatalf("decision = %s, want disabled", got)
	}
	if !strings.Contains(out, "Not an interactive shell — not touching any agent") {
		t.Errorf("agents step did not refuse without a terminal:\n%s", out)
	}
	// #75 (soak S19): the profile and the agents step were both declined
	// for lack of a terminal, so the closing line must name them as
	// skipped and give the command to finish them, never claim everything
	// was already in place.
	if strings.Contains(out, "already in place") {
		t.Errorf("closing message claims everything was already in place when the profile and agents were skipped:\n%s", out)
	}
	profileStep := "shell profile"
	if runtime.GOOS == "windows" {
		profileStep = "user environment"
	}
	if !strings.Contains(out, profileStep) || !strings.Contains(out, "coding agents") {
		t.Errorf("closing message does not name the skipped steps:\n%s", out)
	}
	if !strings.Contains(out, "setup -config") || !strings.Contains(out, "-yes") {
		t.Errorf("closing message does not repeat the command to finish the skipped steps:\n%s", out)
	}
	after := snapshotTree(t, s.root)
	if runtime.GOOS != "windows" && lexists(s.profilePath()) {
		t.Error("a profile was written without a terminal")
	}
	if len(s.userEnv.values) != 0 {
		t.Errorf("the user environment was changed without a terminal: %v", s.userEnv.values)
	}
	assertOwnership(t, before, after, s.home)
}

// The scripted opt-in: no terminal and TOKENDROP_MINING=1 with a payout
// address writes enabled = true and the address, and connect enrolls and
// declares with nobody present.
func TestSetupScriptedMiningOptInEnrollsAndDeclares(t *testing.T) {
	s := newSetupSandbox(t)
	s.platform.claim("mining")
	const addr = "twilight1k5stzqa2sgvfgx9u04cv93pek3gcmm9h5t9hkn"
	s.env["TOKENDROP_MINING"] = "1"
	s.env["TOKENDROP_PAYOUT_ADDRESS"] = addr
	before := snapshotTree(t, s.root)

	code, out, errOut := s.run(nil, false)
	if code != exitOK {
		t.Fatalf("setup exited %d\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
	}
	cfg := loadSetupConfig(t, s.cfgPath())
	if !cfg.MiningEnabledExplicit || !cfg.Mining.Enabled || cfg.Mining.PayoutAddress != addr {
		t.Fatalf("scripted answer not written: explicit=%v enabled=%v address=%q", cfg.MiningEnabledExplicit, cfg.Mining.Enabled, cfg.Mining.PayoutAddress)
	}
	if got := miningDecisionOf(t, filepath.Join(s.home, "state")); got != auth.MiningEnabled {
		t.Fatalf("decision = %s, want enabled", got)
	}
	if reg, ok := loadAgent(t, filepath.Join(s.home, "state")); !ok || reg.LastEnrollmentSlot == "" {
		t.Fatalf("connect did not enroll: %+v", reg)
	}
	if got := s.as.declaredAddress(); got != addr {
		t.Fatalf("declared %q, want %q", got, addr)
	}
	assertOwnership(t, before, snapshotTree(t, s.root), s.home)
}

// TOKENDROP_MINING=1 at a terminal is ignored: the answer is connect's
// question, and enabled = true is never written by an interactive run.
func TestSetupNeverWritesEnabledAtATerminal(t *testing.T) {
	s := newSetupSandbox(t)
	s.platform.claim("credits")
	s.env["TOKENDROP_MINING"] = "1"
	code, out, errOut := s.run(tty("n"), true, "-yes", "-no-agents", "-no-profile")
	if code != exitOK {
		t.Fatalf("setup exited %d\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
	}
	if cfg := loadSetupConfig(t, s.cfgPath()); cfg.MiningEnabledExplicit {
		t.Fatalf("an interactive run wrote [mining] enabled:\n%s", s.readFile(s.cfgPath()))
	}
	if got := miningDecisionOf(t, filepath.Join(s.home, "state")); got != auth.MiningDisabled {
		t.Fatalf("decision = %s, want the terminal's no", got)
	}
}

// Interactive, yes to everything setup asks: the profile block (POSIX) or
// the user environment (Windows) is set once, and the detected agent is set
// up. The mining question is still connect's, answered here with no.
func TestSetupInteractiveYesToEverything(t *testing.T) {
	s := newSetupSandbox(t)
	s.platform.claim("credits")
	s.onPath["codex"] = true
	before := snapshotTree(t, s.root)

	code, out, errOut := s.run(tty("n"), true, "-yes")
	if code != exitOK {
		t.Fatalf("setup exited %d\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
	}
	if !strings.Contains(errOut, "Enable mining rewards?") {
		t.Errorf("connect did not ask its own question at a terminal:\n%s", errOut)
	}
	if got := miningDecisionOf(t, filepath.Join(s.home, "state")); got != auth.MiningDisabled {
		t.Fatalf("decision = %s, want the terminal's no; -yes must never answer it", got)
	}
	paths := s.paths()
	if !lexists(paths.codexSkill) {
		t.Errorf("the detected agent was not set up:\n%s", out)
	}
	if lexists(paths.claudeSkill) {
		t.Error("an undetected agent was set up")
	}
	if runtime.GOOS == "windows" {
		if !pathHasEntry(s.userEnv.values["Path"], filepath.Join(s.home, "bin")) || s.userEnv.values["TOKENDROP_CONFIG"] != s.cfgPath() {
			t.Errorf("user environment not set: %v", s.userEnv.values)
		}
	} else if n := profileBlockCount(s.readFile(s.profilePath())); n != 1 {
		t.Errorf("profile holds %d dropin-miner blocks, want 1", n)
	}
	if !strings.Contains(out, "     dropin-miner search -format model") {
		t.Errorf("closing message does not use the short command after the environment was set:\n%s", out)
	}
	assertOwnership(t, before, snapshotTree(t, s.root), ownedSet(append([]string{s.home, s.profilePath()}, targetOwnedPaths(paths, "codex")...)...)...)
}

// -yes answers the environment and agents questions without a terminal, on
// both systems, so a scripted install can pass it. It still never answers
// the mining question, and never adopts a set-aside installation.
func TestSetupYesAnswersEnvironmentAndAgentsWithoutATerminal(t *testing.T) {
	s := newSetupSandbox(t)
	s.platform.claim("credits")
	s.onPath["codex"] = true
	sibling := s.home + ".bak"
	writeInstallation(t, sibling, "wallet")
	siblingBefore := snapshotTree(t, sibling)
	before := snapshotTree(t, s.root)

	code, out, errOut := s.run(nil, false, "-yes")
	if code != exitOK {
		t.Fatalf("setup exited %d\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
	}
	paths := s.paths()
	if !lexists(paths.codexSkill) {
		t.Errorf("-yes without a terminal did not set up the detected agent:\n%s", out)
	}
	if runtime.GOOS == "windows" {
		if !pathHasEntry(s.userEnv.values["Path"], filepath.Join(s.home, "bin")) || s.userEnv.values["TOKENDROP_CONFIG"] != s.cfgPath() {
			t.Errorf("-yes without a terminal did not set the user environment: %v", s.userEnv.values)
		}
	} else if b, err := os.ReadFile(s.profilePath()); err != nil || profileBlockCount(b) != 1 { // #nosec G304 -- the sandbox's profile
		t.Errorf("-yes without a terminal did not write one profile block: %v\n%s", err, out)
	}
	if !reflect.DeepEqual(siblingBefore, snapshotTree(t, sibling)) || lexists(filepath.Join(s.home, "wallet")) {
		t.Error("-yes adopted a set-aside installation without a terminal")
	}
	if cfg := loadSetupConfig(t, s.cfgPath()); cfg.MiningEnabledExplicit {
		t.Error("-yes wrote a mining answer")
	}
	if got := miningDecisionOf(t, filepath.Join(s.home, "state")); got != auth.MiningDisabled {
		t.Errorf("decision = %s, want the config's silence (off); -yes must never answer it", got)
	}
	assertOwnership(t, before, snapshotTree(t, s.root), ownedSet(append([]string{s.home, s.profilePath()}, targetOwnedPaths(paths, "codex")...)...)...)
}

// B.2: a second setup on a completed installation is a no-op in every way
// that matters. Setup-owned static artifacts are compared byte for byte;
// connect-owned state is compared by identity.
func TestSetupSecondRunIsANoOp(t *testing.T) {
	s := newSetupSandbox(t)
	s.platform.claim("credits")
	s.onPath["claude"] = true
	s.onPath["codex"] = true
	if code, out, errOut := s.run(tty("n"), true, "-yes"); code != exitOK {
		t.Fatalf("first setup exited %d\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
	}

	// A record waiting in the spool and one in intake must survive the rerun.
	spooled := filepath.Join(s.home, "spool", "record-1.json")
	intake := filepath.Join(s.home, "intake", "served-1.json")
	for _, p := range []string{spooled, intake} {
		if err := os.WriteFile(p, []byte(`{"v":1}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	paths := s.paths()
	static := []string{s.cfgPath(), paths.claudeSkill, paths.claudeSettings, paths.codexSkill, paths.codexConfig}
	if runtime.GOOS == "windows" {
		static = append(static, filepath.Join(s.home, setupEnvJournalFile))
	} else {
		static = append(static, s.profilePath())
	}
	staticBefore := map[string][]byte{}
	for _, p := range static {
		staticBefore[p] = s.readFile(p)
	}
	stateDir := filepath.Join(s.home, "state")
	regBefore, _ := loadAgent(t, stateDir)
	credsBefore := s.readFile(filepath.Join(s.home, credentialsFile))
	dpopBefore, _ := os.ReadFile(filepath.Join(stateDir, "dpop.key")) // #nosec G304 -- absent until an enrollment; compared either way
	envBefore := map[string]string{}
	for k, v := range s.userEnv.values {
		envBefore[k] = v
	}
	decisionBefore := miningDecisionOf(t, stateDir)

	code, out, errOut := s.run(tty(), true, "-yes")
	if code != exitOK {
		t.Fatalf("second setup exited %d\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
	}
	for _, p := range static {
		if got := s.readFile(p); !bytes.Equal(got, staticBefore[p]) {
			t.Errorf("%s changed on a second run:\n--- before ---\n%s\n--- after ---\n%s", p, staticBefore[p], got)
		}
	}
	if runtime.GOOS != "windows" {
		if n := profileBlockCount(s.readFile(s.profilePath())); n != 1 {
			t.Errorf("profile holds %d blocks after a rerun, want 1", n)
		}
	}
	if !reflect.DeepEqual(s.userEnv.values, envBefore) {
		t.Errorf("user environment changed: %v -> %v", envBefore, s.userEnv.values)
	}
	if reg, _, _ := s.platform.counts(); reg != 1 {
		t.Errorf("register calls = %d after a rerun, want 1", reg)
	}
	regAfter, _ := loadAgent(t, stateDir)
	if regAfter.AgentID != regBefore.AgentID {
		t.Errorf("agent id %q -> %q", regBefore.AgentID, regAfter.AgentID)
	}
	if !bytes.Equal(s.readFile(filepath.Join(s.home, credentialsFile)), credsBefore) {
		t.Error("the search credential changed")
	}
	if dpopAfter, _ := os.ReadFile(filepath.Join(stateDir, "dpop.key")); !bytes.Equal(dpopAfter, dpopBefore) { // #nosec G304 -- as above
		t.Error("the DPoP key changed")
	}
	if got := miningDecisionOf(t, stateDir); got != decisionBefore {
		t.Errorf("mining decision %s -> %s", decisionBefore, got)
	}
	for _, p := range []string{spooled, intake} {
		if !lexists(p) {
			t.Errorf("%s was lost", p)
		}
	}
	if strings.Contains(out, "\nwrote ") {
		t.Errorf("an agent file was rewritten on a second run:\n%s", out)
	}
	if !strings.Contains(out, "Everything setup looks after was already in place; nothing was changed.") {
		t.Errorf("closing message does not say everything was found in place:\n%s", out)
	}
}

// An existing config with a [miner] table is left byte for byte.
func TestSetupLeavesAConfigWithMinerByteIdentical(t *testing.T) {
	s := newSetupSandbox(t)
	s.platform.claim("credits")
	if err := os.MkdirAll(s.home, 0o700); err != nil {
		t.Fatal(err)
	}
	body := "# mine, by hand\n[platform]\nbase_url = \"" + s.platform.srv.URL + "\"\n\n[mining]\nas_url = \"" + s.as.srv.URL +
		"\"\nchain_id = \"twilight-1\"\nslot_id = 7\nstate_dir = " + mustTOML(t, filepath.Join(s.home, "state")) +
		"\n\n[miner]\nenabled = true\nintake_dir = " + mustTOML(t, filepath.Join(s.home, "intake")) + "\n"
	if err := os.WriteFile(s.cfgPath(), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := s.run(nil, false)
	if code != exitOK {
		t.Fatalf("setup exited %d\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
	}
	if got := s.readFile(s.cfgPath()); string(got) != body {
		t.Fatalf("config changed:\n--- want ---\n%s\n--- got ---\n%s", body, got)
	}
}

func mustTOML(t *testing.T, s string) string {
	t.Helper()
	q, err := tomlString(s)
	if err != nil {
		t.Fatal(err)
	}
	return q
}

// A tokendrop-proxy config — valid, no [platform], no [miner] — gains only
// the two missing tables. Its own bytes stay the file's prefix, and the
// result loads.
func TestSetupAppendsOnlyTheMissingTablesToAProxyConfig(t *testing.T) {
	s := newSetupSandbox(t)
	s.platform.claim("credits")
	if err := os.MkdirAll(s.home, 0o700); err != nil {
		t.Fatal(err)
	}
	proxy := "[proxy]\nlisten = \"127.0.0.1:8787\"\n\n[[provider]]\nname = \"search-router\"\nupstream = \"https://router.invalid.test\"\n\n" +
		"[mining]\nas_url = \"" + s.as.srv.URL + "\"\nchain_id = \"twilight-1\"\nslot_id = 7\nstate_dir = " + mustTOML(t, filepath.Join(s.home, "state")) + "\n"
	if err := os.WriteFile(s.cfgPath(), []byte(proxy), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := s.run(nil, false)
	if code != exitOK {
		t.Fatalf("setup exited %d\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
	}
	got := s.readFile(s.cfgPath())
	if !bytes.HasPrefix(got, []byte(proxy)) {
		t.Fatalf("the proxy config's own bytes were not kept as the prefix:\n%s", got)
	}
	platform, miner, err := configTablesDefined(got)
	if err != nil || !platform || !miner {
		t.Fatalf("tables after migration: platform=%v miner=%v err=%v\n%s", platform, miner, err, got)
	}
	if n := bytes.Count(got, []byte("[platform]")); n != 1 {
		t.Errorf("[platform] appears %d times", n)
	}
	cfg := loadSetupConfig(t, s.cfgPath())
	if cfg.Listen.Address != "127.0.0.1:8787" || !cfg.Miner.Enabled {
		t.Errorf("migrated config lost the proxy's settings or gained no miner: %+v %+v", cfg.Listen, cfg.Miner)
	}
}

// A valid config that already has [platform] gains only [miner].
func TestSetupAppendsOnlyMinerWhenPlatformExists(t *testing.T) {
	s := newSetupSandbox(t)
	s.platform.claim("credits")
	if err := os.MkdirAll(s.home, 0o700); err != nil {
		t.Fatal(err)
	}
	body := "[platform]\nbase_url = \"" + s.platform.srv.URL + "\"\nagents_api_url = \"" + s.platform.srv.URL + "\"\n\n" +
		"[mining]\nas_url = \"" + s.as.srv.URL + "\"\nchain_id = \"twilight-1\"\nslot_id = 7\nstate_dir = " + mustTOML(t, filepath.Join(s.home, "state")) + "\n"
	if err := os.WriteFile(s.cfgPath(), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := s.run(nil, false)
	if code != exitOK {
		t.Fatalf("setup exited %d\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
	}
	got := s.readFile(s.cfgPath())
	if !bytes.HasPrefix(got, []byte(body)) {
		t.Fatalf("existing bytes not kept:\n%s", got)
	}
	if n := bytes.Count(got, []byte("[platform]")); n != 1 {
		t.Fatalf("[platform] appears %d times, want 1:\n%s", n, got)
	}
	if n := bytes.Count(got, []byte("[miner]")); n != 1 {
		t.Fatalf("[miner] appears %d times, want 1:\n%s", n, got)
	}
	loadSetupConfig(t, s.cfgPath())
}

// A config that does not parse is refused whatever its text holds — here, a
// [miner] line — and nothing runs after the refusal.
func TestSetupRefusesAnInvalidConfigEvenWithMinerText(t *testing.T) {
	s := newSetupSandbox(t)
	if err := os.MkdirAll(s.home, 0o700); err != nil {
		t.Fatal(err)
	}
	body := "[miner]\nenabled = true\nthis is not toml\n"
	if err := os.WriteFile(s.cfgPath(), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := s.run(nil, false)
	if code == exitOK {
		t.Fatalf("setup accepted an invalid config\nstdout:\n%s", out)
	}
	if !strings.Contains(errOut, s.cfgPath()) {
		t.Errorf("refusal does not name the path:\n%s", errOut)
	}
	if got := s.readFile(s.cfgPath()); string(got) != body {
		t.Errorf("refused config was modified:\n%s", got)
	}
	if reg, _, _ := s.platform.counts(); reg != 0 {
		t.Errorf("connect ran after the refusal (%d registers)", reg)
	}
}

// A home path holding a space, a double quote, a backslash and shell
// metacharacters round-trips through the config and the profile block.
// Windows forbids the quote in a file name and uses the backslash as its
// separator, so its variant keeps the rest.
func TestSetupHomePathWithSpecialCharactersRoundTrips(t *testing.T) {
	s := newSetupSandbox(t)
	s.platform.claim("credits")
	weird := `we ird$;&'x`
	if runtime.GOOS != "windows" {
		weird = `we ird"q\b$;&'x`
	}
	s.home = filepath.Join(s.userHome, weird, ".tokendrop")
	s.env["TOKENDROP_HOME"] = s.home
	s.exe = filepath.Join(s.root, weird+"bin", "dropin-miner")
	if err := os.MkdirAll(filepath.Dir(s.exe), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.exe, []byte("x"), 0o700); err != nil { // #nosec G306 -- a placeholder in the test's own sandbox
		t.Fatal(err)
	}

	code, out, errOut := s.run(tty("n"), true, "-yes", "-no-agents")
	if code != exitOK {
		t.Fatalf("setup exited %d\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
	}
	cfg := loadSetupConfig(t, s.cfgPath())
	if cfg.Mining.StateDir != filepath.Join(s.home, "state") || cfg.Miner.IntakeDir != filepath.Join(s.home, "intake") {
		t.Fatalf("paths did not round-trip: state=%q intake=%q, want under %q", cfg.Mining.StateDir, cfg.Miner.IntakeDir, s.home)
	}
	if runtime.GOOS == "windows" {
		return
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Fatal("sh is required to source the profile block")
	}
	cmd := exec.Command(sh, "-c", `. "$1"; printf '%s\n%s\n' "$TOKENDROP_CONFIG" "$PATH"`, "sh", s.profilePath()) // #nosec G204 -- sh from PATH sourcing this test's own profile
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + s.userHome}
	got, err := cmd.Output()
	if err != nil {
		t.Fatalf("sourcing the profile failed: %v\n%s", err, s.readFile(s.profilePath()))
	}
	lines := strings.Split(strings.TrimRight(string(got), "\n"), "\n")
	if len(lines) != 2 || lines[0] != s.cfgPath() {
		t.Fatalf("TOKENDROP_CONFIG after sourcing = %q, want %q", lines, s.cfgPath())
	}
	if !strings.HasSuffix(lines[1], ":"+filepath.Dir(s.exe)) {
		t.Fatalf("PATH after sourcing = %q, want it to end with %q", lines[1], filepath.Dir(s.exe))
	}
	// Sourcing twice adds the directory once.
	cmd = exec.Command(sh, "-c", `. "$1"; . "$1"; printf '%s' "$PATH"`, "sh", s.profilePath()) // #nosec G204 -- as above
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + s.userHome}
	twice, err := cmd.Output()
	if err != nil || strings.Count(string(twice), filepath.Dir(s.exe)) != 1 {
		t.Fatalf("sourcing twice: PATH=%q err=%v", twice, err)
	}
}

// An installation already in the installation directory is used as it is:
// nothing is adopted, and a set-aside sibling is not even offered.
func TestSetupUsesAnInstallationAlreadyInHome(t *testing.T) {
	s := newSetupSandbox(t)
	store, err := auth.OpenStore(filepath.Join(s.home, "state"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveMiningEnabled(false); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveAgentRegistration(auth.AgentRegistration{AgentID: "prior", Status: "claimed", Scopes: []string{"credits"}}); err != nil {
		t.Fatal(err)
	}
	sibling := s.home + ".bak-1"
	writeInstallation(t, sibling, "wallet")
	siblingBefore := snapshotTree(t, sibling)

	_, out, _ := s.run(nil, true, "-yes", "-no-agents", "-no-profile")
	if !strings.Contains(out, "Previous installation found in "+s.home) {
		t.Errorf("did not report the installation in place:\n%s", out)
	}
	if strings.Contains(out, "set aside at") {
		t.Errorf("offered a sibling although home holds an installation:\n%s", out)
	}
	if !strings.Contains(out, "stored mining decision came with this installation: mining is OFF") {
		t.Errorf("did not announce the stored decision:\n%s", out)
	}
	if !reflect.DeepEqual(siblingBefore, snapshotTree(t, sibling)) {
		t.Error("the sibling was touched")
	}
}

// writeInstallation lays out a previous installation's files. parts names
// what it holds: wallet, identity (state + credentials), pending (only an
// interrupted registration), spool, config.
func writeInstallation(t *testing.T, dir string, parts ...string) {
	t.Helper()
	write := func(rel, body string) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, part := range parts {
		switch part {
		case "wallet":
			write(filepath.Join("wallet", walletKeyFile), "sealed-key")
			write(filepath.Join("wallet", walletSidecarFile), `{"address":"twilight1old"}`)
		case "identity":
			write(filepath.Join("state", "dpop.key"), "old-dpop")
			write(filepath.Join("state", "refresh.token"), "old-refresh")
			write(filepath.Join("state", "agent.json"), `{"agent_id":"old-agent","status":"claimed"}`)
			write(credentialsFile, `{"api_key":"sr-old"}`)
		case "pending":
			write(filepath.Join("state", "registration_pending.json"), `{"agent_id":"pending-agent"}`)
		case "spool":
			write(filepath.Join("spool", "unsent-1.json"), `{"v":1}`)
		case "config":
			write(setupConfigFile, "[mining]\nstate_dir = \"/nowhere\"\n")
		}
	}
}

// A set-aside installation, adopted at a terminal, moves by bundle: the
// identity whole, the wallet whole, the spool merged — and the source,
// emptied, is removed.
func TestSetupAdoptsASetAsideInstallationByBundles(t *testing.T) {
	s := newSetupSandbox(t)
	sibling := s.home + ".bak-20260101"
	writeInstallation(t, sibling, "wallet", "identity", "spool")
	before := snapshotTree(t, s.root)

	// Answer "use it", then connect's mining question. The adopted identity
	// is claimed on the platform by nobody; connect's own outcome is not this
	// case's concern.
	_, out, _ := s.run(tty("y", "n"), true, "-no-agents", "-no-profile")
	for _, want := range []string{"adopted the identity", "adopted the wallet", "adopted spool (1 moved", "removed the now-empty " + sibling} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	if got := string(s.readFile(filepath.Join(s.home, "state", "refresh.token"))); got != "old-refresh" {
		t.Errorf("refresh token = %q", got)
	}
	if got := string(s.readFile(filepath.Join(s.home, credentialsFile))); !strings.Contains(got, "sr-old") {
		t.Errorf("credentials did not travel with the state: %q", got)
	}
	if !lexists(filepath.Join(s.home, "wallet", walletKeyFile)) || !lexists(filepath.Join(s.home, "spool", "unsent-1.json")) {
		t.Error("wallet or spool not adopted")
	}
	if lexists(sibling) {
		t.Error("the emptied source was not removed")
	}
	if runtime.GOOS != "windows" {
		for _, p := range []string{filepath.Join(s.home, "state"), filepath.Join(s.home, "wallet")} {
			if info, err := os.Stat(p); err != nil || info.Mode().Perm() != 0o700 {
				t.Errorf("%s mode after adoption: %v %v", p, info.Mode().Perm(), err)
			}
		}
	}
	assertOwnership(t, before, snapshotTree(t, s.root), s.home, sibling)
}

// A source with more than setup adopts keeps the rest, with a note.
func TestSetupLeavesANonEmptySourceWithANote(t *testing.T) {
	s := newSetupSandbox(t)
	sibling := s.home + "-old"
	writeInstallation(t, sibling, "wallet")
	if err := os.WriteFile(filepath.Join(sibling, "notes.txt"), []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, out, _ := s.run(tty("y", "n"), true, "-no-agents", "-no-profile")
	if !strings.Contains(out, "left the rest of "+sibling+" in place (notes.txt)") {
		t.Errorf("no note for the non-empty source:\n%s", out)
	}
	if !lexists(filepath.Join(sibling, "notes.txt")) {
		t.Error("the participant's own file was not left in place")
	}
}

// Without a terminal nothing is adopted, whatever -yes says, and the path is
// printed.
func TestSetupDoesNotAdoptWithoutATerminal(t *testing.T) {
	s := newSetupSandbox(t)
	s.platform.claim("credits")
	sibling := s.home + ".old"
	writeInstallation(t, sibling, "wallet", "identity")
	siblingBefore := snapshotTree(t, sibling)
	_, out, _ := s.run(nil, false, "-yes")
	if !strings.Contains(out, "Not an interactive shell — not touching it. Move it to "+s.home) {
		t.Errorf("did not print the path:\n%s", out)
	}
	if !reflect.DeepEqual(siblingBefore, snapshotTree(t, sibling)) {
		t.Error("the set-aside installation was touched without a terminal")
	}
	if lexists(filepath.Join(s.home, "wallet")) {
		t.Error("a wallet was adopted without a terminal")
	}
}

// A source holding only an interrupted registration is an installation, and
// its state is adopted — ignoring it would mint a second identity.
func TestSetupAdoptsASourceHoldingOnlyAPendingRegistration(t *testing.T) {
	s := newSetupSandbox(t)
	sibling := s.home + ".bak"
	writeInstallation(t, sibling, "pending")
	_, out, _ := s.run(tty("y", "n"), true, "-no-agents", "-no-profile")
	if !strings.Contains(out, "A previous installation is set aside at "+sibling) {
		t.Fatalf("a pending registration did not count as an installation:\n%s", out)
	}
	if !lexists(filepath.Join(s.home, "state", "registration_pending.json")) && !lexists(filepath.Join(s.home, "state", "agent.json")) {
		t.Errorf("the pending registration was not adopted:\n%s", out)
	}
}

// A destination state/ that holds only an unfinished enrollment's DPoP key is
// set aside whole, and the adopted identity takes its place.
func TestSetupSetsAsideAnUnenrolledDestinationState(t *testing.T) {
	s := newSetupSandbox(t)
	if err := os.MkdirAll(filepath.Join(s.home, "state"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.home, "state", "dpop.key"), []byte("unfinished"), 0o600); err != nil {
		t.Fatal(err)
	}
	sibling := s.home + ".bak"
	writeInstallation(t, sibling, "identity")
	_, out, _ := s.run(tty("y", "n"), true, "-no-agents", "-no-profile")
	aside := filepath.Join(s.home, "state.unenrolled-"+fixedSetupClock().UTC().Format("20060102150405"))
	if got := string(s.readFile(filepath.Join(aside, "dpop.key"))); got != "unfinished" {
		t.Fatalf("unfinished key not set aside whole at %s (%q):\n%s", aside, got, out)
	}
	if got := string(s.readFile(filepath.Join(s.home, "state", "dpop.key"))); got != "old-dpop" {
		t.Errorf("adopted state's key = %q, want the enrolled one", got)
	}
}

// A destination that already holds an identity is a conflict, and setup
// stops on it: non-zero, before connect is ever invoked, with both paths and
// the instruction to choose one installation. Nothing moves — not the
// identity, and not the wallet that would otherwise have been adopted after
// it. A state/ with a participation secret is identity without being an
// installation marker, so this is reachable end to end.
func TestSetupStopsOnAnIdentityConflictBeforeConnect(t *testing.T) {
	s := newSetupSandbox(t)
	if err := os.MkdirAll(filepath.Join(s.home, "state"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.home, "state", "participation.secret"), []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	sibling := s.home + ".bak"
	writeInstallation(t, sibling, "identity", "wallet")
	siblingBefore := snapshotTree(t, sibling)
	before := snapshotTree(t, s.root)

	code, out, errOut := s.run(tty("y", "n"), true, "-yes", "-no-agents", "-no-profile")
	if code == exitOK {
		t.Fatalf("setup exited 0 after an identity conflict\nstdout:\n%s\nstderr:\n%s", out, errOut)
	}
	if s.connectCalls != 0 {
		t.Fatalf("connect was invoked %d time(s) after an identity conflict", s.connectCalls)
	}
	if reg, status, _ := s.platform.counts(); reg+status != 0 {
		t.Errorf("the platform was reached after an identity conflict: register=%d status=%d", reg, status)
	}
	for _, want := range []string{s.home, filepath.Join(s.home, "state", "participation.secret"), sibling, "Choose one installation", "run setup again"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("conflict report missing %q:\n%s", want, errOut)
		}
	}
	if !reflect.DeepEqual(siblingBefore, snapshotTree(t, sibling)) {
		t.Error("the source changed despite the conflict")
	}
	if lexists(filepath.Join(s.home, "wallet")) || lexists(filepath.Join(s.home, credentialsFile)) {
		t.Error("something was adopted despite the conflict")
	}
	if strings.Contains(out, "Setup complete.") {
		t.Error("setup narrated completion after stopping")
	}
	assertOwnership(t, before, snapshotTree(t, s.root), s.home)
}

// assertAdoptionStopped is what every fatal adoption leaves behind: a
// non-zero exit, connect never invoked, the platform never reached, a report
// naming the bundle and both installations, and no narration of success.
func (s *setupSandbox) assertAdoptionStopped(t *testing.T, code int, out, errOut, bundle, source string) {
	t.Helper()
	if code == exitOK {
		t.Fatalf("setup exited 0 after a failed adoption\nstdout:\n%s\nstderr:\n%s", out, errOut)
	}
	if s.connectCalls != 0 {
		t.Errorf("connect was invoked %d time(s) after a failed adoption", s.connectCalls)
	}
	if reg, status, enroll := s.platform.counts(); reg+status+enroll != 0 {
		t.Errorf("the platform was reached after a failed adoption: register=%d status=%d enroll=%d", reg, status, enroll)
	}
	for _, want := range []string{"could not adopt the " + bundle, source, s.home} {
		if !strings.Contains(errOut, want) {
			t.Errorf("failure report missing %q:\n%s", want, errOut)
		}
	}
	if strings.Contains(out, "Setup complete.") {
		t.Error("setup narrated completion after a failed adoption")
	}
}

// failMoveFrom makes adoption's move fail for one source path and behave
// normally for every other.
func failMoveFrom(path string) func(from, to string) error {
	return func(from, to string) error {
		if from == path {
			return errors.New("injected move failure")
		}
		return fsx.MoveFileDurable(from, to)
	}
}

// assertCustodyAtSource is the outer transaction's invariant when it fails:
// the accepted installation's identity and wallet are both exactly where they
// started, and the destination holds neither.
func (s *setupSandbox) assertCustodyAtSource(t *testing.T, sibling string, before map[string]fileSig) {
	t.Helper()
	if after := snapshotTree(t, sibling); !reflect.DeepEqual(before, after) {
		t.Error("the source installation is not exactly as it was: the identity or the wallet was not rolled back")
	}
	for _, rel := range []string{credentialsFile, filepath.Join("state", "refresh.token"), filepath.Join("state", "agent.json"), filepath.Join("wallet", walletKeyFile)} {
		if lexists(filepath.Join(s.home, rel)) {
			t.Errorf("the destination holds %s after a failed custody transaction", rel)
		}
	}
}

// A refused wallet — a symlink, or not a directory at all — after the
// identity was adopted rolls the identity back too: both stay at the source,
// the destination holds neither, and setup stops before connect with nothing
// after the wallet moved.
func TestSetupStopsWhenTheWalletIsRefused(t *testing.T) {
	for _, c := range []struct {
		name  string
		setup func(t *testing.T, s *setupSandbox, sibling string)
	}{
		{"not a directory", func(t *testing.T, _ *setupSandbox, sibling string) {
			if err := os.WriteFile(filepath.Join(sibling, "wallet"), []byte("not a wallet directory"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"symlink", func(t *testing.T, s *setupSandbox, sibling string) {
			if runtime.GOOS == "windows" {
				t.Skip("creating symlinks needs a privilege the Windows runner does not grant")
			}
			elsewhere := filepath.Join(s.root, "elsewhere-wallet")
			writeInstallation(t, elsewhere, "wallet")
			if err := os.Symlink(filepath.Join(elsewhere, "wallet"), filepath.Join(sibling, "wallet")); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newSetupSandbox(t)
			sibling := s.home + ".bak"
			writeInstallation(t, sibling, "identity", "spool")
			c.setup(t, s, sibling)
			before := snapshotTree(t, sibling)

			code, out, errOut := s.run(tty("y", "n"), true, "-no-agents", "-no-profile")
			s.assertAdoptionStopped(t, code, out, errOut, "wallet", sibling)
			if !strings.Contains(out, "adopted the identity") {
				t.Fatalf("the identity stage did not run first, so its rollback is not being tested:\n%s", out)
			}
			if !strings.Contains(errOut, "the identity and the wallet are both where they started") {
				t.Errorf("the report does not say the identity was rolled back:\n%s", errOut)
			}
			s.assertCustodyAtSource(t, sibling, before)
			if lexists(filepath.Join(s.home, "spool", "unsent-1.json")) {
				t.Error("a later bundle (spool) moved after the wallet was refused")
			}
		})
	}
}

// The wallet failing to move, or to be restricted after its move, once the
// identity has moved: the same invariant.
func TestSetupRollsBackTheIdentityWhenTheWalletMoveOrRestrictionFails(t *testing.T) {
	for _, c := range []string{"move", "restriction"} {
		t.Run(c, func(t *testing.T) {
			s := newSetupSandbox(t)
			sibling := s.home + ".bak"
			writeInstallation(t, sibling, "identity", "wallet")
			if c == "move" {
				s.move = failMoveFrom(filepath.Join(sibling, "wallet"))
			} else {
				dstWallet := filepath.Join(s.home, "wallet")
				s.restrict = func(path string, dir bool) error {
					if path == dstWallet {
						return errors.New("injected restriction failure")
					}
					return restrictToOwner(path, dir)
				}
			}
			before := snapshotTree(t, sibling)

			code, out, errOut := s.run(tty("y", "n"), true, "-no-agents", "-no-profile")
			s.assertAdoptionStopped(t, code, out, errOut, "wallet", sibling)
			if !strings.Contains(out, "adopted the identity") {
				t.Fatalf("the identity stage did not run first:\n%s", out)
			}
			s.assertCustodyAtSource(t, sibling, before)
			if !dirEmpty(filepath.Join(s.home, "state")) {
				t.Error("the destination's empty state/ was not restored")
			}
		})
	}
}

// A destination state/ set aside by the identity stage comes back when the
// wallet stage then fails, alongside the identity itself.
func TestSetupRestoresASetAsideStateWhenTheWalletFailsAfterTheIdentity(t *testing.T) {
	s := newSetupSandbox(t)
	if err := os.MkdirAll(filepath.Join(s.home, "state"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.home, "state", "dpop.key"), []byte("unfinished"), 0o600); err != nil {
		t.Fatal(err)
	}
	sibling := s.home + ".bak"
	writeInstallation(t, sibling, "identity", "wallet")
	s.move = failMoveFrom(filepath.Join(sibling, "wallet"))
	before := snapshotTree(t, sibling)

	code, out, errOut := s.run(tty("y", "n"), true, "-no-agents", "-no-profile")
	s.assertAdoptionStopped(t, code, out, errOut, "wallet", sibling)
	if !strings.Contains(out, "set aside "+filepath.Join(s.home, "state")) || !strings.Contains(out, "adopted the identity") {
		t.Fatalf("the identity stage did not set the state aside and adopt first:\n%s", out)
	}
	s.assertCustodyAtSource(t, sibling, before)
	if got, err := os.ReadFile(filepath.Join(s.home, "state", "dpop.key")); err != nil || string(got) != "unfinished" { // #nosec G304 -- the sandbox's own file
		t.Fatalf("the set-aside destination state was not restored: %q %v", got, err)
	}
	if matches, _ := filepath.Glob(filepath.Join(s.home, "state.unenrolled-*")); len(matches) != 0 {
		t.Errorf("a set-aside copy was left behind: %v", matches)
	}
}

// A non-empty destination wallet/ is partial state: it is set aside as
// wallet.incomplete-<time> and the chosen source wallet is adopted; if the
// transaction then fails, it comes back and the source keeps its wallet.
func TestSetupSetsAsideANonEmptyDestinationWallet(t *testing.T) {
	partial := func(t *testing.T, s *setupSandbox) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(s.home, "wallet"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(s.home, "wallet", walletSidecarFile), []byte("partial"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	incomplete := func(s *setupSandbox) string {
		return filepath.Join(s.home, "wallet.incomplete-"+fixedSetupClock().UTC().Format("20060102150405"))
	}

	t.Run("adopted", func(t *testing.T) {
		s := newSetupSandbox(t)
		partial(t, s)
		sibling := s.home + ".bak"
		writeInstallation(t, sibling, "identity", "wallet")
		_, out, errOut := s.run(tty("y", "n"), true, "-no-agents", "-no-profile")
		if strings.Contains(errOut, "could not adopt") {
			t.Fatalf("adoption failed:\n%s", errOut)
		}
		if got, err := os.ReadFile(filepath.Join(incomplete(s), walletSidecarFile)); err != nil || string(got) != "partial" { // #nosec G304 -- the sandbox's own file
			t.Fatalf("the partial destination wallet was not set aside: %q %v\n%s", got, err, out)
		}
		if got, err := os.ReadFile(filepath.Join(s.home, "wallet", walletKeyFile)); err != nil || string(got) != "sealed-key" { // #nosec G304 -- the sandbox's own file
			t.Fatalf("the chosen source wallet was not adopted: %q %v", got, err)
		}
		if lexists(filepath.Join(sibling, "wallet")) {
			t.Error("the source wallet is still at the source")
		}
	})

	t.Run("rolled back", func(t *testing.T) {
		s := newSetupSandbox(t)
		partial(t, s)
		sibling := s.home + ".bak"
		writeInstallation(t, sibling, "identity", "wallet")
		dstWallet := filepath.Join(s.home, "wallet")
		s.restrict = func(path string, dir bool) error {
			if path == dstWallet {
				return errors.New("injected restriction failure")
			}
			return restrictToOwner(path, dir)
		}
		before := snapshotTree(t, sibling)
		code, out, errOut := s.run(tty("y", "n"), true, "-no-agents", "-no-profile")
		s.assertAdoptionStopped(t, code, out, errOut, "wallet", sibling)
		if got, err := os.ReadFile(filepath.Join(dstWallet, walletSidecarFile)); err != nil || string(got) != "partial" { // #nosec G304 -- the sandbox's own file
			t.Fatalf("the set-aside destination wallet was not restored: %q %v", got, err)
		}
		if lexists(incomplete(s)) {
			t.Error("a wallet.incomplete copy was left behind after the rollback")
		}
		if !reflect.DeepEqual(before, snapshotTree(t, sibling)) {
			t.Error("the source installation is not exactly as it was")
		}
		if lexists(filepath.Join(s.home, credentialsFile)) || lexists(filepath.Join(dstWallet, walletKeyFile)) {
			t.Error("the destination holds part of the source's custody after the rollback")
		}
	})
}

// A symlink anywhere inside the source state/ stops setup with zero connect
// and zero platform calls, and nothing of the identity moves.
func TestSetupStopsOnASymlinkInsideTheSourceState(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating symlinks needs a privilege the Windows runner does not grant")
	}
	s := newSetupSandbox(t)
	sibling := s.home + ".bak"
	writeInstallation(t, sibling, "identity", "wallet")
	if err := os.Symlink(filepath.Join(s.root, "anywhere"), filepath.Join(sibling, "state", "linked")); err != nil {
		t.Fatal(err)
	}
	siblingBefore := snapshotTree(t, sibling)

	code, out, errOut := s.run(tty("y", "n"), true, "-no-agents", "-no-profile")
	s.assertAdoptionStopped(t, code, out, errOut, identityBundle, sibling)
	if !strings.Contains(errOut, "symlink") {
		t.Errorf("the reason does not name the symlink:\n%s", errOut)
	}
	if !reflect.DeepEqual(siblingBefore, snapshotTree(t, sibling)) {
		t.Error("the source changed although the identity was refused")
	}
	if lexists(filepath.Join(s.home, credentialsFile)) || lexists(filepath.Join(s.home, "state", "refresh.token")) || lexists(filepath.Join(s.home, "wallet")) {
		t.Error("something was adopted after the identity was refused")
	}
}

// The source state/ failing to move stops setup before connect, with
// everything the identity step changed put back.
func TestSetupStopsWhenTheSourceStateMoveFails(t *testing.T) {
	s := newSetupSandbox(t)
	sibling := s.home + ".bak"
	writeInstallation(t, sibling, "identity", "wallet")
	s.move = failMoveFrom(filepath.Join(sibling, "state"))
	siblingBefore := snapshotTree(t, sibling)

	code, out, errOut := s.run(tty("y", "n"), true, "-no-agents", "-no-profile")
	s.assertAdoptionStopped(t, code, out, errOut, identityBundle, sibling)
	if !strings.Contains(errOut, "was put back") {
		t.Errorf("the report does not say the step was rolled back:\n%s", errOut)
	}
	if !reflect.DeepEqual(siblingBefore, snapshotTree(t, sibling)) {
		t.Error("the source changed although its state could not move")
	}
	if !dirEmpty(filepath.Join(s.home, "state")) {
		t.Error("the destination's empty state/ was not restored")
	}
}

// The key failing to move after the state has moved puts the state back
// where it came from: half an identity is never left in either place.
func TestSetupRestoresTheSourceStateWhenTheKeyMoveFails(t *testing.T) {
	s := newSetupSandbox(t)
	sibling := s.home + ".bak"
	writeInstallation(t, sibling, "identity")
	s.move = failMoveFrom(filepath.Join(sibling, credentialsFile))
	siblingBefore := snapshotTree(t, sibling)

	code, out, errOut := s.run(tty("y", "n"), true, "-no-agents", "-no-profile")
	s.assertAdoptionStopped(t, code, out, errOut, identityBundle, sibling)
	if !reflect.DeepEqual(siblingBefore, snapshotTree(t, sibling)) {
		t.Error("the source state was not put back exactly as it was")
	}
	if lexists(filepath.Join(s.home, "state", "refresh.token")) || lexists(filepath.Join(s.home, credentialsFile)) {
		t.Error("half the identity was left in the destination")
	}
}

// A failed restriction after a move is a failed move, rolled back the same
// way.
func TestSetupTreatsAFailedRestrictionAsAFailedMove(t *testing.T) {
	s := newSetupSandbox(t)
	sibling := s.home + ".bak"
	writeInstallation(t, sibling, "identity")
	dstCreds := filepath.Join(s.home, credentialsFile)
	s.restrict = func(path string, dir bool) error {
		if path == dstCreds {
			return errors.New("injected restriction failure")
		}
		return restrictToOwner(path, dir)
	}
	siblingBefore := snapshotTree(t, sibling)

	code, out, errOut := s.run(tty("y", "n"), true, "-no-agents", "-no-profile")
	s.assertAdoptionStopped(t, code, out, errOut, identityBundle, sibling)
	if !reflect.DeepEqual(siblingBefore, snapshotTree(t, sibling)) {
		t.Error("the source was not put back after a failed restriction")
	}
	if lexists(dstCreds) {
		t.Error("an unrestricted key was left in the destination")
	}
}

// A destination state/ set aside for an unfinished enrollment comes back when
// the adoption then fails, and no set-aside copy is left behind.
func TestSetupRestoresASetAsideDestinationStateWhenAdoptionFails(t *testing.T) {
	s := newSetupSandbox(t)
	if err := os.MkdirAll(filepath.Join(s.home, "state"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.home, "state", "dpop.key"), []byte("unfinished"), 0o600); err != nil {
		t.Fatal(err)
	}
	sibling := s.home + ".bak"
	writeInstallation(t, sibling, "identity")
	s.move = failMoveFrom(filepath.Join(sibling, credentialsFile))

	code, out, errOut := s.run(tty("y", "n"), true, "-no-agents", "-no-profile")
	s.assertAdoptionStopped(t, code, out, errOut, identityBundle, sibling)
	if got, err := os.ReadFile(filepath.Join(s.home, "state", "dpop.key")); err != nil || string(got) != "unfinished" { // #nosec G304 -- the sandbox's own file
		t.Fatalf("the destination state was not restored: %q %v", got, err)
	}
	matches, _ := filepath.Glob(filepath.Join(s.home, "state.unenrolled-*"))
	if len(matches) != 0 {
		t.Errorf("a set-aside copy was left behind: %v", matches)
	}
	if !lexists(filepath.Join(sibling, "state", "refresh.token")) {
		t.Error("the source state was not put back")
	}
}

// A rollback that cannot finish stops at once and names every path a part of
// the bundle is now at.
func TestSetupNamesEveryPathWhenARollbackFails(t *testing.T) {
	s := newSetupSandbox(t)
	sibling := s.home + ".bak"
	writeInstallation(t, sibling, "identity")
	srcCreds, dstState := filepath.Join(sibling, credentialsFile), filepath.Join(s.home, "state")
	s.move = func(from, to string) error {
		if from == srcCreds || from == dstState {
			return errors.New("injected move failure")
		}
		return fsx.MoveFileDurable(from, to)
	}

	code, out, errOut := s.run(tty("y", "n"), true, "-no-agents", "-no-profile")
	s.assertAdoptionStopped(t, code, out, errOut, identityBundle, sibling)
	if !strings.Contains(errOut, "Putting it back failed too") {
		t.Fatalf("the failed rollback is not reported:\n%s", errOut)
	}
	for _, p := range []string{dstState, srcCreds} {
		if !strings.Contains(errOut, "  "+p+"\n") {
			t.Errorf("surviving path %s not listed:\n%s", p, errOut)
		}
	}
}

// An adopted config goes through the migration policy like any other.
func TestSetupMigratesAnAdoptedConfig(t *testing.T) {
	s := newSetupSandbox(t)
	sibling := s.home + ".bak"
	writeInstallation(t, sibling, "wallet")
	proxy := "[mining]\nas_url = \"" + s.as.srv.URL + "\"\nchain_id = \"twilight-1\"\nslot_id = 7\nstate_dir = " + mustTOML(t, filepath.Join(s.home, "state")) + "\n"
	if err := os.WriteFile(filepath.Join(sibling, setupConfigFile), []byte(proxy), 0o600); err != nil {
		t.Fatal(err)
	}
	_, out, _ := s.run(tty("y", "n"), true, "-no-agents", "-no-profile")
	if !strings.Contains(out, "adopted the config") || !strings.Contains(out, "Added [platform] and [miner] to "+s.cfgPath()) {
		t.Fatalf("adopted config not migrated:\n%s", out)
	}
	if got := s.readFile(s.cfgPath()); !bytes.HasPrefix(got, []byte(proxy)) {
		t.Errorf("adopted config's bytes not kept:\n%s", got)
	}
}

// -dry-run writes nothing, moves nothing and runs neither connect nor the
// agents step.
func TestSetupDryRunWritesNothing(t *testing.T) {
	s := newSetupSandbox(t)
	s.onPath["claude"] = true
	writeInstallation(t, s.home+".bak", "wallet", "identity")
	before := snapshotTree(t, s.root)
	code, out, errOut := s.run(tty("y"), true, "-dry-run", "-yes")
	if code != exitOK {
		t.Fatalf("dry run exited %d\n%s\n%s", code, out, errOut)
	}
	if after := snapshotTree(t, s.root); !reflect.DeepEqual(before, after) {
		assertOwnership(t, before, after) // names every changed path
		t.Fatal("dry run changed the sandbox")
	}
	if reg, status, _ := s.platform.counts(); reg+status != 0 {
		t.Errorf("dry run reached the platform: register=%d status=%d", reg, status)
	}
	for _, want := range []string{"would move", "would write " + s.cfgPath(), "would run:", "write  ~" + string(filepath.Separator) + ".claude"} {
		if !strings.Contains(out, want) {
			t.Errorf("dry run output missing %q:\n%s", want, out)
		}
	}
}

func countOccurrences(s, sub string) int { return strings.Count(s, sub) }

// -with codex sets up codex whether or not it was detected.
func TestSetupWithInstallsATargetRegardlessOfDetection(t *testing.T) {
	s := newSetupSandbox(t)
	s.platform.claim("credits")
	before := snapshotTree(t, s.root)
	code, out, errOut := s.run(nil, false, "-with", "codex")
	if code != exitOK {
		t.Fatalf("setup exited %d\n%s\n%s", code, out, errOut)
	}
	paths := s.paths()
	if !lexists(paths.codexSkill) {
		t.Fatalf("codex was not set up:\n%s", out)
	}
	assertOwnership(t, before, snapshotTree(t, s.root), append([]string{s.home}, targetOwnedPaths(paths, "codex")...)...)
}

// -with codex -with codex plans and writes codex once.
func TestSetupWithDeduplicatesRepeatedIDs(t *testing.T) {
	s := newSetupSandbox(t)
	s.platform.claim("credits")
	code, out, errOut := s.run(nil, false, "-with", "codex", "-with", "CODEX")
	if code != exitOK {
		t.Fatalf("setup exited %d\n%s\n%s", code, out, errOut)
	}
	skill := "wrote " + tilde(s.userHome, s.paths().codexSkill)
	if n := countOccurrences(out, skill); n != 1 {
		t.Fatalf("%q appears %d times, want once:\n%s", skill, n, out)
	}
}

// -no-agents -with codex installs exactly codex, even with other agents
// detected.
func TestSetupNoAgentsWithCodexInstallsExactlyCodex(t *testing.T) {
	s := newSetupSandbox(t)
	s.platform.claim("credits")
	s.onPath["claude"] = true
	s.onPath["cursor"] = true
	before := snapshotTree(t, s.root)
	// "n" answers the mining question, which -yes deliberately does not
	// (S6). Before #81 an empty tty answered it by accident: the read
	// error was discarded and the resulting empty line counted as "no".
	code, out, errOut := s.run(tty("n"), true, "-yes", "-no-profile", "-no-agents", "-with", "codex")
	if code != exitOK {
		t.Fatalf("setup exited %d\n%s\n%s", code, out, errOut)
	}
	paths := s.paths()
	if !lexists(paths.codexSkill) || lexists(paths.claudeSkill) || lexists(paths.cursorSkill) {
		t.Fatalf("want exactly codex: codex=%v claude=%v cursor=%v", lexists(paths.codexSkill), lexists(paths.claudeSkill), lexists(paths.cursorSkill))
	}
	assertOwnership(t, before, snapshotTree(t, s.root), append([]string{s.home}, targetOwnedPaths(paths, "codex")...)...)
}

// ── D2 (#59): a dry run's agent plan matches the real run's ─────────────

// configFixtureOutcome names which of the three outcomes planSetupConfig
// can reach a test wants set up on disk before setup ever runs.
type configFixtureOutcome int

const (
	fixtureConfigFresh configFixtureOutcome = iota
	fixtureConfigLeft
	fixtureConfigMigrated
)

// seedConfigFixture writes what outcome needs onto s's disk before setup
// runs: nothing (fresh), a complete config with [miner] (left, untouched),
// or a proxy-only config with [mining] but no [miner] (migrated — setup
// adds the missing tables).
func seedConfigFixture(t *testing.T, s *setupSandbox, outcome configFixtureOutcome) {
	t.Helper()
	if outcome == fixtureConfigFresh {
		return
	}
	if err := os.MkdirAll(s.home, 0o700); err != nil {
		t.Fatal(err)
	}
	var data []byte
	switch outcome {
	case fixtureConfigLeft:
		v, err := resolveSetupValues(s.home, s.getenv, false)
		if err != nil {
			t.Fatal(err)
		}
		data, err = renderFreshConfig(v)
		if err != nil {
			t.Fatal(err)
		}
	case fixtureConfigMigrated:
		data = []byte("[mining]\nas_url = " + mustTOML(t, s.as.srv.URL) + "\nchain_id = \"twilight-1\"\nslot_id = 7\nstate_dir = " +
			mustTOML(t, filepath.Join(s.home, "state")) + "\n")
	}
	if err := os.WriteFile(s.cfgPath(), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// agentPlanWriteLines drives setup -with id, with or without -dry-run and
// against one of the three config outcomes already seeded on disk, and
// returns the set of paths printPlan listed under "write": the exact text
// both a dry run and the real run print for the plan, before the real run
// goes on to commit it. Comparing this set between the two, for a fresh
// installation, an existing one, and one being migrated, is the literal
// test D.2 (#59) asks for.
func agentPlanWriteLines(t *testing.T, id string, dry bool, outcome configFixtureOutcome) map[string]bool {
	t.Helper()
	s := newSetupSandbox(t)
	s.platform.claim("credits")
	seedConfigFixture(t, s, outcome)
	args := []string{"-with", id}
	if dry {
		args = append(args, "-dry-run")
	}
	code, out, errOut := s.run(nil, false, args...)
	if code != exitOK {
		t.Fatalf("setup -with %s (dry=%v outcome=%v): exit %d\n%s%s", id, dry, outcome, code, out, errOut)
	}
	const prefix = "    write  "
	set := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		rest := line[len(prefix):]
		i := strings.Index(rest, "  (")
		if i < 0 {
			t.Fatalf("write line has no trailing (why): %q", line)
		}
		set[rest[:i]] = true
	}
	if len(set) == 0 {
		t.Fatalf("setup -with %s (dry=%v outcome=%v) planned no writes:\n%s", id, dry, outcome, out)
	}
	return set
}

func assertSameStringSet(t *testing.T, label string, dry, real map[string]bool) {
	t.Helper()
	for p := range dry {
		if !real[p] {
			t.Errorf("%s: dry run listed %s, the real run never wrote it", label, p)
		}
	}
	for p := range real {
		if !dry[p] {
			t.Errorf("%s: the real run wrote %s, the dry run never listed it", label, p)
		}
	}
}

// TestDryRunAgentPlanPathsMatchTheRealRunAcrossAllHosts guards D.2 (#59):
// on a fresh installation or one being migrated, the Codex plan a dry run
// prints used to list only the skill plus an advisory note (fresh) or the
// pre-migration roots (migrated), because codexSandboxRoots read the
// config from disk and the real run's config either did not exist yet or
// had not gained its [miner] table yet — the dry run never publishes it
// (validateConfigSyntax's own invariant: nothing is written anywhere to
// load it through pkg/config). setup's dry run now plans every agent step
// from the config it would write or add, parsed by the config package
// itself (config.LoadBytes) from those exact bytes, so its listing equals
// the real run's writes for both outcomes. The existing-config ("left")
// case is already correct — both a dry run and the real run read the same
// bytes off disk, unmodified — and is pinned here so it cannot regress
// alongside the other two.
func TestDryRunAgentPlanPathsMatchTheRealRunAcrossAllHosts(t *testing.T) {
	requireGoldenSequence(t)
	cases := []struct {
		name    string
		outcome configFixtureOutcome
	}{
		{"fresh", fixtureConfigFresh},
		{"existing", fixtureConfigLeft},
		{"migrated", fixtureConfigMigrated},
	}
	for _, id := range goldenHostIDs {
		for _, tc := range cases {
			t.Run(tc.name+"/"+id, func(t *testing.T) {
				dry := agentPlanWriteLines(t, id, true, tc.outcome)
				real := agentPlanWriteLines(t, id, false, tc.outcome)
				assertSameStringSet(t, id, dry, real)
			})
		}
	}
}

// assertSameWriteContent is the content parity test's literal assertion:
// the set of paths written and the bytes written to each must be identical
// between the two plans.
func assertSameWriteContent(t *testing.T, label string, dry, real agentPlan) {
	t.Helper()
	dryContents := map[string][]byte{}
	for _, w := range dry.writes {
		dryContents[w.path] = w.contents
	}
	realContents := map[string][]byte{}
	for _, w := range real.writes {
		realContents[w.path] = w.contents
	}
	for p, c := range dryContents {
		rc, ok := realContents[p]
		if !ok {
			t.Errorf("%s: dry run plans a write to %s the real run's plan does not", label, p)
			continue
		}
		if !bytes.Equal(c, rc) {
			t.Errorf("%s: content for %s differs:\n--- dry ---\n%s\n--- real ---\n%s", label, p, c, rc)
		}
	}
	for p := range realContents {
		if _, ok := dryContents[p]; !ok {
			t.Errorf("%s: the real run plans a write to %s the dry run's plan does not", label, p)
		}
	}
}

// normalizeAgentPlanRoots replaces every occurrence of root — a sandbox's
// own temp-directory root, embedded in every path a plan names and in
// every rendered command line a skill or a Codex sandbox block carries —
// with one fixed placeholder. Two plans captured from two different
// sandboxes never share the same root, so without this every path and
// every rendered command would differ for a reason that has nothing to do
// with behavior.
func normalizeAgentPlanRoots(p agentPlan, root string) agentPlan {
	// A path a plan names is the raw root; a path a plan RENDERS is not
	// always escaped just once. TOML, Go %q and JSON each double a
	// backslash, and an escaping layer can nest: a Claude/Cursor allow
	// rule or hook command is built with %q (one doubling) and that whole
	// command string is then a JSON string value in settings.json/
	// hooks.json (a second doubling on top), so the SAME root's
	// backslashes appear doubled in a Codex sandbox block or a bare
	// (unquoted) command segment, but quadrupled inside a quoted command
	// segment embedded in JSON. On Windows the raw root's own backslashes
	// then never occur as a contiguous run inside that quadrupled text at
	// all (TestAgentsHookAndAllowRuleMatchingSurvivesAWindowsStyleBinaryPath
	// is this same defect, guarded on the production side). Try the most
	// escaped form first, so its already-doubled backslashes are not
	// partly consumed by a shorter form's replacement first. A no-op on
	// every other OS, where root has no backslash to double at any depth.
	forms := []string{root}
	for i := 0; i < 2; i++ {
		forms = append(forms, strings.ReplaceAll(forms[len(forms)-1], `\`, `\\`))
	}
	repl := func(s string) string {
		for i := len(forms) - 1; i >= 0; i-- {
			s = strings.ReplaceAll(s, forms[i], "<ROOT>")
		}
		return s
	}
	replBytes := func(b []byte) []byte { return []byte(repl(string(b))) }
	out := agentPlan{skipped: p.skipped, refused: p.refused}
	for _, w := range p.writes {
		out.writes = append(out.writes, agentWrite{
			surface:  w.surface,
			path:     repl(w.path),
			contents: replBytes(w.contents),
			mode:     w.mode,
			why:      w.why,
		})
	}
	for _, r := range p.removes {
		out.removes = append(out.removes, agentRemove{surface: r.surface, path: repl(r.path)})
	}
	for _, n := range p.notes {
		out.notes = append(out.notes, repl(n))
	}
	return out
}

// TestNormalizeAgentPlanRootsHandlesWindowsStyleEscaping guards
// normalizeAgentPlanRoots against the exact defect
// TestAgentsHookAndAllowRuleMatchingSurvivesAWindowsStyleBinaryPath guards
// elsewhere: a root's backslashes never occur as a contiguous run inside
// content that quoted it (Go %q, TOML, JSON all double a backslash), so a
// replacement that only tries the raw root leaves rendered content
// untouched on Windows — a synthetic Windows-style root here, not an
// actual Windows path, so this runs on every OS the test matrix does.
func TestNormalizeAgentPlanRootsHandlesWindowsStyleEscaping(t *testing.T) {
	root := `C:\Users\runner\AppData\Local\Temp\TestName123`
	once := strings.ReplaceAll(root, `\`, `\\`)
	twice := strings.ReplaceAll(once, `\`, `\\`)
	plan := agentPlan{writes: []agentWrite{{
		surface: "Claude Code",
		path:    root + `\user\.claude\settings.json`,
		// The shapes actually seen in a real settings.json: a raw
		// (unquoted) command segment escaped once by JSON alone, and a
		// %q-quoted command segment escaped once for the quoting and
		// again for JSON — the case the first version of this fix missed.
		contents: []byte(`{"allow":["Bash(` + once + `\\bin\\dropin-miner search:*)",` +
			`"Bash(\"` + twice + `\\\\bin\\\\dropin-miner\" search -config \"` + twice + `\\\\user\\\\.tokendrop\\\\tokendrop.toml\":*)"]}`),
	}}}
	got := normalizeAgentPlanRoots(plan, root)
	w := got.writes[0]
	if strings.Contains(w.path, root) {
		t.Errorf("path still carries the raw root: %s", w.path)
	}
	content := string(w.contents)
	if strings.Contains(content, root) || strings.Contains(content, once) || strings.Contains(content, twice) {
		t.Errorf("content still carries the root, raw or escaped at some depth:\n%s", content)
	}
	if !strings.Contains(w.path, "<ROOT>") || strings.Count(content, "<ROOT>") != 3 {
		t.Errorf("expected one <ROOT> in the path and three in the content:\npath: %s\ncontent: %s", w.path, content)
	}
}

// agentObservedPlan drives setup -with id (optionally -dry-run) against a
// fresh sandbox seeded for outcome, and returns the plan agentsStep itself
// built — captured through setupDeps' agentPlanObserver seam, not a plan
// the test computed on its own — along with the sandbox that produced it
// (its root is what normalizeAgentPlanRoots needs).
func agentObservedPlan(t *testing.T, id string, dry bool, outcome configFixtureOutcome) (agentPlan, *setupSandbox) {
	t.Helper()
	s := newSetupSandbox(t)
	s.platform.claim("credits")
	seedConfigFixture(t, s, outcome)
	var captured agentPlan
	observed := false
	s.agentPlanObserver = func(p agentPlan) { captured = p; observed = true }
	args := []string{"-with", id}
	if dry {
		args = append(args, "-dry-run")
	}
	code, out, errOut := s.run(nil, false, args...)
	if code != exitOK {
		t.Fatalf("setup -with %s (dry=%v outcome=%v): exit %d\n%s%s", id, dry, outcome, code, out, errOut)
	}
	if !observed {
		t.Fatalf("setup -with %s (dry=%v outcome=%v) never reached agentsStep's plan", id, dry, outcome)
	}
	if len(captured.writes) == 0 {
		t.Fatalf("setup -with %s (dry=%v outcome=%v) observed a plan with no writes", id, dry, outcome)
	}
	return captured, s
}

// TestDryRunAgentPlanContentMatchesTheRealRunAcrossAllHostsAndOutcomes is
// D.2 (#59)'s content half: the path-set test above can agree on WHICH
// paths a dry run and the real run write while still disagreeing on WHAT
// they write there — a fresh or migrated config that renders with the
// wrong number of Codex sandbox roots (Miner.Enabled reads false until
// [miner] actually lands, even though finishMiner already derives
// intake_dir/sessions_dir from the state dir before checking it) changes
// the Codex config.toml content, not its existence. For every host and all
// three outcomes, this drives the setup CLI itself — once with -dry-run,
// once without — against two independent sandboxes, capturing each run's
// actual agentsStep plan through the observer seam (setupDeps.
// agentPlanObserver), and asserts every planned write's bytes are
// identical once both sandboxes' own temp-directory roots are normalized
// to the same placeholder. Driving the CLI, not buildInstallPlan directly,
// is the point: a version of this test that built its own entry and called
// buildInstallPlan itself passed even when agentsStep's own wiring of
// entry.rendered was mutated away, because it never exercised that wiring
// at all.
func TestDryRunAgentPlanContentMatchesTheRealRunAcrossAllHostsAndOutcomes(t *testing.T) {
	requireGoldenSequence(t)
	cases := []struct {
		name    string
		outcome configFixtureOutcome
	}{
		{"fresh", fixtureConfigFresh},
		{"existing", fixtureConfigLeft},
		{"migrated", fixtureConfigMigrated},
	}
	for _, id := range goldenHostIDs {
		for _, tc := range cases {
			t.Run(tc.name+"/"+id, func(t *testing.T) {
				dryPlan, dryS := agentObservedPlan(t, id, true, tc.outcome)
				realPlan, realS := agentObservedPlan(t, id, false, tc.outcome)
				assertSameWriteContent(t, id,
					normalizeAgentPlanRoots(dryPlan, dryS.root),
					normalizeAgentPlanRoots(realPlan, realS.root))
			})
		}
	}
}

// The npm launch marker: an exec cache and a project-local install are
// refused before anything is written; a global install and a non-npm binary
// are accepted.
func TestSetupRefusesNpmCopiesThatWillBeDiscarded(t *testing.T) {
	cases := []struct {
		exe, launch string
		ok          bool
	}{
		{"/usr/local/bin/dropin-miner", "", true},
		{"/usr/local/lib/node_modules/dropin-miner/bin/dropin-miner", "npm:global", true},
		{"/home/u/proj/node_modules/dropin-miner/bin/dropin-miner", "npm:local", false},
		{"/home/u/proj/node_modules/dropin-miner/bin/dropin-miner", "npm:unknown", false},
		{"/home/u/.npm/_npx/9f/node_modules/dropin-miner/bin/dropin-miner", "npm:local", false},
		{"/home/u/.npm/_npx/9f/node_modules/dropin-miner/bin/dropin-miner", "npm:global", false},
		{`C:\Users\u\AppData\Local\npm-cache\_npx\9f\node_modules\dropin-miner\bin\dropin-miner.exe`, "", false},
		{"/home/u/.npm/_cacache/tmp/dropin-miner", "", false},
		{"/opt/dropin-miner", "something-else", false},
		{"/usr/local/lib/node_modules/dropin-miner/bin/dropin-miner", "", false},
		{`C:\Users\u\AppData\Roaming\npm\node_modules\dropin-miner\bin\dropin-miner.exe`, "", false},
	}
	for _, c := range cases {
		err := checkSetupLaunch(c.exe, c.launch)
		if (err == nil) != c.ok {
			t.Errorf("checkSetupLaunch(%q, %q) = %v, want ok=%v", c.exe, c.launch, err, c.ok)
		}
		if err != nil && !errors.Is(err, errNpmLaunch) && !errors.Is(err, errNpmDirect) {
			t.Errorf("refusal for %q does not name the supported route: %v", c.exe, err)
		}
		// The exec cache is refused as such first; any other node_modules
		// binary with no marker was run around the launcher.
		inCache := strings.Contains(c.exe, "_npx") || strings.Contains(c.exe, "_cacache") || strings.Contains(c.exe, "npm-cache")
		if c.launch == "" && strings.Contains(c.exe, "node_modules") && !inCache && !errors.Is(err, errNpmDirect) {
			t.Errorf("checkSetupLaunch(%q, unset) = %v, want the direct-run refusal", c.exe, err)
		}
	}

	s := newSetupSandbox(t)
	s.env["DROPIN_MINER_LAUNCH"] = "npm:local"
	before := snapshotTree(t, s.root)
	code, _, errOut := s.run(nil, false)
	if code != exitUsage || !strings.Contains(errOut, "npm install -g dropin-miner") {
		t.Fatalf("a project-local npm copy was not refused: exit %d\n%s", code, errOut)
	}
	delete(s.env, "DROPIN_MINER_LAUNCH")
	s.exe = filepath.Join(s.root, "node_modules", "dropin-miner", "bin", "dropin-miner")
	code, _, errOut = s.run(nil, false)
	if code != exitUsage || !strings.Contains(errOut, "run the dropin-miner command npm installed") {
		t.Fatalf("npm's binary run directly was not refused: exit %d\n%s", code, errOut)
	}
	if !reflect.DeepEqual(before, snapshotTree(t, s.root)) {
		t.Error("a refused launch wrote something")
	}
}

// The JS launcher's side of the marker, against fixture paths.
func TestNpmLauncherClassifiesItsInstall(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal("node is required to verify the npm launcher")
	}
	script := `
const m = require(process.argv[1]);
const has = (set) => (f) => set.includes(f);
const cases = [
  ["/usr/local/lib/node_modules/dropin-miner", "/usr/local/bin/node", {}, "linux", has([]), "global"],
  ["/home/u/.nvm/versions/node/v22/lib/node_modules/dropin-miner", "/home/u/.nvm/versions/node/v22/bin/node", {}, "linux", has([]), "global"],
  ["/home/u/.npm-global/lib/node_modules/dropin-miner", "/usr/bin/node", {npm_config_prefix: "/home/u/.npm-global"}, "linux", has([]), "global"],
  ["/home/u/.npm-global/lib/node_modules/dropin-miner", "/usr/bin/node", {}, "linux", has(["/home/u/.npm-global/bin/dropin-miner"]), "global"],
  ["/home/u/proj/node_modules/dropin-miner", "/usr/local/bin/node", {}, "linux", has([]), "local"],
  ["/home/u/lib/node_modules/dropin-miner", "/usr/local/bin/node", {}, "linux", has([]), "local"],
  ["/home/u/.npm/_npx/9f/node_modules/dropin-miner", "/usr/local/bin/node", {}, "linux", has([]), "local"],
  ["/opt/unpacked/dropin-miner", "/usr/local/bin/node", {}, "linux", has([]), "unknown"],
  ["C:\\Users\\u\\AppData\\Roaming\\npm\\node_modules\\dropin-miner", "C:\\Program Files\\nodejs\\node.exe", {}, "win32", has(["C:\\Users\\u\\AppData\\Roaming\\npm\\dropin-miner.cmd"]), "global"],
  ["C:\\Program Files\\nodejs\\node_modules\\dropin-miner", "C:\\Program Files\\nodejs\\node.exe", {}, "win32", has([]), "global"],
  ["C:\\src\\proj\\node_modules\\dropin-miner", "C:\\Program Files\\nodejs\\node.exe", {}, "win32", has([]), "local"],
];
const bad = [];
for (const [pkg, exec, env, platform, exists, want] of cases) {
  const got = m.launchKind(pkg, exec, env, platform, exists);
  if (got !== want) bad.push(pkg + ": got " + got + ", want " + want);
}
if (bad.length) { console.log(bad.join("\n")); process.exit(1); }
`
	launcher, err := filepath.Abs(filepath.Join("..", "..", "npm", "bin", "dropin-miner.js"))
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, "-e", script, launcher).CombinedOutput() // #nosec G204 -- node from PATH running a fixed script against this repo's launcher
	if err != nil {
		t.Fatalf("launcher classification: %v\n%s", err, out)
	}
}

// A profile that is a symlink is edited through the link: the target is
// replaced in its own directory with its mode, and the link stays a link.
func TestSetupEditsASymlinkedProfileThroughTheLink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no profile on Windows")
	}
	s := newSetupSandbox(t)
	s.platform.claim("credits")
	dotfiles := filepath.Join(s.userHome, "dotfiles")
	if err := os.MkdirAll(dotfiles, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dotfiles, "zshrc")
	if err := os.WriteFile(target, []byte("alias ll='ls -l'\n"), 0o640); err != nil { // #nosec G306 -- a profile with a deliberate non-default mode, to prove it is kept
		t.Fatal(err)
	}
	if err := os.Symlink(target, s.profilePath()); err != nil {
		t.Fatal(err)
	}
	before := snapshotTree(t, s.root)
	code, out, errOut := s.run(tty("n"), true, "-yes", "-no-agents")
	if code != exitOK {
		t.Fatalf("setup exited %d\n%s\n%s", code, out, errOut)
	}
	if info, err := os.Lstat(s.profilePath()); err != nil || info.Mode()&fs.ModeSymlink == 0 {
		t.Fatal("the profile link was replaced by a file")
	}
	got := s.readFile(target)
	if !bytes.HasPrefix(got, []byte("alias ll='ls -l'\n")) || profileBlockCount(got) != 1 {
		t.Fatalf("target not edited through the link:\n%s", got)
	}
	if info, _ := os.Stat(target); info.Mode().Perm() != 0o640 {
		t.Errorf("target mode = %v, want 0640 kept", info.Mode().Perm())
	}
	assertOwnership(t, before, snapshotTree(t, s.root), s.home, s.profilePath(), target)
}

// Markers that are not one well-formed block are refused: the profile is left
// byte for byte, the lines are printed, and setup continues.
func TestSetupRefusesMalformedProfileMarkers(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no profile on Windows")
	}
	s := newSetupSandbox(t)
	s.platform.claim("credits")
	body := "export A=1\n" + profileMarkerStart + "\nexport OLD=1\n# the end marker was deleted by hand\nexport B=2\n"
	if err := os.WriteFile(s.profilePath(), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := s.run(tty("n"), true, "-yes", "-no-agents")
	if code != exitOK {
		t.Fatalf("setup exited %d; a malformed profile must not stop it\n%s\n%s", code, out, errOut)
	}
	if got := s.readFile(s.profilePath()); string(got) != body {
		t.Fatalf("malformed profile was edited:\n%s", got)
	}
	if !strings.Contains(out, "Not touching "+s.profilePath()) || !strings.Contains(out, "export TOKENDROP_CONFIG=") {
		t.Errorf("refusal or lines to add by hand not printed:\n%s", out)
	}
	if !strings.Contains(out, "Setup complete") {
		t.Error("setup did not continue after the refusal")
	}
}

// `setup -h` and `setup --help` print setup's usage and exit 0: the
// installers' capability probe. On the base commit — before setup existed —
// the same probe exits non-zero as an unknown command, which is what the
// bridge's legacy branch relies on.
func TestSetupHelpIsTheCapabilityProbe(t *testing.T) {
	for _, flag := range []string{"-h", "--help", "-help"} {
		var out, errOut bytes.Buffer
		d := setupDeps{stdout: &out, stderr: &errOut}
		if code := setupMain(d, []string{flag}); code != exitOK {
			t.Errorf("setup %s exited %d, want 0", flag, code)
		}
		if !strings.Contains(out.String(), "usage: dropin-miner setup") || !strings.Contains(out.String(), allTargetIDs()) {
			t.Errorf("setup %s did not print setup's usage:\n%s", flag, out.String())
		}
	}

	const base = "37906ec85b6f41da66b9125c74db9cd0963599dd"
	root := moduleRoot(t)
	if err := exec.Command("git", "-C", root, "cat-file", "-e", base+"^{commit}").Run(); err != nil { // #nosec G204 -- fixed git arguments
		t.Logf("base commit %s is not in this clone (a shallow checkout); the pre-setup half of this test runs where it is", base)
		return
	}
	src := t.TempDir()
	archive := exec.Command("git", "-C", root, "archive", "--format=tar", base, "go.mod", "go.sum", "cmd", "pkg") // #nosec G204 -- fixed git arguments
	untar := exec.Command("tar", "-x", "-C", src)                                                                 // #nosec G204 -- tar from PATH into this test's own temp dir
	pipe, err := archive.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	untar.Stdin = pipe
	if err := untar.Start(); err != nil {
		t.Fatal(err)
	}
	if err := archive.Run(); err != nil {
		t.Fatal(err)
	}
	if err := untar.Wait(); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "dropin-miner-base")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	build := exec.Command("go", "build", "-o", bin, "./cmd/dropin-miner") // #nosec G204 -- this test's own temp paths
	build.Dir = src
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build base commit: %v\n%s", err, out)
	}
	var errOut bytes.Buffer
	probe := exec.Command(bin, "setup", "-h") // #nosec G204 -- the binary this test just built
	probe.Stderr = &errOut
	err = probe.Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() == 0 || !strings.Contains(errOut.String(), `unknown command "setup"`) {
		t.Fatalf("base commit's `setup -h`: err=%v stderr=%q, want a non-zero unknown-command exit", err, errOut.String())
	}
}

// Windows: the journal records added_by_setup false when the PATH entry was
// already there, keeps the original previous_value across a rerun, and the
// rerun changes nothing.
func TestSetupWindowsJournalRecordsDeltas(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("the User environment is Windows'; planUserEnvironment's own test covers the logic everywhere")
	}
	s := newSetupSandbox(t)
	s.platform.claim("credits")
	binDir := filepath.Join(s.home, "bin")
	s.userEnv.values["Path"] = `C:\Windows;` + strings.ToUpper(binDir) + `\`
	s.userEnv.values["TOKENDROP_CONFIG"] = `C:\old\tokendrop.toml`

	if code, out, errOut := s.run(tty("n"), true, "-yes", "-no-agents"); code != exitOK {
		t.Fatalf("setup exited %d\n%s\n%s", code, out, errOut)
	}
	journalPath := filepath.Join(s.home, setupEnvJournalFile)
	first, err := readEnvJournal(journalPath)
	if err != nil || first == nil {
		t.Fatalf("journal: %v %v", first, err)
	}
	if first.Path.AddedBySetup {
		t.Error("added_by_setup = true for a PATH entry that was already there")
	}
	if !first.TokendropConfig.PreviousPresent || first.TokendropConfig.PreviousValue != `C:\old\tokendrop.toml` {
		t.Errorf("previous TOKENDROP_CONFIG not recorded: %+v", first.TokendropConfig)
	}
	if s.userEnv.values["TOKENDROP_CONFIG"] != s.cfgPath() {
		t.Errorf("TOKENDROP_CONFIG = %q", s.userEnv.values["TOKENDROP_CONFIG"])
	}
	journalBytes := s.readFile(journalPath)

	if code, out, errOut := s.run(tty(), true, "-yes", "-no-agents"); code != exitOK {
		t.Fatalf("rerun exited %d\n%s\n%s", code, out, errOut)
	}
	if got := s.readFile(journalPath); !bytes.Equal(got, journalBytes) {
		t.Errorf("journal changed on a rerun:\n%s\n->\n%s", journalBytes, got)
	}
	second, _ := readEnvJournal(journalPath)
	if second.TokendropConfig.PreviousValue != `C:\old\tokendrop.toml` {
		t.Errorf("rerun overwrote previous_value with %q", second.TokendropConfig.PreviousValue)
	}
}

// The config setup writes is the config setup.sh wrote. The fixtures were
// captured by running scripts/setup.sh on the base commit (37906ec) with a
// stub binary, TOKENDROP_HOME set to a scratch directory and stdin from
// /dev/null — once plain, once with TOKENDROP_MINING=1 and a payout address
// — and that directory replaced by {{HOME}}. The two are compared as loaded
// Configs, with the home's directories normalized, because setup.sh spelled
// them with a forward slash on every platform.
func TestSetupConfigEqualsSetupShFixture(t *testing.T) {
	for _, c := range []struct {
		fixture string
		mining  bool
	}{
		{"setup_sh_default.toml", false},
		{"setup_sh_mining.toml", true},
	} {
		t.Run(c.fixture, func(t *testing.T) {
			dir := t.TempDir()
			home := filepath.Join(dir, "home")
			raw, err := os.ReadFile(filepath.Join("testdata", "setup", c.fixture)) // #nosec G304 -- fixed testdata path
			if err != nil {
				t.Fatal(err)
			}
			// The script wrote the home raw between quotes; TOML-escape it
			// for the substitution (the script itself could not, which is
			// half of why this command exists).
			escaped := strings.Trim(mustTOML(t, home), `"`)
			script := filepath.Join(dir, "script.toml")
			if err := os.WriteFile(script, bytes.ReplaceAll(raw, []byte("{{HOME}}"), []byte(escaped)), 0o600); err != nil { // #nosec G703 -- this test's own temp dir
				t.Fatal(err)
			}

			env := map[string]string{}
			if c.mining {
				env["TOKENDROP_MINING"] = "1"
				env["TOKENDROP_PAYOUT_ADDRESS"] = "twilight1k5stzqa2sgvfgx9u04cv93pek3gcmm9h5t9hkn"
			}
			v, err := resolveSetupValues(home, func(k string) string { return env[k] }, false)
			if err != nil {
				t.Fatal(err)
			}
			data, err := renderFreshConfig(v)
			if err != nil {
				t.Fatal(err)
			}
			ours := filepath.Join(dir, "setup.toml")
			if err := os.WriteFile(ours, data, 0o600); err != nil {
				t.Fatal(err)
			}

			normalize := func(c *config.Config) *config.Config {
				c.Mining.StateDir = filepath.Clean(c.Mining.StateDir)
				c.Mining.SpoolDir = filepath.Clean(c.Mining.SpoolDir)
				c.Miner.IntakeDir = filepath.Clean(c.Miner.IntakeDir)
				c.Miner.SessionsDir = filepath.Clean(c.Miner.SessionsDir)
				return c
			}
			want, got := normalize(loadSetupConfig(t, script)), normalize(loadSetupConfig(t, ours))
			if !reflect.DeepEqual(want, got) {
				t.Fatalf("setup's config differs from setup.sh's:\n--- setup.sh ---\n%+v\n--- setup ---\n%+v\n--- setup's bytes ---\n%s", want, got, data)
			}
		})
	}
}

// Setup runs no process other than itself: no setup source file imports
// os/exec. Agent detection is a PATH lookup in agents.go, and connect runs
// in-process. A source-reading guard, so it is proven by editing a file, not
// by an overlay.
func TestSetupFilesNeverImportOSExec(t *testing.T) {
	files, err := filepath.Glob("setup*.go")
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), f, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range parsed.Imports {
			if imp.Path.Value == `"os/exec"` {
				t.Errorf("%s imports os/exec: setup must run no process other than itself", f)
			}
		}
		checked++
	}
	if checked < 6 {
		t.Fatalf("checked %d setup source files, want at least 6: %v", checked, files)
	}
}
