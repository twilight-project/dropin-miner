package main

// #83: this installation's pre_tool_call entry under a hooks: block the
// client did not write — recognized by install and status, removed by
// uninstall, and removed as exactly the lines the renderer writes or not at
// all.
//
// Hermes' config.yaml is edited here by hand with no parser, which is the
// same position L2 was in with Codex's TOML before review found the header
// it could not see. So the cases are weighted the way that lesson says they
// should be: for every shape that is removed there is a neighbor that must
// not be, and what is asserted about a removal is never only "our entry is
// gone" but "every other byte is where it was".
//
// The command in every fixture comes from a real install on this platform,
// not from a string typed here: the Windows runners quote it differently
// and resolve the config path differently, and a fixture that guessed would
// test the guess.

import (
	"strconv"
	"strings"
	"testing"
)

// ourHermesEntry is the resolved entry and the command a real install
// writes for it on this platform.
func ourHermesEntry(t *testing.T) (entry binEntry, cmd string) {
	t.Helper()
	m, ops := newFakeMachine("hermes")
	if code, out, errOut := runAgents(t, ops, nil, "install", "-config", testCfg, "-yes"); code != exitOK {
		t.Fatalf("install: %d\n%s%s", code, out, errOut)
	}
	for _, line := range strings.Split(string(m.files[hermesConfigPath]), "\n") {
		if c, ok := hermesDecodeCommandLine(line); ok {
			cmd = c
		}
	}
	if cmd == "" {
		t.Fatalf("a real install wrote no command line:\n%s", m.files[hermesConfigPath])
	}
	entry, _, err := resolveEntry(ops, testCfg, envOf(nil))
	if err != nil {
		t.Fatal(err)
	}
	return entry, cmd
}

// entryLines is the two lines of one hook entry, as the renderer writes them.
func entryLines(cmd string) string {
	l := hermesHookLines(cmd)
	return l[2] + "\n" + l[3] + "\n"
}

const (
	hermesBefore    = "model: gpt\n\n# my hooks\n"
	hermesAfter     = "database:\n  journal_mode: \"wal\"\n"
	foreignEntry    = "    - command: 'my-own-hook --flag'\n      matcher: \"terminal\"\n"
	foreignPostTool = "  post_tool_call:\n    - command: 'my-own-hook'\n"
)

// hermesCase is a whole config and what uninstall must leave of it.
type hermesCase struct {
	name   string
	config func(cmd string) string
	want   func(cmd string) string // the file afterwards; nil means byte-identical
	note   string                  // a phrase the plan must carry when it leaves ours in place
}

var hermesRemovals = []hermesCase{
	{
		name: "ours is the only hook: hooks:, pre_tool_call: and the entry all go",
		config: func(c string) string {
			return hermesBefore + "hooks:\n  pre_tool_call:\n" + entryLines(c) + hermesAfter
		},
		want: func(string) string { return hermesBefore + hermesAfter },
	},
	{
		name:   "ours is the only hook and hooks: is the last key",
		config: func(c string) string { return hermesBefore + "hooks:\n  pre_tool_call:\n" + entryLines(c) },
		want:   func(string) string { return hermesBefore },
	},
	{
		name: "a foreign entry after ours in the same list: only our two lines go",
		config: func(c string) string {
			return hermesBefore + "hooks:\n  pre_tool_call:\n" + entryLines(c) + foreignEntry + hermesAfter
		},
		want: func(string) string { return hermesBefore + "hooks:\n  pre_tool_call:\n" + foreignEntry + hermesAfter },
	},
	{
		name: "a foreign entry before ours in the same list",
		config: func(c string) string {
			return hermesBefore + "hooks:\n  pre_tool_call:\n" + foreignEntry + entryLines(c) + hermesAfter
		},
		want: func(string) string { return hermesBefore + "hooks:\n  pre_tool_call:\n" + foreignEntry + hermesAfter },
	},
	{
		name: "ours alone under pre_tool_call, another event beside it: pre_tool_call: goes with it, hooks: stays",
		config: func(c string) string {
			return hermesBefore + "hooks:\n  pre_tool_call:\n" + entryLines(c) + foreignPostTool + hermesAfter
		},
		want: func(string) string { return hermesBefore + "hooks:\n" + foreignPostTool + hermesAfter },
	},
	{
		name: "the other event comes first",
		config: func(c string) string {
			return hermesBefore + "hooks:\n" + foreignPostTool + "  pre_tool_call:\n" + entryLines(c) + hermesAfter
		},
		want: func(string) string { return hermesBefore + "hooks:\n" + foreignPostTool + hermesAfter },
	},
	{
		name: "a CRLF file: the lines that stay keep their own endings",
		config: func(c string) string {
			return strings.ReplaceAll(hermesBefore+"hooks:\n  pre_tool_call:\n"+entryLines(c)+foreignEntry+hermesAfter, "\n", "\r\n")
		},
		want: func(string) string {
			return strings.ReplaceAll(hermesBefore+"hooks:\n  pre_tool_call:\n"+foreignEntry+hermesAfter, "\n", "\r\n")
		},
	},
}

// The neighbors: each is one step away from a shape above, and must come
// back byte-identical. A missed boundary costs a refusal, never someone
// else's hook.
var hermesLeftAlone = []hermesCase{
	{
		name: "a further key inside the item: an entry somebody edited is theirs",
		config: func(c string) string {
			return "hooks:\n  pre_tool_call:\n" + entryLines(c) + "      timeout: 5\n" + hermesAfter
		},
	},
	{
		name: "a comment between pre_tool_call: and its only entry",
		config: func(c string) string {
			return "hooks:\n  pre_tool_call:\n    # added by me\n" + entryLines(c) + hermesAfter
		},
		note: "separated from its pre_tool_call: line",
	},
	{
		name: "a blank line between hooks: and its only event",
		config: func(c string) string {
			return "hooks:\n\n  pre_tool_call:\n" + entryLines(c) + hermesAfter
		},
		note: "separated from its hooks: line",
	},
	{
		name: "the same entry twice",
		config: func(c string) string {
			return "hooks:\n  pre_tool_call:\n" + entryLines(c) + entryLines(c) + hermesAfter
		},
		note: "more than once",
	},
	{
		name: "another subcommand of this same binary",
		config: func(c string) string {
			return "hooks:\n  pre_tool_call:\n" + entryLines(strings.Replace(c, "hermes pre_tool_call", "hermes post_tool_call", 1)) + hermesAfter
		},
	},
	{
		name: "an extra argument after ours",
		config: func(c string) string {
			return "hooks:\n  pre_tool_call:\n" + entryLines(c+" --verbose") + hermesAfter
		},
	},
	{
		name: "a different matcher",
		config: func(c string) string {
			return "hooks:\n  pre_tool_call:\n" + hermesHookLines(c)[2] + "\n      matcher: \".*\"\n" + hermesAfter
		},
	},
	{
		name: "a double-quoted scalar: not how the renderer writes it",
		config: func(c string) string {
			return "hooks:\n  pre_tool_call:\n    - command: " + strconv.Quote(c) + "\n      matcher: \"terminal\"\n" + hermesAfter
		},
	},
	{
		name: "a trailing comment on the command line",
		config: func(c string) string {
			return "hooks:\n  pre_tool_call:\n" + hermesHookLines(c)[2] + " # mine\n      matcher: \"terminal\"\n" + hermesAfter
		},
	},
	{
		name: "under another event",
		config: func(c string) string {
			return "hooks:\n  post_tool_call:\n" + entryLines(c) + hermesAfter
		},
	},
	{
		name: "nested one level deeper than the renderer puts it",
		config: func(c string) string {
			return "hooks:\n  group:\n    pre_tool_call:\n" + strings.ReplaceAll(entryLines(c), "    - ", "      - ") + hermesAfter
		},
	},
	{
		name: "hooks spelled another way",
		config: func(c string) string {
			return "\"hooks\":\n  pre_tool_call:\n" + entryLines(c) + hermesAfter
		},
	},
	{
		name: "two hooks: keys",
		config: func(c string) string {
			return "hooks:\n  pre_tool_call:\n" + entryLines(c) + "hooks:\n" + foreignPostTool
		},
	},
	{
		name: "a second YAML document",
		config: func(c string) string {
			return "hooks:\n  pre_tool_call:\n" + entryLines(c) + "---\nother: 1\n"
		},
	},
	{
		name: "tabs in the indentation",
		config: func(c string) string {
			return "hooks:\n\tpre_tool_call:\n" + entryLines(c) + hermesAfter
		},
	},
}

func TestUninstallRemovesOurHermesEntryAndNothingElse(t *testing.T) {
	_, cmd := ourHermesEntry(t)
	for _, tc := range hermesRemovals {
		t.Run(tc.name, func(t *testing.T) {
			m, ops := newFakeMachine("hermes")
			before := tc.config(cmd)
			m.files[hermesConfigPath] = []byte(before)

			// The dry run lists it, and changes nothing.
			code, out, errOut := runAgents(t, ops, nil, "uninstall", "-config", testCfg, "-dry-run")
			if code != exitOK {
				t.Fatalf("dry run: %d\n%s%s", code, out, errOut)
			}
			if !strings.Contains(out, "remove lineage hook from a hooks: block dropin-miner did not write") {
				t.Errorf("the dry run did not list the entry:\n%s", out)
			}
			if got := string(m.files[hermesConfigPath]); got != before {
				t.Fatalf("the dry run changed the file:\n%q", got)
			}

			if code, out, errOut := runAgents(t, ops, nil, "uninstall", "-config", testCfg, "-yes"); code != exitOK {
				t.Fatalf("uninstall: %d\n%s%s", code, out, errOut)
			}
			if got, want := string(m.files[hermesConfigPath]), tc.want(cmd); got != want {
				t.Errorf("after uninstall:\n--- got ---\n%q\n--- want ---\n%q", got, want)
			}
		})
	}
}

func TestUninstallLeavesAHermesEntryItCannotProveIsExactlyOurs(t *testing.T) {
	_, cmd := ourHermesEntry(t)
	for _, tc := range hermesLeftAlone {
		t.Run(tc.name, func(t *testing.T) {
			m, ops := newFakeMachine("hermes")
			before := tc.config(cmd)
			m.files[hermesConfigPath] = []byte(before)

			code, out, errOut := runAgents(t, ops, nil, "uninstall", "-config", testCfg, "-yes")
			if code != exitOK {
				t.Fatalf("uninstall: %d\n%s%s", code, out, errOut)
			}
			if got := string(m.files[hermesConfigPath]); got != before {
				t.Fatalf("the file was changed:\n--- got ---\n%q\n--- was ---\n%q", got, before)
			}
			if tc.note != "" && !strings.Contains(out, tc.note) {
				t.Errorf("ours was recognized and left, but the plan did not say why (%q):\n%s", tc.note, out)
			}
		})
	}
}

// Every spelling this client has written a command in, under H5's rule. The
// YAML around the command has been the same since the first Hermes commit;
// the command inside it is what varies by platform and by version.
func TestOurHermesEntryIsRecognizedInEverySpelling(t *testing.T) {
	entry, current := ourHermesEntry(t)
	tail := func(bin, cfg string) string { return bin + " hook -config " + cfg + " hermes pre_tool_call" }
	posix, _ := hermesHookCommand(entry, false)
	spellings := map[string]string{
		"what install writes here today": current,
		"Hermes on POSIX":                posix,
		"POSIX quotes":                   tail(posixQuoteArg(entry.command), posixQuoteArg(entry.cfg)),
		"v0.2.9's %q":                    tail(strconv.Quote(entry.command), strconv.Quote(entry.cfg)),
	}
	if windows, ok := hermesHookCommand(entry, true); ok {
		spellings["Hermes on Windows"] = windows
	}
	for name, cmd := range spellings {
		t.Run(name, func(t *testing.T) {
			m, ops := newFakeMachine("hermes")
			m.files[hermesConfigPath] = []byte("hooks:\n  pre_tool_call:\n" + entryLines(cmd) + foreignEntry)
			if code, out, errOut := runAgents(t, ops, nil, "uninstall", "-config", testCfg, "-yes"); code != exitOK {
				t.Fatalf("uninstall: %d\n%s%s", code, out, errOut)
			}
			if got, want := string(m.files[hermesConfigPath]), "hooks:\n  pre_tool_call:\n"+foreignEntry; got != want {
				t.Errorf("spelling %q was not removed cleanly:\n%q", cmd, got)
			}
		})
	}
}

// H5's rule: the same binary naming another installation's config is that
// installation's hook. Two installations share a binary whenever one was set
// up from the other, so the binary alone decides nothing.
func TestAnotherInstallationsHermesEntryIsNotOurs(t *testing.T) {
	entry, _ := ourHermesEntry(t)
	other := entry
	other.cfg = "/home/u/dm-disposable/tokendrop.toml"
	cmd, ok := hermesHookCommand(other, false)
	if !ok {
		t.Fatal("cannot render the other installation's command")
	}
	before := "hooks:\n  pre_tool_call:\n" + entryLines(cmd)

	m, ops := newFakeMachine("hermes")
	m.files[hermesConfigPath] = []byte(before)
	if code, out, errOut := runAgents(t, ops, nil, "uninstall", "-config", testCfg, "-yes"); code != exitOK {
		t.Fatalf("uninstall: %d\n%s%s", code, out, errOut)
	}
	if got := string(m.files[hermesConfigPath]); got != before {
		t.Fatalf("another installation's hook was removed:\n%q", got)
	}
	// And install does not mistake it for being set up.
	code, out, _ := runAgents(t, ops, nil, "install", "-config", testCfg, "-yes")
	if code != exitTransport || !strings.Contains(out, "already declares a top-level hooks: key") {
		t.Errorf("install with only another installation's entry: exit %d, want the refusal\n%s", code, out)
	}
}

// Install: a hooks: block that already holds our entry is not a refusal.
func TestInstallReportsAlreadySetUpWhenOurHermesEntryIsThere(t *testing.T) {
	_, cmd := ourHermesEntry(t)
	before := hermesBefore + "hooks:\n  pre_tool_call:\n" + foreignEntry + entryLines(cmd) + hermesAfter
	m, ops := newFakeMachine("hermes")
	m.files[hermesConfigPath] = []byte(before)

	code, out, errOut := runAgents(t, ops, nil, "install", "-config", testCfg, "-yes")
	if code != exitOK {
		t.Fatalf("install exited %d; an entry that is already there is not a failure\n%s%s", code, out, errOut)
	}
	if strings.Contains(out, "refused") || strings.Contains(out, "could not be set up") {
		t.Errorf("install still reports a refusal:\n%s", out)
	}
	if !strings.Contains(out, "already set up") {
		t.Errorf("install did not say the hook is already set up:\n%s", out)
	}
	if got := string(m.files[hermesConfigPath]); got != before {
		t.Errorf("install rewrote a hooks: block it did not write:\n%q", got)
	}
	_, status, _ := runAgents(t, ops, nil, "status", "-config", testCfg)
	if !strings.Contains(status, "installed (skill+hook)") {
		t.Errorf("status does not see the hook:\n%s", status)
	}
}

// A hooks: block with no entry of ours still refuses, in the same words.
func TestInstallStillRefusesAHooksBlockWithoutOurEntry(t *testing.T) {
	before := "hooks:\n  pre_tool_call:\n" + foreignEntry
	m, ops := newFakeMachine("hermes")
	m.files[hermesConfigPath] = []byte(before)
	code, out, _ := runAgents(t, ops, nil, "install", "-config", testCfg, "-yes")
	if code != exitTransport {
		t.Fatalf("exit %d, want the refusal exit\n%s", code, out)
	}
	for _, want := range []string{"already declares a top-level hooks: key", "add this pre_tool_call entry by hand", "pre_tool_call:"} {
		if !strings.Contains(out, want) {
			t.Errorf("the refusal lost %q:\n%s", want, out)
		}
	}
	if got := string(m.files[hermesConfigPath]); got != before {
		t.Errorf("the participant's config was modified:\n%q", got)
	}
}

// The net on its own, handed a run the scan would never produce: whatever
// the scan concluded, lines that are not the renderer's do not go.
func TestTheHermesNetRefusesARunThatIsNotRendered(t *testing.T) {
	_, cmd := ourHermesEntry(t)
	file := "hooks:\n  pre_tool_call:\n" + foreignEntry + entryLines(cmd)
	lines := hermesLines([]byte(file))
	const at = 4 // our command line
	for _, tc := range []struct {
		name       string
		start, end int
		want       bool
	}{
		{"exactly our two lines", 4, 6, true},
		{"our lines and the foreign entry's matcher above them", 3, 6, false},
		{"our lines and the whole foreign entry", 2, 6, false},
		{"the foreign entry instead of ours", 2, 4, false},
		{"one line of ours", 4, 5, false},
		{"more lines than the renderer writes", 0, 6, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := hermesRunIsRendered(lines, hermesOwnEntry{found: true, start: tc.start, end: tc.end}, at); got != tc.want {
				t.Fatalf("hermesRunIsRendered[%d,%d) = %v, want %v", tc.start, tc.end, got, tc.want)
			}
		})
	}
}
