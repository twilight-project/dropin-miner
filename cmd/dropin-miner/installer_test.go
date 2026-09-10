package main

// scripts/setup.sh, exercised for real: a built binary, a real `sh`, real
// httptest stubs for the platform and the AS. What matters here is what
// the shell script itself writes and invokes — connect's own behavior
// (interactive vs. scripted, the poll, the enrollment) is already covered
// exhaustively at the Go level; these tests are the installer's half of
// design item 1 ("every install ends with a decision on file") and item 5
// ("must not carry a stale mining_decision.json across without saying so").
//
// Every run here is non-interactive by construction: exec.Cmd's Stdin,
// left nil, connects the child to the OS's null device, which is never a
// terminal — the same shape `curl | sh` gives a real install (the pipe
// that fed the script text to `sh` leaves nothing behind for `read` to
// see). setup.sh's own `[ -t 0 ]` checks and connect's `isInteractive`
// both see the same thing a piped install would.
//
// A registration is pre-claimed on the stub BEFORE setup.sh runs (the
// stub is agent-ID-agnostic — claim state is one flag, not keyed by ID),
// so connect's first poll already finds it claimed and the whole
// register-ask-poll-enroll-declare pass completes in one call. Without
// this every test here would wait out the real (production, not
// test-shortened) 3-minute foreground poll budget: a separately built
// binary does not share this package's test-only var overrides.
import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/twilight-project/dropin-miner/pkg/auth"
)

var installerTestBinary struct {
	mu   sync.Mutex
	path string
}

// buildInstallerTestBinary builds the real dropin-miner binary once per
// test process and reuses it — these tests build a config setup.sh has
// no other way to exercise, so a fake or stubbed binary would not prove
// anything about the shell script's own behavior.
func buildInstallerTestBinary(t *testing.T) string {
	t.Helper()
	installerTestBinary.mu.Lock()
	defer installerTestBinary.mu.Unlock()
	if installerTestBinary.path != "" {
		if _, err := os.Stat(installerTestBinary.path); err == nil {
			return installerTestBinary.path
		}
	}
	dir, err := os.MkdirTemp("", "dropin-miner-installer-test-bin")
	if err != nil {
		t.Fatal(err)
	}
	name := "dropin-miner"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	bin := filepath.Join(dir, name)
	cmd := exec.Command("go", "build", "-o", bin, ".") // #nosec G204 -- bin is this test's own t.TempDir() path, not external input
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build test binary: %v\n%s", err, out)
	}
	installerTestBinary.path = bin
	return bin
}

func setupShPath(t *testing.T) string {
	t.Helper()
	path := filepath.Join("..", "..", "scripts", "setup.sh")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("setup.sh not found at %s: %v", path, err)
	}
	return path
}

// runSetupSh runs the real script under `sh`, non-interactively (see the
// package doc comment above). extraEnv is merged over the process's own
// environment, so TOKENDROP_HOME/TOKENDROP_BIN/the stub URLs below are
// the only things a case needs to name explicitly.
func runSetupSh(t *testing.T, bin, home string, extraEnv map[string]string) (code int, stdout, stderr string) {
	t.Helper()
	cmd := exec.Command("sh", setupShPath(t)) // #nosec G204 -- this repo's own scripts/setup.sh, a fixed relative path
	env := append([]string{}, os.Environ()...)
	env = append(env, "TOKENDROP_BIN="+bin, "TOKENDROP_HOME="+home)
	for k, v := range extraEnv {
		env = append(env, k+"="+v)
	}
	cmd.Env = env
	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	err := cmd.Run()
	if err == nil {
		return 0, out.String(), errOut.String()
	}
	if exitErr, ok := err.(*exec.ExitError); ok {
		return exitErr.ExitCode(), out.String(), errOut.String()
	}
	t.Fatalf("run setup.sh: %v\nstdout:\n%s\nstderr:\n%s", err, out.String(), errOut.String())
	return -1, out.String(), errOut.String()
}

// tomlSection returns the text between a "[section]" header and the next
// one (or end of file) — enough to assert a key is, or is not, present
// without a full TOML parser.
func tomlSection(t *testing.T, content, header string) string {
	t.Helper()
	idx := strings.Index(content, header)
	if idx < 0 {
		t.Fatalf("section %q not found in:\n%s", header, content)
	}
	rest := content[idx+len(header):]
	if next := strings.Index(rest, "\n["); next >= 0 {
		rest = rest[:next]
	}
	return rest
}

func commonInstallerEnv(platformURL, asURL string) map[string]string {
	return map[string]string{ // #nosec G101 -- env var names, not credentials; no secret value here
		"TOKENDROP_AS_URL":         asURL,
		"TOKENDROP_ROUTER_URL":     "https://router.invalid.test",
		"TOKENDROP_PLATFORM_URL":   platformURL,
		"TOKENDROP_AGENTS_API_URL": platformURL,
	}
}

// Design item 1: [mining] never says enabled = true unconditionally. A
// plain scripted run (no TOKENDROP_MINING opt-in) writes no `enabled` key
// at all — and still ends with a real decision (off) on file, because
// connect's own non-interactive path persists whatever [mining].enabled
// resolves to, key present or not.
func TestSetupShDefaultScriptedRunWritesNoUnconditionalEnabledAndEndsStopped(t *testing.T) {
	bin := buildInstallerTestBinary(t)
	platform := newStubPlatform(t)
	as := newStubAS(t)
	platform.claim("credits") // pre-claimed so the first poll resolves at once; see package doc comment

	home := filepath.Join(t.TempDir(), "home")
	code, out, errOut := runSetupSh(t, bin, home, commonInstallerEnv(platform.srv.URL, as.srv.URL))
	if code != 0 {
		t.Fatalf("setup.sh exited %d\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
	}

	cfg, err := os.ReadFile(filepath.Join(home, "tokendrop.toml")) // #nosec G304 -- this test's own t.TempDir()-rooted config file
	if err != nil {
		t.Fatal(err)
	}
	mining := tomlSection(t, string(cfg), "[mining]")
	if strings.Contains(mining, "enabled") {
		t.Errorf("[mining] has an enabled key with no TOKENDROP_MINING opt-in:\n%s", mining)
	}
	if !strings.Contains(mining, "as_url") || !strings.Contains(mining, "state_dir") {
		t.Errorf("[mining] missing expected keys:\n%s", mining)
	}
	platformSection := tomlSection(t, string(cfg), "[platform]")
	if !strings.Contains(platformSection, platform.srv.URL) {
		t.Errorf("[platform] does not point at the stub:\n%s", platformSection)
	}
	miner := tomlSection(t, string(cfg), "[miner]")
	if !strings.Contains(miner, "enabled") {
		t.Errorf("[miner] enabled missing (this key is unrelated to the mining decision and must stay unconditional):\n%s", miner)
	}

	store, err := auth.OpenStore(filepath.Join(home, "state"))
	if err != nil {
		t.Fatal(err)
	}
	enabled, ok, err := store.LoadMiningEnabled()
	if err != nil || !ok {
		t.Fatalf("no decision on file after setup.sh: ok=%v err=%v", ok, err)
	}
	if enabled {
		t.Fatal("decision = true, want false: nothing opted in")
	}
	if got, _, _ := platform.counts(); got != 1 {
		t.Fatalf("register calls = %d, want 1", got)
	}
}

// The scripted opt-in: TOKENDROP_MINING=1 writes enabled = true and (with
// TOKENDROP_PAYOUT_ADDRESS set) payout_address, and connect carries the
// whole thing through — registers, polls the pre-claimed agent, enrolls,
// declares — because a terminal never entered into it.
func TestSetupShScriptedMiningOptInEnrolsAndDeclares(t *testing.T) {
	bin := buildInstallerTestBinary(t)
	platform := newStubPlatform(t)
	as := newStubAS(t)
	platform.claim("mining") // pre-claimed with the mining scope granted

	const addr = "twilight1k5stzqa2sgvfgx9u04cv93pek3gcmm9h5t9hkn"
	home := filepath.Join(t.TempDir(), "home")
	env := commonInstallerEnv(platform.srv.URL, as.srv.URL)
	env["TOKENDROP_MINING"] = "1"
	env["TOKENDROP_PAYOUT_ADDRESS"] = addr
	code, out, errOut := runSetupSh(t, bin, home, env)
	if code != 0 {
		t.Fatalf("setup.sh exited %d\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
	}

	cfg, err := os.ReadFile(filepath.Join(home, "tokendrop.toml")) // #nosec G304 -- this test's own t.TempDir()-rooted config file
	if err != nil {
		t.Fatal(err)
	}
	mining := tomlSection(t, string(cfg), "[mining]")
	if !strings.Contains(mining, "enabled   = true") {
		t.Errorf("[mining] missing enabled = true despite TOKENDROP_MINING=1:\n%s", mining)
	}
	if !strings.Contains(mining, addr) {
		t.Errorf("[mining] missing the scripted payout_address:\n%s", mining)
	}

	store, err := auth.OpenStore(filepath.Join(home, "state"))
	if err != nil {
		t.Fatal(err)
	}
	enabled, ok, err := store.LoadMiningEnabled()
	if err != nil || !ok || !enabled {
		t.Fatalf("decision on file = enabled=%v ok=%v err=%v, want true", enabled, ok, err)
	}
	reg, ok := loadAgent(t, filepath.Join(home, "state"))
	if !ok || reg.LastEnrollmentSlot == "" {
		t.Fatalf("connect did not enroll: %+v ok=%v", reg, ok)
	}
	if got := as.declaredAddress(); got != addr {
		t.Fatalf("declared address = %q, want %q", got, addr)
	}
}

// Design item 5: a decision already on disk before setup.sh runs (an
// existing installation in $TOKENDROP_HOME, not a sibling needing the
// interactive adopt prompt) is announced, not silently carried across —
// checked and printed before connect ever runs, so it holds regardless of
// what connect itself goes on to do in this same invocation.
func TestSetupShReportsAStaleMiningDecisionFromAnExistingInstallation(t *testing.T) {
	bin := buildInstallerTestBinary(t)
	platform := newStubPlatform(t)
	as := newStubAS(t)

	home := filepath.Join(t.TempDir(), "home")
	store, err := auth.OpenStore(filepath.Join(home, "state"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveAgentRegistration(auth.AgentRegistration{
		AgentID: "prior-agent", Status: "claimed", Scopes: []string{"credits"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveMiningEnabled(false); err != nil {
		t.Fatal(err)
	}

	// Whatever connect does in this same run (this test never faked a
	// prior credentials.json, so it has no key to poll with) is not this
	// test's concern — only that the pre-existing decision was announced
	// before connect had any chance to run at all.
	_, out, _ := runSetupSh(t, bin, home, commonInstallerEnv(platform.srv.URL, as.srv.URL))
	if !strings.Contains(out, "stored mining decision") || !strings.Contains(out, "OFF") {
		t.Fatalf("setup.sh did not announce the pre-existing decision:\n%s", out)
	}
}
