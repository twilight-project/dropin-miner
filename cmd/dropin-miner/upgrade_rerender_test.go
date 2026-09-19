package main

// #111: an upgrade re-renders the host integrations this installation owns —
// after the replacement commits, without ever failing the upgrade, and
// without touching a host that is another installation's.
//
// The child process is the one thing stubbed, and it is stubbed with
// production: the runner below answers `agents install …` by running
// agentsMain itself, against this fixture's real files, as the binary at the
// path it was handed. What it adds is a record of WHICH VERSION WAS AT THAT
// PATH when it was asked to render, because that — not the bytes, which this
// one test binary renders the same whatever a fixture file says its version
// is — is the observable that tells "after the replacement committed" from
// "before it".

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/twilight-project/dropin-miner/internal/selfupdate"
)

type renderCall struct {
	path     string
	args     []string
	version  string // what the file at path said when the render was asked for
	previous string // what <path>.previous said at that moment
}

// renderingRunner tells a version check from a re-render. A version check
// goes to the fixture's contentRunner, injected failures and all; an
// `agents …` invocation is run for real, in-process.
type renderingRunner struct {
	f     *upgradeFixture
	calls []renderCall
	fail  bool
}

func (r *renderingRunner) Run(ctx context.Context, path string, args, env []string) ([]byte, []byte, error) {
	if len(args) == 0 || args[0] != "agents" {
		return r.f.runner.Run(ctx, path, args, env)
	}
	r.calls = append(r.calls, renderCall{
		path: path, args: append([]string(nil), args...),
		version: r.f.content(path), previous: r.f.content(selfupdate.PreviousPath(path)),
	})
	if r.fail {
		return nil, []byte("injected: the host files could not be written\n"), errors.New("exit status 69")
	}
	ops := r.f.agents
	ops.executable = func() (string, error) { return path, nil }
	var out, errOut bytes.Buffer
	if code := agentsMain(ops, args[1:], strings.NewReader(""), &out, &errOut, envOf(r.f.env)); code != exitOK {
		return out.Bytes(), errOut.Bytes(), errors.New("exit status " + string(rune('0'+code%10)))
	}
	return out.Bytes(), errOut.Bytes(), nil
}

// rerenderMachine is an upgrade fixture with four coding agents on PATH:
// Claude Code and Cursor set up by this installation, Codex found and never
// set up, and Pi set up by ANOTHER installation that shares this binary. Every
// installed file is then made stale, the two installations' alike.
type rerenderMachine struct {
	*upgradeFixture
	runner   *renderingRunner
	paths    agentPaths
	cfg      string
	otherCfg string
	fresh    map[string]string // what this binary renders, by path
	stale    map[string]string // what was on disk when the upgrade began
}

func newRerenderMachine(t *testing.T, latest string, bodies map[string]string) *rerenderMachine {
	t.Helper()
	f := newUpgradeFixture(t, newLocalRelease(t, latest, bodies))
	f.agents.lookPath = func(name string) (string, error) {
		switch name {
		case "claude", "cursor", "codex", "pi":
			return filepath.Join(f.agents.home, "path", name), nil
		}
		return "", errors.New("not found")
	}
	m := &rerenderMachine{
		upgradeFixture: f,
		runner:         &renderingRunner{f: f},
		paths:          f.agents.paths(envOf(f.env)),
		cfg:            filepath.Join(f.home, setupConfigFile),
		otherCfg:       filepath.Join(f.agents.home, "dm-disposable", setupConfigFile),
		fresh:          map[string]string{},
		stale:          map[string]string{},
	}
	f.runnerOverride = m.runner
	writeFileT(t, m.otherCfg, "")

	install := func(cfg string, clients ...string) {
		t.Helper()
		args := []string{"install", "-yes", "-config", cfg}
		for _, c := range clients {
			args = append(args, "-client", c)
		}
		var out, errOut bytes.Buffer
		if code := agentsMain(f.agents, args, strings.NewReader(""), &out, &errOut, envOf(f.env)); code != exitOK {
			t.Fatalf("agents %v: exit %d\n%s\n%s", args, code, out.String(), errOut.String())
		}
	}
	install(m.cfg, "claude", "cursor")
	install(m.otherCfg, "pi")

	// What an earlier version rendered. Each edit keeps the command the file
	// teaches — binary and config — because that is what it is attributed by;
	// the hook matcher is the real one, #111's Windows comment: `"Bash"` where
	// the current renderer writes `"Bash|PowerShell"`.
	for _, p := range []string{m.paths.claudeSkill, m.paths.claudeSettings, m.paths.cursorSkill, m.paths.piSkill, m.paths.piExtension} {
		b, err := os.ReadFile(p) // #nosec G304 -- this test's own sandbox
		if err != nil {
			t.Fatalf("install did not write %s: %v", p, err)
		}
		m.fresh[p] = string(b)
		old := "as an earlier version rendered it\n" + string(b)
		if p == m.paths.claudeSettings {
			old = strings.Replace(string(b), claudeToolMatcher, "Bash", 1)
		}
		if old == string(b) {
			t.Fatalf("%s: the stale form is the fresh one, so a re-render of it would prove nothing", p)
		}
		writeFileT(t, p, old)
		m.stale[p] = old
	}
	return m
}

func (m *rerenderMachine) onDisk(path string) string {
	b, err := os.ReadFile(path) // #nosec G304 -- this test's own sandbox
	if err != nil {
		return "<absent>"
	}
	return string(b)
}

func TestUpgradeReRendersTheHostsThisInstallationOwnsAndNoOthers(t *testing.T) {
	m := newRerenderMachine(t, "0.3.1", map[string]string{"0.3.1": "0.3.1\n"})
	code, out, errOut := m.run()
	if code != exitOK {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}

	// The two hosts this installation set up are what this binary renders.
	for _, p := range []string{m.paths.claudeSkill, m.paths.claudeSettings, m.paths.cursorSkill} {
		if got := m.onDisk(p); got != m.fresh[p] {
			t.Errorf("%s was not re-rendered by the upgrade:\n%s", p, got)
		}
	}
	// The third host was found on this machine and never set up. It stays so.
	for _, p := range []string{m.paths.codexSkill, m.paths.codexConfig} {
		if got := m.onDisk(p); got != "<absent>" {
			t.Errorf("the upgrade installed a host nobody had set up: %s\n%s", p, got)
		}
	}
	// Another installation's host is not this one's to refresh, stale or not.
	for _, p := range []string{m.paths.piSkill, m.paths.piExtension} {
		if got := m.onDisk(p); got != m.stale[p] {
			t.Errorf("the upgrade rewrote another installation's %s:\n%s", p, got)
		}
	}

	// After the replacement committed, by the binary now installed.
	if len(m.runner.calls) != 1 {
		t.Fatalf("re-render calls: %+v, want exactly one", m.runner.calls)
	}
	call := m.runner.calls[0]
	if call.path != m.exe || call.version != "0.3.1" || call.previous != "0.3.0" {
		t.Errorf("rendered by %s saying %q with .previous %q; want the launch path, the new release, and the replaced binary already committed", call.path, call.version, call.previous)
	}
	if got, want := strings.Join(call.args, " "), "agents install -yes -config "+m.cfg+" -client claude -client cursor"; got != want {
		t.Errorf("child arguments:\n got %s\nwant %s", got, want)
	}

	// Reported in the shape `agents install` reports it — it IS that report —
	// after the upgrade's own success line, with the foreign host named.
	upgraded := strings.Index(out, "upgraded "+m.exe+" from 0.3.0 to 0.3.1")
	refreshing := strings.Index(out, "refreshing the agent integrations this installation owns: Claude Code, Cursor")
	if upgraded < 0 || refreshing < upgraded {
		t.Errorf("the success line must come first, then the refresh:\n%s", out)
	}
	for _, want := range []string{
		"dropin-miner agents install\n", "  agents: Claude Code, Cursor\n", "wrote ", "done. Restart any agent",
		"Pi: left in place; it belongs to the installation configured by " + m.otherCfg + ", not this installation",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if errOut != "" {
		t.Errorf("a clean re-render wrote to stderr:\n%s", errOut)
	}
}

// An upgrade whose replacement fails restores the prior binary, and the host
// files must then still be the prior binary's: nothing is rendered before the
// transaction has committed.
func TestAnUpgradeThatIsRolledBackLeavesTheHostFilesAsTheyWere(t *testing.T) {
	m := newRerenderMachine(t, "0.3.1", map[string]string{"0.3.1": "0.3.1\n"})
	m.upgradeFixture.runner.failFor = m.exe // the canonical path does not run: Install restores
	code, out, errOut := m.run()
	if code == exitOK {
		t.Fatalf("the injected replacement failure did not fail the upgrade:\n%s\n%s", out, errOut)
	}
	if m.content(m.exe) != "0.3.0" {
		t.Fatalf("the prior binary was not restored (%s), so this case is not the one it says it is", m.content(m.exe))
	}
	for p, want := range m.stale {
		if got := m.onDisk(p); got != want {
			t.Errorf("%s changed under an upgrade that was rolled back:\n%s", p, got)
		}
	}
	if len(m.runner.calls) != 0 {
		t.Errorf("a re-render ran under an upgrade that did not commit: %+v", m.runner.calls)
	}
}

// `upgrade -rollback` is a replacement too, and the host files it leaves are
// those of the version rolled back to: rendered by the restored binary, after
// the swap committed.
func TestRollbackReRendersWithTheBinaryRolledBackTo(t *testing.T) {
	m := newRerenderMachine(t, "0.3.1", nil)
	m.write(selfupdate.PreviousPath(m.exe), "0.2.9")
	code, out, errOut := m.run("-rollback")
	if code != exitOK {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	if len(m.runner.calls) != 1 {
		t.Fatalf("re-render calls: %+v, want exactly one", m.runner.calls)
	}
	if call := m.runner.calls[0]; call.version != "0.2.9" || call.previous != "0.3.0" {
		t.Errorf("rendered by a binary saying %q with .previous %q; want the version rolled back to, after the swap", call.version, call.previous)
	}
	for _, p := range []string{m.paths.claudeSkill, m.paths.claudeSettings, m.paths.cursorSkill} {
		if got := m.onDisk(p); got != m.fresh[p] {
			t.Errorf("%s was not re-rendered by the rollback:\n%s", p, got)
		}
	}
}

// The binary is in place and .previous committed by the time anything is
// rendered, so a render that fails is reported and the upgrade still
// succeeded — with the one command that finishes the job.
func TestAReRenderFailureDoesNotFailTheUpgradeAndNamesTheCommand(t *testing.T) {
	m := newRerenderMachine(t, "0.3.1", map[string]string{"0.3.1": "0.3.1\n"})
	m.runner.fail = true
	code, out, errOut := m.run()
	if code != exitOK {
		t.Fatalf("a re-render failure failed the upgrade: exit %d\n%s\n%s", code, out, errOut)
	}
	if m.content(m.exe) != "0.3.1" || !strings.Contains(out, "upgraded "+m.exe+" from 0.3.0 to 0.3.1") {
		t.Errorf("the upgrade must still be reported as the success it was:\n%s", out)
	}
	finish := displayPath(m.exe) + " agents install -config " + displayPath(m.cfg) + " -client claude -client cursor"
	for _, want := range []string{"the binary was replaced, but the agent integrations were not all refreshed", "finish with: " + finish, "injected: the host files could not be written"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("stderr lacks %q:\n%s", want, errOut)
		}
	}
}

// A file that names no installation cannot be claimed by this one: it is left
// and said, never overwritten on the assumption it is ours.
func TestUpgradeLeavesAHostFileThatNamesNoInstallation(t *testing.T) {
	m := newRerenderMachine(t, "0.3.1", map[string]string{"0.3.1": "0.3.1\n"})
	const unstamped = "// an adapter from before adapters carried INSTALL_CONFIG\n"
	writeFileT(t, m.paths.opencodePlugin, unstamped)
	code, out, errOut := m.run()
	if code != exitOK {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	if got := m.onDisk(m.paths.opencodePlugin); got != unstamped {
		t.Errorf("the upgrade rewrote a file it could not attribute:\n%s", got)
	}
	if !strings.Contains(out, "opencode: left in place; ") || !strings.Contains(out, "names no installation, so this one cannot claim it") {
		t.Errorf("the unattributed host was not reported:\n%s", out)
	}
	if got := strings.Join(m.runner.calls[0].args, " "); strings.Contains(got, "opencode") {
		t.Errorf("the child was pointed at the unattributed host: %s", got)
	}
}

// An installation with no agents set up upgrades exactly as it did: nothing
// is rendered, nothing is said about agents, no process is started.
func TestUpgradeWithNoOwnedHostsStartsNoRender(t *testing.T) {
	f := newUpgradeFixture(t, newLocalRelease(t, "0.3.1", map[string]string{"0.3.1": "0.3.1\n"}))
	r := &renderingRunner{f: f}
	f.runnerOverride = r
	code, out, errOut := f.run()
	if code != exitOK {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	if len(r.calls) != 0 || strings.Contains(out, "refreshing") || errOut != "" {
		t.Errorf("calls %+v\n%s\n%s", r.calls, out, errOut)
	}
}
