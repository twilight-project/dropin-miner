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
	otherRoots := []string{filepath.Join(other, "state")}
	block := string(codexSandboxBlock(otherRoots))
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
	// The reason names the owner, in the words a left skill is named in
	// (#128). Taken from the production reading rather than typed: the
	// sentence spells the home the roots lie under, and a typed copy of it
	// would pass while production named something else.
	if want := belongsTo(describeSandboxOwner(otherRoots)); !strings.Contains(out, want) {
		t.Errorf("the reason was not reported\nwant to find: %s\ngot:\n%s", want, out)
	}
}

// ── L2b: a header the first pattern could not see ───────────────────────

// Codex keys a project's trust by its path, so a folder named `work [1]`
// puts a `]` inside a quoted key. The first header pattern said "anything
// but ]" between the brackets, could not match these, and so saw no
// boundary: the table merged into our section and was deleted with it, exit
// 0, no note. Reproduced through both real paths before this was written.
var bracketedHeaders = []struct{ name, table string }{
	{"a ] inside a single-quoted key", "[projects.'/home/u/work [1]']\ntrust_level = \"trusted\"\n"},
	{"a ] inside a double-quoted key", "[projects.\"/home/u/a]b\"]\ntrust_level = \"trusted\"\n"},
	{"an escaped quote before the ]", "[projects.\"/home/u/say \\\"hi\\\" ]x\"]\ntrust_level = \"trusted\"\n"},
}

// insideOurMarkers runs a real install and then puts text where Codex puts
// its appends: at the end of the file, which is inside our markers.
func insideOurMarkers(t *testing.T, text string) (m *fakeMachine, ops agentOps, cfgPath, seeded string) {
	t.Helper()
	cfgPath, _ = sandboxTestConfig(t)
	m, ops = newFakeMachine("codex")
	m.files["/home/u/.codex/config.toml"] = []byte("model = \"gpt-5\"\n")
	if code, out, errOut := runAgents(t, ops, nil, "install", "-config", cfgPath, "-yes"); code != exitOK {
		t.Fatalf("install: %d\n%s%s", code, out, errOut)
	}
	installed := string(m.files["/home/u/.codex/config.toml"])
	i := strings.LastIndex(installed, agentsMarkerEnd)
	if i < 0 {
		t.Fatalf("no end marker in the installed config:\n%s", installed)
	}
	seeded = installed[:i] + text + installed[i:]
	m.files["/home/u/.codex/config.toml"] = []byte(seeded)
	return m, ops, cfgPath, seeded
}

// Kept AND removed, not merely refused: a refusal also leaves the table in
// the file, so asserting only that it survived would pass on the net alone
// and leave the grammar unpinned.
func TestUninstallKeepsATableWhoseHeaderHoldsABracket(t *testing.T) {
	for _, tc := range bracketedHeaders {
		t.Run(tc.name, func(t *testing.T) {
			m, ops, cfgPath, _ := insideOurMarkers(t, tc.table)
			code, out, errOut := runAgents(t, ops, nil, "uninstall", "-config", cfgPath, "-yes")
			if code != exitOK {
				t.Fatalf("uninstall: %d\n%s%s", code, out, errOut)
			}
			got := string(m.files["/home/u/.codex/config.toml"])
			if !strings.Contains(got, tc.table) {
				t.Fatalf("the table was destroyed:\n%s", got)
			}
			if strings.Contains(got, agentsMarkerBegin) || strings.Contains(got, "["+codexSandboxTable+"]") {
				t.Errorf("our own table was not removed — the header was not recognized as a boundary, and only the net saved the table:\n%s\n%s", got, out)
			}
			if !strings.Contains(out, "keeping 1 table") {
				t.Errorf("the plan did not name what it kept:\n%s", out)
			}
		})
	}
}

func TestInstallKeepsATableWhoseHeaderHoldsABracket(t *testing.T) {
	for _, tc := range bracketedHeaders {
		t.Run(tc.name, func(t *testing.T) {
			m, ops, cfgPath, _ := insideOurMarkers(t, tc.table)
			code, out, errOut := runAgents(t, ops, nil, "install", "-config", cfgPath, "-yes")
			if code != exitOK {
				t.Fatalf("install: %d\n%s%s", code, out, errOut)
			}
			got := string(m.files["/home/u/.codex/config.toml"])
			if !strings.Contains(got, tc.table) {
				t.Fatalf("the table was destroyed:\n%s", got)
			}
			end := strings.Index(got, agentsMarkerEnd)
			if end < 0 || strings.Index(got, tc.table) < end {
				t.Errorf("the table was not moved out below our block — the header was not recognized as a boundary, and only the net saved the table:\n%s\n%s", got, out)
			}
		})
	}
}

// The net, handed directly the thing it exists to stop: a section classified
// as ours that is carrying somebody else's table. No header trick is needed
// to build it, which is the point — the net must not depend on knowing which
// header the scan will miss next.
func TestTheNetRefusesASectionThatIsNotOnlyOurs(t *testing.T) {
	ours := sandboxSettings([]string{"/home/u/.tokendrop/state"})
	for _, tc := range []struct {
		name string
		text string
		want bool
	}{
		{"our table alone", ours, true},
		{"our table with our comments above it", "# a comment\n" + ours, true},
		{"our table and a foreign one", ours + "[windows]\nsandbox = \"unelevated\"\n", false},
		{"our table and the table the first pattern missed", ours + "[projects.'/home/u/work [1]']\ntrust_level = \"trusted\"\n", false},
		{"a sub-table nested under our own name", ours + "[" + codexSandboxTable + ".'a]b']\nk = 1\n", false},
		{"an inline table inside ours", ours + "extra = { k = 1 }\n", false},
		{"a foreign table alone", "[windows]\nsandbox = \"unelevated\"\n", false},
		{"not TOML", "= = =\n", false},
		{"nothing at all", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := oursIsOnlyOurs(tc.text); got != tc.want {
				t.Fatalf("oursIsOnlyOurs = %v, want %v, for:\n%s", got, tc.want, tc.text)
			}
		})
	}
}

// The net's other direction, through both real paths. A sub-table written
// under our own name is recognized as a header and classified as somebody
// else's — correctly, the renderer never writes it — but keeping it while
// removing or re-rendering our table would leave Codex a config that defines
// [sandbox_workspace_write] twice, or a child with no parent it was written
// for. Neither path may guess, so both leave the block exactly as it is and
// say so.
func TestABlockHoldingATableUnderOurNameIsLeftAlone(t *testing.T) {
	nested := "[" + codexSandboxTable + ".mine]\nk = 1\n"
	m, ops, cfgPath, seeded := insideOurMarkers(t, nested)
	for _, verb := range []string{"uninstall", "install"} {
		code, out, errOut := runAgents(t, ops, nil, verb, "-config", cfgPath, "-yes")
		if code != exitOK && verb == "uninstall" {
			t.Fatalf("%s: %d\n%s%s", verb, code, out, errOut)
		}
		if got := string(m.files["/home/u/.codex/config.toml"]); got != seeded {
			t.Fatalf("%s changed a block it could not attribute:\n%s", verb, got)
		}
		if !strings.Contains(out+errOut, "cannot be read as TOML tables") {
			t.Errorf("%s did not say why it left the block:\n%s%s", verb, out, errOut)
		}
	}
}

// A key the participant added inside OUR table goes with the table — the
// table between our markers is ours to render — and is named in the plan
// first, on both paths, never dropped silently.
func TestAKeyAddedInsideOurTableIsNamedBeforeItGoes(t *testing.T) {
	for _, verb := range []string{"uninstall", "install"} {
		t.Run(verb, func(t *testing.T) {
			_, ops, cfgPath, _ := insideOurMarkers(t, "exclude_slash_tmp = true\n")
			code, out, errOut := runAgents(t, ops, nil, verb, "-config", cfgPath, "-dry-run")
			if code != exitOK {
				t.Fatalf("%s -dry-run: %d\n%s%s", verb, code, out, errOut)
			}
			for _, want := range []string{"exclude_slash_tmp", "did not write", "goes with the table"} {
				if !strings.Contains(out, want) {
					t.Errorf("the plan did not say %q:\n%s", want, out)
				}
			}
		})
	}
}
