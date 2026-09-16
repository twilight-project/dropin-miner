package main

// H4's subject: which machines count as having a host on them, and what the
// participant is told made them count.
//
// #61 is the whole argument for this file existing. v0.2.9 detected Cursor by
// `lookPath("cursor")` alone, which is true in exactly one of the three ways
// Cursor is installed: the editor's shell shim, added only by running
// "Install 'cursor' command in PATH" from the palette. The soak machine had
// /Applications/Cursor.app, a populated ~/.cursor and the Agent CLI on PATH
// as cursor-agent, and setup reported "Found on this machine: Claude Code,
// Codex". The test that would have caught it is the first one below.

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// detectOpsWithHome is realAgentOps over a temp home, with PATH under the
// test's control. It reads the real filesystem on purpose: a config
// directory is a directory, and the in-memory fake machine synthesizes one
// only where it holds a file, which is not what detection asks about.
func detectOpsWithHome(t *testing.T, onPath ...string) (agentOps, agentPaths) {
	t.Helper()
	ops := realAgentOps()
	ops.home = t.TempDir()
	present := map[string]bool{}
	for _, n := range onPath {
		present[n] = true
	}
	ops.lookPath = func(name string) (string, error) {
		if present[name] {
			return filepath.Join(ops.home, "fake-path", name), nil
		}
		return "", os.ErrNotExist
	}
	return ops, ops.paths(noEnv)
}

// TestEveryHostIsDetectedByItsOwnCommand pins the signal each host answers
// with, so a renamed or dropped command name is a reviewed diff rather than a
// host that quietly stops being offered.
func TestEveryHostIsDetectedByItsOwnCommand(t *testing.T) {
	for _, tc := range []struct{ id, command, want string }{
		{"claude", "claude", "claude on PATH"},
		{"codex", "codex", "codex on PATH"},
		{"cursor", "cursor", "cursor on PATH"},
		{"opencode", "opencode", "opencode on PATH"},
		{"pi", "pi", "pi on PATH"},
		{"hermes", "hermes", "hermes on PATH"},
	} {
		t.Run(tc.id, func(t *testing.T) {
			target, ok := hostByID(t, tc.id)
			if !ok {
				t.Fatalf("no host registered as %q", tc.id)
			}
			ops, paths := detectOpsWithHome(t, tc.command)
			if got := target.Detect(ops, paths, noEnv); got != tc.want {
				t.Errorf("Detect with %s on PATH = %q, want %q", tc.command, got, tc.want)
			}
			bare, barePaths := detectOpsWithHome(t)
			if got := target.Detect(bare, barePaths, noEnv); got != "" {
				t.Errorf("Detect on an empty machine = %q, want no signal", got)
			}
		})
	}
}

// TestCursorIsDetectedInEveryConfigurationItShipsIn is #61's unit case. Each
// row is a machine a participant actually has.
func TestCursorIsDetectedInEveryConfigurationItShipsIn(t *testing.T) {
	for _, tc := range []struct {
		name      string
		onPath    []string
		configDir bool
		want      string
	}{
		{"editor with the palette shim", []string{"cursor"}, false, "cursor on PATH"},
		{"agent CLI only", []string{"cursor-agent"}, false, "cursor-agent on PATH"},
		{"editor without the shim, config directory only", nil, true, "~/.cursor"},
		{"both commands: the editor's is reported", []string{"cursor", "cursor-agent"}, true, "cursor on PATH"},
		{"a config directory as well as the CLI", []string{"cursor-agent"}, true, "cursor-agent on PATH"},
		{"neither", nil, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ops, paths := detectOpsWithHome(t, tc.onPath...)
			if tc.configDir {
				if err := os.MkdirAll(cursorConfigDir(paths), 0o700); err != nil {
					t.Fatalf("creating the config directory: %v", err)
				}
			}
			got := cursorTarget{}.Detect(ops, paths, noEnv)
			// ~/.cursor is printed tilde-shortened, and the home here is a
			// temp directory, so compare against what tilde would produce.
			want := tc.want
			if want == "~/.cursor" {
				want = tilde(ops.home, cursorConfigDir(paths))
			}
			if got != want {
				t.Errorf("Detect = %q, want %q", got, want)
			}
		})
	}
}

// TestCursorsConfigDirectoryIsTheOneItInstallsInto keeps detection and
// installation pointed at the same directory. Were they derived separately,
// a machine could be detected by a directory nothing is ever written to.
func TestCursorsConfigDirectoryIsTheOneItInstallsInto(t *testing.T) {
	ops, paths := detectOpsWithHome(t)
	dir := cursorConfigDir(paths)
	for _, p := range []string{paths.cursorHooks, paths.cursorSkill} {
		if !strings.HasPrefix(p, dir+string(filepath.Separator)) {
			t.Errorf("%s is not under the directory detection reports (%s)", p, dir)
		}
	}
	if want := filepath.Join(ops.home, ".cursor"); dir != want {
		t.Errorf("Cursor's config directory = %s, want %s", dir, want)
	}
}

// TestDetectionSignalsReachTheParticipant proves the signal is not merely
// computed: selectSurfaces hands it back keyed by host id, which is what
// setup's "Found on this machine" line and `agents status` print.
func TestDetectionSignalsReachTheParticipant(t *testing.T) {
	ops, paths := detectOpsWithHome(t, "cursor-agent", "codex")
	selected, detected, signals, err := selectSurfaces(ops, paths, noEnv, nil)
	if err != nil {
		t.Fatalf("selectSurfaces: %v", err)
	}
	if len(selected) != len(detected) {
		t.Fatalf("with no -client, selection and detection must be the same set: %v vs %v", labels(selected), labels(detected))
	}
	want := map[string]string{"codex": "codex on PATH", "cursor": "cursor-agent on PATH"}
	if !reflect.DeepEqual(signals, want) {
		t.Fatalf("signals = %v, want %v", signals, want)
	}
	got := labelsWithSignals(detected, signals)
	if !reflect.DeepEqual(got, []string{"Codex (codex on PATH)", "Cursor (cursor-agent on PATH)"}) {
		t.Fatalf("the detected-agents line reads %v", got)
	}
}

// TestCursorSurvivesAnUninstallSetupRoundTrip is soak row S18, end to end.
//
// The two commands disagreed about which agents exist: uninstall removes by
// what is installed, setup restores by what is detected. So on a machine
// where Cursor is not on PATH as `cursor`, an integration installed with
// `-with cursor` was removed by uninstall and never came back — setup printed
// "Found on this machine: Claude Code, Codex" and said nothing about Cursor
// at all. Anything installed with -with was lost by a round trip.
//
// Both rows here are machines the soak actually found: one with the Agent CLI
// only, one with neither command and just the config directory. The assertion
// is byte-identity, not existence: "restored" means the skill and the hook
// entries come back as they were, not that something with the right name is
// there.
func TestCursorSurvivesAnUninstallSetupRoundTrip(t *testing.T) {
	for _, tc := range []struct{ name, onPath string }{
		{"the Agent CLI only", "cursor-agent"},
		{"the config directory only", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newSetupSandbox(t)
			s.platform.claim("credits")
			if tc.onPath != "" {
				s.onPath[tc.onPath] = true
			}
			paths := s.paths()
			cursorDir := cursorConfigDir(paths)
			if tc.onPath == "" {
				// The editor's own directory, as a participant who never ran
				// the palette command has it.
				if err := os.MkdirAll(filepath.Join(cursorDir, "extensions"), 0o700); err != nil {
					t.Fatalf("creating Cursor's config directory: %v", err)
				}
			}

			// -with cursor is the workaround #61 documents: it installs
			// whether or not detection would have found the host.
			if code, out, errOut := s.run(nil, false, "-yes", "-with", "cursor"); code != exitOK {
				t.Fatalf("setup -with cursor exited %d\n%s\n%s", code, out, errOut)
			}
			if !lexists(paths.cursorSkill) {
				t.Fatal("setup -with cursor wrote no skill, so this test would prove nothing")
			}
			installed := snapshotTree(t, cursorDir)

			if code, out, errOut := s.uninstall(t, nil, false, nil, "-yes"); code != exitOK {
				t.Fatalf("uninstall exited %d\n%s\n%s", code, out, errOut)
			}
			if lexists(paths.cursorSkill) {
				t.Fatal("uninstall left Cursor's skill behind, so the restore below proves nothing")
			}

			// Plain setup, detection only. This is the run that did nothing
			// in v0.2.9.
			code, out, errOut := s.run(nil, false, "-yes")
			if code != exitOK {
				t.Fatalf("setup exited %d\n%s\n%s", code, out, errOut)
			}
			wantSignal := "cursor-agent on PATH"
			if tc.onPath == "" {
				wantSignal = tilde(s.userHome, cursorDir)
			}
			if !strings.Contains(out, "Found on this machine: Cursor ("+wantSignal+")") {
				t.Errorf("setup did not report Cursor and what found it (want the signal %q):\n%s", wantSignal, out)
			}
			if got := snapshotTree(t, cursorDir); !reflect.DeepEqual(installed, got) {
				t.Errorf("Cursor was not restored byte for byte.\n installed %v\n restored  %v", installed, got)
			}
		})
	}
}

func hostByID(t *testing.T, id string) (installTarget, bool) {
	t.Helper()
	for _, h := range targetsByKind(targetHost) {
		if h.ID() == id {
			return h, true
		}
	}
	return nil, false
}
