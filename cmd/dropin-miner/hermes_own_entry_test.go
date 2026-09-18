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
	"fmt"
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
		name: "a further key inside the item: an entry somebody edited is theirs to remove",
		config: func(c string) string {
			return "hooks:\n  pre_tool_call:\n" + entryLines(c) + "      timeout: 5\n" + hermesAfter
		},
		note: "has a further key inside it",
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
		note: "does not have the matcher line",
	},
	{
		name: "the command line with nothing under it, at the end of the file",
		config: func(c string) string {
			return "hooks:\n  pre_tool_call:\n" + hermesHookLines(c)[2] + "\n"
		},
		note: "does not have the matcher line",
	},
	{
		// Still left after #108 widened removal to the form Hermes writes:
		// that form is a plain or single-quoted scalar, which is what its
		// dumper produces. A double-quoted one decodes to the same command and
		// is written by neither, so deleting on its say-so would go past the
		// evidence. Left, and the plan says so.
		name: "a double-quoted scalar: written by neither the renderer nor Hermes",
		config: func(c string) string {
			return "hooks:\n  pre_tool_call:\n    - command: " + strconv.Quote(c) + "\n      matcher: \"terminal\"\n" + hermesAfter
		},
		note: "nor as Hermes rewrites it when it saves config.yaml",
	},
	{
		name: "a trailing comment on the command line",
		config: func(c string) string {
			return "hooks:\n  pre_tool_call:\n" + hermesHookLines(c)[2] + " # mine\n      matcher: \"terminal\"\n" + hermesAfter
		},
		note: "names this installation's pre_tool_call hook command",
	},
	{
		name: "under another event",
		config: func(c string) string {
			return "hooks:\n  post_tool_call:\n" + entryLines(c) + hermesAfter
		},
		note: "names this installation's pre_tool_call hook command",
	},
	{
		name: "nested one level deeper than the renderer puts it",
		config: func(c string) string {
			return "hooks:\n  group:\n    pre_tool_call:\n" + strings.ReplaceAll(entryLines(c), "    - ", "      - ") + hermesAfter
		},
		note: "names this installation's pre_tool_call hook command",
	},
	{
		name: "hooks spelled another way",
		config: func(c string) string {
			return "\"hooks\":\n  pre_tool_call:\n" + entryLines(c) + hermesAfter
		},
		note: "names this installation's pre_tool_call hook command",
	},
	{
		name: "two hooks: keys",
		config: func(c string) string {
			return "hooks:\n  pre_tool_call:\n" + entryLines(c) + "hooks:\n" + foreignPostTool
		},
		note: "names this installation's pre_tool_call hook command",
	},
	{
		name: "a second YAML document",
		config: func(c string) string {
			return "hooks:\n  pre_tool_call:\n" + entryLines(c) + "---\nother: 1\n"
		},
		note: "names this installation's pre_tool_call hook command",
	},
	{
		name: "tabs in the indentation",
		config: func(c string) string {
			return "hooks:\n\tpre_tool_call:\n" + entryLines(c) + hermesAfter
		},
		note: "names this installation's pre_tool_call hook command",
	},
}

// L3's review, F2 to F5: shapes a YAML parser and a line scan read
// differently. Each was found by running 2,875 generated files through the
// real uninstall and asking PyYAML — the parser Hermes uses — what each file
// meant before and after (hermes_differential_test.go carries that on).
func init() {
	list := func(above string) func(string) string {
		return func(c string) string { return "hooks:\n  pre_tool_call:\n" + above + entryLines(c) + hermesAfter }
	}
	const mention = "names this installation's pre_tool_call hook command"
	// F2 and F3 answer at the mention tier (L3d): when either rule trips,
	// this scan does not know what a parser makes of the lines it matched,
	// so it may not call them found.
	const cannotRead, several = mention, mention
	hermesLeftAlone = append(hermesLeftAlone,
		// F2. Content at list depth that is not a list entry. Counted as a
		// sibling, it made "only our two lines go" the plan; PyYAML then read
		// a file that no longer parsed, a null, and an emptied block scalar.
		hermesCase{name: "F2: a tag alone on the line under pre_tool_call:", config: list("    !!seq\n"), note: cannotRead},
		hermesCase{name: "F2: an anchor alone on the line", config: list("    &a\n"), note: cannotRead},
		hermesCase{name: "F2: a literal block scalar indicator", config: list("    |\n"), note: cannotRead},
		hermesCase{name: "F2: a folded block scalar indicator", config: list("    >-\n"), note: cannotRead},
		hermesCase{name: "F2: a line at depth three inside the list", config: func(c string) string {
			return "hooks:\n  pre_tool_call:\n" + entryLines(c) + "   odd: 1\n"
		}, note: cannotRead},
		// F3. YAML keeps the last of two identical keys. Taking ours out of
		// the second un-shadows whatever sits under the first.
		hermesCase{name: "F3: ours under the second of two pre_tool_call: keys", config: func(c string) string {
			return "hooks:\n  pre_tool_call:\n" + foreignEntry + "  pre_tool_call:\n" + entryLines(c)
		}, note: several},
		hermesCase{name: "F3: ours under the first of two", config: func(c string) string {
			return "hooks:\n  pre_tool_call:\n" + entryLines(c) + "  pre_tool_call:\n" + foreignEntry
		}, note: several},
		hermesCase{name: "F3: the second one spelled in quotes", config: func(c string) string {
			return "hooks:\n  \"pre_tool_call\":\n" + foreignEntry + "  pre_tool_call:\n" + entryLines(c)
		}, note: several},
		// F4. A YAML parser reads each of these as a live hook of ours. The
		// structured find will not read them; the plan used to say "Hermes:
		// not installed". A sentence now, never an edit.
		hermesCase{name: "F4: hooks: with a trailing space", config: func(c string) string { return "hooks: \n  pre_tool_call:\n" + entryLines(c) }, note: mention},
		hermesCase{name: "F4: hooks: with a trailing comment", config: func(c string) string { return "hooks: # mine\n  pre_tool_call:\n" + entryLines(c) }, note: mention},
		hermesCase{name: "F4: hooks: with an anchor", config: func(c string) string { return "hooks: &h\n  pre_tool_call:\n" + entryLines(c) }, note: mention},
		hermesCase{name: "F4: pre_tool_call: with a trailing space", config: func(c string) string { return "hooks:\n  pre_tool_call: \n" + entryLines(c) }, note: mention},
		hermesCase{name: "F4: pre_tool_call: with a trailing comment", config: func(c string) string { return "hooks:\n  pre_tool_call: # mine\n" + entryLines(c) }, note: mention},
		hermesCase{name: "F4: pre_tool_call in quotes", config: func(c string) string { return "hooks:\n  \"pre_tool_call\":\n" + entryLines(c) }, note: mention},
		hermesCase{name: "F4: the explicit key form", config: func(c string) string { return "hooks:\n  ? pre_tool_call\n  :\n" + entryLines(c) }, note: mention},
		hermesCase{name: "F4: a closing document marker", config: func(c string) string { return "hooks:\n  pre_tool_call:\n" + entryLines(c) + "...\n" }, note: mention},
		hermesCase{name: "F4: HOOKS: beside hooks:", config: func(c string) string { return "HOOKS:\n  x: 1\nhooks:\n  pre_tool_call:\n" + entryLines(c) }, note: mention},
		hermesCase{name: "F4: the command in a flow mapping", config: func(c string) string {
			return "hooks:\n  pre_tool_call:\n    - {command: " + hermesYAMLSingleQuoted(c) + ", matcher: \"terminal\"}\n"
		}, note: mention},
		// F5a. Pins the scan's parent check. A two-line run carries no
		// heading of its own, so with that check off only the net's walk up
		// to `  pre_tool_call:` stands between this file and an edit — and
		// then the sentence is the found-but-left one, not this one.
		hermesCase{name: "F5a: ours under post_tool_call:, sharing the list with a foreign entry", config: func(c string) string {
			return "hooks:\n  post_tool_call:\n" + foreignEntry + entryLines(c) + hermesAfter
		}, note: mention},
		// F5c. Pins the tab refusal: the tab is in a line that is none of
		// ours, so no exact-text check on our own lines can catch it.
		hermesCase{name: "F5c: a tab in the indentation of a sibling entry", config: func(c string) string {
			return "hooks:\n  pre_tool_call:\n" + entryLines(c) + "\t- command: 'theirs'\n"
		}, note: mention},
	)
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
			// F6: no skill is present on this machine, so the plan used to
			// add "Hermes: not installed" under the sentence saying our hook
			// is there. One of the two is false.
			if tc.note != "" && strings.Contains(out, "Hermes: not installed") {
				t.Errorf("the plan says our hook is there and that Hermes is not installed:\n%s", out)
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

// An entry of ours that somebody edited still fires, so install counts it as
// set up rather than refusing and printing a second copy to paste beside it.
func TestInstallCountsAnEditedEntryOfOursAsSetUp(t *testing.T) {
	_, cmd := ourHermesEntry(t)
	before := "hooks:\n  pre_tool_call:\n" + entryLines(cmd) + "      timeout: 5\n"
	m, ops := newFakeMachine("hermes")
	m.files[hermesConfigPath] = []byte(before)
	code, out, errOut := runAgents(t, ops, nil, "install", "-config", testCfg, "-yes")
	if code != exitOK || !strings.Contains(out, "already set up") {
		t.Fatalf("install: exit %d, want already set up\n%s%s", code, out, errOut)
	}
	if got := string(m.files[hermesConfigPath]); got != before {
		t.Errorf("install changed the file:\n%q", got)
	}
}

// F5b. The round trip in hermesUnquoteSingle, which no fixture pinned
// because no fixture command held a quote. A lone quote inside a
// single-quoted scalar decodes, leniently, to the very same command — so
// without the round trip this file would be edited on the strength of a
// scalar no YAML parser reads that way.
func TestALoneQuoteInsideTheScalarIsNotOurLine(t *testing.T) {
	for _, tc := range []struct {
		line string
		want bool
	}{
		{`    - command: '/b hook hermes pre_tool_call'`, true},
		{`    - command: '/O''Neil/b hook hermes pre_tool_call'`, true},
		{`    - command: '/O'Neil/b hook hermes pre_tool_call'`, false},
		{`    - command: '/b hook hermes pre_tool_call''`, false},
		{`    - command: '/b hook hermes pre_tool_call`, false},
		{`    - command: /b hook hermes pre_tool_call`, false},
	} {
		if _, ok := hermesDecodeCommandLine(tc.line); ok != tc.want {
			t.Errorf("hermesDecodeCommandLine(%q) ok = %v, want %v", tc.line, ok, tc.want)
		}
	}
}

// The same through the real uninstall, with a binary whose path needs
// quoting, so the rendered scalar is full of doubled quotes to get wrong.
func TestABinaryPathThatNeedsQuotesIsRemovedCleanlyAndALoneQuoteIsNot(t *testing.T) {
	const bin = "/Users/O'Neil/My Tools/dropin-miner"
	m, ops := newFakeMachine("hermes")
	ops.executable = func() (string, error) { return bin, nil }
	if code, out, errOut := runAgents(t, ops, nil, "install", "-config", testCfg, "-yes"); code != exitOK {
		t.Fatalf("install: %d\n%s%s", code, out, errOut)
	}
	var line string
	for _, l := range strings.Split(string(m.files[hermesConfigPath]), "\n") {
		if _, ok := hermesDecodeCommandLine(l); ok {
			line = l
		}
	}
	if !strings.Contains(line, "''") {
		t.Fatalf("this path was meant to put a doubled quote in the scalar:\n%s", line)
	}
	matcher := hermesHookLines("")[3]

	good := "hooks:\n  pre_tool_call:\n" + foreignEntry + line + "\n" + matcher + "\n"
	m.files[hermesConfigPath] = []byte(good)
	if code, out, errOut := runAgents(t, ops, nil, "uninstall", "-config", testCfg, "-yes"); code != exitOK {
		t.Fatalf("uninstall: %d\n%s%s", code, out, errOut)
	}
	if got, want := string(m.files[hermesConfigPath]), "hooks:\n  pre_tool_call:\n"+foreignEntry; got != want {
		t.Errorf("a path that needs quotes was not removed cleanly:\n%q", got)
	}

	// One doubled quote collapsed to a lone one, after the opening quote.
	i := strings.Index(line[len(hermesCommandPrefix)+1:], "''") + len(hermesCommandPrefix) + 1
	broken := "hooks:\n  pre_tool_call:\n" + foreignEntry + line[:i] + line[i+1:] + "\n" + matcher + "\n"
	m.files[hermesConfigPath] = []byte(broken)
	if code, out, errOut := runAgents(t, ops, nil, "uninstall", "-config", testCfg, "-yes"); code != exitOK {
		t.Fatalf("uninstall: %d\n%s%s", code, out, errOut)
	}
	if got := string(m.files[hermesConfigPath]); got != broken {
		t.Errorf("a scalar holding a lone quote was edited:\n%q", got)
	}
	// Unchanged is not enough to pin the round trip: with it switched off the
	// file still comes back byte-identical, because the net refuses a line
	// the renderer does not produce for the decoded command. What differs is
	// what the commands SAY. No YAML parser reads that scalar as our command,
	// so it is not a hook of ours: uninstall may mention it, and install must
	// not call it already set up.
	_, out, _ := runAgents(t, ops, nil, "uninstall", "-config", testCfg, "-dry-run")
	if !strings.Contains(out, "names this installation's pre_tool_call hook command") || strings.Contains(out, "was left there because") {
		t.Errorf("uninstall took a scalar with a lone quote in it for our entry:\n%s", out)
	}
	code, out, _ := runAgents(t, ops, nil, "install", "-config", testCfg, "-yes")
	// "already set up —" is the note's own phrasing; the refusal below also
	// says "check whether it is already set up", which is the opposite claim.
	if code != exitTransport || strings.Contains(out, "already set up —") || !strings.Contains(out, "already declares a top-level hooks: key") {
		t.Errorf("install: exit %d; a scalar no parser reads as our command is not a hook that is already set up\n%s", code, out)
	}
	if got := string(m.files[hermesConfigPath]); got != broken {
		t.Errorf("install edited the file:\n%q", got)
	}
}

// Install's refusal, when the file names our command where this client
// cannot vouch for it: still a refusal, but it says what it saw, so the
// paste advice does not put a second copy beside a live one unannounced.
func TestInstallsRefusalSaysWhenTheFileAlreadyNamesOurCommand(t *testing.T) {
	_, cmd := ourHermesEntry(t)
	before := "hooks: # mine\n  pre_tool_call:\n" + entryLines(cmd)
	m, ops := newFakeMachine("hermes")
	m.files[hermesConfigPath] = []byte(before)
	code, out, _ := runAgents(t, ops, nil, "install", "-config", testCfg, "-yes")
	if code != exitTransport {
		t.Fatalf("exit %d, want the refusal exit\n%s", code, out)
	}
	for _, want := range []string{"already declares a top-level hooks: key", "line 3 of it already names this installation's pre_tool_call hook command", "check whether it is already set up"} {
		if !strings.Contains(out, want) {
			t.Errorf("the refusal did not say %q:\n%s", want, out)
		}
	}
	if got := string(m.files[hermesConfigPath]); got != before {
		t.Errorf("the file was changed:\n%q", got)
	}
}

// A command quoted in somebody's notes is not a hook: the mention tier may
// produce a sentence, never "already set up".
func TestACommandQuotedInABlockScalarIsNotAHook(t *testing.T) {
	_, cmd := ourHermesEntry(t)
	before := "notes: |\n  to add the hook, paste:\n  " + strings.ReplaceAll(entryLines(cmd), "\n", "\n  ") + "\nmodel: x\n"
	m, ops := newFakeMachine("hermes")
	m.files[hermesConfigPath] = []byte(before)
	if own := findHermesOwnEntry([]byte(before), refFor(binEntry{})); own.found {
		t.Fatal("found with no installation to find it for")
	}
	code, out, errOut := runAgents(t, ops, nil, "install", "-config", testCfg, "-yes")
	if code != exitOK || strings.Contains(out, "already set up") {
		t.Fatalf("install took a quoted command for a hook: exit %d\n%s%s", code, out, errOut)
	}
	if !strings.Contains(string(m.files[hermesConfigPath]), agentsMarkerBegin) {
		t.Errorf("install did not write its block:\n%s", m.files[hermesConfigPath])
	}
}

// The net on its own, handed a run the scan would never produce: whatever
// the scan concluded, lines that are not the renderer's do not go.
func TestTheHermesNetRefusesARunThatIsNotRendered(t *testing.T) {
	entry, cmd := ourHermesEntry(t)
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
			if got := hermesRunIsOurs(lines, hermesOwnEntry{found: true, start: tc.start, end: tc.end}, at, refFor(entry)); got != tc.want {
				t.Fatalf("hermesRunIsOurs[%d,%d) = %v, want %v", tc.start, tc.end, got, tc.want)
			}
		})
	}
}

// The other half of the net. Every line in these runs IS a line the renderer
// writes, and removing them would still break the file, because something
// that belongs to what went is left behind. A mutation that switched off the
// scan's own check for this left the net silent and an orphaned `timeout:`
// in the participant's config, which is how the half came to exist.
func TestTheHermesNetRefusesARunThatLeavesSomethingOfItsOwnBehind(t *testing.T) {
	entry, cmd := ourHermesEntry(t)
	body := strings.Join(hermesHookLines(cmd), "\n") + "\n"
	for _, tc := range []struct {
		name       string
		file       string
		start, end int
		want       bool
	}{
		{"our entry, then a column-zero key", body + hermesAfter, 0, 4, true},
		{"our entry at the end of the file", body, 0, 4, true},
		{"our two lines, then a sibling entry", body + foreignEntry, 2, 4, true},
		{"our two lines, a comment, then a sibling entry", body + "    # theirs\n" + foreignEntry, 2, 4, true},
		{"our two lines, with a further key of the same item below them", body + "      timeout: 5\n", 2, 4, false},
		{"the same, behind a blank line and a comment", body + "\n      # mine\n      timeout: 5\n", 2, 4, false},
		{"pre_tool_call: and our lines, with a sibling entry left under nothing", body + foreignEntry, 1, 4, false},
		{"all four lines, with another event left under nothing", body + foreignPostTool, 0, 4, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lines := hermesLines([]byte(tc.file))
			if got := hermesRunIsOurs(lines, hermesOwnEntry{found: true, start: tc.start, end: tc.end}, 2, refFor(entry)); got != tc.want {
				t.Fatalf("hermesRunIsOurs[%d,%d) = %v, want %v, in:\n%s", tc.start, tc.end, got, tc.want, tc.file)
			}
		})
	}
}

// The third part of the net (F5a). A two-line run carries no heading of its
// own, so every line in it can be the renderer's while it sits under somebody
// else's event. The run must begin under what the renderer puts above it.
func TestTheHermesNetRefusesARunThatIsNotUnderTheRenderersHeadings(t *testing.T) {
	entry, cmd := ourHermesEntry(t)
	ours := entryLines(cmd)
	for _, tc := range []struct {
		name       string
		file       string
		start, end int
		at         int
		want       bool
	}{
		{"under hooks: and pre_tool_call:", "hooks:\n  pre_tool_call:\n" + foreignEntry + ours, 4, 6, 4, true},
		{"under post_tool_call:", "hooks:\n  post_tool_call:\n" + foreignEntry + ours, 4, 6, 4, false},
		{"under pre_tool_call: under another top-level key", "plugins:\n  pre_tool_call:\n" + foreignEntry + ours, 4, 6, 4, false},
		{"under a heading at depth three", "hooks:\n   pre_tool_call:\n" + foreignEntry + ours, 4, 6, 4, false},
		{"with nothing above it at all", ours, 0, 2, 0, false},
		{"pre_tool_call: and ours, under another top-level key", "plugins:\n  pre_tool_call:\n" + ours, 1, 4, 2, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lines := hermesLines([]byte(tc.file))
			if got := hermesRunIsOurs(lines, hermesOwnEntry{found: true, start: tc.start, end: tc.end}, tc.at, refFor(entry)); got != tc.want {
				t.Fatalf("hermesRunIsOurs[%d,%d) = %v, want %v, in:\n%s", tc.start, tc.end, got, tc.want, tc.file)
			}
		})
	}
}

// L3d, D1. Install's refusal must carry the warning clause wherever the file
// names our command and the structured find will not vouch for it. Forcing
// that clause off left every test green, and under that mutant each of these
// got the plain paste advice beside a hook PyYAML reads as live: the advice
// to add a second copy, with nothing saying the first was there.
func TestInstallWarnsBeforeAdvisingASecondCopyBesideALiveHook(t *testing.T) {
	_, cmd := ourHermesEntry(t)
	ours := entryLines(cmd)
	quoted := hermesYAMLSingleQuoted(cmd)
	for _, tc := range []struct {
		name   string
		config string
		line   int // where the command is named
	}{
		{"hooks: with a trailing space", "model: x\nhooks: \n  pre_tool_call:\n" + ours, 4},
		{"hooks: with a trailing comment", "hooks: # mine\n  pre_tool_call:\n" + ours, 3},
		{"hooks: with an anchor", "hooks: &h\n  pre_tool_call:\n" + ours, 3},
		{"pre_tool_call: with a trailing space", "hooks:\n  pre_tool_call: \n" + ours, 3},
		{"pre_tool_call: with a trailing comment", "hooks:\n  pre_tool_call: # mine\n" + ours, 3},
		{"pre_tool_call in quotes", "hooks:\n  \"pre_tool_call\":\n" + ours, 3},
		{"the explicit key form", "hooks:\n  ? pre_tool_call\n  :\n" + ours, 4},
		{"a closing document marker", "hooks:\n  pre_tool_call:\n" + ours + "...\n", 3},
		{"HOOKS: beside hooks:", "HOOKS:\n  x: 1\nhooks:\n  pre_tool_call:\n" + ours, 5},
		{"a trailing comment on a plain command", "hooks:\n  pre_tool_call:\n    - command: " + cmd + " # mine\n      matcher: terminal\n", 3},
		{"an anchored value", "hooks:\n  pre_tool_call:\n    - command: &c " + quoted + "\n      matcher: terminal\n", 3},
		{"a flow-style item", "hooks:\n  pre_tool_call:\n    - {command: " + quoted + ", matcher: terminal}\n", 3},
		{"command is not the first key", "hooks:\n  pre_tool_call:\n    - matcher: terminal\n      command: " + quoted + "\n", 4},
		{"PyYAML's default zero-indented list", "hooks:\n  pre_tool_call:\n  - command: " + quoted + "\n    matcher: terminal\n", 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, ops := newFakeMachine("hermes")
			m.files[hermesConfigPath] = []byte(tc.config)
			code, out, _ := runAgents(t, ops, nil, "install", "-config", testCfg, "-yes")
			if code != exitTransport {
				t.Fatalf("exit %d, want the refusal exit\n%s", code, out)
			}
			want := fmt.Sprintf("line %d of it already names this installation's pre_tool_call hook command", tc.line)
			if !strings.Contains(out, want) || !strings.Contains(out, "check whether it is already set up before adding this entry by hand") {
				t.Errorf("install advised pasting a second copy without saying %q:\n%s", want, out)
			}
			if got := string(m.files[hermesConfigPath]); got != tc.config {
				t.Errorf("the file was changed:\n%q", got)
			}
		})
	}
}

// L3d, D2. "Already set up" is a claim about what Hermes will run, and these
// are shapes where PyYAML reads no live hook of ours although every line the
// scan matched is there: our two lines as the TEXT of a block scalar, and
// ours under the first of two pre_tool_call: keys, which YAML discards. The
// hits were collected before the list-entry rule and the one-key rule ran,
// so found was true. When either rule trips the answer is now the mention
// tier: install refuses with the warning, status does not count it, and
// uninstall gives the mention note — wording that is also still true for
// the reverse duplicate, where ours is under the last key and IS live.
func TestAStructuralRuleThatTripsAnswersAtTheMentionTier(t *testing.T) {
	_, cmd := ourHermesEntry(t)
	ours := entryLines(cmd)
	plain := "    - command: " + cmd + "\n      matcher: terminal\n"
	head := "hooks:\n  pre_tool_call:\n"
	for _, tc := range []struct{ name, config string }{
		{"a literal block scalar holding our quoted lines", head + "    |\n" + ours},
		{"a folded block scalar holding our quoted lines", head + "    >-\n" + ours},
		{"a literal block scalar holding the plain form", head + "    |\n" + plain},
		{"a folded block scalar holding the plain form", head + "    >-\n" + plain},
		{"ours under the first of two pre_tool_call: keys", head + ours + "  pre_tool_call:\n" + foreignEntry},
		{"ours under the first, the second one null", head + ours + "  pre_tool_call: null\n"},
		{"the reverse: ours under the last key, and live", head + foreignEntry + "  pre_tool_call:\n" + ours},
		// Files PyYAML rejects outright, which the oracle found install calling
		// already set up. Hermes cannot load them, so nothing in them is live.
		{"unparseable: a line at depth one after ours", head + ours + " odd: 1\n"},
		{"unparseable: a line at depth five inside our entry", head + ours + "     odd: 1\n"},
		{"unparseable: a line at depth three in the block", "hooks:\n   odd: 1\n  pre_tool_call:\n" + ours},
		{"unparseable: the next key glued to our matcher line", head + foreignEntry + strings.TrimRight(ours, "\n") + "database:\n  x: 1\n"},
		{"unparseable: a deeper line in our entry with no key above it", head + hermesHookLines(cmd)[2] + "\n          stray\n" + hermesHookLines(cmd)[3] + "\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, ops := newFakeMachine("hermes")
			m.files[hermesConfigPath] = []byte(tc.config)
			ref := refFor(func() binEntry { e, _ := ourHermesEntry(t); return e }())

			if own := findHermesOwnEntry([]byte(tc.config), ref); own.found || own.mention == 0 {
				t.Fatalf("found=%v mention=%d; want not found, and mentioned", own.found, own.mention)
			}
			code, out, _ := runAgents(t, ops, nil, "install", "-config", testCfg, "-yes")
			if code != exitTransport || strings.Contains(out, "already set up —") {
				t.Errorf("install: exit %d; \"already set up\" is claimed where this scan cannot say what a parser reads:\n%s", code, out)
			}
			if !strings.Contains(out, "already names this installation's pre_tool_call hook command") {
				t.Errorf("install's refusal did not carry the warning:\n%s", out)
			}
			_, status, _ := runAgents(t, ops, nil, "status", "-config", testCfg)
			if strings.Contains(status, "skill+hook") {
				t.Errorf("status counts a hook this scan cannot vouch for:\n%s", status)
			}
			code, out, _ = runAgents(t, ops, nil, "uninstall", "-config", testCfg, "-yes")
			if code != exitOK || !strings.Contains(out, "names this installation's pre_tool_call hook command") || strings.Contains(out, "was left there because") {
				t.Errorf("uninstall: exit %d, want the mention note and not the found-and-left one:\n%s", code, out)
			}
			if got := string(m.files[hermesConfigPath]); got != tc.config {
				t.Errorf("the file was changed:\n%q", got)
			}
		})
	}
}
