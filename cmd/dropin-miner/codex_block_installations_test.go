package main

// #128: Codex's config.toml is one file, and our marked block in it is one
// slot, so it is #112's rule again — the rule PR #123 applied to the skill
// and the two JavaScript adapters and did not apply here.
//
// Measured on macOS in the 0.2.12 release check: a second installation's
// `agents install` replaced the machine installation's writable_roots with
// its own, and the scratch installation's later `agents uninstall` took the
// block away altogether. The machine's Codex searches then wrote nowhere,
// and nothing said to run `agents install` again.
//
// On real files, and with real configs: the roots are read out of the block
// and compared against the directory each installation's config sits in, so
// a fake filesystem would be testing the fixture rather than the rule.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// twoCodexInstallations is one machine, one binary, and two installations of
// it, each with its own config whose mining directories lie under its own
// home — which is how the block names the installation that wrote it.
type twoCodexInstallations struct {
	t      *testing.T
	ops    agentOps
	paths  agentPaths
	first  string // the machine installation's config
	second string // the disposable installation's config
}

func newTwoCodexInstallations(t *testing.T) *twoCodexInstallations {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ops := realAgentOps()
	ops.home = filepath.Join(root, "user")
	ops.lookPath = func(name string) (string, error) {
		if name == "codex" {
			return "/usr/local/bin/codex", nil
		}
		return "", errors.New("not found")
	}
	ops.executable = func() (string, error) {
		return filepath.Join(root, "user", ".tokendrop", "bin", "dropin-miner"), nil
	}
	ops.isTerminal = func() bool { return false }
	m := &twoCodexInstallations{t: t, ops: ops, paths: ops.paths(noEnv)}
	m.first = writeMiningHomeConfig(t, filepath.Join(root, "user", ".tokendrop"))
	m.second = writeMiningHomeConfig(t, filepath.Join(root, "dm-disposable"))
	return m
}

// writeMiningHomeConfig is one installation's home: a real config whose
// four mining directories sit under it, which is what codexSandboxRoots
// writes into the block and what the attribution reads back.
func writeMiningHomeConfig(t *testing.T, home string) string {
	t.Helper()
	for _, d := range []string{"state", "spool", "intake", "sessions"} {
		if err := os.MkdirAll(filepath.Join(home, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	doc := fmt.Sprintf(`
[mining]
enabled = true
as_url = "https://as.example.invalid"
chain_id = "twilight-1"
slot_id = 7
state_dir = %q
spool_dir = %q

[miner]
enabled = true
router_url = "https://router.example.invalid"
intake_dir = %q
sessions_dir = %q
`, filepath.ToSlash(filepath.Join(home, "state")), filepath.ToSlash(filepath.Join(home, "spool")),
		filepath.ToSlash(filepath.Join(home, "intake")), filepath.ToSlash(filepath.Join(home, "sessions")))
	cfg := filepath.Join(home, setupConfigFile)
	writeFileT(t, cfg, doc)
	return cfg
}

func (m *twoCodexInstallations) agents(args ...string) (int, string) {
	m.t.Helper()
	code, out, errOut := runAgents(m.t, m.ops, nil, args...)
	return code, out + errOut
}

func (m *twoCodexInstallations) install(cfg string) string {
	m.t.Helper()
	code, out := m.agents("install", "-yes", "-config", cfg, "-client", "codex")
	if code != exitOK {
		m.t.Fatalf("agents install -config %s: exit %d\n%s", cfg, code, out)
	}
	return out
}

// codexConfig is the whole of Codex's config.toml, which is what the
// assertion is about: not one setting of ours, every byte of the file.
func (m *twoCodexInstallations) codexConfig() string {
	m.t.Helper()
	b, err := os.ReadFile(m.paths.codexConfig) // #nosec G304 -- this test's own sandbox
	if err != nil {
		m.t.Fatalf("Codex's config.toml was not written, so its surviving would prove nothing: %v", err)
	}
	return string(b)
}

func (m *twoCodexInstallations) unchangedSince(before, what string) {
	m.t.Helper()
	if got := m.codexConfig(); got != before {
		m.t.Errorf("%s rewrote Codex's config.toml, which belongs to the installation configured by %s\n--- before ---\n%s\n--- after ---\n%s",
			what, m.first, before, got)
	}
}

// leftSentence is the skill's own sentence, which #128 asks the block to be
// named in too. The installation is named by its config because the block's
// roots lie under a home that holds one.
func (m *twoCodexInstallations) leftSentence() string {
	return "Codex: left in place; it belongs to the installation configured by " + m.first + ", not this installation"
}

// rootsOf is the writable_roots line as the file carries it, for a failure
// message that says which installation's roots are in there.
func rootsOf(file string) string {
	for _, l := range strings.Split(file, "\n") {
		if strings.HasPrefix(l, "writable_roots") {
			return l
		}
	}
	return "(no writable_roots line)"
}

func TestASecondInstallationLeavesTheFirstsCodexSandboxBlock(t *testing.T) {
	m := newTwoCodexInstallations(t)
	m.install(m.first)
	before := m.codexConfig()
	if !strings.Contains(rootsOf(before), filepath.Dir(m.first)) {
		t.Fatalf("the first installation's own install did not write its roots, so nothing below would prove anything:\n%s", before)
	}

	// The second installs: the command `setup -home` names in its closing
	// line, run by the same binary.
	out := m.install(m.second)
	m.unchangedSince(before, "the second installation's agents install")
	if !strings.Contains(out, m.leftSentence()) {
		t.Errorf("the install plan does not say whose the sandbox block is:\n%s", out)
	}
	// One fact, said once: the skill and the block are both the first's.
	if n := strings.Count(out, m.leftSentence()); n != 1 {
		t.Errorf("the sentence appears %d times, want 1:\n%s", n, out)
	}
	if strings.Contains(rootsOf(m.codexConfig()), filepath.Dir(m.second)) {
		t.Errorf("the second installation's roots replaced the first's: %s", rootsOf(m.codexConfig()))
	}

	// The second uninstalls: what it left it does not then take.
	code, out := m.agents("uninstall", "-yes", "-config", m.second, "-client", "codex")
	if code != exitOK {
		t.Fatalf("agents uninstall: exit %d\n%s", code, out)
	}
	m.unchangedSince(before, "the second installation's agents uninstall")
	if !strings.Contains(out, "it belongs to the installation configured by "+m.first+", not this installation") {
		t.Errorf("the uninstall plan does not say whose the block it left is:\n%s", out)
	}

	// And `agents status`, asked by the installation that wrote none of it,
	// names the one that did.
	code, out = m.agents("status", "-config", m.second)
	if code != exitOK {
		t.Fatalf("agents status: exit %d\n%s", code, out)
	}
	line := statusLineFor(t, out, "Codex")
	if !strings.Contains(line, "it belongs to the installation configured by "+m.first+", not this installation") {
		t.Errorf("status does not say whose Codex's files are:\n%s", line)
	}
}

// The block is left even when it is the only thing of the first
// installation's on the host: without this the sentence above could be the
// skill's alone, and the block's own refusal untested.
func TestTheCodexBlockIsLeftEvenWhenTheSkillIsGone(t *testing.T) {
	m := newTwoCodexInstallations(t)
	m.install(m.first)
	before := m.codexConfig()
	if err := os.RemoveAll(filepath.Dir(m.paths.codexSkill)); err != nil {
		t.Fatal(err)
	}

	out := m.install(m.second)
	m.unchangedSince(before, "the second installation's agents install")
	if !strings.Contains(out, m.leftSentence()) {
		t.Errorf("the install plan does not say whose the sandbox block is:\n%s", out)
	}
	// It continued with the rest of the host: the second's own skill is in.
	b, err := os.ReadFile(m.paths.codexSkill) // #nosec G304 -- this test's own sandbox
	if err != nil {
		t.Fatalf("leaving the block must not stop the rest of the host: %v", err)
	}
	if !strings.Contains(string(b), m.second) {
		t.Errorf("the second installation's skill was not written:\n%s", b)
	}
}

// An installation refreshing its own block is what it always was: a stale
// block is rewritten, and a current one is a no-op.
func TestAnInstallationStillRefreshesItsOwnCodexSandboxBlock(t *testing.T) {
	m := newTwoCodexInstallations(t)
	m.install(m.first)
	fresh := m.codexConfig()

	stale := strings.Replace(fresh, "network_access = true", "network_access = false", 1)
	if stale == fresh {
		t.Fatal("the block fixture did not change, so nothing about a refresh is being tested")
	}
	writeFileT(t, m.paths.codexConfig, stale)

	out := m.install(m.first)
	if got := m.codexConfig(); got != fresh {
		t.Errorf("an installation was refused its own block:\n%s\n--- file ---\n%s", out, got)
	}
	if strings.Contains(out, "left in place") {
		t.Errorf("an installation left its own block as though it were another's:\n%s", out)
	}
	if out = m.install(m.first); !strings.Contains(out, "nothing to do") {
		t.Errorf("a second identical install is not a no-op:\n%s", out)
	}
}

// A block whose roots cannot be read names no installation at all. It is
// left, like one that cannot be parsed, and the sentence says what could not
// be read rather than naming an owner it does not have.
func TestACodexBlockWithNoRootsIsLeftAndSaysWhy(t *testing.T) {
	m := newTwoCodexInstallations(t)
	handEdited := agentsMarkerBegin + "\n[" + codexSandboxTable + "]\nnetwork_access = true\n" + agentsMarkerEnd + "\n"
	writeFileT(t, m.paths.codexConfig, handEdited)

	out := m.install(m.first)
	if got := m.codexConfig(); got != handEdited {
		t.Errorf("a block that cannot be attributed was rewritten:\n%s", got)
	}
	if !strings.Contains(out, "its writable roots cannot be read") {
		t.Errorf("the plan does not say why the block was left:\n%s", out)
	}
}
