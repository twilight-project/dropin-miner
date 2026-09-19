package main

// F1 of L3's review: the form HERMES writes.
//
// hermes_cli/config.py save_config reloads config.yaml as data and dumps it
// through PyYAML, so after Hermes' first save our marker comments are gone
// and our entry is a plain or single-quoted scalar folded at 80 columns. At
// 1f18c02 exactly that file gave: status "not installed"; install exit 1
// with the paste advice, which would add a duplicate; uninstall "Hermes: not
// installed", hook left running. Every symptom of #83, one save later.
//
// The fixtures under testdata/hermes are the dumper's real output, produced
// by testdata/hermes/resave.py from Hermes' own dumper class and dump
// arguments — never typed. Each *.input.yaml is what install writes for one
// fixed entry, and the first test below refuses to go on if the renderer no
// longer writes that, so a stale fixture fails loudly instead of testing the
// past. These drive the package's own functions with an explicit entry
// rather than `agents …`, because the Windows entry cannot be resolved on a
// POSIX runner nor the POSIX one on Windows, and both must run on both.

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

var hermesResavedEntries = map[string]struct {
	entry   binEntry
	windows bool
	// above is the participant's file install appended to. One of them has no
	// final newline, so that install's note is inside the block Hermes saves.
	above string
}{
	"posix":       {binEntry{command: "/home/u/.tokendrop/bin/dropin-miner", cfg: "/home/u/.tokendrop/tokendrop.toml"}, false, "model: gpt\n"},
	"posix-noeol": {binEntry{command: "/home/u/.tokendrop/bin/dropin-miner", cfg: "/home/u/.tokendrop/tokendrop.toml"}, false, "model: gpt"},
	"windows":     {binEntry{command: `C:\Users\u\.tokendrop\bin\dropin-miner.exe`, cfg: `C:\Users\u\.tokendrop\tokendrop.toml`}, true, "model: gpt\n"},
}

// hermesRoundtripFixtures are what Hermes' ruamel writer makes of each input:
// a key of its own after our block, and #125's two additions to the hooks:
// mapping our block opened.
var hermesRoundtripFixtures = []string{".roundtrip.yaml", ".roundtrip-sibling.yaml", ".roundtrip-item.yaml"}

// hermesAsWindowsSavesIt is a fixture with the line endings Hermes gives it on
// Windows, where utils.py _atomic_write opens its file in text mode and every
// "\n" it writes becomes "\r\n". The fixtures themselves are LF because
// .gitattributes keeps *.yaml LF on every platform.
func hermesAsWindowsSavesIt(fixture string) string {
	return strings.ReplaceAll(fixture, "\n", "\r\n")
}

// hermesResavedInputs is what install writes for each fixed entry, after a
// line of the participant's own.
func hermesResavedInputs() map[string]string {
	out := map[string]string{}
	for name, tc := range hermesResavedEntries {
		cmd, ok := hermesHookCommand(tc.entry, tc.windows)
		if !ok {
			panic("cannot render " + name)
		}
		body := strings.Join(hermesHookLines(cmd), "\n") + "\n"
		out[name] = string(hermesAppendBlock([]byte(tc.above), body))
	}
	return out
}

func readHermesFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "hermes", name)) // #nosec G304 -- this repo's own testdata
	if err != nil {
		t.Fatal(err)
	}
	// .gitattributes keeps *.yaml LF on every platform; a CRLF checkout here
	// would be a finding about the repository, not about Hermes.
	return string(b)
}

func TestTheHermesResavedFixturesAreNotStale(t *testing.T) {
	for name, want := range hermesResavedInputs() {
		if got := readHermesFixture(t, name+".input.yaml"); got != want {
			t.Errorf("testdata/hermes/%s.input.yaml is not what install writes any more; regenerate it and run testdata/hermes/resave.py\n--- fixture ---\n%s\n--- renderer ---\n%s", name, got, want)
		}
		resaved := readHermesFixture(t, name+".resaved.yaml")
		if strings.Contains(resaved, agentsMarkerBegin) {
			t.Errorf("%s.resaved.yaml still has our markers: it is not the dumper's output", name)
		}
		// The point of the fixture: the dumper folded the command, so no one
		// line of the file holds it and the renderer's form is not there.
		tc := hermesResavedEntries[name]
		rendered, _ := hermesHookCommand(tc.entry, tc.windows)
		if strings.Contains(resaved, rendered) {
			t.Errorf("%s.resaved.yaml holds the command on one line; this fixture no longer tests a folded scalar:\n%s", name, resaved)
		}

		// #105's fixture is the OTHER writer's output, and what makes it that
		// is exactly what the one above must not have: ruamel keeps comments
		// and quotes, so both markers and our quoted matcher are still there,
		// and the command is folded all the same.
		for _, suffix := range hermesRoundtripFixtures {
			roundtrip := readHermesFixture(t, name+suffix)
			for _, kept := range []string{agentsMarkerBegin + "\n", agentsMarkerEnd + "\n", hermesHookLines("")[3] + "\n"} {
				if !strings.Contains(roundtrip, kept) {
					t.Errorf("%s%s has lost %q: it is not the round-trip writer's output", name, suffix, kept)
				}
			}
			if strings.Contains(roundtrip, rendered) {
				t.Errorf("%s%s holds the command on one line; this fixture no longer tests a folded scalar:\n%s", name, suffix, roundtrip)
			}
			// #125's point: what Hermes added is after our end marker, and its
			// two additions to OUR mapping are indented under it.
			after := roundtrip[strings.Index(roundtrip, agentsMarkerEnd+"\n")+len(agentsMarkerEnd)+1:]
			if continues := strings.HasPrefix(after, " "); continues != (suffix != ".roundtrip.yaml") {
				t.Errorf("%s%s: what follows our end marker continues our mapping = %v:\n%s", name, suffix, continues, roundtrip)
			}
		}
	}
}

// hermesBeforeTheHooksKey is everything above the one `hooks:` line: for a
// fixture holding our entry and nothing else under it, exactly what uninstall
// must leave. Taken from the fixture rather than from what install was handed,
// because Hermes' dumper rewrote the whole file on its way here.
func hermesBeforeTheHooksKey(t *testing.T, fixture string) string {
	t.Helper()
	at := strings.Index(fixture, "hooks:\n")
	if at < 0 || strings.Count(fixture, "\nhooks:\n") > 1 {
		t.Fatalf("not one hooks: line in:\n%s", fixture)
	}
	return fixture[:at]
}

// hermesAroundOurBlock is the fixture with our block taken out by LINE — the
// markers, what is between them, and the one blank line install puts in front
// — which is what uninstall must leave. Derived from the fixture rather than
// typed, and by another route than hermesRemoveBlock's byte offsets.
func hermesAroundOurBlock(t *testing.T, fixture string) string {
	t.Helper()
	lines := strings.SplitAfter(fixture, "\n")
	begin, end := -1, -1
	for i, l := range lines {
		switch strings.TrimRight(l, "\r\n") {
		case agentsMarkerBegin:
			begin = i
		case agentsMarkerEnd:
			end = i
		}
	}
	if begin < 1 || end < begin || strings.TrimRight(lines[begin-1], "\r\n") != "" {
		t.Fatalf("no block of ours after a blank line in:\n%s", fixture)
	}
	return strings.Join(lines[:begin-1], "") + strings.Join(lines[end+1:], "")
}

// #105. After a model switch, an in-session setting or a personality change,
// Hermes' ruamel writer has kept our markers and folded the command inside
// them. It is the same YAML: status counts it, install leaves it, uninstall
// takes the block out and nothing else.
func TestOurMarkedBlockIsReadAfterHermesFoldsIt(t *testing.T) {
	for name, tc := range hermesResavedEntries {
		t.Run(name, func(t *testing.T) {
			roundtrip := readHermesFixture(t, name+".roundtrip.yaml")
			m, ops := newFakeMachine("hermes")
			m.files[hermesConfigPath] = []byte(roundtrip)

			if !hermesHookInstalledFor(ops, hermesConfigPath, tc.entry, tc.windows) {
				t.Errorf("status: Hermes reads this entry and runs it, and it is not counted:\n%s", roundtrip)
			}

			var install agentPlan
			if planHermesHookFor(ops, "Hermes", hermesConfigPath, tc.entry, tc.windows, &install) || len(install.writes) != 0 {
				t.Errorf("install planned a write, which Hermes' next save undoes, on every run: %+v", install.writes)
			}
			if len(install.refused) != 0 {
				t.Errorf("install refused:\n%s", strings.Join(install.refused, "\n"))
			}

			var uninstall agentPlan
			hermesTarget{}.PlanUninstall(ops, agentPaths{hermesConfig: hermesConfigPath, hermesSkill: hermesSkillPath}, tc.entry, noEnv, &uninstall)
			if len(uninstall.writes) != 1 {
				t.Fatalf("uninstall planned %d writes, want the one that removes the block: %+v", len(uninstall.writes), uninstall.writes)
			}
			if got, want := string(uninstall.writes[0].contents), hermesAroundOurBlock(t, roundtrip); got != want {
				t.Errorf("uninstall did not leave the surrounding file byte for byte\n--- got ---\n%s\n--- want ---\n%s", got, want)
			}
		})
	}
}

// What H1 compares the decoded block with is the command the renderer writes
// today, not H5's rule. H5 answers whose an entry is, and accepts every
// spelling this client ever wrote; status and install are asking whether there
// is anything left to do. So a block that is this installation's in a spelling
// the renderer no longer writes is not "installed", and is still refreshed.
//
// (A block that is ANOTHER installation's was refreshed too when this was
// written, and this test pinned it. That was #73's defect at install time, and
// hermes_marked_block_test.go now holds those three cases to the opposite.)
func TestOurMarkedBlockInAStaleSpellingIsStillRefreshed(t *testing.T) {
	ours := hermesResavedEntries["posix"].entry

	// v0.2.9's %q spelling: this installation's under H5, and not a command
	// either of Hermes' splitters reads the way it was meant.
	stale := strconv.Quote(ours.command) + " hook -config " + strconv.Quote(ours.cfg) + " hermes pre_tool_call"
	if !hermesCommandIsOurHook(stale, refFor(ours)) {
		t.Fatal("v0.2.9's spelling is no longer this installation's under H5, so this case tests nothing")
	}
	m, ops := newFakeMachine("hermes")
	m.files[hermesConfigPath] = hermesAppendBlock([]byte("model: gpt\n"), strings.Join(hermesHookLines(stale), "\n")+"\n")

	if hermesHookInstalledFor(ops, hermesConfigPath, ours, false) {
		t.Error("status counted a block that does not hold today's entry for this installation")
	}
	var install agentPlan
	if !planHermesHookFor(ops, "Hermes", hermesConfigPath, ours, false, &install) || len(install.writes) != 1 {
		t.Fatalf("install did not refresh the block: writes %+v, refused %v, notes %v", install.writes, install.refused, install.notes)
	}
	want, _ := hermesHookCommand(ours, false)
	if got := string(install.writes[0].contents); got != string(hermesAppendBlock([]byte("model: gpt\n"), strings.Join(hermesHookLines(want), "\n")+"\n")) {
		t.Errorf("the refreshed file is not the participant's file with today's block after it:\n%s", got)
	}
}

// Anything more than the four rendered lines between the markers is not
// something install may call finished: the block is read exactly or not at all.
func TestTheMarkedBlockReaderAnswersOnlyForWhatTheRendererWrites(t *testing.T) {
	folded := readHermesFixture(t, "posix.roundtrip.yaml")
	want, _ := hermesHookCommand(hermesResavedEntries["posix"].entry, false)
	if !hermesMarkedBlockIsCurrent([]byte(folded), want) {
		t.Fatal("the fixture itself is not read; every case below would pass for the wrong reason")
	}
	matcher := hermesHookLines("")[3] + "\n"
	for name, edit := range map[string]func(string) string{
		"a further key in the entry":   func(s string) string { return strings.Replace(s, matcher, matcher+"      timeout: 5\n", 1) },
		"a second entry":               func(s string) string { return strings.Replace(s, matcher, matcher+"    - command: other\n", 1) },
		"another event":                func(s string) string { return strings.Replace(s, matcher, matcher+"  post_tool_call: []\n", 1) },
		"a different matcher":          func(s string) string { return strings.Replace(s, matcher, "      matcher: \"browser\"\n", 1) },
		"no matcher":                   func(s string) string { return strings.Replace(s, matcher, "", 1) },
		"a blank line inside the fold": func(s string) string { return strings.Replace(s, "\n        hermes", "\n\n        hermes", 1) },
		// A tab where only the tab check can see it. The first version of this
		// case put it in front of pre_tool_call:, a line compared as text, so
		// it failed whether or not tabs were checked and the check could be
		// deleted with every test green. These two lines are judged by depth,
		// and len() of their leading whitespace counts a tab as one column: six
		// for the matcher, eight for the continuation, exactly as rendered.
		"a tab in the matcher's indentation": func(s string) string {
			return strings.Replace(s, "      matcher:", "  \t   matcher:", 1)
		},
		"a tab in the continuation's indentation": func(s string) string {
			return strings.Replace(s, "\n        hermes", "\n\t       hermes", 1)
		},
		// The entry two columns deeper, every line of it: still a YAML list
		// entry, and not where the renderer puts one.
		"the entry re-indented": func(s string) string {
			for _, line := range []string{"    - command:", "        hermes pre_tool_call'", "      matcher:"} {
				s = strings.Replace(s, "\n"+line, "\n  "+line, 1)
			}
			return s
		},
		// The command line alone, with the matcher left where it was. This is
		// the case that pins the command's OWN depth, and the case above does
		// not: moving the matcher too makes the matcher's depth check refuse
		// the block first, so the command's depth can stop being read at all
		// and every test stays green. A mutation making the `    - command:`
		// prefix indentation-insensitive survived the whole package until this
		// case existed. What it lets through is a file no parser loads — a
		// sequence item and a mapping key at one depth — which is `already set
		// up` claimed on a config Hermes cannot start on.
		"the command line alone re-indented": func(s string) string {
			return strings.Replace(s, "\n    - command:", "\n      - command:", 1)
		},
		// Today's command and then more: an equality weakened to "contains"
		// calls this installed, and Hermes would run the rest.
		"today's command with more after it": func(s string) string {
			return strings.Replace(s, "hermes pre_tool_call'", "hermes pre_tool_call && curl https://example.invalid/x'", 1)
		},
	} {
		t.Run(name, func(t *testing.T) {
			edited := edit(folded)
			if edited == folded {
				t.Fatal("the edit did not apply")
			}
			if hermesMarkedBlockIsCurrent([]byte(edited), want) {
				t.Errorf("read as today's entry and nothing besides:\n%s", edited)
			}
		})
	}
}

func TestOurHookIsFoundInTheFormHermesWrites(t *testing.T) {
	for name, tc := range hermesResavedEntries {
		t.Run(name, func(t *testing.T) {
			resaved := readHermesFixture(t, name+".resaved.yaml")
			ref := refFor(tc.entry)

			own := findHermesOwnEntry([]byte(resaved), ref)
			if !own.found {
				t.Fatalf("not found in the form Hermes writes:\n%s", resaved)
			}
			// #108: removal is no longer limited to the renderer's own form.
			// Until this commit the two lines below read "removal by line is
			// limited to the renderer's own form; this is Hermes'" and
			// required own.why to name that, which is the deferral #108 was
			// filed against: for every participant whose Hermes has saved once
			// — all of them, eventually — uninstall ended in a manual step.
			if !own.removable() {
				t.Fatalf("not removable in the form Hermes writes (%s):\n%s", own.why, resaved)
			}
			if own.why != "" {
				t.Errorf("removable and yet left because it %q", own.why)
			}
			// The lines it names are the entry's: from `- command:` through
			// `matcher:`, and nothing of the participant's.
			lines := strings.Split(resaved, "\n")
			if !strings.Contains(lines[own.first-1], "- command:") || !strings.Contains(lines[own.last-1], "matcher:") {
				t.Errorf("named %s, which is not the entry:\n%s", own.where(), resaved)
			}
			// And what goes is the entry and its heading lines, never a line
			// of the participant's: this fixture is our entry alone under
			// hooks:, so the whole of both goes and what came above it stays.
			//
			// Which, for the fixture whose original had no final newline, is
			// `model: gpt` WITH one: Hermes re-dumped the file, its dumper
			// ends every file with a newline, and the note that recorded the
			// original state was a comment PyYAML dropped. There is nothing
			// left on disk that says the file once ended without one, and
			// inventing it back would be an edit nobody asked for.
			want := hermesBeforeTheHooksKey(t, resaved)
			if got := string(removeHermesOwnEntry([]byte(resaved), own)); got != want {
				t.Errorf("uninstall left\n%q\nwant\n%q", got, want)
			}

			m, ops := newFakeMachine("hermes")
			m.files[hermesConfigPath] = []byte(resaved)

			if !hermesHookInstalled(ops, hermesConfigPath, tc.entry) {
				t.Error("status: the hook is there and will fire, and is not counted")
			}

			var install agentPlan
			if planHermesHook(ops, "Hermes", hermesConfigPath, tc.entry, &install) || len(install.writes) != 0 {
				t.Errorf("install planned a write: %+v", install.writes)
			}
			if len(install.refused) != 0 {
				t.Errorf("install refused, and printed a second copy to paste beside the one that is there:\n%s", strings.Join(install.refused, "\n"))
			}
			if got := strings.Join(install.notes, "\n"); !strings.Contains(got, "already set up") || !strings.Contains(got, own.where()) {
				t.Errorf("install did not say it is already set up, and where:\n%s", got)
			}

			var uninstall agentPlan
			hermesTarget{}.PlanUninstall(ops, agentPaths{hermesConfig: hermesConfigPath, hermesSkill: hermesSkillPath}, tc.entry, noEnv, &uninstall)
			if len(uninstall.writes) != 1 {
				t.Fatalf("uninstall planned %d writes, want the one that removes the entry; notes: %v", len(uninstall.writes), uninstall.notes)
			}
			if got := string(uninstall.writes[0].contents); got != want {
				t.Errorf("uninstall left\n%q\nwant the participant's file\n%q", got, want)
			}
			if notes := strings.Join(uninstall.notes, "\n"); strings.Contains(notes, "remove that entry by hand") {
				t.Errorf("uninstall still asks for the manual step #108 is about:\n%s", notes)
			}
			if got := strings.Join(uninstall.skipped, "\n"); strings.Contains(got, "not installed") {
				t.Errorf("uninstall says both that our hook is there and that Hermes is not installed:\n%s", got)
			}
		})
	}
}

// The decode accepts a single-quoted scalar only when the renderer's encoder
// reproduces it, and inside our block that check is load-bearing in one place:
// a path with an apostrophe. Un-double one of its quotes and a lenient reading
// decodes to the very same command — byte for byte today's entry — from a
// file no YAML parser accepts, so Hermes cannot load it and no hook in it is
// live. Without the check install calls that finished and never repairs it.
func TestAMarkedBlockWithALoneQuoteIsNotTodaysEntry(t *testing.T) {
	entry := binEntry{command: "/Users/O'Neil/bin/dropin-miner", cfg: testCfg}
	cmd, _ := hermesHookCommand(entry, false)
	rendered := string(hermesAppendBlock([]byte("model: gpt\n"), strings.Join(hermesHookLines(cmd), "\n")+"\n"))
	if !hermesMarkedBlockIsCurrent([]byte(rendered), cmd) {
		t.Fatal("the rendered block itself is not read; the case below would pass for the wrong reason")
	}
	// The renderer spells the apostrophe '\'' and YAML doubles each quote of
	// it; take the doubling off the last one.
	const doubled = `''\''''`
	if strings.Count(rendered, doubled) != 1 {
		t.Fatalf("the apostrophe is not spelled the way this test expects:\n%s", rendered)
	}
	broken := strings.Replace(rendered, doubled, `''\'''`, 1)
	// The lenient reading is hermesUnquoteSingle without its check: the outer
	// quotes off, every doubled quote undone. It must give today's command, or
	// this case passes without the check it is here to pin.
	var lenient string
	for _, line := range strings.Split(broken, "\n") {
		if scalar, ok := strings.CutPrefix(line, hermesCommandPrefix); ok {
			lenient = strings.ReplaceAll(scalar[1:len(scalar)-1], "''", "'")
		}
	}
	if lenient != cmd {
		t.Fatalf("a lenient reading of the broken scalar is %q, not today's command %q", lenient, cmd)
	}
	if hermesMarkedBlockIsCurrent([]byte(broken), cmd) {
		t.Errorf("a scalar no YAML parser accepts was read as today's entry:\n%s", broken)
	}
}

// And through the real commands, where this runner can resolve the entry.
func TestTheRealCommandsReadOurBlockAfterHermesFoldsIt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the POSIX entry does not resolve on Windows; the function-level cases above run here instead")
	}
	roundtrip := readHermesFixture(t, "posix.roundtrip.yaml")
	m, ops := newFakeMachine("hermes")
	m.files[hermesConfigPath] = []byte(roundtrip)

	// The first run has the skill to write; the config is already right.
	if code, out, errOut := runAgents(t, ops, nil, "install", "-config", testCfg, "-yes"); code != exitOK {
		t.Fatalf("install: exit %d\n%s%s", code, out, errOut)
	}
	if got := string(m.files[hermesConfigPath]); got != roundtrip {
		t.Errorf("install rewrote a block that already holds today's entry:\n%s", got)
	}
	code, out, errOut := runAgents(t, ops, nil, "install", "-config", testCfg, "-yes")
	if code != exitOK || !strings.Contains(out, "Hermes: already installed") {
		t.Errorf("a second install is not a no-op: exit %d\n%s%s", code, out, errOut)
	}
	_, status, _ := runAgents(t, ops, nil, "status", "-config", testCfg)
	if !strings.Contains(status, "installed (skill+hook)") {
		t.Errorf("status:\n%s", status)
	}
	if code, out, errOut := runAgents(t, ops, nil, "uninstall", "-config", testCfg, "-yes"); code != exitOK {
		t.Fatalf("uninstall: exit %d\n%s%s", code, out, errOut)
	}
	if got, want := string(m.files[hermesConfigPath]), hermesAroundOurBlock(t, roundtrip); got != want {
		t.Errorf("uninstall did not leave the surrounding file byte for byte\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// And through the real commands, where this runner can resolve the entry.
func TestTheRealCommandsFindOurHookInTheFormHermesWrites(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the POSIX entry does not resolve on Windows; the function-level cases above run here instead")
	}
	resaved := readHermesFixture(t, "posix.resaved.yaml")
	m, ops := newFakeMachine("hermes")
	m.files[hermesConfigPath] = []byte(resaved)

	code, out, errOut := runAgents(t, ops, nil, "install", "-config", testCfg, "-yes")
	if code != exitOK || !strings.Contains(out, "already set up") || strings.Contains(out, "by hand") {
		t.Errorf("install: exit %d\n%s%s", code, out, errOut)
	}
	_, status, _ := runAgents(t, ops, nil, "status", "-config", testCfg)
	if !strings.Contains(status, "installed (skill+hook)") {
		t.Errorf("status:\n%s", status)
	}
	// #108: uninstall takes it out, where it used to name lines 4-6 and ask
	// the participant to delete them.
	code, out, errOut = runAgents(t, ops, nil, "uninstall", "-config", testCfg, "-yes")
	if code != exitOK || strings.Contains(out, "remove that entry by hand") {
		t.Errorf("uninstall: exit %d\n%s%s", code, out, errOut)
	}
	if got, want := string(m.files[hermesConfigPath]), "model: gpt\n"; got != want {
		t.Errorf("uninstall left\n%q\nwant the participant's own file\n%q", got, want)
	}
}
