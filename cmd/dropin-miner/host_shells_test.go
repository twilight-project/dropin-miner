package main

// The shell declaration (targets.go) as a reviewed table.
//
// Each host says, per OS, what runs the strings rendered for it. The table
// below is that declaration written out by hand: changing a cell in
// targets.go without changing it here is a failure, so a cell never moves
// from unknown to established — or from one shell to another — without a
// diff a reviewer reads next to its evidence.

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// hostShellOSes are the operating systems a host can declare. Anything else
// (a GOOS this binary cross-compiles for but no host has been seen on) is
// undeclared for every host.
var hostShellOSes = []string{"darwin", "linux", "windows"}

type declaredRow struct {
	host, goos string
	tool, hook shellEvidence
	toolShells []shellKind
	hookShells []shellKind
	// toolChoice is who picks among toolShells, and is part of the declared
	// fact wherever there is more than one: it decides what the skill says
	// above each block, and a wrong answer there sends the participant to the
	// form their shell cannot run (#96).
	toolChoice shellChoice
}

// The H1 evidence table. Unknown cells gate the commits that would render
// for them: an unknown tool cell blocks H2 for that host and OS only, an
// unknown hook cell blocks H3 for that host and OS only.
var declaredShellTable = []declaredRow{
	{"claude", "darwin", evidenceEstablished, evidenceEstablished, []shellKind{shellPOSIX}, []shellKind{shellPOSIX}, ""},
	{"claude", "linux", evidenceEstablished, evidenceEstablished, []shellKind{shellPOSIX}, []shellKind{shellPOSIX}, ""},
	{"claude", "windows", evidenceEstablished, evidenceEstablished, []shellKind{shellPOSIX, shellPowerShell}, []shellKind{shellPOSIX}, chosenPerCall},
	{"codex", "darwin", evidenceEstablished, evidenceNone, []shellKind{shellPOSIX}, nil, ""},
	{"codex", "linux", evidenceEstablished, evidenceNone, []shellKind{shellPOSIX}, nil, ""},
	{"codex", "windows", evidenceEstablished, evidenceNone, []shellKind{shellPowerShell}, nil, ""},
	{"cursor", "darwin", evidenceEstablished, evidenceEstablished, []shellKind{shellPOSIX}, []shellKind{shellPOSIX}, ""},
	{"cursor", "linux", evidenceEstablished, evidenceRuled, []shellKind{shellPOSIX}, []shellKind{shellPOSIX}, ""},
	{"cursor", "windows", evidenceEstablished, evidenceRuled, []shellKind{shellPowerShell, shellPOSIX}, []shellKind{shellCmd, shellPowerShell}, chosenByParticipant},
	{"opencode", "darwin", evidenceEstablished, evidenceNone, []shellKind{shellPOSIX}, nil, ""},
	{"opencode", "linux", evidenceEstablished, evidenceNone, []shellKind{shellPOSIX}, nil, ""},
	{"opencode", "windows", evidenceEstablished, evidenceNone, []shellKind{shellPowerShell}, nil, ""},
	{"pi", "darwin", evidenceEstablished, evidenceNone, []shellKind{shellPOSIX}, nil, ""},
	{"pi", "linux", evidenceEstablished, evidenceNone, []shellKind{shellPOSIX}, nil, ""},
	{"pi", "windows", evidenceEstablished, evidenceNone, []shellKind{shellPOSIX}, nil, ""},
	{"hermes", "darwin", evidenceEstablished, evidenceEstablished, []shellKind{shellPOSIX}, []shellKind{shellArgv}, ""},
	{"hermes", "linux", evidenceEstablished, evidenceEstablished, []shellKind{shellPOSIX}, []shellKind{shellArgv}, ""},
	{"hermes", "windows", evidenceEstablished, evidenceEstablished, []shellKind{shellPOSIX}, []shellKind{shellArgv}, ""},
}

func shellDeclarer(t *testing.T, id string) shellDeclaringTarget {
	t.Helper()
	tg, ok := targetByID(installTargets, id)
	if !ok {
		t.Fatalf("no target %q", id)
	}
	d, ok := tg.(shellDeclaringTarget)
	if !ok {
		t.Fatalf("%s declares no shells", id)
	}
	return d
}

// Every host declares both cells on every OS, and every cell is well formed:
// an established or ruled cell names its shells and its source; a cell with
// no channel or no evidence names no shells, so nothing can render from it.
func TestEveryHostDeclaresItsShells(t *testing.T) {
	for _, tg := range targetsByKind(targetHost) {
		d := shellDeclarer(t, tg.ID())
		for _, goos := range hostShellOSes {
			decl := d.Shells(goos)
			for name, cell := range map[string]shellCell{"tool": decl.tool, "hook": decl.hook} {
				switch cell.evidence {
				case evidenceEstablished, evidenceRuled:
					if len(cell.shells) == 0 || strings.TrimSpace(cell.source) == "" {
						t.Errorf("%s %s %s: %s cell without shells or source: %+v", tg.ID(), goos, name, cell.evidence, cell)
					}
				case evidenceNone, evidenceUnknown:
					if len(cell.shells) != 0 {
						t.Errorf("%s %s %s: %s cell names shells %v", tg.ID(), goos, name, cell.evidence, cell.shells)
					}
				default:
					t.Errorf("%s %s %s: cell has no evidence state: %+v", tg.ID(), goos, name, cell)
				}
			}
		}
	}
}

// The declaration is exactly the reviewed table, row for row, and the table
// covers every host on every OS.
func TestShellDeclarationIsTheEvidenceTable(t *testing.T) {
	requireGoldenSequence(t)
	if want := len(goldenHostIDs) * len(hostShellOSes); len(declaredShellTable) != want {
		t.Fatalf("the evidence table has %d rows, want %d (every host on every OS)", len(declaredShellTable), want)
	}
	for _, row := range declaredShellTable {
		decl := shellDeclarer(t, row.host).Shells(row.goos)
		if decl.tool.evidence != row.tool || !slices.Equal(decl.tool.shells, row.toolShells) {
			t.Errorf("%s on %s, tool: declared %s %v, table says %s %v", row.host, row.goos, decl.tool.evidence, decl.tool.shells, row.tool, row.toolShells)
		}
		if decl.hook.evidence != row.hook || !slices.Equal(decl.hook.shells, row.hookShells) {
			t.Errorf("%s on %s, hook: declared %s %v, table says %s %v", row.host, row.goos, decl.hook.evidence, decl.hook.shells, row.hook, row.hookShells)
		}
		if decl.tool.choice != row.toolChoice {
			t.Errorf("%s on %s, tool: declared chosen by %q, table says %q", row.host, row.goos, decl.tool.choice, row.toolChoice)
		}
	}
}

// A tool cell naming more than one shell says who picks between them, and a
// cell naming one says nothing.
//
// This is not tidiness. The skill puts a condition above each block, and the
// condition is only answerable by whoever does the picking: "the tool you are
// calling" means nothing to a Cursor participant with one tool, and "your
// terminal" means nothing to a model choosing between two tools in one
// session. A cell that named two shells and left the choice empty would take
// the per-call wording by default, which is the wrong half for a host like
// Cursor and would point a Git Bash participant at the form that mangled
// their query (#96).
func TestEveryMultiShellToolCellSaysWhoChooses(t *testing.T) {
	for _, tg := range targetsByKind(targetHost) {
		d := shellDeclarer(t, tg.ID())
		for _, goos := range hostShellOSes {
			cell := d.Shells(goos).tool
			switch {
			case len(cell.shells) > 1 && cell.choice == "":
				t.Errorf("%s on %s declares %v and no choice: the skill would label both blocks by the tool being called, which is right only where the model picks per call", tg.ID(), goos, cell.shells)
			case len(cell.shells) <= 1 && cell.choice != "":
				t.Errorf("%s on %s declares %v with choice %q: there is nothing to pick between", tg.ID(), goos, cell.shells, cell.choice)
			case cell.choice != "" && cell.choice != chosenPerCall && cell.choice != chosenByParticipant:
				t.Errorf("%s on %s declares choice %q, which no renderer knows how to word", tg.ID(), goos, cell.choice)
			}
		}
	}
}

// declaredShells is what a renderer asks. An unknown cell, an OS nobody
// declared and a target that declares nothing are each a typed refusal
// naming the host, the OS and the channel; a channel the host does not have
// is nothing to render, not an error.
func TestDeclaredShellsRefusesWhatIsNotEstablished(t *testing.T) {
	refused := func(t *testing.T, tg installTarget, goos string, ch shellChannel) {
		t.Helper()
		shells, err := declaredShells(tg, goos, ch)
		var undeclared *undeclaredShellError
		if !errors.As(err, &undeclared) {
			t.Fatalf("%s %s %s: got shells %v, err %v; want an *undeclaredShellError", tg.ID(), goos, ch, shells, err)
		}
		if shells != nil || undeclared.host != tg.Label() || undeclared.goos != goos || undeclared.channel != ch {
			t.Fatalf("%s %s %s: refusal %+v with shells %v", tg.ID(), goos, ch, undeclared, shells)
		}
		if msg := err.Error(); !strings.Contains(msg, tg.Label()) || !strings.Contains(msg, goos) || !strings.Contains(msg, "not established") {
			t.Fatalf("refusal message %q does not name the host, the OS and the reason", msg)
		}
	}
	codex, _ := targetByID(installTargets, "codex")
	cursor, _ := targetByID(installTargets, "cursor")
	claude, _ := targetByID(installTargets, "claude")

	// A fixture, because no real host has a declared-OS tool cell left
	// unknown: Codex on Windows was the last one and a live run established
	// it. The refusal is the rule, not a fact about whichever host happens
	// still to be unmeasured, so it is asserted against a cell that cannot
	// be filled in from under it.
	t.Run("unknown tool cell", func(t *testing.T) {
		refused(t, unestablishedToolHost{}, "windows", channelTool)
	})
	t.Run("unknown hook cell", func(t *testing.T) { refused(t, claude, "freebsd", channelHook) })
	t.Run("an OS no host declares", func(t *testing.T) { refused(t, claude, "freebsd", channelTool) })
	t.Run("a target that declares nothing", func(t *testing.T) {
		refused(t, fakeIntegrationTarget{}, "linux", channelTool)
	})
	t.Run("no hook channel is nothing to render", func(t *testing.T) {
		shells, err := declaredShells(codex, "linux", channelHook)
		if shells != nil || err != nil {
			t.Fatalf("got %v, %v; want nil, nil", shells, err)
		}
	})
	t.Run("established and ruled cells answer their shells", func(t *testing.T) {
		if shells, err := declaredShells(claude, "windows", channelTool); err != nil || !slices.Equal(shells, []shellKind{shellPOSIX, shellPowerShell}) {
			t.Fatalf("claude windows tool: %v, %v", shells, err)
		}
		if shells, err := declaredShells(cursor, "windows", channelHook); err != nil || !slices.Equal(shells, []shellKind{shellCmd, shellPowerShell}) {
			t.Fatalf("cursor windows hook: %v, %v", shells, err)
		}
	})
}

// TestAMultiRunnerHookCellKeepsTheFormItCanProve states H3's first ruling as
// a test, so it holds on every runner and not only where it can be executed.
//
// Cursor's Windows hook cell names two runners and no single string serves
// both: six candidate forms were measured against four paths on a Windows
// runner, and the best of them stops at a path containing a $ or a %. So the
// command is the one form proven under cmd — v0.2.9's, the only runner ever
// observed to run a Cursor hook — and the install plan says why. Rendering a
// form proven nowhere would be worse than the status quo; declaring the cell
// unknown would install no hooks at all, because a hook command has no
// fallback: written for the wrong runner it fails silently, which is #69.
//
// The execution side of the same ruling is asserted on a Windows runner by
// TestInstalledHookCommandsRunInTheirRunner, which requires the command to
// run under cmd and to fail under the others.
func TestAMultiRunnerHookCellKeepsTheFormItCanProve(t *testing.T) {
	cursor, _ := targetByID(installTargets, "cursor")
	shells, err := declaredShells(cursor, "windows", channelHook)
	if err != nil || len(shells) < 2 {
		t.Fatalf("cursor windows hook declares %v (%v); this test exists for a cell naming more than one runner", shells, err)
	}
	e := binEntry{command: `C:\Program Files\tokendrop\dropin-miner.exe`, cfg: `C:\Users\u\tokendrop.toml`}
	cmd, note, err := e.hookCommandForRunners(shells, "cursor", "stop")
	if err != nil {
		t.Fatalf("rendering for %v: %v", shells, err)
	}
	wantCmd, err := e.hookCommandForShell(shellCmd, "cursor", "stop")
	if err != nil {
		t.Fatal(err)
	}
	if cmd != wantCmd {
		t.Errorf("multi-runner cell rendered %q, want the form proven under cmd %q", cmd, wantCmd)
	}
	if note == "" {
		t.Error("no plan note says the runner is not established; a participant would see nothing")
	}
	// A single-runner cell renders for that runner and says nothing extra.
	one, note, err := e.hookCommandForRunners([]shellKind{shellPOSIX}, "cursor", "stop")
	if err != nil || note != "" {
		t.Fatalf("single-runner cell: %q, note %q, err %v", one, note, err)
	}
	if want, _ := e.hookCommandForShell(shellPOSIX, "cursor", "stop"); one != want {
		t.Errorf("single-runner cell rendered %q, want %q", one, want)
	}
}
