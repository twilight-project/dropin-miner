package main

// Cursor on Windows runs whichever terminal the participant configured.
//
// 0.2.10 declared PowerShell alone for that cell, which is true of a default
// install and false of one whose terminal.integrated.defaultProfile.windows
// is Git Bash. On such a machine the model wrapped the PowerShell form in
// powershell.exe -Command "…", bash expanded $OutputEncoding out of it before
// PowerShell ever saw it, the encoding line failed, and Windows PowerShell
// 5.1 delivered `café 東京` as `caf? ??` — a search that succeeded and
// answered a different question (#96). 0.2.9's Bash heredoc ran byte-exact on
// the same machine, so this is a regression 0.2.10 shipped.
//
// The tests here hold the two halves of the fix: the skill teaches a form for
// each terminal, labeled by the terminal rather than by the tool, and the
// recognizer accepts a search rendered for either of them.

import (
	"strings"
	"testing"
)

// cursorSkillOn is Cursor's rendered skill for one OS, with the shells that
// rendered it.
func cursorSkillOn(t *testing.T, goos string) (string, skillShells) {
	t.Helper()
	cursor, ok := targetByID(installTargets, "cursor")
	if !ok {
		t.Fatal("no cursor target")
	}
	shells, note := toolShellsForSkill(cursor, goos)
	if note != "" {
		t.Fatalf("cursor on %s renders with the not-established note %q; this test is about a declared cell", goos, note)
	}
	return renderedSkillFor("cursor", hostStringsEntry(goos), goos), shells
}

// On Windows the skill carries both forms, and each is introduced by the
// terminal it belongs to. The heredoc is the one #96's machine could run; the
// here-string is the one a default install runs.
func TestCursorOnWindowsTeachesAFormForEachTerminal(t *testing.T) {
	skill, shells := cursorSkillOn(t, "windows")
	if len(shells.kinds) != 2 {
		t.Fatalf("cursor on windows renders for %v, want both PowerShell and POSIX", shells.kinds)
	}

	// Both search forms are present, each in its own fence.
	blocks := skillCommandBlocks(skill)
	var fences []string
	for _, b := range blocks {
		fences = append(fences, b.lang)
	}
	for _, want := range []string{"bash", "powershell"} {
		if !containsString(fences, want) {
			t.Errorf("the skill's command fences are %v, with no %s block: a participant on that terminal has no runnable form", fences, want)
		}
	}
	search := skillBlockFor(t, skill, shellPOSIX, "search")
	if !strings.Contains(search.body, "<<'JSON'") {
		t.Errorf("the POSIX search block is not the quoted heredoc 0.2.9 ran byte-exact:\n%s", search.body)
	}
	psSearch := skillBlockFor(t, skill, shellPowerShell, "search")
	if !strings.Contains(psSearch.body, "$OutputEncoding") {
		t.Errorf("the PowerShell search block has lost its encoding line:\n%s", psSearch.body)
	}

	// And each is labeled by the participant's terminal, not by the tool —
	// Cursor offers one tool, so "the tool you are calling" is a question
	// nobody can answer about it.
	for _, want := range []string{"If your terminal is PowerShell", "If your terminal is Git Bash"} {
		if !strings.Contains(skill, want) {
			t.Errorf("the skill never says %q, so a participant cannot tell which block is theirs:\n%s", want, skill)
		}
	}
	if strings.Contains(skill, "If the tool you are calling runs") {
		t.Errorf("Cursor's skill labels its blocks by the tool being called; Cursor runs one tool and the participant picks the shell:\n%s", skill)
	}
}

// Claude Code keeps the other label, and must: it runs two tools, the model
// picks one per call, and the call names which. This is the half that makes
// the test above an assertion about *whose* choice it is rather than about
// wording in general.
func TestClaudeCodeStillLabelsItsBlocksByTheToolBeingCalled(t *testing.T) {
	skill := renderedSkillFor("claude", hostStringsEntry("windows"), "windows")
	if !strings.Contains(skill, "If the tool you are calling runs") {
		t.Errorf("Claude Code's skill no longer labels its blocks by the tool:\n%s", skill)
	}
	if strings.Contains(skill, "If your terminal is") {
		t.Errorf("Claude Code's skill asks about the participant's terminal; its shell is the model's per-call choice, not a setting:\n%s", skill)
	}
}

// macOS and Linux are untouched: one shell, one block, no condition above it.
// The set is a Windows fact about a Windows setting, and widening it
// elsewhere would teach two forms where one is correct.
func TestCursorTeachesOneFormWhereItRunsOneShell(t *testing.T) {
	for _, goos := range []string{"darwin", "linux"} {
		skill, shells := cursorSkillOn(t, goos)
		if len(shells.kinds) != 1 || shells.kinds[0] != shellPOSIX {
			t.Errorf("cursor on %s renders for %v, want POSIX alone", goos, shells.kinds)
		}
		if strings.Contains(skill, "If your terminal is") || strings.Contains(skill, "If the tool you are calling runs") {
			t.Errorf("cursor on %s teaches a conditional form for a host that runs one shell:\n%s", goos, skill)
		}
		if got := strings.Count(skill, "```powershell"); got != 0 {
			t.Errorf("cursor on %s renders %d PowerShell blocks, want none", goos, got)
		}
	}
}

// The recognizer accepts a search rendered for either declared shell. It
// iterates the declared set already — this runs it, because "it iterates the
// set" is the assumption #96 makes load-bearing: a form the skill teaches and
// the hook refuses is a permission prompt on every search.
func TestTheRecognizerAcceptsASearchRenderedForEitherTerminal(t *testing.T) {
	const goos = "windows"
	entry := hostStringsEntry(goos)
	skill, shells := cursorSkillOn(t, goos)
	for _, sh := range shells.kinds {
		block := skillBlockFor(t, skill, sh, "search")
		command := strings.Replace(block.body, "<query>", "exact query text", 1)
		got := recognizerVerdict(command, entry, goos)
		want := "matches the rendered `search` for " + string(sh)
		if got != want {
			t.Errorf("the %s search form the skill teaches is %q, want %q; a form we teach and refuse prompts on every search", sh, got, want)
		}
	}
}
