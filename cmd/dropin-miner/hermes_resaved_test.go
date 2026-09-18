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
}{
	"posix":   {binEntry{command: "/home/u/.tokendrop/bin/dropin-miner", cfg: "/home/u/.tokendrop/tokendrop.toml"}, false},
	"windows": {binEntry{command: `C:\Users\u\.tokendrop\bin\dropin-miner.exe`, cfg: `C:\Users\u\.tokendrop\tokendrop.toml`}, true},
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
		out[name] = string(hermesAppendBlock([]byte("model: gpt\n"), body))
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
		roundtrip := readHermesFixture(t, name+".roundtrip.yaml")
		for _, kept := range []string{agentsMarkerBegin + "\n", agentsMarkerEnd + "\n", hermesHookLines("")[3] + "\n"} {
			if !strings.Contains(roundtrip, kept) {
				t.Errorf("%s.roundtrip.yaml has lost %q: it is not the round-trip writer's output", name, kept)
			}
		}
		if strings.Contains(roundtrip, rendered) {
			t.Errorf("%s.roundtrip.yaml holds the command on one line; this fixture no longer tests a folded scalar:\n%s", name, roundtrip)
		}
	}
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
		switch strings.TrimRight(l, "\n") {
		case agentsMarkerBegin:
			begin = i
		case agentsMarkerEnd:
			end = i
		}
	}
	if begin < 1 || end < begin || lines[begin-1] != "\n" {
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
			hermesTarget{}.PlanUninstall(ops, agentPaths{hermesConfig: hermesConfigPath, hermesSkill: hermesSkillPath}, tc.entry, &uninstall)
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
// is anything left to do. Each entry below differs from this installation's in
// one way, is read out of the folded fixture or rendered into a block, and
// must still be refreshed exactly as it was before #105.
func TestAMarkedBlockThatIsNotTodaysEntryIsStillRefreshed(t *testing.T) {
	ours := hermesResavedEntries["posix"].entry
	folded := readHermesFixture(t, "posix.roundtrip.yaml")

	// v0.2.9's %q spelling: this installation's under H5, and not a command
	// either of Hermes' splitters reads the way it was meant.
	stale := strconv.Quote(ours.command) + " hook -config " + strconv.Quote(ours.cfg) + " hermes pre_tool_call"
	if !hermesCommandIsOurHook(stale, refFor(ours)) {
		t.Fatal("v0.2.9's spelling is no longer this installation's under H5, so this case tests nothing")
	}
	staleBlock := string(hermesAppendBlock([]byte("model: gpt\n"), strings.Join(hermesHookLines(stale), "\n")+"\n"))

	for name, tc := range map[string]struct {
		config string
		entry  binEntry
	}{
		"folded, another config":      {folded, binEntry{command: ours.command, cfg: "/somewhere/else/tokendrop.toml"}},
		"folded, another binary":      {folded, binEntry{command: "/somewhere/else/bin/dropin-miner", cfg: ours.cfg}},
		"folded, no config":           {folded, binEntry{command: ours.command}},
		"ours in the v0.2.9 spelling": {staleBlock, ours},
	} {
		t.Run(name, func(t *testing.T) {
			m, ops := newFakeMachine("hermes")
			m.files[hermesConfigPath] = []byte(tc.config)
			if hermesHookInstalledFor(ops, hermesConfigPath, tc.entry, false) {
				t.Error("status counted a block that does not hold today's entry for this installation")
			}
			var install agentPlan
			if !planHermesHookFor(ops, "Hermes", hermesConfigPath, tc.entry, false, &install) || len(install.writes) != 1 {
				t.Fatalf("install did not refresh the block: writes %+v, refused %v", install.writes, install.refused)
			}
			want, _ := hermesHookCommand(tc.entry, false)
			if got := string(install.writes[0].contents); !strings.Contains(got, strings.Join(hermesHookLines(want), "\n")+"\n") {
				t.Errorf("the refreshed block does not hold the rendered entry:\n%s", got)
			}
		})
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
		"a tab in the indentation":     func(s string) string { return strings.Replace(s, "  pre_tool_call:", "\tpre_tool_call:", 1) },
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
			if own.removable() {
				t.Fatalf("removal by line is limited to the renderer's own form; this is Hermes':\n%s", resaved)
			}
			if !strings.Contains(own.why, "Hermes rewrites config.yaml") {
				t.Errorf("why = %q", own.why)
			}
			// The lines it names are the entry's: from `- command:` through
			// `matcher:`, and nothing of the participant's.
			lines := strings.Split(resaved, "\n")
			if !strings.Contains(lines[own.first-1], "- command:") || !strings.Contains(lines[own.last-1], "matcher:") {
				t.Errorf("named %s, which is not the entry:\n%s", own.where(), resaved)
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
			hermesTarget{}.PlanUninstall(ops, agentPaths{hermesConfig: hermesConfigPath, hermesSkill: "/home/u/.hermes/skills/dropin-miner/SKILL.md"}, tc.entry, &uninstall)
			if len(uninstall.writes) != 0 {
				t.Fatalf("uninstall edited a form it must only report: %+v", uninstall.writes)
			}
			notes := strings.Join(uninstall.notes, "\n")
			for _, want := range []string{"this installation's pre_tool_call hook", own.where(), "remove that entry by hand"} {
				if !strings.Contains(notes, want) {
					t.Errorf("uninstall did not say %q:\n%s", want, notes)
				}
			}
			if got := strings.Join(uninstall.skipped, "\n"); strings.Contains(got, "not installed") {
				t.Errorf("uninstall says both that our hook is there and that Hermes is not installed:\n%s\n%s", notes, got)
			}
			if got := string(m.files[hermesConfigPath]); got != resaved {
				t.Errorf("the file was changed:\n%s", got)
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
	code, out, errOut = runAgents(t, ops, nil, "uninstall", "-config", testCfg, "-yes")
	if code != exitOK || !strings.Contains(out, "remove that entry by hand") || !strings.Contains(out, "lines 4-6") {
		t.Errorf("uninstall: exit %d\n%s%s", code, out, errOut)
	}
	if got := string(m.files[hermesConfigPath]); got != resaved {
		t.Errorf("the file was changed:\n%s", got)
	}
}
