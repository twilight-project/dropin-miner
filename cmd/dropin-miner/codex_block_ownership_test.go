package main

// #82 and #88 item 4, driven through `agents install` and `agents uninstall`
// against the shapes Codex actually leaves behind.
//
// The damage #82 reports is the reason these cases exist rather than a unit
// test of the split alone: the tester's uninstall left a 0-byte
// ~/.codex/config.toml, destroying Codex's folder trust and its `[windows]
// sandbox = "unelevated"` choice, and the setup that followed restored only
// dropin-miner's own block. Codex's sandbox settings are what decide whether
// a search records at all, so this is the one host where losing a config
// costs a participant their earnings as well as their settings.
//
// Every fixture puts Codex's tables where Codex puts them — appended at the
// end of the file, which is INSIDE our markers whenever our block is last,
// and install made it last.

import (
	"path/filepath"
	"strings"
	"testing"
)

// codexFixture is a Codex config.toml with our installed block and whatever
// the host appended, in the position named.
type codexPlacement int

const (
	insideOurBlock codexPlacement = iota // what Codex does: append to the end of the file
	afterOurBlock                        // #88 item 4: restored by hand below the end marker
	beforeOurBlock                       // our block is not the last thing in the file
)

const (
	codexTrust   = "[projects.'/home/u/work']\ntrust_level = \"trusted\"\n"
	codexWindows = "[windows]\nsandbox = \"unelevated\"\n"
)

// installedCodexConfig runs a real install, then places Codex's own two
// tables where the case wants them. It returns the file as it then stands.
func installedCodexConfig(t *testing.T, where codexPlacement) (m *fakeMachine, ops agentOps, cfgPath, before string) {
	t.Helper()
	cfgPath, _ = sandboxTestConfig(t)
	m, ops = newFakeMachine("codex")
	m.files["/home/u/.codex/config.toml"] = []byte("model = \"gpt-5\"\n")
	if code, out, errOut := runAgents(t, ops, nil, "install", "-config", cfgPath, "-yes"); code != exitOK {
		t.Fatalf("install: %d\n%s%s", code, out, errOut)
	}
	installed := string(m.files["/home/u/.codex/config.toml"])
	host := codexTrust + codexWindows

	switch where {
	case insideOurBlock:
		// Codex appends to the end of the file. Our block is last, so its
		// end marker is the last line and the append lands between the
		// markers — which is the whole of #82.
		i := strings.LastIndex(installed, agentsMarkerEnd)
		if i < 0 {
			t.Fatalf("no end marker in the installed config:\n%s", installed)
		}
		installed = installed[:i] + host + installed[i:]
	case afterOurBlock:
		installed = strings.TrimRight(installed, "\n") + "\n\n" + host
	case beforeOurBlock:
		i := strings.Index(installed, agentsMarkerBegin)
		if i < 0 {
			t.Fatalf("no begin marker in the installed config:\n%s", installed)
		}
		installed = installed[:i] + host + "\n" + installed[i:]
	}
	m.files["/home/u/.codex/config.toml"] = []byte(installed)
	return m, ops, cfgPath, installed
}

// keptVerbatim is the assertion #82 is about: the host's tables are still
// there, byte for byte, and our own is not.
func keptVerbatim(t *testing.T, got string) {
	t.Helper()
	for _, want := range []string{codexTrust, codexWindows} {
		if !strings.Contains(got, want) {
			t.Errorf("Codex's own table did not survive byte-identical.\nwant to find:\n%s\ngot file:\n%s", want, got)
		}
	}
	if strings.Contains(got, agentsMarkerBegin) || strings.Contains(got, "["+codexSandboxTable+"]") {
		t.Errorf("our own block survived the uninstall:\n%s", got)
	}
	if !strings.Contains(got, "model = \"gpt-5\"") {
		t.Errorf("the participant's own setting outside the block was lost:\n%s", got)
	}
}

func TestUninstallKeepsTheTablesCodexAppendedIntoOurBlock(t *testing.T) {
	for _, tc := range []struct {
		name  string
		where codexPlacement
	}{
		{"Codex appended them inside our markers", insideOurBlock},
		{"our block is not the last thing in the file", beforeOurBlock},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, ops, cfgPath, _ := installedCodexConfig(t, tc.where)
			code, out, errOut := runAgents(t, ops, nil, "uninstall", "-config", cfgPath, "-yes")
			if code != exitOK {
				t.Fatalf("uninstall: %d\n%s%s", code, out, errOut)
			}
			keptVerbatim(t, string(m.files["/home/u/.codex/config.toml"]))
		})
	}
}

// The plan says what it is keeping and names it, so a participant reading a
// dry run knows their settings are not about to go.
func TestTheUninstallDryRunNamesTheTablesItKeeps(t *testing.T) {
	m, ops, cfgPath, before := installedCodexConfig(t, insideOurBlock)
	code, out, errOut := runAgents(t, ops, nil, "uninstall", "-config", cfgPath, "-dry-run")
	if code != exitOK {
		t.Fatalf("dry run: %d\n%s%s", code, out, errOut)
	}
	for _, want := range []string{"keeping 2 tables", "[projects.'/home/u/work']", "[windows]"} {
		if !strings.Contains(out, want) {
			t.Errorf("the dry run did not say %q:\n%s", want, out)
		}
	}
	if after := string(m.files["/home/u/.codex/config.toml"]); after != before {
		t.Errorf("the dry run changed the file:\n--- before ---\n%s\n--- after ---\n%s", before, after)
	}
}

// A file holding nothing but our block is unchanged from v0.2.9: the block
// goes and what is left is what was left before. This is the case the
// existing golden covers, pinned here so the keeping path cannot quietly
// change it.
func TestUninstallWithNothingOfCodexsInTheBlockIsUnchanged(t *testing.T) {
	cfgPath, _ := sandboxTestConfig(t)
	m, ops := newFakeMachine("codex")
	if code, out, errOut := runAgents(t, ops, nil, "install", "-config", cfgPath, "-yes"); code != exitOK {
		t.Fatalf("install: %d\n%s%s", code, out, errOut)
	}
	if code, out, errOut := runAgents(t, ops, nil, "uninstall", "-config", cfgPath, "-yes"); code != exitOK {
		t.Fatalf("uninstall: %d\n%s%s", code, out, errOut)
	}
	if got := strings.TrimSpace(string(m.files["/home/u/.codex/config.toml"])); got != "" {
		t.Errorf("a config holding only our block did not come back empty:\n%q", got)
	}
}

// #88 item 4: the idempotence check compares our block's content, not the
// file's tail. With Codex's tables restored below the end marker, the block
// is unchanged and there is nothing to do.
func TestASecondInstallHasNothingToDoWhateverFollowsOurBlock(t *testing.T) {
	for _, tc := range []struct {
		name  string
		where codexPlacement
	}{
		{"Codex's tables below our end marker", afterOurBlock},
		{"our block is not the last thing in the file", beforeOurBlock},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, ops, cfgPath, before := installedCodexConfig(t, tc.where)
			code, out, errOut := runAgents(t, ops, nil, "install", "-config", cfgPath, "-dry-run")
			if code != exitOK {
				t.Fatalf("dry run: %d\n%s%s", code, out, errOut)
			}
			if !strings.Contains(out, "nothing to do") {
				t.Errorf("a second install still plans a write although our block is unchanged:\n%s", out)
			}
			if after := string(m.files["/home/u/.codex/config.toml"]); after != before {
				t.Errorf("the dry run changed the file:\n%s", after)
			}
		})
	}
}

// The full round trip the plan asks for: a real uninstall, a fresh install,
// then a dry run that has nothing to do. This is what the tester did by
// hand, and where they found install planning a write forever.
func TestUninstallThenInstallThenADryRunHasNothingToDo(t *testing.T) {
	m, ops, cfgPath, _ := installedCodexConfig(t, insideOurBlock)

	if code, out, errOut := runAgents(t, ops, nil, "uninstall", "-config", cfgPath, "-yes"); code != exitOK {
		t.Fatalf("uninstall: %d\n%s%s", code, out, errOut)
	}
	keptVerbatim(t, string(m.files["/home/u/.codex/config.toml"]))

	if code, out, errOut := runAgents(t, ops, nil, "install", "-config", cfgPath, "-yes"); code != exitOK {
		t.Fatalf("reinstall: %d\n%s%s", code, out, errOut)
	}
	restored := string(m.files["/home/u/.codex/config.toml"])
	for _, want := range []string{codexTrust, codexWindows, agentsMarkerBegin} {
		if !strings.Contains(restored, want) {
			t.Fatalf("after reinstall, %q is missing:\n%s", want, restored)
		}
	}

	code, out, errOut := runAgents(t, ops, nil, "install", "-config", cfgPath, "-dry-run")
	if code != exitOK {
		t.Fatalf("dry run: %d\n%s%s", code, out, errOut)
	}
	if !strings.Contains(out, "nothing to do") {
		t.Errorf("the dry run after a fresh install still plans a write:\n%s", out)
	}
	if after := string(m.files["/home/u/.codex/config.toml"]); after != restored {
		t.Errorf("the dry run changed the file:\n%s", after)
	}
}

// Install moves a table Codex appended into our block out below it, so the
// host's next append lands outside ours and cannot be swept up again. The
// same byte-range delete was on the install path too — the issue reported
// only uninstall because that is where the tester met it.
func TestInstallMovesCodexsTablesOutOfOurBlockRatherThanDeletingThem(t *testing.T) {
	m, ops, cfgPath, _ := installedCodexConfig(t, insideOurBlock)

	code, out, errOut := runAgents(t, ops, nil, "install", "-config", cfgPath, "-yes")
	if code != exitOK {
		t.Fatalf("install: %d\n%s%s", code, out, errOut)
	}
	got := string(m.files["/home/u/.codex/config.toml"])
	for _, want := range []string{codexTrust, codexWindows} {
		if !strings.Contains(got, want) {
			t.Fatalf("install deleted a table Codex had appended into our block:\n%s", got)
		}
	}
	end := strings.Index(got, agentsMarkerEnd)
	if end < 0 {
		t.Fatalf("our block is gone:\n%s", got)
	}
	for _, want := range []string{codexTrust, codexWindows} {
		if strings.Index(got, want) < end {
			t.Errorf("%q is still inside our markers, where the next append would join it:\n%s", want, got)
		}
	}
	if !strings.Contains(out, "moving 2 tables") {
		t.Errorf("the plan did not say it was moving them:\n%s", out)
	}
}

// A block whose tables cannot be read is left exactly as it is, and said so
// — the refuse-rather-than-guess rule the installer uses everywhere else.
// Deleting a region this client cannot parse is how a participant's config
// gets destroyed, which is the whole of #82.
func TestABlockThatCannotBeReadIsLeftAloneAndReported(t *testing.T) {
	cfgPath, _ := sandboxTestConfig(t)
	m, ops := newFakeMachine("codex")
	if code, out, errOut := runAgents(t, ops, nil, "install", "-config", cfgPath, "-yes"); code != exitOK {
		t.Fatalf("install: %d\n%s%s", code, out, errOut)
	}
	installed := string(m.files["/home/u/.codex/config.toml"])
	// A table header spelled inside a multi-line string: a line scan that
	// cut there would leave an unterminated string on both sides.
	i := strings.LastIndex(installed, agentsMarkerEnd)
	broken := installed[:i] + "[notes]\ntext = \"\"\"\n[windows]\nnot a table\n\"\"\"\n" + installed[i:]
	m.files["/home/u/.codex/config.toml"] = []byte(broken)

	code, out, errOut := runAgents(t, ops, nil, "uninstall", "-config", cfgPath, "-yes")
	if code != exitOK {
		t.Fatalf("uninstall: %d\n%s%s", code, out, errOut)
	}
	if got := string(m.files["/home/u/.codex/config.toml"]); got != broken {
		t.Errorf("a block that could not be read was changed anyway:\n%s", got)
	}
	if !strings.Contains(out, "cannot be read as TOML tables") {
		t.Errorf("the refusal was not reported:\n%s", out)
	}
}

// H5's attribution still decides first: another installation's block is left
// alone whatever is inside it, so this commit cannot have widened what
// uninstall is willing to touch.
func TestAnotherInstallationsBlockIsStillLeftAloneWithHostTablesInIt(t *testing.T) {
	cfgPath, _ := sandboxTestConfig(t)
	m, ops := newFakeMachine("codex")
	other := filepath.Join(t.TempDir(), "other-install")
	block := string(codexSandboxBlock([]string{filepath.Join(other, "state")}))
	i := strings.LastIndex(block, agentsMarkerEnd)
	seeded := "model = \"gpt-5\"\n\n" + block[:i] + codexWindows + block[i:]
	m.files["/home/u/.codex/config.toml"] = []byte(seeded)

	code, out, errOut := runAgents(t, ops, nil, "uninstall", "-config", cfgPath, "-yes")
	if code != exitOK {
		t.Fatalf("uninstall: %d\n%s%s", code, out, errOut)
	}
	if got := string(m.files["/home/u/.codex/config.toml"]); got != seeded {
		t.Errorf("another installation's block was touched:\n--- before ---\n%s\n--- after ---\n%s", seeded, got)
	}
	if !strings.Contains(out, "another installation's") {
		t.Errorf("the reason was not reported:\n%s", out)
	}
}
