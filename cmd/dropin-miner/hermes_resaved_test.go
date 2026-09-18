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
		if rendered, _ := hermesHookCommand(tc.entry, tc.windows); strings.Contains(resaved, rendered) {
			t.Errorf("%s.resaved.yaml holds the command on one line; this fixture no longer tests a folded scalar:\n%s", name, resaved)
		}
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
