package main

// #111's diagnosis: a participant on the fixed binary whose host still
// misbehaves needs `agents status` to say the host is not on it.

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// The production sentence itself, not a copy of it: what these cases are
// about is WHICH files are called stale and how many, and a typed copy would
// only pin the wording twice while going red every time it is reworded.
const staleWords = staleSentence

func staleLines(out string) []string {
	var lines []string
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, staleWords) {
			lines = append(lines, strings.TrimSpace(l))
		}
	}
	return lines
}

func TestStatusReportsAStaleRenderingUntilAnInstallRefreshesIt(t *testing.T) {
	m, ops := newFakeMachine("claude", "cursor")
	if code, out, errOut := runAgents(t, ops, nil, "install", "-yes", "-config", testCfg); code != exitOK {
		t.Fatalf("install: exit %d\n%s\n%s", code, out, errOut)
	}
	_, out, _ := runAgents(t, ops, nil, "status", "-config", testCfg)
	if got := staleLines(out); len(got) != 0 {
		t.Fatalf("a fresh install is reported stale: %q\n%s", got, out)
	}

	// An earlier version's skill, and an earlier version's hook matcher — the
	// one #111's Windows comment found, `"Bash"` for `"Bash|PowerShell"`.
	// Taken from ops.paths, not typed: paths joins with the host separator,
	// so on Windows the file production names is \home\u\.claude\... and a
	// typed forward-slash literal is a different string -- which is what was
	// still red on both Windows runners after the first attempt at this.
	// slash() is how the in-memory machine keys them, and tilde() is how the
	// line spells them.
	paths := ops.paths(noEnv)
	skill, settings := slash(paths.claudeSkill), slash(paths.claudeSettings)
	m.files[skill] = append([]byte("as an earlier version rendered it\n"), m.files[skill]...)
	stale := strings.Replace(string(m.files[settings]), claudeToolMatcher, "Bash", 1)
	if stale == string(m.files[settings]) {
		t.Fatal("the settings fixture did not change, so nothing about hooks is being tested")
	}
	m.files[settings] = []byte(stale)

	_, out, _ = runAgents(t, ops, nil, "status", "-config", testCfg)
	got := staleLines(out)
	// Named through tilde, not spelled "~/...": this machine's home is the
	// literal /home/u while ops.paths joins with the host separator, so on
	// Windows there is no prefix for tilde to shorten and the line reads
	// \home\u\.claude\... -- which is what both Windows runners reported
	// on #123's first CI run. How a path is abbreviated is printPlan's
	// subject and is pinned there; what this test guards is WHICH files are
	// called stale, and how many.
	want := []string{
		tilde(ops.home, paths.claudeSkill) + ": " + staleWords,
		tilde(ops.home, paths.claudeSettings) + ": " + staleWords,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("want exactly Claude Code's skill and settings named as stale\n got %q\nwant %q\n%s", got, want, out)
	}
	// The line sits under its own host, not under the next one.
	claude, cursor := strings.Index(out, "Claude Code"), strings.Index(out, "Cursor")
	if first := strings.Index(out, staleWords); first < claude || first > cursor {
		t.Errorf("the stale lines are not under Claude Code:\n%s", out)
	}

	if code, o, e := runAgents(t, ops, nil, "install", "-yes", "-config", testCfg); code != exitOK {
		t.Fatalf("reinstall: exit %d\n%s\n%s", code, o, e)
	}
	_, out, _ = runAgents(t, ops, nil, "status", "-config", testCfg)
	if got := staleLines(out); len(got) != 0 {
		t.Errorf("status still reports a stale rendering after the install that refreshes it: %q", got)
	}
}

// #130: a skill naming this installation's config and a binary somewhere else
// is this installation's, and out of date. It is the moved-binary case U2 was
// written for — npm to native, a reinstall elsewhere, or, as the release check
// produced it, the candidate run from a worktree against files the installed
// binary rendered. Asking H5's question of a single-slot file read it as
// another installation's and printed nothing, so the one participant who most
// needs to be told the host is not on this binary was told "installed (skill)".
func TestStatusCallsASkillOfOursStaleWhenItNamesABinaryAtAnotherPath(t *testing.T) {
	m, ops := newFakeMachine("claude")
	if code, out, errOut := runAgents(t, ops, nil, "install", "-yes", "-config", testCfg); code != exitOK {
		t.Fatalf("install: exit %d\n%s\n%s", code, out, errOut)
	}
	paths := ops.paths(noEnv)
	skill := slash(paths.claudeSkill)

	// The same installation, its binary somewhere else: every spelling of
	// the old path inside the rendered skill becomes the new one, which is
	// what a move actually leaves behind. Taken from ops.executable and
	// rendered by production's own renderer rather than typed, so the two
	// paths are spelled the way this OS's shells spell them.
	was := mustExe(t, ops)
	moved := filepath.Join(filepath.Dir(was), "moved", filepath.Base(was))
	before := string(m.files[skill])
	after := before
	for _, prefix := range binaryPrefixes(was) {
		after = strings.ReplaceAll(after, prefix, strings.Replace(prefix, was, moved, 1))
	}
	if after == before {
		t.Fatal("the skill fixture did not change, so nothing about a moved binary is being tested")
	}
	m.files[skill] = []byte(after)

	_, out, _ := runAgents(t, ops, nil, "status", "-config", testCfg)
	want := []string{tilde(ops.home, paths.claudeSkill) + ": " + staleWords}
	if got := staleLines(out); !reflect.DeepEqual(got, want) {
		t.Errorf("a skill of ours rendered by a binary at another path is not reported as stale\n got %q\nwant %q\n%s", got, want, out)
	}
	if !strings.Contains(out, "`agents install`") {
		t.Errorf("the refresh is not named:\n%s", out)
	}

	// And the file is still ours: status reports, it does not re-attribute.
	line := statusLineFor(t, out, "Claude Code")
	if strings.Contains(line, "not this installation") {
		t.Errorf("a skill this installation's config names was called another's:\n%s", line)
	}
}

// Two things `agents install` would write that are not staleness: a half that
// is missing, which the status detail already names, and a skill that is
// another installation's, which is not this one's to call out of date.
func TestStatusDoesNotCallAMissingHalfOrAForeignSkillStale(t *testing.T) {
	m, ops := newFakeMachine("claude")
	if code, out, errOut := runAgents(t, ops, nil, "install", "-yes", "-config", testCfg); code != exitOK {
		t.Fatalf("install: exit %d\n%s\n%s", code, out, errOut)
	}
	// The host's own settings, our hooks never merged in.
	m.files["/home/u/.claude/settings.json"] = []byte("{\"theme\":\"dark\"}\n")
	_, out, _ := runAgents(t, ops, nil, "status", "-config", testCfg)
	if !strings.Contains(out, "installed (skill only)") {
		t.Fatalf("this fixture is meant to be a skill with no hooks:\n%s", out)
	}
	if got := staleLines(out); len(got) != 0 {
		t.Errorf("a settings.json with none of our hooks in it was called an earlier version's rendering: %q", got)
	}

	// The same machine, asked about by an installation the skill does not name.
	_, out, _ = runAgents(t, ops, nil, "status", "-config", "/home/u/dm-disposable/tokendrop.toml")
	if got := staleLines(out); len(got) != 0 {
		t.Errorf("another installation's skill was reported as this one's stale rendering: %q", got)
	}
}
