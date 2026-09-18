package main

// #108: uninstall removes our entry in the form Hermes leaves when it
// re-dumps config.yaml, under the same net and not a weaker one.
//
// The four cases the plan names, on the dumper's real output: our entry alone
// under a hooks: block with no markers; beside a sibling entry, where only
// ours goes; with a further key inside it, where nothing goes and the plan
// says where it is; and one that decodes to another installation's hook,
// which is not ours to touch. The structural refusals are unchanged and live
// in hermes_own_entry_test.go — this file is about what the widening added,
// and about what it deliberately did not.

import (
	"strings"
	"testing"
)

// hermesWrittenFixture is the dumper's output for one of the fixed entries,
// with everything above the hooks: key as it stands, ready to have lines put
// after the entry.
func hermesWrittenFixture(t *testing.T, name string) (config, above string) {
	t.Helper()
	config = readHermesFixture(t, name+".resaved.yaml")
	return config, hermesBeforeTheHooksKey(t, config)
}

// The whole path, driven with both platforms' entries on every runner —
// findHermesOwnEntry, not just the net — because the two forms coincide on
// Windows in the one place the reader used to decide between them.
//
// On Windows the command carries the quotes and backslashes of its own argv
// quoting, so a plain scalar cannot hold it and both writers single-quote it:
// the command lines are byte-identical and only the matcher differs, ours
// quoted and Hermes' not. A reader that picked its branch from the command
// line sent every Windows file Hermes had saved down the byte-for-byte branch
// and refused it there. Every runner sees that now; before this, only the two
// Windows ones did.
func TestTheFormHermesWritesIsRemovableForBothPlatformsCommands(t *testing.T) {
	for name, tc := range hermesResavedEntries {
		t.Run(name, func(t *testing.T) {
			cmd, ok := hermesHookCommand(tc.entry, tc.windows)
			if !ok {
				t.Fatal("could not render the command")
			}
			// What Hermes leaves: its own scalar style, its unquoted matcher.
			file := "model: gpt\nhooks:\n  pre_tool_call:\n" +
				hermesCommandPrefix + hermesWrittenScalar(cmd) + "\n      matcher: terminal\n"
			if tc.windows && !strings.Contains(file, hermesHookLines(cmd)[2]) {
				t.Fatalf("on Windows the two forms' command lines must coincide, which is the case this pins:\n%s", file)
			}

			own := findHermesOwnEntry([]byte(file), refFor(tc.entry))
			if !own.removable() {
				t.Fatalf("not removable (%s):\n%s", own.why, file)
			}
			if got, want := string(removeHermesOwnEntry([]byte(file), own)), "model: gpt\n"; got != want {
				t.Errorf("uninstall left %q, want %q", got, want)
			}
		})
	}
}

func TestTheFormHermesWritesIsRemovedWholeAndLeavesTheRest(t *testing.T) {
	for name, tc := range hermesResavedEntries {
		for eol, conv := range map[string]func(string) string{
			"lf":   func(s string) string { return s },
			"crlf": hermesAsWindowsSavesIt,
		} {
			t.Run(name+"/"+eol, func(t *testing.T) {
				raw, above := hermesWrittenFixture(t, name)
				config := conv(raw)
				m, ops := newFakeMachine("hermes")
				m.files[hermesConfigPath] = []byte(config)

				uninstall := hermesPlanUninstall(ops, tc.entry)
				if len(uninstall.writes) != 1 {
					t.Fatalf("uninstall planned %d writes; notes: %v", len(uninstall.writes), uninstall.notes)
				}
				if got, want := string(uninstall.writes[0].contents), conv(above); got != want {
					t.Errorf("uninstall left\n%q\nwant\n%q", got, want)
				}
			})
		}
	}
}

// A sibling entry the participant wrote: ours goes, theirs is copied through
// byte for byte, and the hooks: and pre_tool_call: lines stay because the list
// is not empty afterwards.
func TestOnlyOurEntryGoesFromTheFormHermesWrites(t *testing.T) {
	entry := hermesResavedEntries["posix"].entry
	raw, _ := hermesWrittenFixture(t, "posix")
	sibling := "    - command: /usr/bin/theirs\n      matcher: terminal\n"

	for name, config := range map[string]string{
		"theirs after ours":  raw + sibling,
		"theirs before ours": strings.Replace(raw, "    - command:", sibling+"    - command:", 1),
	} {
		t.Run(name, func(t *testing.T) {
			m, ops := newFakeMachine("hermes")
			m.files[hermesConfigPath] = []byte(config)

			uninstall := hermesPlanUninstall(ops, entry)
			if len(uninstall.writes) != 1 {
				t.Fatalf("uninstall planned %d writes; notes: %v", len(uninstall.writes), uninstall.notes)
			}
			got := string(uninstall.writes[0].contents)
			if want := hermesBeforeTheHooksKey(t, raw) + "hooks:\n  pre_tool_call:\n" + sibling; got != want {
				t.Errorf("uninstall left\n%q\nwant\n%q", got, want)
			}
			if strings.Contains(got, "dropin-miner hook") {
				t.Errorf("our own entry survived:\n%s", got)
			}
		})
	}
}

// A further key inside our entry is the participant's, and the whole net
// stands: nothing goes, and the plan names where the entry is so they can
// decide for themselves.
func TestAFurtherKeyInTheFormHermesWritesKeepsTheEntry(t *testing.T) {
	entry := hermesResavedEntries["posix"].entry
	raw, _ := hermesWrittenFixture(t, "posix")
	config := strings.Replace(raw, "      matcher: terminal\n", "      matcher: terminal\n      timeout: 5\n", 1)
	if config == raw {
		t.Fatal("the edit did not apply")
	}

	m, ops := newFakeMachine("hermes")
	m.files[hermesConfigPath] = []byte(config)
	uninstall := hermesPlanUninstall(ops, entry)
	if len(uninstall.writes) != 0 {
		t.Fatalf("uninstall removed an entry carrying a key it did not write:\n%s", uninstall.writes[0].contents)
	}
	notes := strings.Join(uninstall.notes, "\n")
	for _, want := range []string{"this installation's pre_tool_call hook", "has a further key inside it", "remove that entry by hand"} {
		if !strings.Contains(notes, want) {
			t.Errorf("uninstall did not say %q:\n%s", want, notes)
		}
	}
	if got := strings.Join(uninstall.skipped, "\n"); strings.Contains(got, "not installed") {
		t.Errorf("uninstall says both that our hook is there and that Hermes is not installed:\n%s", got)
	}
}

// Another installation's hook in the form Hermes writes. H5's rule decides it,
// on the command the folding was undone from, exactly as it decides the
// rendered form: not ours, not touched, and not counted by status either.
func TestAnotherInstallationsEntryInTheFormHermesWritesIsUntouched(t *testing.T) {
	ours := hermesResavedEntries["posix"].entry
	raw, _ := hermesWrittenFixture(t, "posix")

	for name, other := range map[string]binEntry{
		"sharing the binary":   {command: ours.command, cfg: "/tmp/disposable/tokendrop.toml"},
		"another binary":       {command: "/somewhere/else/bin/dropin-miner", cfg: ours.cfg},
		"running on no config": {command: ours.command},
	} {
		t.Run(name, func(t *testing.T) {
			m, ops := newFakeMachine("hermes")
			m.files[hermesConfigPath] = []byte(raw)

			if hermesHookInstalledFor(ops, hermesConfigPath, other, false) {
				t.Error("status counted another installation's hook as this one's")
			}
			uninstall := hermesPlanUninstall(ops, other)
			if len(uninstall.writes) != 0 {
				t.Fatalf("uninstall removed another installation's hook:\n%s", uninstall.writes[0].contents)
			}
			if got := strings.Join(uninstall.notes, "\n"); strings.Contains(got, "this installation's pre_tool_call hook is in") {
				t.Errorf("uninstall called another installation's hook this one's:\n%s", got)
			}
		})
	}
}

// The net on its own, on the widened path. hermesRunIsOurs is asked whatever
// the scan concluded, so it is tested the way the scan cannot reach it: handed
// a run directly, including one the scan would never hand it.
//
// The ownership check here is redundant while the scan is right — the scan
// only collects entries whose command is ours — and that is what a net is for.
// It is also why a mutation switching it off left the whole package green
// until this test existed: every path to it went through a scan that had
// already asked. This asks the net itself.
// Both platforms' entries on every runner, through hermesResavedEntries, as
// the fixture tests do: the command this client writes on Windows begins with
// the double quote of its own argv quoting, so the scalar carrying it is
// single-quoted there and plain on POSIX. Taking the entry from the runner
// instead means a POSIX machine never sees the Windows shape, which is how
// the first version of this test passed here and failed on both Windows
// runners.
func TestTheHermesNetOnTheWidenedPathRefusesAnotherInstallationsCommand(t *testing.T) {
	for name, tc := range hermesResavedEntries {
		cmd, ok := hermesHookCommand(tc.entry, tc.windows)
		if !ok {
			t.Fatalf("%s: could not render the command", name)
		}
		other, ok := hermesHookCommand(binEntry{command: tc.entry.command, cfg: "/tmp/disposable/tokendrop.toml"}, tc.windows)
		if !ok || hermesCommandIsOurHook(other, refFor(tc.entry)) {
			t.Fatalf("%s: the other installation's command is still ours under H5: %q", name, other)
		}

		// One entry, in the form Hermes leaves: the scalar in the style its
		// dumper would choose, folded onto a second line.
		fileFor := func(c string) string {
			q := hermesWrittenScalar(c)
			at := strings.LastIndex(q, " ")
			return "hooks:\n  pre_tool_call:\n" + hermesCommandPrefix + q[:at] + "\n        " + q[at+1:] + "\n      matcher: terminal\n"
		}
		for sub, tt := range map[string]struct {
			cmd  string
			want bool
		}{
			"ours":                      {cmd, true},
			"another installation's":    {other, false},
			"ours with a word appended": {cmd + " --extra", false},
		} {
			t.Run(name+"/"+sub, func(t *testing.T) {
				file := fileFor(tt.cmd)
				lines := hermesLines([]byte(file))
				const at = 2 // the command line
				e := hermesOwnEntry{found: true, start: 0, end: len(lines)}
				if got := hermesRunIsOurs(lines, e, at, refFor(tc.entry)); got != tt.want {
					t.Fatalf("hermesRunIsOurs = %v, want %v, for:\n%s", got, tt.want, file)
				}
			})
		}
	}
}

// The widening is to the form Hermes writes, and to nothing else. Each of
// these decodes to this installation's command and is written by neither the
// renderer nor Hermes' dumper, so each is still left and reported — the
// deletion rule may not grow past the evidence for it.
func TestTheWideningStopsAtTheFormHermesWrites(t *testing.T) {
	entry := hermesResavedEntries["posix"].entry
	cmd, _ := hermesHookCommand(entry, false)
	head := "model: gpt\nhooks:\n  pre_tool_call:\n"

	for name, config := range map[string]string{
		// A fold whose continuation sits at the matcher's own depth: at that
		// depth it is a key of the entry, not part of the scalar, and the two
		// readings differ about where the entry ends.
		"a continuation at the matcher's depth": head +
			"    - command: " + cmd[:strings.LastIndex(cmd, " ")] + "\n" +
			"      " + cmd[strings.LastIndex(cmd, " ")+1:] + "\n      matcher: terminal\n",
		// The matcher one level deeper than the renderer puts it.
		"a matcher at the wrong depth": head +
			"    - command: " + cmd + "\n        matcher: terminal\n",
		// A matcher that is not the tool name we ask for.
		"another matcher": head + "    - command: " + cmd + "\n      matcher: browser\n",
	} {
		t.Run(name, func(t *testing.T) {
			m, ops := newFakeMachine("hermes")
			m.files[hermesConfigPath] = []byte(config)
			if uninstall := hermesPlanUninstall(ops, entry); len(uninstall.writes) != 0 {
				t.Fatalf("uninstall edited a form neither writer produces:\n%s", uninstall.writes[0].contents)
			}
		})
	}
}
