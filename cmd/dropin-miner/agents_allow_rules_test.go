package main

// #114: an allow rule for this installation's binary and config has one
// current spelling. A superseded one is replaced, not added beside.
//
// The fixtures below are spellings ruleIsOurs recognises that the current
// renderer does not write -- a fully bare one, and the PowerShell
// call-operator one. They stand in for "a spelling this client has written
// that is no longer current", which is a state the file reaches on any
// renderer change; today's three forms happen to be a superset of v0.2.9's
// two, so the Windows machine of the 0.2.11 release check settled at three
// rather than growing without bound, and the defect there was that nothing
// makes it settle.

import (
	"encoding/json"
	"strings"
	"testing"
)

const claudeSettingsPath = "/home/u/.claude/settings.json"

// seedAllow writes a settings.json holding exactly these allow rules.
func seedAllow(t *testing.T, m *fakeMachine, rules ...string) {
	t.Helper()
	doc := map[string]any{"permissions": map[string]any{"allow": rules}}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	m.files[claudeSettingsPath] = append(b, '\n')
}

// supersededSpellings are two rules for bin and cfg that ruleIsOurs
// recognises and claudeAllowRules does not write.
func supersededSpellings(t *testing.T, bin, cfg string) (bare, powershell string) {
	t.Helper()
	bare = "Bash(" + bin + " search -config " + cfg + ":*)"
	powershell = "Bash(& " + powerShellQuoteArg(bin) + " search -config " + powerShellQuoteArg(cfg) + ":*)"
	ref := installationRef{bins: []string{bin}, cfg: cfg}
	for _, r := range []string{bare, powershell} {
		if !ruleIsOurs(r, ref) {
			t.Fatalf("this fixture is meant to be a rule of ours in an older spelling, and is not recognised as one: %s", r)
		}
		for _, current := range claudeAllowRules(binEntry{command: bin, cfg: cfg}) {
			if r == current {
				t.Fatalf("this fixture is meant to be SUPERSEDED, and the current renderer writes it: %s", r)
			}
		}
	}
	return bare, powershell
}

func TestASupersededAllowRuleIsReplacedRatherThanAddedBeside(t *testing.T) {
	m, ops := newFakeMachine("claude")
	const bin = "/home/u/.tokendrop/bin/dropin-miner"
	const foreignCfg = "/home/u/dm-disposable/tokendrop.toml"
	entry := binEntry{command: bin, cfg: testCfg}
	bare, powershell := supersededSpellings(t, bin, testCfg)

	// The participant's own rule, two superseded spellings of ours, and one
	// belonging to a second installation that shares this binary.
	const mine = "Bash(git status:*)"
	foreign := "Bash(" + bin + " search -config " + foreignCfg + ":*)"
	seedAllow(t, m, mine, bare, powershell, foreign)

	if code, out, errOut := runAgents(t, ops, nil, "install", "-yes", "-config", testCfg, "-client", "claude"); code != exitOK {
		t.Fatalf("install: exit %d\n%s\n%s", code, out, errOut)
	}

	allow := allowOf(t, m, claudeSettingsPath)
	want := append([]string{mine, foreign}, claudeAllowRules(entry)...)
	if len(allow) != len(want) {
		t.Fatalf("allow rules after the install: %d, want %d\n got %q\nwant %q", len(allow), len(want), allow, want)
	}
	for i := range want {
		if allow[i] != want[i] {
			t.Errorf("allow rule %d:\n got %s\nwant %s", i, allow[i], want[i])
		}
	}
	for _, gone := range []string{bare, powershell} {
		if countString(allow, gone) != 0 {
			t.Errorf("a superseded spelling of our own rule survived the install: %s\n%q", gone, allow)
		}
	}
	if countString(allow, foreign) != 1 {
		t.Errorf("another installation's rule was not left exactly as it was: %q", allow)
	}
	if countString(allow, mine) != 1 {
		t.Errorf("the participant's own rule was disturbed: %q", allow)
	}

	// And it settles: a second install writes nothing at all.
	before := string(m.files[claudeSettingsPath])
	if code, out, errOut := runAgents(t, ops, nil, "install", "-yes", "-config", testCfg, "-client", "claude"); code != exitOK {
		t.Fatalf("second install: exit %d\n%s\n%s", code, out, errOut)
	}
	if got := string(m.files[claudeSettingsPath]); got != before {
		t.Errorf("a second install rewrote settings.json:\n got %s\nwant %s", got, before)
	}
}

// The other half, which needed no change: planHooksRemove already asks
// ruleIsOurs, so uninstall took out every spelling before #114 and this
// records it rather than claiming it as new.
func TestUninstallRemovesASupersededAllowRuleToo(t *testing.T) {
	m, ops := newFakeMachine("claude")
	const bin = "/home/u/.tokendrop/bin/dropin-miner"
	const foreignCfg = "/home/u/dm-disposable/tokendrop.toml"
	bare, powershell := supersededSpellings(t, bin, testCfg)
	const mine = "Bash(git status:*)"
	foreign := "Bash(" + bin + " search -config " + foreignCfg + ":*)"
	seedAllow(t, m, mine, bare, powershell, foreign)

	if code, out, errOut := runAgents(t, ops, nil, "uninstall", "-yes", "-config", testCfg, "-client", "claude"); code != exitOK {
		t.Fatalf("uninstall: exit %d\n%s\n%s", code, out, errOut)
	}
	allow := allowOf(t, m, claudeSettingsPath)
	for _, gone := range []string{bare, powershell} {
		if countString(allow, gone) != 0 {
			t.Errorf("uninstall left a superseded spelling of this installation's rule: %s\n%q", gone, allow)
		}
	}
	if countString(allow, foreign) != 1 || countString(allow, mine) != 1 {
		t.Errorf("uninstall disturbed a rule that was not this installation's: %q", allow)
	}
}

// A file already holding exactly the current set is left alone, whatever
// order it holds them in and wherever the participant's own rules sit among
// them: the set is what is current, not its arrangement.
func TestAnAllowRuleSetAlreadyCurrentIsLeftInPlace(t *testing.T) {
	m, ops := newFakeMachine("claude")
	const bin = "/home/u/.tokendrop/bin/dropin-miner"
	current := claudeAllowRules(binEntry{command: bin, cfg: testCfg})
	if len(current) < 3 {
		t.Fatalf("this test interleaves the participant's rule among ours and needs at least three: %q", current)
	}
	// Ours, out of order, with the participant's own rule in the middle.
	scrambled := []string{current[2], "Bash(git status:*)", current[0], current[1]}
	seedAllow(t, m, scrambled...)

	if code, out, errOut := runAgents(t, ops, nil, "install", "-yes", "-config", testCfg, "-client", "claude"); code != exitOK {
		t.Fatalf("install: exit %d\n%s\n%s", code, out, errOut)
	}
	allow := allowOf(t, m, claudeSettingsPath)
	if strings.Join(allow, "\n") != strings.Join(scrambled, "\n") {
		t.Errorf("a set already current was rearranged:\n got %q\nwant %q", allow, scrambled)
	}
	if !strings.Contains(string(m.files[claudeSettingsPath]), `"allow"`) {
		t.Fatal("the allow list vanished")
	}
}
