package main

// #122: the snippet a participant pastes is the block the skill teaches.
//
// A host with no skill directory — opencode, and the generic "any other
// agent" line — receives its instructions as text in the install plan,
// copied by hand into AGENTS.md rather than written to a file. Until this
// commit that text was a bare command with the request on the line below
// it: no heredoc, no here-string, no pipe, so nothing carried the second
// line to stdin, and on Windows nothing set the output encoding the
// rendered skills have set since 0.2.11 — #96's mechanism reaching the one
// host whose instructions a model composes the wrapper for itself.
//
// Every case here renders for a named OS rather than for the runner, so a
// macOS or Linux runner exercises the Windows form too.

import (
	"runtime"
	"strings"
	"testing"
)

// hintForHost is the note agents install prints for a host with no skill
// directory on goos: rendered from that host's own declared tool shells,
// exactly as its PlanInstall renders it.
func hintForHost(t *testing.T, id string, entry binEntry, goos string) (string, skillShells) {
	t.Helper()
	tg, ok := targetByID(installTargets, id)
	if !ok {
		t.Fatalf("no target %q", id)
	}
	shells, _ := toolShellsForSkill(tg, goos)
	return rulesSnippetFor(entry, shells), shells
}

// TestTheAGENTSHintCarriesTheSkillsOwnSearchBlock: for every OS, the block
// inside opencode's AGENTS.md note is byte for byte the block the skill
// renders for that host on that OS.
//
// Both sides are rendered and their blocks compared. Neither is a literal
// typed into this file, because a literal is a second copy of the very text
// that drifted — the hint had been a form of its own since before the block
// renderer existed, and no test noticed for two releases.
func TestTheAGENTSHintCarriesTheSkillsOwnSearchBlock(t *testing.T) {
	for _, goos := range hostShellOSes {
		t.Run(goos, func(t *testing.T) {
			entry := hostStringsEntry(goos)
			hint, shells := hintForHost(t, "opencode", entry, goos)
			skill := renderedSkillFor("opencode", entry, goos)
			for _, sh := range shells.kinds {
				want := skillBlockFor(t, skill, sh, "search")
				got := skillBlockFor(t, hint, sh, "search")
				if got.lang != want.lang || got.body != want.body {
					t.Errorf("the hint's %s block is not the skill's:\n--- hint ---\n%s\n--- skill ---\n%s",
						sh, fenced(got.lang, got.body), fenced(want.lang, want.body))
				}
				// Containment as well as equality of the extracted
				// bodies: the fences have to be present, and at the start
				// of their own lines, or what a participant pastes into a
				// markdown file is not a block at all.
				if !strings.Contains(hint, fenced(want.lang, want.body)) {
					t.Errorf("the hint does not carry the skill's %s block verbatim:\n--- hint ---\n%s", sh, hint)
				}
			}
		})
	}
}

// TestTheWindowsHintCarriesTheOutputEncodingLine: opencode runs its tool
// calls in PowerShell on Windows, and the line that makes a query carrying
// an apostrophe or any non-ASCII character survive Windows PowerShell 5.1
// has to be in the block a participant pastes — first, where the renderer
// puts it — rather than left for a model to invent (#96, #122).
func TestTheWindowsHintCarriesTheOutputEncodingLine(t *testing.T) {
	entry := hostStringsEntry("windows")
	hint, shells := hintForHost(t, "opencode", entry, "windows")
	var powershell bool
	for _, sh := range shells.kinds {
		if sh == shellPowerShell {
			powershell = true
		}
	}
	if !powershell {
		t.Fatalf("opencode no longer declares PowerShell on Windows (%v); this case's premise moved, not its rule", shells.kinds)
	}
	blk := skillBlockFor(t, hint, shellPowerShell, "search")
	if !strings.HasPrefix(blk.body, psOutputEncodingLine+"\n") {
		t.Errorf("the Windows hint's block does not open with the output-encoding line:\n%s", fenced(blk.lang, blk.body))
	}
}

// TestTheGenericRulesSnippetIsThePOSIXForm: the "for any other agent" line
// knows nothing about the agent reading it, so it keeps v0.2.9's POSIX
// form on every OS — the same command it always printed, now inside the
// block that makes it run.
func TestTheGenericRulesSnippetIsThePOSIXForm(t *testing.T) {
	for _, goos := range hostShellOSes {
		t.Run(goos, func(t *testing.T) {
			entry := hostStringsEntry(goos)
			snippet := rulesSnippet(entry)
			lang, script, err := searchBlockForShell(shellPOSIX, entry, exampleRequest)
			if err != nil {
				t.Fatalf("rendering the POSIX block: %v", err)
			}
			if !strings.Contains(snippet, fenced(lang, script)) {
				t.Errorf("the generic snippet is not the POSIX block:\n--- snippet ---\n%s\n--- want ---\n%s",
					snippet, fenced(lang, script))
			}
			if strings.Contains(snippet, psOutputEncodingLine) || strings.Contains(snippet, "```powershell") {
				t.Errorf("the generic snippet rendered a PowerShell form for an agent nothing is known about:\n%s", snippet)
			}
		})
	}
}

// TestTheHintsBlockIsPasteable: no line inside the block carries the hint's
// indentation. A quoted heredoc ends only at a line that is exactly its
// delimiter, and a PowerShell here-string only at a line that begins with
// '@, so an indented block is the unrunnable snippet #122 is about wearing
// a fence — a failure that would read as a formatting nicety in review.
func TestTheHintsBlockIsPasteable(t *testing.T) {
	for _, goos := range hostShellOSes {
		t.Run(goos, func(t *testing.T) {
			entry := hostStringsEntry(goos)
			hint, shells := hintForHost(t, "opencode", entry, goos)
			for _, sh := range shells.kinds {
				blk := skillBlockFor(t, hint, sh, "search")
				for _, line := range strings.Split(blk.body, "\n") {
					if line != strings.TrimLeft(line, " \t") {
						t.Errorf("a line of the %s block is indented, which no shell reads as a terminator: %q", sh, line)
					}
				}
				switch sh {
				case shellPOSIX:
					if !strings.Contains(hint, "\nJSON\n") {
						t.Errorf("the heredoc terminator does not begin its own line:\n%s", hint)
					}
				case shellPowerShell:
					if !strings.Contains(hint, "\n'@ | ") {
						t.Errorf("the here-string terminator does not begin its own line:\n%s", hint)
					}
				}
			}
		})
	}
}

// TestTheInstallPlanNoteCarriesTheSkillsBlock drives the real planner, so
// the cases above are about what `agents install` prints rather than about
// a renderer nothing calls. It runs for the runner's own OS, which is the
// only one buildInstallPlan renders for.
func TestTheInstallPlanNoteCarriesTheSkillsBlock(t *testing.T) {
	tg, ok := targetByID(installTargets, "opencode")
	if !ok {
		t.Fatal("no opencode target")
	}
	entry := goldenEntry()
	_, ops := newFakeMachine()
	plan := buildInstallPlan(ops, ops.paths(noEnv), []installTarget{tg}, entry, noEnv)

	shells, _ := toolShellsForSkill(tg, runtime.GOOS)
	skill := renderedSkillFor("opencode", entry, runtime.GOOS)
	for _, sh := range shells.kinds {
		want := skillBlockFor(t, skill, sh, "search")
		var found bool
		for _, n := range plan.notes {
			if strings.Contains(n, fenced(want.lang, want.body)) {
				found = true
			}
		}
		if !found {
			t.Errorf("no note in opencode's install plan carries the skill's %s block:\n%v", sh, plan.notes)
		}
	}
}
