package main

// #112: a host has one skill directory, and the skill in it belongs to the
// installation that wrote it. A second installation's `agents install` leaves
// it and says whose it is; its `agents uninstall` leaves it and says the same.
//
// On real files, because the removal side attributes a skill DIRECTORY and
// reading one goes to the filesystem.

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// twoInstallations is one machine, one binary, and two installations of it:
// the machine's own, and a disposable one made by running the same copy.
type twoInstallations struct {
	t      *testing.T
	ops    agentOps
	paths  agentPaths
	first  string // the machine installation's config
	second string // the disposable installation's config
}

var singleSlotHosts = []string{"claude", "cursor", "opencode", "pi"}

func newTwoInstallations(t *testing.T) *twoInstallations {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ops := realAgentOps()
	ops.home = filepath.Join(root, "user")
	ops.lookPath = func(string) (string, error) { return "", errors.New("not found") }
	ops.executable = func() (string, error) { return filepath.Join(root, "user", ".tokendrop", "bin", "dropin-miner"), nil }
	ops.isTerminal = func() bool { return false }
	m := &twoInstallations{
		t: t, ops: ops, paths: ops.paths(noEnv),
		first:  filepath.Join(root, "user", ".tokendrop", setupConfigFile),
		second: filepath.Join(root, "dm-disposable", setupConfigFile),
	}
	writeFileT(t, m.first, "")
	writeFileT(t, m.second, "")
	return m
}

func (m *twoInstallations) agents(args ...string) (int, string) {
	m.t.Helper()
	var out, errOut bytes.Buffer
	code := agentsMain(m.ops, args, strings.NewReader(""), &out, &errOut, noEnv)
	return code, out.String() + errOut.String()
}

func (m *twoInstallations) install(cfg string) string {
	m.t.Helper()
	args := []string{"install", "-yes", "-config", cfg}
	for _, h := range singleSlotHosts {
		args = append(args, "-client", h)
	}
	code, out := m.agents(args...)
	if code != exitOK {
		m.t.Fatalf("agents install -config %s: exit %d\n%s", cfg, code, out)
	}
	return out
}

// singleSlot is every file a host has exactly one of.
func (m *twoInstallations) singleSlot() []string {
	return []string{m.paths.claudeSkill, m.paths.cursorSkill, m.paths.opencodePlugin, m.paths.piSkill, m.paths.piExtension}
}

func (m *twoInstallations) snapshot() map[string]string {
	m.t.Helper()
	out := map[string]string{}
	for _, p := range m.singleSlot() {
		b, err := os.ReadFile(p) // #nosec G304 -- this test's own sandbox
		if err != nil {
			m.t.Fatalf("%s was not written, so its surviving would prove nothing: %v", p, err)
		}
		out[p] = string(b)
	}
	return out
}

func (m *twoInstallations) unchangedSince(before map[string]string, what string) {
	m.t.Helper()
	for p, want := range before {
		b, err := os.ReadFile(p) // #nosec G304 -- this test's own sandbox
		switch {
		case err != nil:
			m.t.Errorf("%s removed %s, which belongs to the installation configured by %s", what, p, m.first)
		case string(b) != want:
			m.t.Errorf("%s rewrote %s, which belongs to the installation configured by %s:\n%s", what, p, m.first, b)
		}
	}
}

func (m *twoInstallations) leftSentence(label string) string {
	return label + ": left in place; it belongs to the installation configured by " + m.first + ", not this installation"
}

func TestASecondInstallationLeavesTheFirstsSkillsAtInstallAndAtUninstall(t *testing.T) {
	m := newTwoInstallations(t)
	m.install(m.first)
	before := m.snapshot()

	// The second installs: the command `setup -home` names in its closing line.
	out := m.install(m.second)
	m.unchangedSince(before, "the second installation's agents install")
	for _, label := range []string{"Claude Code", "Cursor", "opencode", "Pi"} {
		if !strings.Contains(out, m.leftSentence(label)) {
			t.Errorf("the install plan does not say whose %s's files are:\n%s", label, out)
		}
		if strings.Contains(out, label+": already installed") {
			t.Errorf("%s is reported as already installed, over a skill that is another installation's:\n%s", label, out)
		}
	}
	// One fact per host, said once: Pi has two such files.
	if n := strings.Count(out, m.leftSentence("Pi")); n != 1 {
		t.Errorf("Pi's sentence appears %d times, want 1:\n%s", n, out)
	}
	// It continued with the rest of the host: the second's hook entries are in.
	second := installationRef{bins: []string{mustExe(t, m.ops)}, cfg: m.second}
	for _, hooks := range []string{m.paths.claudeSettings, m.paths.cursorHooks} {
		found := false
		for _, e := range allHookEntries(t, hooks) {
			found = found || entryIsOurs(e, second)
		}
		if !found {
			t.Errorf("%s holds no hook entry of the second installation: leaving the skill must not stop the rest of the host", hooks)
		}
	}

	// The second uninstalls: its hook entries go, the first's files stay.
	args := []string{"uninstall", "-yes", "-config", m.second}
	for _, h := range singleSlotHosts {
		args = append(args, "-client", h)
	}
	code, out := m.agents(args...)
	if code != exitOK {
		t.Fatalf("agents uninstall: exit %d\n%s", code, out)
	}
	m.unchangedSince(before, "the second installation's agents uninstall")
	for _, label := range []string{"Claude Code", "Cursor", "opencode", "Pi"} {
		if !strings.Contains(out, m.leftSentence(label)) {
			t.Errorf("the uninstall plan does not say what it left of %s's and whose it is:\n%s", label, out)
		}
	}
	first := installationRef{bins: second.bins, cfg: m.first}
	for _, hooks := range []string{m.paths.claudeSettings, m.paths.cursorHooks} {
		kept := false
		for _, e := range allHookEntries(t, hooks) {
			if entryIsOurs(e, second) {
				t.Errorf("%s still holds an entry of the installation just uninstalled", hooks)
			}
			kept = kept || entryIsOurs(e, first)
		}
		if !kept {
			t.Errorf("%s lost the first installation's entries", hooks)
		}
	}
}

// One installation reinstalling over its own skill is what it always was: a
// stale skill is rewritten, and a current one is a no-op.
func TestAnInstallationStillRefreshesItsOwnSkill(t *testing.T) {
	m := newTwoInstallations(t)
	m.install(m.first)
	fresh := m.snapshot()
	for p, b := range fresh {
		writeFileT(t, p, "as an earlier version rendered it\n"+b)
	}
	out := m.install(m.first)
	m.unchangedSince(fresh, "a reinstall that should have restored the fresh rendering, and instead")
	if strings.Contains(out, "left in place") {
		t.Errorf("an installation left its own files as though they were another's:\n%s", out)
	}
	if out = m.install(m.first); !strings.Contains(out, "Claude Code: already installed") || !strings.Contains(out, "nothing to do") {
		t.Errorf("a second identical install is not a no-op:\n%s", out)
	}
}

// The config decides, not the binary: an installation whose binary moved
// still names the same config, and must be able to refresh its own skill.
func TestAMovedBinaryStillOwnsTheSkillItsConfigNames(t *testing.T) {
	m := newTwoInstallations(t)
	m.install(m.first)
	moved := filepath.Join(filepath.Dir(filepath.Dir(m.second)), "moved", "dropin-miner")
	m.ops.executable = func() (string, error) { return moved, nil }
	out := m.install(m.first)
	if strings.Contains(out, "left in place") {
		t.Fatalf("the same installation, run from a new path, was refused its own skill:\n%s", out)
	}
	if b, _ := os.ReadFile(m.paths.claudeSkill); !strings.Contains(string(b), moved) { // #nosec G304 -- this test's own sandbox
		t.Errorf("the skill was not re-rendered for the binary's new path:\n%s", b)
	}
}

// `agents prefer` rewrites every installed skill, so it goes through the same
// rule: the second installation's preference is not written into the first's.
func TestPreferDoesNotRewriteAnotherInstallationsSkill(t *testing.T) {
	m := newTwoInstallations(t)
	m.install(m.first)
	before := m.snapshot()
	if code, out := m.agents("prefer", "off", "-config", m.second); code != exitOK {
		t.Fatalf("agents prefer off: exit %d\n%s", code, out)
	}
	m.unchangedSince(before, "the second installation's agents prefer")
}

func mustExe(t *testing.T, ops agentOps) string {
	t.Helper()
	exe, err := ops.executable()
	if err != nil {
		t.Fatal(err)
	}
	return exe
}

// `agents status` calls a host installed from the file being there, so a
// host another installation set up read as this one's. It now says whose it
// is, in uninstall's own words (#112's reporting half, on the status side).
func TestStatusNamesTheInstallationAHostBelongsTo(t *testing.T) {
	m := newTwoInstallations(t)
	m.install(m.first)

	// Asked about by the installation that did NOT write these files.
	code, out := m.agents("status", "-config", m.second)
	if code != exitOK {
		t.Fatalf("status: exit %d\n%s", code, out)
	}
	want := "it belongs to the installation configured by " + m.first + ", not this installation"
	for _, label := range []string{"Claude Code", "Cursor", "opencode", "Pi"} {
		line := statusLineFor(t, out, label)
		if !strings.Contains(line, want) {
			t.Errorf("status does not say whose %s's files are:\n%s", label, line)
		}
		if strings.Contains(line, "installed (") {
			t.Errorf("status still calls another installation's %s installed:\n%s", label, line)
		}
	}

	// And the installation that DID write them is still told it is installed.
	code, out = m.agents("status", "-config", m.first)
	if code != exitOK {
		t.Fatalf("status: exit %d\n%s", code, out)
	}
	for _, label := range []string{"Claude Code", "Cursor", "opencode", "Pi"} {
		line := statusLineFor(t, out, label)
		if !strings.Contains(line, "installed (") || strings.Contains(line, "not this installation") {
			t.Errorf("the installation that wrote %s's files is not told it is installed:\n%s", label, line)
		}
	}
}

// statusLineFor is the one status row for a host label.
func statusLineFor(t *testing.T, out, label string) string {
	t.Helper()
	var found []string
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), label+" ") {
			found = append(found, l)
		}
	}
	if len(found) != 1 {
		t.Fatalf("want exactly one status row for %s, got %d:\n%s", label, len(found), out)
	}
	return found[0]
}
