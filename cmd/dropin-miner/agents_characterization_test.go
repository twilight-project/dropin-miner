package main

// The pre-registry characterization: what agents.go's three host switches
// (install, uninstall, status), its selection rule, `agents prefer` and
// the top-level help text do today, captured before any of it moves into
// targets.go. This file references nothing commit 2 introduces — no
// installTarget, no targetKind, no registry of any kind — only
// agentSurfaces and the switch-based functions that already exist. The
// refactor that follows must reproduce every plan, every status line,
// every rewritten skill and the rendered help byte-for-byte; this is the
// baseline that is checked against.
//
// The goldens under testdata/ are read-only inputs to this file. There is
// no flag that regenerates them: a future intentional behavior change
// updates the fixture as a reviewed diff, the same as any other file in
// the tree, never by re-running the test with a write switch.
//
// goldenHostIDs and goldenHostLabels are literals, not derived from
// agentSurfaces: dropping or reordering a host must fail this file by
// diverging from the literal, not by silently characterizing a different
// set or sequence.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

var (
	goldenHostIDs    = []string{"claude", "codex", "cursor", "opencode", "pi", "hermes"}
	goldenHostLabels = []string{"Claude Code", "Codex", "Cursor", "opencode", "Pi", "Hermes"}
)

const goldenBin = "/home/u/.tokendrop/bin/dropin-miner"

func goldenEntry() binEntry { return binEntry{command: goldenBin, cfg: testCfg} }

// containsFlat matches want against s with s's whitespace collapsed: the
// skill is hand-wrapped prose, so a phrase can straddle a line break, and
// a reflow is not the drift a guard like this is watching for.
func containsFlat(s, want string) bool {
	return strings.Contains(strings.Join(strings.Fields(s), " "), want)
}

// requireGoldenSequence is the guard every characterization test in this
// file starts with: the registry-to-be has exactly these six hosts, in
// exactly this order, with exactly these labels. Every other test in this
// file assumes that and would otherwise be characterizing hosts that no
// longer match the literal it reports against.
func requireGoldenSequence(t *testing.T) {
	t.Helper()
	if len(agentSurfaces) != len(goldenHostIDs) {
		t.Fatalf("agentSurfaces has %d hosts, want the literal %d (%v): a host was added or removed",
			len(agentSurfaces), len(goldenHostIDs), goldenHostIDs)
	}
	for i, s := range agentSurfaces {
		if s.id != goldenHostIDs[i] || s.label != goldenHostLabels[i] {
			t.Fatalf("agentSurfaces[%d] = {%q,%q}, want {%q,%q} in this order",
				i, s.id, s.label, goldenHostIDs[i], goldenHostLabels[i])
		}
	}
}

// ── install / uninstall plan goldens ────────────────────────────────────

type goldenWrite struct {
	Surface string `json:"surface"`
	Path    string `json:"path"`
	Mode    uint32 `json:"mode"`
	Why     string `json:"why"`
	Content string `json:"content"`
}

type goldenPlan struct {
	Writes  []goldenWrite `json:"writes"`
	Removes []string      `json:"removes,omitempty"`
	Skipped []string      `json:"skipped,omitempty"`
	Refused []string      `json:"refused,omitempty"`
	Notes   []string      `json:"notes,omitempty"`
}

// capturePlan slashes removes and notes, not only a write's own path.
// removes names a path directly (filepath.Dir of a host's skill path, on
// Windows built with the native separator); a note carries a path too if
// its host ever quotes one. Left unslashed, either could carry a native
// Windows separator a golden captured on a POSIX machine does not, and
// fail to compare equal for a reason that has nothing to do with
// behavior — the failure this fixes, on the Windows CI runner.
func capturePlan(p agentPlan) goldenPlan {
	g := goldenPlan{Skipped: p.skipped, Refused: p.refused}
	for _, r := range p.removes {
		g.Removes = append(g.Removes, slash(r))
	}
	for _, n := range p.notes {
		g.Notes = append(g.Notes, slash(n))
	}
	for _, w := range p.writes {
		g.Writes = append(g.Writes, goldenWrite{
			Surface: w.surface,
			Path:    slash(w.path),
			Mode:    uint32(w.mode),
			Why:     w.why,
			Content: string(w.contents),
		})
	}
	return g
}

// compareGoldenPlan reads path as a fixed, checked-in fixture and requires
// got to match it byte-for-byte once marshaled. There is no write mode:
// a missing or differing golden is a test failure, not something this
// function can fix.
func compareGoldenPlan(t *testing.T, got goldenPlan, path string) {
	t.Helper()
	gotJSON, err := json.MarshalIndent(got, "", "  ")
	if err != nil {
		t.Fatalf("marshal plan: %v", err)
	}
	gotJSON = append(gotJSON, '\n')
	want, err := os.ReadFile(path) // #nosec G304 -- a fixed testdata path this file builds, not an external one
	if err != nil {
		t.Fatalf("reading golden %s: %v", path, err)
	}
	if string(want) != string(gotJSON) {
		t.Errorf("plan differs from %s:\n--- got ---\n%s\n--- want ---\n%s", path, gotJSON, want)
	}
}

// capturePlanForGolden is capturePlan plus, for Hermes only, two
// placeholder normalizations, the same technique the Codex sandbox pair
// uses for its temp directory: Hermes' home is OS-chosen by design
// (hermesHomeDir branches on runtime.GOOS, not on the fake ops' home),
// and the hook command line the install writes into config.yaml is
// *also* platform-dependent by design (hermesHookCommand quotes every
// argument on Windows and leaves a bare POSIX path unquoted — Hermes'
// own splitter needs each, and hermes_install_test.go's
// TestHermesHookCommandSurvivesYAMLAndTheArgvSplitter is what actually
// proves each is right; this test only needs the two to compare equal).
// Without normalizing both, this golden can only ever be right on the OS
// it was captured on.
func capturePlanForGolden(id string, ops agentOps, entry binEntry, plan agentPlan) goldenPlan {
	got := capturePlan(plan)
	if id == "hermes" {
		got = normalizeHermesHome(got, slash(hermesHomeDir(ops.home, noEnv)))
		if cmd, ok := hermesHookCommand(entry, runtime.GOOS == "windows"); ok {
			got = replaceInGoldenPlan(got, hermesYAMLSingleQuoted(cmd), "<HERMES_HOOK_COMMAND>")
		}
	}
	return got
}

// TestInstallPlanGoldenPerHost characterizes buildInstallPlan one host at a
// time, against a fixed binEntry and an empty in-memory machine (nothing
// pre-existing to merge with).
func TestInstallPlanGoldenPerHost(t *testing.T) {
	requireGoldenSequence(t)
	entry := goldenEntry()
	for _, id := range goldenHostIDs {
		t.Run(id, func(t *testing.T) {
			surface, ok := surfaceByID(id)
			if !ok {
				t.Fatalf("no agentSurface registered for %q", id)
			}
			_, ops := newFakeMachine()
			paths := ops.paths(noEnv)
			plan := buildInstallPlan(ops, paths, []agentSurface{surface}, entry, noEnv)
			compareGoldenPlan(t, capturePlanForGolden(id, ops, entry, plan), filepath.Join("testdata", "agents", id+".install.golden"))
		})
	}
}

// TestUninstallPlanGoldenPerHost characterizes buildUninstallPlan against
// the state a real install for that host actually leaves behind — Cursor
// and Claude's hook merges, Pi's two files, Hermes' hook block — rather
// than a hand-built fixture that could drift from what install truly
// writes. Codex's marked-sandbox-block branch is characterized separately
// (TestCodexSandboxPlanGolden below): with the fixed, unresolvable testCfg
// every other host test in this package uses, codexSandboxRoots' real
// (non-injectable) config read fails and no block is ever written, so this
// pair characterizes the no-config note branch, same as install's.
func TestUninstallPlanGoldenPerHost(t *testing.T) {
	requireGoldenSequence(t)
	entry := goldenEntry()
	for _, id := range goldenHostIDs {
		t.Run(id, func(t *testing.T) {
			surface, ok := surfaceByID(id)
			if !ok {
				t.Fatalf("no agentSurface registered for %q", id)
			}
			_, ops := newFakeMachine()
			paths := ops.paths(noEnv)
			installPlan := buildInstallPlan(ops, paths, []agentSurface{surface}, entry, noEnv)
			if failures := commitPlan(ops, &installPlan, io.Discard, io.Discard); failures != 0 {
				t.Fatalf("committing the install %s's uninstall is characterized against: %d failures", id, failures)
			}
			plan := buildUninstallPlan(ops, paths, []agentSurface{surface}, entry)
			compareGoldenPlan(t, capturePlanForGolden(id, ops, entry, plan), filepath.Join("testdata", "agents", id+".uninstall.golden"))
		})
	}
}

// ── Codex's marked-sandbox-block branch, as its own golden pair ─────────

// sandboxGoldenConfig writes a real config (codexSandboxRoots' loadConfig
// call reads the real filesystem, not the fake ops) into t.TempDir(), with
// the miner enabled, so all four writable roots resolve — the branch
// codex_sandbox_test.go's sandboxTestConfig exercises. Every path here is
// built with an explicit forward slash rather than filepath.Join, and
// os.MkdirAll/os.WriteFile/os.ReadFile all accept that on every OS this
// package's test matrix runs on (Windows included): that keeps the
// captured text free of a native separator that would make the golden
// differ by platform. The one thing that still varies between runs — the
// temp directory's random name — is normalized out before comparison.
func sandboxGoldenConfig(t *testing.T) (cfgPath, home string) {
	t.Helper()
	home = filepath.ToSlash(t.TempDir())
	for _, d := range []string{"state", "spool", "intake", "sessions"} {
		if err := os.MkdirAll(home+"/"+d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	doc := fmt.Sprintf(`
[mining]
enabled = true
as_url = "https://as.example.invalid"
chain_id = "twilight-1"
slot_id = 7
state_dir = %q
spool_dir = %q

[miner]
enabled = true
router_url = "https://router.example.invalid"
intake_dir = %q
sessions_dir = %q
`, home+"/state", home+"/spool", home+"/intake", home+"/sessions")
	cfgPath = home + "/tokendrop.toml"
	if err := os.WriteFile(cfgPath, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfgPath, home
}

// replaceInGoldenPlan replaces every occurrence of from with placeholder,
// in every string field a captured plan carries (paths, removes, notes,
// and file content), so the golden comparison is independent of a value
// that is expected to vary by run or by machine rather than by behavior.
func replaceInGoldenPlan(g goldenPlan, from, placeholder string) goldenPlan {
	repl := func(s string) string { return strings.ReplaceAll(s, from, placeholder) }
	out := goldenPlan{}
	for _, w := range g.Writes {
		out.Writes = append(out.Writes, goldenWrite{
			Surface: w.Surface,
			Path:    repl(w.Path),
			Mode:    w.Mode,
			Why:     w.Why,
			Content: repl(w.Content),
		})
	}
	for _, r := range g.Removes {
		out.Removes = append(out.Removes, repl(r))
	}
	for _, s := range g.Skipped {
		out.Skipped = append(out.Skipped, repl(s))
	}
	for _, r := range g.Refused {
		out.Refused = append(out.Refused, repl(r))
	}
	for _, n := range g.Notes {
		out.Notes = append(out.Notes, repl(n))
	}
	return out
}

// normalizeGoldenPlan replaces every occurrence of the per-run temp
// directory with a stable placeholder, so the golden comparison is
// independent of where the OS happened to put this run's temp directory.
//
// On Windows only, that is not the whole story: sandboxSettings writes
// each writable root through strconv.Quote, and by the time a root
// reaches it, codexSandboxRoots' filepath.Clean has already turned every
// "/" this test built home with into "\" — so Quote then escapes each of
// those into "\\", and the raw, forward-slash home this test constructed
// never appears in the file at all; the replacement above silently
// matches nothing. This additionally computes that TOML-escaped Windows
// form of home with the same strconv.Quote sandboxSettings uses, replaces
// it with the same placeholder, and then turns whatever backslash
// remains inside the substituted roots — the doubled path separators
// between TESTHOME and each root's own leaf name, also a product of
// Quote's escaping — into forward slashes, so the golden reads the same
// regardless of which OS captured it. Both extra steps are the identity
// on POSIX, where there is no backslash for Quote to escape in the first
// place.
func normalizeGoldenPlan(g goldenPlan, home string) goldenPlan {
	out := replaceInGoldenPlan(g, home, "TESTHOME")
	if runtime.GOOS == "windows" {
		winHome := filepath.FromSlash(home)
		quoted := strconv.Quote(winHome)
		escapedWinHome := quoted[1 : len(quoted)-1] // strip the surrounding quotes Quote added
		out = replaceInGoldenPlan(out, escapedWinHome, "TESTHOME")
		out = replaceInGoldenPlan(out, `\\`, "/")
	}
	return out
}

// normalizeHermesHome replaces the resolved hermesHomeDir value with a
// stable placeholder, the same way normalizeGoldenPlan does for the Codex
// sandbox pair's temp directory: Hermes' home is OS-chosen by design
// (hermesHomeDir branches on runtime.GOOS, not on the fake ops' home), so
// the captured plan otherwise carries a real Windows path where a POSIX
// run captured a real POSIX one, and the golden — captured on one OS —
// fails to compare equal on the other for a reason that has nothing to do
// with behavior. hermesHome must already be slash-converted, matching how
// capturePlan slashes every path-shaped field it produces.
func normalizeHermesHome(g goldenPlan, hermesHome string) goldenPlan {
	return replaceInGoldenPlan(g, hermesHome, "<HERMES_HOME>")
}

func TestCodexSandboxPlanGolden(t *testing.T) {
	surface, ok := surfaceByID("codex")
	if !ok {
		t.Fatal("no agentSurface for codex")
	}

	t.Run("install", func(t *testing.T) {
		cfgPath, home := sandboxGoldenConfig(t)
		entry := binEntry{command: goldenBin, cfg: cfgPath}
		_, ops := newFakeMachine()
		paths := ops.paths(noEnv)
		plan := buildInstallPlan(ops, paths, []agentSurface{surface}, entry, noEnv)
		compareGoldenPlan(t, normalizeGoldenPlan(capturePlan(plan), home), filepath.Join("testdata", "agents", "codex-sandbox.install.golden"))
	})

	t.Run("uninstall", func(t *testing.T) {
		cfgPath, home := sandboxGoldenConfig(t)
		entry := binEntry{command: goldenBin, cfg: cfgPath}
		_, ops := newFakeMachine()
		paths := ops.paths(noEnv)
		installPlan := buildInstallPlan(ops, paths, []agentSurface{surface}, entry, noEnv)
		if failures := commitPlan(ops, &installPlan, io.Discard, io.Discard); failures != 0 {
			t.Fatalf("committing the sandboxed install: %d failures", failures)
		}
		plan := buildUninstallPlan(ops, paths, []agentSurface{surface}, entry)
		compareGoldenPlan(t, normalizeGoldenPlan(capturePlan(plan), home), filepath.Join("testdata", "agents", "codex-sandbox.uninstall.golden"))
	})
}

// ── status: exact states, table-driven ──────────────────────────────────

// statusOutputFor installs exactly one host into a fresh in-memory
// machine (or nothing, for the "absent" cases), optionally deletes some of
// what install wrote to produce a partial state, and returns agents
// status's full rendered output.
func statusOutputFor(t *testing.T, id string, install bool, remove func(agentPaths) []string) string {
	t.Helper()
	_, ops := newFakeMachine()
	paths := ops.paths(noEnv)
	entry := goldenEntry()
	if install {
		surface, ok := surfaceByID(id)
		if !ok {
			t.Fatalf("no agentSurface for %q", id)
		}
		plan := buildInstallPlan(ops, paths, []agentSurface{surface}, entry, noEnv)
		if failures := commitPlan(ops, &plan, io.Discard, io.Discard); failures != 0 {
			t.Fatalf("install %s: %d failures", id, failures)
		}
		if remove != nil {
			for _, p := range remove(paths) {
				if _, err := ops.readFile(p); err != nil {
					t.Fatalf("install did not write %s, so removing it proves nothing about a partial state", p)
				}
				_ = ops.removeAll(p)
			}
		}
	}
	var out bytes.Buffer
	printAgentStatus(ops, paths, entry, nil, &out)
	return out.String()
}

// exactHostStatusLine returns the single status line for label, in the
// exact bytes printAgentStatus produced ("  " + a 12-wide label field + a
// space + a 12-wide found field + a space + the state, per its
// "  %-12s %-12s %s\n" format). It fails if there is not exactly one such
// line, so a duplicated row is itself a failure, not just a mismatched one.
func exactHostStatusLine(t *testing.T, out, label string) string {
	t.Helper()
	const labelField = 12
	var matches []string
	for _, line := range strings.Split(out, "\n") {
		if len(line) < 2+labelField {
			continue
		}
		if strings.TrimRight(line[2:2+labelField], " ") == label {
			matches = append(matches, line)
		}
	}
	if len(matches) != 1 {
		t.Fatalf("status has %d line(s) naming %q (want exactly 1):\n%s", len(matches), label, out)
	}
	return matches[0]
}

// TestAgentStatusExactStates enumerates, per host, absent, fully
// installed, and every partial state printAgentStatus's own switch
// currently distinguishes (Claude and Cursor: skill without hooks; Pi and
// Hermes: either half alone). A state the switch does not name — such as
// Claude's hooks surviving with no skill file — is not characterized here
// because the current code does not distinguish it either; it prints
// "not installed" the same as true absence. The comparison is exact
// equality on the extracted line, not a substring match, so one changed
// byte or a second line for the same host both fail it.
func TestAgentStatusExactStates(t *testing.T) {
	requireGoldenSequence(t)
	for _, tc := range []struct {
		id, label, name string
		install         bool
		remove          func(agentPaths) []string
		want            string
	}{
		{"claude", "Claude Code", "absent", false, nil, "not installed"},
		{"claude", "Claude Code", "skill only", true, func(p agentPaths) []string { return []string{p.claudeSettings} }, "installed (skill only)"},
		{"claude", "Claude Code", "skill+hooks", true, nil, "installed (skill+hooks)"},

		{"codex", "Codex", "absent", false, nil, "not installed"},
		{"codex", "Codex", "installed", true, nil, "installed (skill)"},

		{"cursor", "Cursor", "absent", false, nil, "not installed"},
		{"cursor", "Cursor", "skill only", true, func(p agentPaths) []string { return []string{p.cursorHooks} }, "installed (skill only)"},
		{"cursor", "Cursor", "skill+hooks", true, nil, "installed (skill+hooks)"},

		{"opencode", "opencode", "absent", false, nil, "not installed"},
		{"opencode", "opencode", "installed", true, nil, "installed (plugin)"},

		{"pi", "Pi", "absent", false, nil, "not installed"},
		{"pi", "Pi", "skill only", true, func(p agentPaths) []string { return []string{p.piExtension} }, "installed (skill only)"},
		{"pi", "Pi", "extension only", true, func(p agentPaths) []string { return []string{p.piSkill} }, "installed (extension only)"},
		{"pi", "Pi", "skill+extension", true, nil, "installed (skill+extension)"},

		{"hermes", "Hermes", "absent", false, nil, "not installed"},
		{"hermes", "Hermes", "skill only", true, func(p agentPaths) []string { return []string{p.hermesConfig} }, "installed (skill only)"},
		{"hermes", "Hermes", "hook only", true, func(p agentPaths) []string { return []string{p.hermesSkill} }, "installed (hook only)"},
		{"hermes", "Hermes", "skill+hook", true, nil, "installed (skill+hook)"},
	} {
		t.Run(tc.id+"/"+tc.name, func(t *testing.T) {
			out := statusOutputFor(t, tc.id, tc.install, tc.remove)
			got := exactHostStatusLine(t, out, tc.label)
			want := fmt.Sprintf("  %-12s %-12s %s", tc.label, "not on PATH", tc.want)
			if got != want {
				t.Errorf("status line for %s:\n got  %q\n want %q", tc.label, got, want)
			}
		})
	}
}

// ── selection semantics ──────────────────────────────────────────────────
//
// selectSurfaces' rule is frozen exactly as it stands: it is what A.2
// calls "selection semantics" for the registry, and PR B's setup -with
// builds its own resolver on the same foundation, so every corner of the
// current behavior — not just the headline no-client/explicit-client
// split — has to be pinned before either resolver replaces it.

// TestSelectSurfacesSemantics pins the headline rule: no -client means the
// detected hosts and nothing more; an explicit -client selects that host
// regardless of detection, because naming a host by id is a stronger
// signal than a PATH probe.
func TestSelectSurfacesSemantics(t *testing.T) {
	_, ops := newFakeMachine("claude")

	selected, detected, err := selectSurfaces(ops, nil)
	if err != nil {
		t.Fatalf("selectSurfaces(nil): %v", err)
	}
	if len(detected) != 1 || detected[0].id != "claude" {
		t.Fatalf("detected = %v, want just claude", detected)
	}
	if len(selected) != 1 || selected[0].id != "claude" {
		t.Fatalf("no -client should select exactly what was detected: %v", selected)
	}

	selected, _, err = selectSurfaces(ops, []string{"codex"})
	if err != nil {
		t.Fatalf("selectSurfaces([codex]): %v", err)
	}
	if len(selected) != 1 || selected[0].id != "codex" {
		t.Fatalf("-client codex must select codex even though it is not on PATH: %v", selected)
	}
}

// TestSelectSurfacesTrimsAndLowercasesAnExplicitID pins the normalization
// applied to each -client value before it is looked up.
func TestSelectSurfacesTrimsAndLowercasesAnExplicitID(t *testing.T) {
	_, ops := newFakeMachine()
	selected, _, err := selectSurfaces(ops, []string{" CoDeX "})
	if err != nil {
		t.Fatalf("selectSurfaces([\" CoDeX \"]): %v", err)
	}
	if len(selected) != 1 || selected[0].id != "codex" {
		t.Fatalf("whitespace and case should be normalized away: %v", selected)
	}
}

// TestSelectSurfacesPreservesExplicitArgumentOrder pins that -client
// values come back in the order given, not registry order: an argument
// list is a request, and reordering it silently would be a second,
// undocumented sort the caller never asked for.
func TestSelectSurfacesPreservesExplicitArgumentOrder(t *testing.T) {
	_, ops := newFakeMachine()
	selected, _, err := selectSurfaces(ops, []string{"hermes", "claude"})
	if err != nil {
		t.Fatalf("selectSurfaces([hermes, claude]): %v", err)
	}
	if len(selected) != 2 || selected[0].id != "hermes" || selected[1].id != "claude" {
		t.Fatalf("selected = %v, want [hermes claude] in that order", selected)
	}
}

// TestSelectSurfacesUnknownIDErrorTextIsExact pins the exact wording an
// unknown -client produces, ordered id list included, because PR B's
// setup -with inherits this resolver's shape and its own error text
// depends on this one being frozen first.
func TestSelectSurfacesUnknownIDErrorTextIsExact(t *testing.T) {
	_, ops := newFakeMachine()
	_, _, err := selectSurfaces(ops, []string{"nonesuch"})
	if err == nil {
		t.Fatal("selectSurfaces([nonesuch]): want an error")
	}
	want := fmt.Sprintf("unknown -client %q (%s)", "nonesuch", surfaceIDs())
	if err.Error() != want {
		t.Errorf("error text:\n got  %q\n want %q", err.Error(), want)
	}
}

// TestSelectSurfacesRepeatedIDIsNotDeduplicated pins exactly what the
// current code does with a repeated -client: nothing removes the
// duplicate, so it appears twice in the result. This is not asserted as
// correct or desirable, only as the current behavior the refactor must not
// silently change.
func TestSelectSurfacesRepeatedIDIsNotDeduplicated(t *testing.T) {
	_, ops := newFakeMachine()
	selected, _, err := selectSurfaces(ops, []string{"claude", "claude"})
	if err != nil {
		t.Fatalf("selectSurfaces([claude, claude]): %v", err)
	}
	if len(selected) != 2 || selected[0].id != "claude" || selected[1].id != "claude" {
		t.Fatalf("selected = %v, want [claude claude] (no dedup)", selected)
	}
}

// ── ordered sequence, asserted everywhere it is rendered ────────────────

// TestHostSequenceAndLabelsAreOrderedLiterally is A.2's "order is part of
// the contract", pinned against the rendered output of three different
// commands so a reorder cannot pass by accident in one of them.
func TestHostSequenceAndLabelsAreOrderedLiterally(t *testing.T) {
	requireGoldenSequence(t)

	_, ops := newFakeMachine(goldenHostIDs...)
	code, out, _ := runAgents(t, ops, nil, "install", "-config", testCfg, "-dry-run")
	if code != exitOK {
		t.Fatalf("install -dry-run: exit %d\n%s", code, out)
	}
	if !strings.Contains(out, "agents: "+strings.Join(goldenHostLabels, ", ")) {
		t.Fatalf("the detected-agents line is not the registry order %v:\n%s", goldenHostLabels, out)
	}

	_, out, _ = runAgents(t, ops, nil, "status", "-config", testCfg)
	last := -1
	for _, label := range goldenHostLabels {
		idx := strings.Index(out, "  "+label+" ")
		if idx < 0 {
			t.Fatalf("status output is missing %q:\n%s", label, out)
		}
		if idx < last {
			t.Fatalf("status lists %q out of registry order:\n%s", label, out)
		}
		last = idx
	}
}

// ── agents prefer, across all six hosts ──────────────────────────────────

// TestAgentsPreferAcrossAllSixHosts is the baseline the preferenceTarget
// capability replaces: with all six hosts installed, `prefer off` rewrites
// the five skill-bearing hosts to the off rendering (Hermes' approval note
// survives the rewrite, since it is spliced back in every time), leaves
// the opencode plugin byte-identical (it carries no preference text and
// prefer must not touch it), and invents no opencode skill. `prefer on`
// returns the five to the on rendering.
func TestAgentsPreferAcrossAllSixHosts(t *testing.T) {
	requireGoldenSequence(t)
	m, ops := newFakeMachine(goldenHostIDs...)
	if code, out, _ := runAgents(t, ops, nil, "install", "-config", testCfg, "-yes"); code != exitOK {
		t.Fatalf("install: %d\n%s", code, out)
	}
	paths := ops.paths(noEnv)
	skillPaths := map[string]string{
		"claude": paths.claudeSkill,
		"codex":  paths.codexSkill,
		"cursor": paths.cursorSkill,
		"pi":     paths.piSkill,
		"hermes": paths.hermesSkill,
	}
	opencodeBefore := append([]byte(nil), m.files[slash(paths.opencodePlugin)]...)
	if len(opencodeBefore) == 0 {
		t.Fatal("install did not write the opencode plugin, so this test proves nothing about it")
	}

	if code, out, errOut := runAgents(t, ops, nil, "prefer", "off", "-config", testCfg); code != exitOK {
		t.Fatalf("prefer off: %d\n%s%s", code, out, errOut)
	}
	for id, p := range skillPaths {
		skill := string(m.files[slash(p)])
		if !strings.Contains(skill, "turned OFF as the default") || strings.Contains(skill, "Prefer it over a built-in web search") {
			t.Errorf("%s skill was not rewritten to the off rendering:\n%s", id, skill)
		}
	}
	if hermes := string(m.files[slash(paths.hermesSkill)]); !containsFlat(hermes, "one-time approval prompt for the installed DropinMiner hook") {
		t.Errorf("Hermes lost its approval note when prefer off rewrote its skill:\n%s", hermes)
	}
	if got := m.files[slash(paths.opencodePlugin)]; !bytes.Equal(got, opencodeBefore) {
		t.Error("prefer off touched the opencode plugin, which carries no preference text")
	}
	for p := range m.files {
		if strings.Contains(p, "opencode") && p != slash(paths.opencodePlugin) {
			t.Errorf("prefer off invented an opencode file: %s", p)
		}
	}

	if code, out, _ := runAgents(t, ops, nil, "prefer", "on", "-config", testCfg); code != exitOK {
		t.Fatalf("prefer on: %d\n%s", code, out)
	}
	for id, p := range skillPaths {
		skill := string(m.files[slash(p)])
		if strings.Contains(skill, "turned OFF as the default") || !strings.Contains(skill, "Prefer it over a built-in web search") {
			t.Errorf("%s skill was not returned to the on rendering:\n%s", id, skill)
		}
	}
	if hermes := string(m.files[slash(paths.hermesSkill)]); !containsFlat(hermes, "one-time approval prompt for the installed DropinMiner hook") {
		t.Errorf("Hermes lost its approval note when prefer on rewrote its skill:\n%s", hermes)
	}
	if got := m.files[slash(paths.opencodePlugin)]; !bytes.Equal(got, opencodeBefore) {
		t.Error("prefer on touched the opencode plugin")
	}
}

// ── the rendered help, byte for byte ─────────────────────────────────────

// TestHelpTextGolden captures usageText — exactly what the `help` command
// prints — before it becomes a template rendered from the registry. A.4
// requires the post-refactor render to reproduce this byte for byte.
func TestHelpTextGolden(t *testing.T) {
	path := filepath.Join("testdata", "help", "usage.golden")
	want, err := os.ReadFile(path) // #nosec G304 -- a fixed testdata path this file builds, not an external one
	if err != nil {
		t.Fatalf("reading golden %s: %v", path, err)
	}
	if usageText != string(want) {
		t.Errorf("usageText differs from %s:\n--- got ---\n%s\n--- want ---\n%s", path, usageText, want)
	}
}

// TestAgentsUsageGolden captures agentsUsage byte for byte: commit 2
// changes how its -client list is derived (from targetIDs(targetHost)
// instead of surfaceIDs()), and this is the baseline that derivation must
// still reproduce exactly.
func TestAgentsUsageGolden(t *testing.T) {
	path := filepath.Join("testdata", "help", "agents-usage.golden")
	want, err := os.ReadFile(path) // #nosec G304 -- a fixed testdata path this file builds, not an external one
	if err != nil {
		t.Fatalf("reading golden %s: %v", path, err)
	}
	if agentsUsage != string(want) {
		t.Errorf("agentsUsage differs from %s:\n--- got ---\n%s\n--- want ---\n%s", path, agentsUsage, want)
	}
}
