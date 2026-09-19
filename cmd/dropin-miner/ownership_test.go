package main

// H5's subject: which installation an agent integration belongs to.
//
// #73, soak row S20. Two installations on one machine share a binary whenever
// the second was made by running the first's copy — `~/.tokendrop/bin/
// dropin-miner setup -home ~/dm-disposable` is the documented way. v0.2.9
// matched an integration by its binary path alone, so the disposable
// installation's `uninstall -purge-state` planned the removal of the soak
// installation's Claude Code hooks, Codex sandbox block and Cursor hooks. The
// shell-profile block was already right: it is matched by the config it
// names, and it was correctly left with a line saying why.

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// ── the rule, in isolation ──────────────────────────────────────────────

func TestAnIntegrationIsOursOnlyWhenItNamesOurBinaryAndOurConfig(t *testing.T) {
	const bin = "/home/u/.tokendrop/bin/dropin-miner"
	const ours = "/home/u/.tokendrop/tokendrop.toml"
	const theirs = "/home/u/dm-disposable/tokendrop.toml"
	ref := installationRef{bins: []string{bin}, cfg: ours}

	for _, tc := range []struct {
		name    string
		command string
		want    bool
	}{
		// Every spelling this client has written, naming our config.
		{"POSIX", posixQuoteArg(bin) + " hook -config " + posixQuoteArg(ours) + " lineage", true},
		{"PowerShell", "& " + powerShellQuoteArg(bin) + " hook -config " + powerShellQuoteArg(ours) + " lineage", true},
		{"cmd", `"` + bin + `" hook -config "` + ours + `" lineage`, true},
		{"v0.2.9 %q", strconv.Quote(bin) + " hook -config " + strconv.Quote(ours) + " lineage", true},
		{"bare", bin + " hook -config " + ours + " lineage", true},

		// The same binary, the other installation's config. This is the whole
		// of #73: every one of these was "ours" before H5.
		{"POSIX, their config", posixQuoteArg(bin) + " hook -config " + posixQuoteArg(theirs) + " lineage", false},
		{"PowerShell, their config", "& " + powerShellQuoteArg(bin) + " hook -config " + powerShellQuoteArg(theirs) + " lineage", false},
		{"cmd, their config", `"` + bin + `" hook -config "` + theirs + `" lineage`, false},
		{"v0.2.9 %q, their config", strconv.Quote(bin) + " hook -config " + strconv.Quote(theirs) + " lineage", false},

		// A different binary is not ours whatever config it names.
		{"another binary, our config", posixQuoteArg("/opt/other/dropin-miner") + " hook -config " + posixQuoteArg(ours) + " lineage", false},

		// An installation with a config does not own a discovery command.
		{"no -config at all", posixQuoteArg(bin) + " hook lineage", false},

		// Not ours, and not a command of ours either.
		{"someone else entirely", "/usr/bin/env echo hello", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ref.commandIsOurs(tc.command); got != tc.want {
				t.Errorf("commandIsOurs(%q) = %v, want %v", tc.command, got, tc.want)
			}
		})
	}
}

// An installation running on discovery wrote no -config, so for it the
// absence of the flag is the match and its presence is somebody else's.
func TestADiscoveryInstallationOwnsOnlyCommandsWithNoConfig(t *testing.T) {
	const bin = "/home/u/.tokendrop/bin/dropin-miner"
	ref := installationRef{bins: []string{bin}, cfg: ""}
	if !ref.commandIsOurs(posixQuoteArg(bin) + " hook lineage") {
		t.Error("a discovery installation must own the command it actually writes")
	}
	if ref.commandIsOurs(posixQuoteArg(bin) + " hook -config " + posixQuoteArg("/home/u/.tokendrop/tokendrop.toml") + " lineage") {
		t.Error("a command naming a config belongs to the installation that config configures, not to a discovery one")
	}
}

// The config is compared as a PATH, not as bytes: v0.2.9's %q hands Windows a
// path whose separators are doubled, naming the same file in other bytes.
func TestTheConfigIsComparedAsAPathNotAsBytes(t *testing.T) {
	const bin = `C:\Users\u\.tokendrop\bin\dropin-miner.exe`
	const cfg = `C:\Users\u\.tokendrop\tokendrop.toml`
	ref := installationRef{bins: []string{bin}, cfg: cfg}
	// Exactly what a v0.2.9 hooks.json holds, doubled separators and all.
	command := strconv.Quote(bin) + " hook -config " + strconv.Quote(cfg) + " lineage"
	if !strings.Contains(command, `\\`) {
		t.Fatal("this fixture is meant to carry doubled separators; it does not, so it proves nothing")
	}
	if !ref.commandIsOurs(command) {
		t.Errorf("a v0.2.9 entry for this installation was not recognized: %s", command)
	}
}

func TestRenderedWordsKeepsAQuotedPathWhole(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want []string
	}{
		{`a b c`, []string{"a", "b", "c"}},
		{`'/a path/x' hook -config '/b path/y'`, []string{`'/a path/x'`, "hook", "-config", `'/b path/y'`}},
		{`"C:\x y" hook`, []string{`"C:\x y"`, "hook"}},
		{`'it'\''s' hook`, []string{`'it'\''s'`, "hook"}},
		{`'it''s' hook`, []string{`'it''s'`, "hook"}},
	} {
		if got := renderedWords(tc.in); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("renderedWords(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// ── two installations sharing one binary, end to end ────────────────────

// installationWithAgents sets up an installation under home, using the same
// sandbox — and therefore the same binary — as the first, and gives it its
// agent integrations.
//
// For the machine's own installation setup does both. For any other home it
// no longer does (#84): setup -home <elsewhere> leaves the agents alone, and
// its closing line names `agents install -config <home>/tokendrop.toml` as
// the way to configure them. So that is the path this takes, which keeps the
// scenario below — two installations, one binary, both with integrations —
// reachable the way a participant now reaches it.
func installationWithAgents(t *testing.T, s *setupSandbox, home string, with ...string) {
	t.Helper()
	args := append([]string{"-yes", "-home", home}, with...)
	code, out, errOut := s.run(nil, false, args...)
	if code != exitOK {
		t.Fatalf("setup -home %s exited %d\n%s\n%s", home, code, out, errOut)
	}
	if samePath(home, s.home) {
		return
	}
	cfg := filepath.Join(home, setupConfigFile)
	if !strings.Contains(out, "agents install -config "+cfg) {
		t.Fatalf("setup -home %s did not name the command that configures its agents:\n%s", home, out)
	}
	install := []string{"install", "-config", cfg, "-yes"}
	for i := 0; i+1 < len(with); i += 2 {
		install = append(install, "-client", with[i+1])
	}
	var aout, aerr bytes.Buffer
	if code := agentsMain(s.agentOps(false), install, strings.NewReader(""), &aout, &aerr, s.getenv); code != exitOK {
		t.Fatalf("agents install -config %s exited %d\n%s\n%s", cfg, code, aout.String(), aerr.String())
	}
}

// TestUninstallingOneInstallationLeavesAnothersIntegrations is S20.
//
// Both installations run the SAME binary, which is what made every match
// succeed before H5. The machine installation sets its hosts up first and the
// disposable one second. Until #112 the second install overwrote every
// single-copy file, so this test uninstalled the FIRST and watched the
// second's files survive; that premise was the defect. Now the second install
// leaves them to their owner, so the files on disk name the first — and it is
// uninstalling the SECOND that must leave every one of them exactly as it
// found it, and say which ones and why. The direction changed; what is
// asserted did not.
func TestUninstallingOneInstallationLeavesAnothersIntegrations(t *testing.T) {
	for _, purge := range []bool{false, true} {
		name := "plain"
		if purge {
			name = "purge-state"
		}
		t.Run(name, func(t *testing.T) {
			s := newSetupSandbox(t)
			s.platform.claim("credits")
			hosts := []string{"-with", "claude", "-with", "codex", "-with", "cursor", "-with", "opencode", "-with", "pi", "-with", "hermes"}

			// The first installation is the sandbox's own home; the second is
			// a disposable one set up by running the first's binary.
			installationWithAgents(t, s, s.home, hosts...)
			disposable := filepath.Join(s.root, "dm-disposable")
			installationWithAgents(t, s, disposable, hosts...)

			// The single-copy artifacts — a skill, the plugin, the extension —
			// belong wholly to the installation that wrote them, which since
			// #112 is the one that got there first: the machine installation.
			// They must come through the OTHER installation's uninstall
			// untouched. The premise is checked rather than assumed, because
			// it is exactly what changed: a file naming the disposable's
			// config here would make everything below prove the opposite.
			uninstalled, keeper := disposable, s.home
			keeperCfg := filepath.Join(keeper, setupConfigFile)
			paths := s.paths()
			whole := []string{
				paths.claudeSkill, paths.codexSkill, paths.cursorSkill,
				paths.opencodePlugin, paths.piSkill, paths.piExtension, paths.hermesSkill,
			}
			before := map[string]fileSig{}
			for _, p := range whole {
				snap := snapshotTree(t, p)
				sig, ok := snap[p]
				if !ok {
					t.Fatalf("setup wrote no %s, so leaving it would prove nothing", p)
				}
				before[p] = sig
				if _, cfgs := namedInArtifact(string(s.readFile(p))); !configsInclude(cfgs, keeperCfg) || len(cfgs) == 0 {
					t.Fatalf("%s names %v, not %s: the second install did not leave it to its owner, and this test's premise is gone", p, cfgs, keeperCfg)
				}
			}

			var out string
			if purge {
				// -purge-state needs the typed confirmation at a terminal.
				var code int
				code, out, _ = s.uninstall(t, tty(uninstalled), true, nil, "-purge-state", "-home", uninstalled)
				if code != exitOK {
					t.Fatalf("uninstall -purge-state exited %d\n%s", code, out)
				}
			} else {
				var code int
				var errOut string
				code, out, errOut = s.uninstall(t, nil, false, nil, "-yes", "-home", uninstalled)
				if code != exitOK {
					t.Fatalf("uninstall exited %d\n%s\n%s", code, out, errOut)
				}
			}

			for p, want := range before {
				got, ok := snapshotTree(t, p)[p]
				if !ok {
					t.Errorf("uninstalling %s removed %s, which belongs to %s", uninstalled, p, keeper)
					continue
				}
				if !reflect.DeepEqual(want, got) {
					t.Errorf("uninstalling %s rewrote %s, which belongs to %s", uninstalled, p, keeper)
				}
			}

			// The shared files — the two hook files, which are MERGED rather
			// than overwritten, so each installation owns its own entries —
			// keep the keeper's and lose the uninstalled one's.
			//
			// Asked through the production matcher, over the DECODED JSON.
			// The bytes on disk are JSON-escaped, so on Windows a hook command
			// reads "'C:\\Users\\…'" and a raw path never appears in them: a
			// strings.Contains against filepath.Join can only ever fail there.
			// That is the same mistake as 79ea5ba, f97df97 and 41faaac, and it
			// is what made this test red on both Windows runners.
			mine := installationRef{bins: []string{s.exe, filepath.Join(uninstalled, "bin", binaryNameFor())}, cfg: filepath.Join(uninstalled, setupConfigFile)}
			theirs := installationRef{bins: []string{s.exe, filepath.Join(keeper, "bin", binaryNameFor())}, cfg: keeperCfg}
			for _, hooks := range []string{paths.claudeSettings, paths.cursorHooks} {
				entries := allHookEntries(t, hooks)
				if len(entries) == 0 {
					t.Errorf("uninstalling %s left no hook entries in %s, but %s's were there", uninstalled, hooks, keeper)
					continue
				}
				for _, e := range entries {
					if entryIsOurs(e, mine) {
						t.Errorf("%s still holds an entry of the installation just uninstalled: %v", hooks, e)
					}
				}
				kept := 0
				for _, e := range entries {
					if entryIsOurs(e, theirs) {
						kept++
					}
				}
				if kept == 0 {
					t.Errorf("%s lost every entry belonging to %s: %v", hooks, keeper, entries)
				}
			}

			// And it said what it left, naming the installation it left it to
			// — the way the profile block's refusal already read.
			if !strings.Contains(out, "left in place; it belongs to the installation configured by "+keeperCfg) {
				t.Errorf("uninstall did not report what it left and to whom:\n%s", out)
			}
		})
	}
}

// allHookEntries is every entry in every event of a host's hook file, read
// back the way the production code reads it — decoded, not scanned as bytes.
func allHookEntries(t *testing.T, path string) []any {
	t.Helper()
	body, err := os.ReadFile(path) // #nosec G304 -- this test's own sandbox
	if err != nil {
		t.Errorf("reading %s: %v", path, err)
		return nil
	}
	m, err := decodeJSONObject(body)
	if err != nil {
		t.Errorf("%s is not plain JSON: %v", path, err)
		return nil
	}
	var out []any
	hooks, _ := m["hooks"].(map[string]any)
	for _, v := range hooks {
		if list, ok := v.([]any); ok {
			out = append(out, list...)
		}
	}
	return out
}

func binaryNameFor() string {
	if runtime.GOOS == "windows" {
		return "dropin-miner.exe"
	}
	return "dropin-miner"
}

// ── a rendered path is read back one way ────────────────────────────────

// TestARenderedPathIsReadBackWhicheverShellQuotedIt is the guard the Windows
// runners had to supply before it existed.
//
// H5 shipped with two readings of a rendered path: unquoteRenderedPath, and a
// second helper whose double-quoted branch only tried strconv.Unquote. A
// cmd-rendered Windows path — `"C:\Users\…"`, a LITERAL, which is exactly what
// Cursor's hooks.json holds on Windows — fails Unquote on \U, so that helper
// answered "no path here". The config half of the attribution went red on both
// Windows runners; the binary half did NOT, because an artifact naming no
// binary is treated as contradicting nothing, so it failed open and silently
// widened what uninstall would claim.
//
// Written as a unit case over literal strings so it needs no Windows runner:
// a Windows path is only ever a string here, and nothing executes it.
func TestARenderedPathIsReadBackWhicheverShellQuotedIt(t *testing.T) {
	const winBin = `C:\Users\u\.tokendrop\bin\dropin-miner.exe`
	const winCfg = `C:\Users\u\.tokendrop\tokendrop.toml`
	e := binEntry{command: winBin, cfg: winCfg}

	// One row per renderer that has ever written one of these files.
	for _, tc := range []struct {
		name string
		sh   shellKind
	}{
		{"cmd (Cursor's Windows hook form)", shellCmd},
		{"POSIX (Claude Code's Windows hook form, Git Bash)", shellPOSIX},
		{"PowerShell", shellPowerShell},
	} {
		t.Run(tc.name, func(t *testing.T) {
			command, err := e.hookCommandForShell(tc.sh, "cursor", "sessionStart")
			if err != nil {
				t.Fatalf("rendering for %s: %v", tc.sh, err)
			}

			gotCfg := namedConfigs(command)
			if !containsPath(gotCfg, winCfg) {
				t.Errorf("the config is unreadable in the %s form:\n  %s\n  namedConfigs -> %q", tc.sh, command, gotCfg)
			}
			gotBin := namedBinaries(command)
			if !containsPath(gotBin, winBin) {
				t.Errorf("the binary is unreadable in the %s form:\n  %s\n  namedBinaries -> %q", tc.sh, command, gotBin)
			}
		})
	}

	// v0.2.9's %q, which an upgraded installation still carries.
	legacy := strconv.Quote(winBin) + " hook -config " + strconv.Quote(winCfg) + " cursor sessionStart"
	if !containsPath(namedConfigs(legacy), winCfg) || !containsPath(namedBinaries(legacy), winBin) {
		t.Errorf("a v0.2.9 entry is unreadable:\n  %s\n  configs %q\n  binaries %q", legacy, namedConfigs(legacy), namedBinaries(legacy))
	}
}

// TestTheInstallationNamedInAMessageIsTheDecodedReading is the sentence, not
// the matching.
//
// unquoteRenderedPath returns every reading of a rendered word, the decoded
// one last, and describeOther used to print the FIRST. For a JSON-quoted
// path -- opencode's and Pi's INSTALL_CONFIG line -- the first reading is the
// literal one, so on Windows the message named
// C:\\Users\\...\\tokendrop.toml with every separator doubled while the file
// itself was correctly left alone. Both Windows runners found it on #123's
// first CI run; every other runner was green, because on POSIX nothing in a
// path needs escaping and a word's two readings are the same string.
//
// A unit case over literal strings, so it needs no Windows runner: a Windows
// path is only ever a string here and nothing executes it.
func TestTheInstallationNamedInAMessageIsTheDecodedReading(t *testing.T) {
	const ours = "/home/u/.tokendrop/tokendrop.toml"
	for _, tc := range []struct {
		name     string
		artifact string
		want     string
	}{
		{
			// What renderAgentScript writes into a JavaScript adapter: the
			// path JSON-quoted, which doubles every backslash. This is the
			// row that was red on both Windows runners.
			name:     "a JSON-quoted Windows path (opencode's INSTALL_CONFIG)",
			artifact: `const INSTALL_CONFIG = "C:\\Users\\u\\dm-disposable\\tokendrop.toml";`,
			want:     `C:\Users\u\dm-disposable\tokendrop.toml`,
		},
		{
			// v0.2.9 quoted every path with Go's %q, and an installation that
			// upgraded still carries it until its next agents install.
			name:     "a %q-quoted Windows path (v0.2.9)",
			artifact: strconv.Quote(`C:\Users\u\dm-disposable\dropin-miner.exe`) + " search -config " + strconv.Quote(`C:\Users\u\dm-disposable\tokendrop.toml`),
			want:     `C:\Users\u\dm-disposable\tokendrop.toml`,
		},
		{
			// The POSIX half: a single-quoted path carrying a space, which is
			// what a participant whose home has one actually gets. Its want
			// goes through filepath.Clean because the display does, and on
			// Windows Clean turns / into \ -- a POSIX path inside a Windows
			// artifact is not a case that occurs, and the normalization
			// cancels from both sides, leaving this row guarding the one
			// thing it is here for: that the quotes came off and the space
			// survived.
			name:     "a POSIX single-quoted path with a space",
			artifact: "'/home/u/my configs/dm-disposable/dropin-miner' search -config '/home/u/my configs/dm-disposable/tokendrop.toml'",
			want:     filepath.Clean("/home/u/my configs/dm-disposable/tokendrop.toml"),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, cfgs := namedInArtifact(tc.artifact)
			if len(cfgs) == 0 {
				t.Fatalf("no config was read out of the artifact at all, so this row proves nothing:\n  %s", tc.artifact)
			}
			got := describeOther(nil, cfgs, installationRef{cfg: ours})
			if want := "the installation configured by " + tc.want; got != want {
				t.Errorf("describeOther named the wrong reading\n got %s\nwant %s\nreadings %q", got, want, cfgs)
			}
		})
	}

	// An artifact naming nothing readable still says "another installation"
	// rather than naming an empty path.
	if got := describeOther(nil, nil, installationRef{cfg: ours}); got != "another installation" {
		t.Errorf("an artifact naming nothing: %s", got)
	}
}

// TestAJSONArtifactIsDecodedNotScanned is the Windows failure itself, made
// reproducible on any runner.
//
// A hook file is JSON, so a command inside it carries two layers of escaping:
// the shell's, then JSON's. On Windows Cursor's hook command is cmd-rendered
// — `"C:\Users\…"`, double quotes taken literally — and once JSON has escaped
// it the file holds `\"C:\\Users\\…\"`. A reader of rendered TEXT cannot make
// sense of that: it was read as a lone backslash, so the file named no
// installation and uninstall judged it unattributable. On POSIX the same
// reader was right, because nothing in those paths needs escaping — which is
// why every occurrence of this mistake has been found by the Windows runners.
func TestAJSONArtifactIsDecodedNotScanned(t *testing.T) {
	const winBin = `C:\Users\u\.tokendrop\bin\dropin-miner.exe`
	const winCfg = `C:\Users\u\.tokendrop\tokendrop.toml`
	e := binEntry{command: winBin, cfg: winCfg}

	command, err := e.hookCommandForShell(shellCmd, "cursor", "sessionStart")
	if err != nil {
		t.Fatalf("rendering the cmd form: %v", err)
	}
	// The file as planHooksMerge writes it.
	body, err := json.MarshalIndent(map[string]any{
		"version": 1,
		"hooks":   map[string]any{"sessionStart": []any{map[string]any{"command": command}}},
	}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `\"C:\\Users`) {
		t.Fatalf("this fixture is meant to carry both layers of escaping; it does not, so it proves nothing:\n%s", body)
	}

	bins, cfgs := namedInArtifact(string(body))
	if !containsPath(cfgs, winCfg) {
		t.Errorf("the config is unreadable in a JSON hook file:\n%s\n  configs -> %q", body, cfgs)
	}
	if !containsPath(bins, winBin) {
		t.Errorf("the binary is unreadable in a JSON hook file:\n%s\n  binaries -> %q", body, bins)
	}

	// And a plain-text artifact still reads as text: the distinction is made
	// per artifact, not abolished.
	skill := "run " + posixQuoteArg(winBin) + " search -config " + posixQuoteArg(winCfg) + " --stdin\n"
	if _, cfgs := namedInArtifact(skill); !containsPath(cfgs, winCfg) {
		t.Errorf("a plain-text artifact stopped being scanned: %q", cfgs)
	}
}

func containsPath(got []string, want string) bool {
	for _, g := range got {
		if samePath(g, want) {
			return true
		}
	}
	return false
}

// ── every artifact says whose it is ─────────────────────────────────────

// TestEveryArtifactNamesTheInstallationThatWroteIt is the structural guard
// behind the rule. Uninstall can only attribute a file that names an
// installation, and a file it cannot attribute it must leave — so an artifact
// that names none is one this client can never take back out.
//
// opencode's plugin is why this exists: it rewrites commands and runs none, so
// it named no binary and no config at all, and a disposable installation's
// purge removed the main installation's copy (#73's last comment).
func TestEveryArtifactNamesTheInstallationThatWroteIt(t *testing.T) {
	entry := goldenEntry()
	for _, id := range goldenHostIDs {
		t.Run(id, func(t *testing.T) {
			surface, ok := surfaceByID(id)
			if !ok {
				t.Fatalf("no target registered as %q", id)
			}
			_, ops := newFakeMachine()
			paths := ops.paths(noEnv)
			plan := buildInstallPlan(ops, paths, []installTarget{surface}, entry, noEnv)
			if len(plan.writes) == 0 {
				t.Fatalf("%s wrote nothing, so this proves nothing about what it names", id)
			}
			for _, w := range plan.writes {
				if strings.HasSuffix(w.path, "config.toml") {
					// Codex's sandbox block names directories, not a config;
					// it is attributed by its writable roots instead, which
					// TestTheCodexSandboxBlockIsAttributedByItsWritableRoots
					// covers.
					continue
				}
				// Read the way production reads it: namedInArtifact decodes a
				// JSON file and scans anything else. Scanning a hook file's
				// bytes with a reader of rendered text is what made this guard
				// red on the Windows runners — the command inside is escaped
				// twice there, by the shell and then by JSON.
				_, named := namedInArtifact(string(w.contents))
				ours := false
				for _, c := range named {
					if samePath(c, entry.cfg) {
						ours = true
					}
				}
				if !ours {
					t.Errorf("%s names no installation, so uninstall could never attribute it: %s\nnamed: %v", w.path, w.why, named)
				}
			}
		})
	}
}

// ── the invariant that keeps a hook file out of the attribution ─────────

// TestNoHookFileCanEnterTheAttributionPlan pins something subtle and
// load-bearing: `attributeRemoved` reads raw file bytes, and it must never be
// handed a hook file.
//
// It holds for a reason that is easy to break by accident. Attribution runs
// against the AGNOSTIC plan, built with `uninstallProbeCommand` — a string
// beginning with a NUL byte, which no rendered command starts with — so
// `entryIsOurs` matches nothing, `planHooksRemove` never sets changed, and it
// returns before reaching either the write or the removal it would otherwise
// plan. Break either half and the failure is the #69 family again: a hook file
// that IS ours, unreadable as bytes on Windows, judged unattributable and left
// behind running a binary that has been deleted.
//
// So both halves are asserted, because either alone would pass while the
// invariant was broken.
func TestNoHookFileCanEnterTheAttributionPlan(t *testing.T) {
	entry := goldenEntry()

	// Half one: the probe prefix-matches no command this client renders.
	probe := installationRef{bins: []string{uninstallProbeCommand}, cfg: entry.cfg}
	rendered := 0
	for _, id := range goldenHostIDs {
		tg, ok := surfaceByID(id)
		if !ok {
			t.Fatalf("no target registered as %q", id)
		}
		for _, sh := range []shellKind{shellPOSIX, shellPowerShell, shellCmd} {
			for _, sub := range [][]string{{"lineage"}, {"cursor", "sessionStart"}, {"window", "pre-compact"}} {
				cmd, err := entry.hookCommandForShell(sh, sub...)
				if err != nil {
					continue
				}
				rendered++
				if _, matched := probe.afterBinary(cmd); matched {
					t.Errorf("the uninstall probe matches a rendered command, so a hook file could enter the attribution: %s", cmd)
				}
			}
		}
		_ = tg
	}
	if rendered == 0 {
		t.Fatal("no command was rendered, so the probe was never actually tried against one")
	}

	// Half two: on a fully installed machine, no host's agnostic plan removes
	// a hook file. This is the property attribution actually depends on, and
	// it is asserted directly rather than inferred from half one.
	_, ops := newFakeMachine()
	paths := ops.paths(noEnv)
	all := targetsByKind(targetHost)
	installed := buildInstallPlan(ops, paths, all, entry, noEnv)
	if failures := commitPlan(ops, &installed, io.Discard, io.Discard); failures != 0 {
		t.Fatalf("installing every host: %d failures", failures)
	}
	for _, p := range []string{paths.claudeSettings, paths.cursorHooks} {
		if _, err := ops.readFile(p); err != nil {
			t.Fatalf("install wrote no %s, so this proves nothing about keeping it out", p)
		}
	}
	hookFiles := map[string]bool{slash(paths.claudeSettings): true, slash(paths.cursorHooks): true}
	for _, tg := range all {
		var agnostic agentPlan
		tg.PlanUninstall(ops, paths, binEntry{command: uninstallProbeCommand, cfg: entry.cfg}, noEnv, &agnostic)
		for _, rm := range agnostic.removes {
			if hookFiles[slash(rm.path)] {
				t.Errorf("%s's agnostic plan would remove the hook file %s, which attribution then reads as raw bytes", tg.Label(), rm.path)
			}
		}
		for _, w := range agnostic.writes {
			if hookFiles[slash(w.path)] {
				t.Errorf("%s's agnostic plan would rewrite the hook file %s, so the probe matched something", tg.Label(), w.path)
			}
		}
	}
}

// ── the emptied hook file ───────────────────────────────────────────────

// A hook file left holding nothing but what this client put there is removed,
// and one still holding somebody else's entry is kept. The first half closes
// H4's leftover: uninstall used to leave ~/.cursor/hooks.json as
// `{"hooks":{},"version":1}`, bytes only this client writes, so a machine
// that never had Cursor detected as Cursor from then on.
func TestAnEmptiedHookFileIsRemovedAndASharedOneIsKept(t *testing.T) {
	t.Run("nothing but ours", func(t *testing.T) {
		m, ops := newFakeMachine("cursor")
		if code, out, errOut := runAgents(t, ops, nil, "install", "-config", testCfg, "-yes"); code != exitOK {
			t.Fatalf("install: %d\n%s%s", code, out, errOut)
		}
		if _, ok := m.files["/home/u/.cursor/hooks.json"]; !ok {
			t.Fatal("install wrote no hooks.json, so removing it proves nothing")
		}
		if code, out, errOut := runAgents(t, ops, nil, "uninstall", "-config", testCfg, "-yes"); code != exitOK {
			t.Fatalf("uninstall: %d\n%s%s", code, out, errOut)
		}
		if body, ok := m.files["/home/u/.cursor/hooks.json"]; ok {
			t.Errorf("hooks.json held nothing but ours and survived: %s", body)
		}
	})

	t.Run("a foreign entry keeps the file", func(t *testing.T) {
		m, ops := newFakeMachine("cursor")
		if code, out, errOut := runAgents(t, ops, nil, "install", "-config", testCfg, "-yes"); code != exitOK {
			t.Fatalf("install: %d\n%s%s", code, out, errOut)
		}
		// One hook of the participant's own, on an event we also use.
		doc := hooksOf(t, m, "/home/u/.cursor/hooks.json")
		list, _ := doc["stop"].([]any)
		doc["stop"] = append(list, map[string]any{"command": "/usr/local/bin/their-tool stop"})
		writeJSONFile(t, m, "/home/u/.cursor/hooks.json", map[string]any{"hooks": doc, "version": 1})

		if code, out, errOut := runAgents(t, ops, nil, "uninstall", "-config", testCfg, "-yes"); code != exitOK {
			t.Fatalf("uninstall: %d\n%s%s", code, out, errOut)
		}
		body, ok := m.files["/home/u/.cursor/hooks.json"]
		if !ok {
			t.Fatal("a hooks.json still holding somebody else's hook was removed")
		}
		if !strings.Contains(string(body), "their-tool") {
			t.Errorf("the foreign hook did not survive: %s", body)
		}
	})
}

func writeJSONFile(t *testing.T, m *fakeMachine, path string, v any) {
	t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	m.files[path] = append(b, '\n')
}

// ── Codex's sandbox block ───────────────────────────────────────────────

// The block names no binary and no config; it names DIRECTORIES. So it is
// attributed by them — against the directories THIS config names, which are
// four config keys and not a layout.
//
// The first version of this test asserted the layout ("all under that
// installation's home") and passed, because the default layout does put them
// there. That is what made the reading look right: it is true of every
// installation nobody has configured otherwise, and false of the first one
// that relocates a directory. The cases below therefore include a config whose
// state_dir is nowhere near its home.
func TestTheCodexSandboxBlockIsAttributedByTheDirectoriesItsConfigNames(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "install")
	cfg := writeMiningHomeConfig(t, home)
	entry := binEntry{command: filepath.Join(home, "bin", "dropin-miner"), cfg: cfg}

	blockOf := func(dirs ...string) []byte { return appendMarkedBlock(nil, codexSandboxBlock(dirs)) }

	// A subset of the four is ours: with [miner] off only the state dir is
	// written, and that rendering stays ours after `mining enable`.
	if r := removeOurSandboxBlock(blockOf(filepath.Join(home, "state")), entry, noEnv); !r.had || !r.ours {
		t.Errorf("a state-dir-only block of ours was not recognized (had=%v ours=%v)", r.had, r.ours)
	}
	if r := removeOurSandboxBlock(blockOf(filepath.Join(home, "intake"), filepath.Join(home, "state")), entry, noEnv); !r.had || !r.ours {
		t.Errorf("this installation's own sandbox block was not recognized (had=%v ours=%v)", r.had, r.ours)
	}
	// Another installation's, and a mixture: one root that is not one of
	// ours is enough, because the renderer writes one config's roots as one
	// list and never a mixture.
	if r := removeOurSandboxBlock(blockOf(filepath.Join(root, "other", "state")), entry, noEnv); !r.had || r.ours {
		t.Errorf("another installation's sandbox block was claimed (had=%v ours=%v)", r.had, r.ours)
	}
	if r := removeOurSandboxBlock(blockOf(filepath.Join(home, "state"), filepath.Join(root, "other", "state")), entry, noEnv); !r.had || r.ours {
		t.Errorf("a block mixing our root with a foreign one was claimed (had=%v ours=%v)", r.had, r.ours)
	}
	// A directory this config names, nowhere near the home that holds it.
	// The home-prefix reading called this one another installation's.
	elsewhere := filepath.Join(root, "relocated-state")
	relocated := filepath.Join(root, "installB")
	cfgB := writeRelocatedStateConfig(t, relocated, elsewhere)
	entryB := binEntry{command: entry.command, cfg: cfgB}
	if r := removeOurSandboxBlock(blockOf(elsewhere), entryB, noEnv); !r.had || !r.ours {
		t.Errorf("a block naming this config's own relocated state_dir was not recognized (had=%v ours=%v)", r.had, r.ours)
	}
	if r := removeOurSandboxBlock([]byte("[nothing]\n"), entry, noEnv); r.had {
		t.Error("a config with no marked block reported one")
	}
	// No config to compare against names nothing, so nothing can be claimed.
	missing := binEntry{command: entry.command, cfg: filepath.Join(root, "gone", setupConfigFile)}
	r := removeOurSandboxBlock(blockOf(filepath.Join(home, "state")), missing, noEnv)
	if !r.had || r.ours {
		t.Errorf("a block was claimed by an installation whose config cannot be read (had=%v ours=%v)", r.had, r.ours)
	}
	if !strings.Contains(r.why, "own config cannot be read") {
		t.Errorf("the reason does not say the config could not be read: %q", r.why)
	}
}

func TestPathUnderIsContainmentNotAPrefixTest(t *testing.T) {
	dir := filepath.Join("/home", "u", ".tokendrop")
	for _, tc := range []struct {
		p    string
		want bool
	}{
		{dir, true},
		{filepath.Join(dir, "state"), true},
		{filepath.Join(dir, "a", "b"), true},
		// The prefix test this replaces would have said true: the string
		// starts with the directory's own bytes.
		{"/home/u/.tokendrop-other/state", false},
		{"/home/u", false},
		{"/elsewhere", false},
	} {
		if got := pathUnder(tc.p, dir); got != tc.want {
			t.Errorf("pathUnder(%q, %q) = %v, want %v", tc.p, dir, got, tc.want)
		}
	}
}

// ── the adapters carry their installation ───────────────────────────────

func TestAJavaScriptAdapterCarriesTheConfigItWasInstalledWith(t *testing.T) {
	cfg := filepath.Join(string(filepath.Separator)+"home", "u", ".tokendrop", "tokendrop.toml")
	for name, template := range map[string]string{
		"opencode plugin": opencodePluginJS,
		"pi extension":    piExtensionTS,
	} {
		t.Run(name, func(t *testing.T) {
			rendered := renderAgentScript(template, shellPOSIX, cfg)
			if strings.Contains(rendered, traceConfigMarker) {
				t.Fatal("the marker survived rendering, so the adapter names no installation")
			}
			named := namedConfigs(rendered)
			ours := false
			for _, c := range named {
				if samePath(c, cfg) {
					ours = true
				}
			}
			if !ours {
				t.Errorf("the rendered adapter does not name %s; named: %v", cfg, named)
			}
		})
	}
}

// A Windows path is mostly backslashes, and the marker sits inside a
// JavaScript string literal: rendered without escaping, the adapter would not
// parse and its own path would come back wrong.
func TestTheAdaptersConfigSurvivesAWindowsPath(t *testing.T) {
	const cfg = `C:\Users\u\.tokendrop\tokendrop.toml`
	rendered := renderAgentScript(opencodePluginJS, shellPOSIX, cfg)
	if !strings.Contains(rendered, `"C:\\Users\\u\\.tokendrop\\tokendrop.toml"`) {
		t.Fatalf("the config was not escaped for a JavaScript string literal:\n%s", firstLineNaming(rendered, "INSTALL_CONFIG"))
	}
	for _, c := range namedConfigs(rendered) {
		if c == cfg {
			return
		}
	}
	t.Errorf("the escaped config does not read back as %s: %v", cfg, namedConfigs(rendered))
}

func firstLineNaming(s, want string) string {
	for _, line := range strings.Split(s, "\n") {
		if strings.Contains(line, want) {
			return line
		}
	}
	return "(no line names " + want + ")"
}
