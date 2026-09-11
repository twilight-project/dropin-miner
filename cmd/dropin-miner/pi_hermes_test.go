package main

// Pi and Hermes as agent surfaces: what install writes, what status reports
// back, what `prefer` rewrites, and that uninstall removes exactly what was
// written. The lineage channels themselves are tested next door — the Pi
// extension in pi_extension_test.go (executed in Node), the Hermes hook and
// its config handling in hermes_hook_test.go and hermes_install_test.go.
// All identifiers are synthetic.

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The JavaScript hosts cannot call isSearchCommand, so the shared trace
// source carries a copy of the recognizer's pattern. This guard fails if
// that copy ever drifts from the Go searchCommandRe — the one recognizer
// the Hermes hook, the Cursor hook and Claude Code all go through — so no
// adapter can fall back to a looser (e.g. substring) match without CI
// noticing. It is checked on the shared source because that is now the only
// place the pattern exists: TestEveryJSHostRendersTheSharedTraceSource is
// what proves both adapters actually get it.
func TestPiExtensionRegexMatchesTheCanonicalRecognizer(t *testing.T) {
	m := regexp.MustCompile(`SEARCH_RE\s*=\s*/(.*?)/\n`).FindStringSubmatch(agentTraceCommonJS)
	if m == nil {
		t.Fatal("could not find the SEARCH_RE literal in agent_trace_common.js")
	}
	if got := m[1]; got != searchCommandRe.String() {
		t.Errorf("the shared JS recognizer drifted from searchCommandRe:\n  js: %s\n  go: %s", got, searchCommandRe.String())
	}
}

const (
	piSkillPath     = "/home/u/.pi/agent/skills/dropin-miner/SKILL.md"
	piExtensionPath = "/home/u/.pi/agent/extensions/dropin-miner.ts"
)

// Hermes' home is platform-dependent (%LOCALAPPDATA%\hermes on Windows,
// ~/.hermes elsewhere), so derive the expected paths the way the code does
// rather than hardcode a POSIX layout. The fake machine keys files by
// filepath.ToSlash of the written path, so slash() to match.
var (
	hermesTestHome   = hermesHomeDir("/home/u", func(string) string { return "" })
	hermesSkillPath  = slash(filepath.Join(hermesTestHome, "skills", agentsName, "SKILL.md"))
	hermesConfigPath = slash(filepath.Join(hermesTestHome, "config.yaml"))
)

func TestPiAndHermesInstallWriteSkillsAndUninstallRemovesThem(t *testing.T) {
	m, ops := newFakeMachine("pi", "hermes")

	if code, out, _ := runAgents(t, ops, nil, "install", "-config", testCfg, "-yes"); code != exitOK {
		t.Fatalf("install: %d\n%s", code, out)
	}
	for _, p := range []string{piSkillPath, hermesSkillPath} {
		b, ok := m.files[p]
		if !ok {
			t.Errorf("skill not written: %s", p)
			continue
		}
		s := string(b)
		if !strings.Contains(s, "name: dropin-miner") {
			t.Errorf("%s: not a dropin-miner skill:\n%s", p, s)
		}
		// It teaches the CLI-as-tool invocation, pointed at this config. The
		// config path is made absolute (drive-lettered on Windows), so match
		// the basename, not the literal testCfg.
		if !strings.Contains(s, "search") || !strings.Contains(s, "tokendrop.toml") {
			t.Errorf("%s: skill does not name the search command with the config", p)
		}
	}

	// Pi also gets the lineage extension, auto-discovered from its extensions dir.
	ext, ok := m.files[piExtensionPath]
	if !ok {
		t.Errorf("Pi lineage extension not written: %s", piExtensionPath)
	} else if s := string(ext); !strings.Contains(s, `harness: "pi"`) || !strings.Contains(s, "tokendrop-trace-v1|") || !strings.Contains(s, "TOKENDROP_TRACE_BRIDGE") {
		t.Errorf("Pi extension is not the lineage bridge:\n%s", s)
	}

	// Status reports both installed, and says which halves are present.
	_, out, _ := runAgents(t, ops, nil, "status", "-config", testCfg)
	for _, want := range []string{"Pi", "installed (skill+extension)", "Hermes", "installed (skill+hook)"} {
		if !strings.Contains(out, want) {
			t.Errorf("status missing %q:\n%s", want, out)
		}
	}

	// A second install is a no-op for both.
	if code, out, _ := runAgents(t, ops, nil, "install", "-config", testCfg, "-yes"); code != exitOK || !strings.Contains(out, "already installed") {
		t.Errorf("second install not idempotent: %d\n%s", code, out)
	}

	// Uninstall removes exactly the two skill dirs and nothing else.
	if code, _, _ := runAgents(t, ops, nil, "uninstall", "-config", testCfg, "-yes"); code != exitOK {
		t.Fatal("uninstall failed")
	}
	for _, p := range []string{piSkillPath, piExtensionPath, hermesSkillPath} {
		if _, ok := m.files[p]; ok {
			t.Errorf("still present after uninstall: %s", p)
		}
	}
}

// A half-installed host is a state worth naming: the skill alone runs the
// search but threads no lineage, and the lineage channel alone threads a
// trace for a search the agent has no reason to run. Reporting either as
// complete would answer "why is this not working" with "installed".
func TestPiAndHermesStatusDistinguishesEachHalf(t *testing.T) {
	// Each half state is produced by installing for real and then taking one
	// half away, rather than by hand-building the files. A fixture written
	// from the literal testCfg is not what this installation would ever have
	// written: resolveEntry makes the config path absolute — drive-lettered
	// on Windows — so the hand-built hook names a config that does not match,
	// status rightly declines to claim it, and the test fails on Windows for
	// a reason that has nothing to do with what it is checking.
	installed := func(t *testing.T, remove ...string) string {
		t.Helper()
		m, ops := newFakeMachine("pi", "hermes")
		if code, out, _ := runAgents(t, ops, nil, "install", "-config", testCfg, "-yes"); code != exitOK {
			t.Fatalf("install: %d\n%s", code, out)
		}
		for _, p := range remove {
			if _, ok := m.files[p]; !ok {
				t.Fatalf("install did not write %s, so removing it proves nothing", p)
			}
			delete(m.files, p)
		}
		_, out, _ := runAgents(t, ops, nil, "status", "-config", testCfg)
		return out
	}

	for _, tc := range []struct {
		name   string
		remove []string
		want   []string
	}{
		{"both halves", nil, []string{"installed (skill+extension)", "installed (skill+hook)"}},
		{"skill only", []string{piExtensionPath, hermesConfigPath}, []string{"installed (skill only)"}},
		{"channel only", []string{piSkillPath, hermesSkillPath}, []string{"installed (extension only)", "installed (hook only)"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := installed(t, tc.remove...)
			for _, want := range tc.want {
				if !strings.Contains(out, want) {
					t.Errorf("status missing %q:\n%s", want, out)
				}
			}
		})
	}

	t.Run("nothing", func(t *testing.T) {
		_, ops := newFakeMachine()
		_, out, _ := runAgents(t, ops, nil, "status", "-config", testCfg)
		for _, want := range []string{"Pi           not on PATH  not installed", "Hermes       not on PATH  not installed"} {
			if !strings.Contains(out, want) {
				t.Errorf("status missing %q:\n%s", want, out)
			}
		}
	})
}

// `agents prefer` has to reach every installed skill. A participant who
// turns the default off and finds one agent still preferring this search
// has been told something untrue by the command that printed "in effect
// now, and in every agent from its next start".
func TestAgentsPreferRewritesPiAndHermesSkills(t *testing.T) {
	m, ops := newFakeMachine("claude", "codex", "cursor", "pi", "hermes")
	if code, out, _ := runAgents(t, ops, nil, "install", "-config", testCfg, "-yes"); code != exitOK {
		t.Fatalf("install: %d\n%s", code, out)
	}
	if code, out, errOut := runAgents(t, ops, nil, "prefer", "off", "-config", testCfg); code != exitOK {
		t.Fatalf("prefer off: %d\n%s%s", code, out, errOut)
	}
	for _, p := range []string{piSkillPath, hermesSkillPath} {
		if s := string(m.files[p]); !strings.Contains(s, "turned OFF as the default") {
			t.Errorf("%s was not rewritten by `prefer off`:\n%s", p, s)
		}
	}
	if code, _, _ := runAgents(t, ops, nil, "prefer", "on", "-config", testCfg); code != exitOK {
		t.Fatal("prefer on failed")
	}
	for _, p := range []string{piSkillPath, hermesSkillPath} {
		if s := string(m.files[p]); strings.Contains(s, "turned OFF as the default") {
			t.Errorf("%s was not rewritten back by `prefer on`:\n%s", p, s)
		}
	}
}

// `prefer` writes only files that are already ours: an agent with no skill
// installed does not get one created behind the participant's back.
func TestAgentsPreferDoesNotCreateAMissingPiOrHermesSkill(t *testing.T) {
	m, ops := newFakeMachine("pi", "hermes")
	if code, _, _ := runAgents(t, ops, nil, "prefer", "off", "-config", testCfg); code != exitOK {
		t.Fatal("prefer off failed")
	}
	for _, p := range []string{piSkillPath, hermesSkillPath} {
		if _, ok := m.files[p]; ok {
			t.Errorf("prefer created a skill for an agent that had none: %s", p)
		}
	}
}

// Every implemented host appears in the -client guidance; a host that is
// implemented but undocumented is one nobody can ask for by name.
func TestClientGuidanceNamesEverySurface(t *testing.T) {
	_, ops := newFakeMachine()
	_, _, errOut := runAgents(t, ops, nil, "install", "-client", "nonesuch", "-config", testCfg)
	for _, s := range agentSurfaces {
		if !strings.Contains(errOut, s.id) {
			t.Errorf("the unknown -client error omits %q:\n%s", s.id, errOut)
		}
		if !strings.Contains(agentsUsage, s.id) {
			t.Errorf("agents usage omits -client %q", s.id)
		}
		if !strings.Contains(usageText, s.label) && !strings.Contains(usageText, s.id) {
			t.Errorf("dropin-miner help omits %q", s.label)
		}
	}
}

// Hermes installs on machines we do not control, so the skills dir must
// follow HERMES_HOME the way Hermes itself resolves it.
func TestHermesInstallHonorsHermesHome(t *testing.T) {
	m, ops := newFakeMachine("hermes")
	env := map[string]string{"HERMES_HOME": "/data/profiles/work"}

	if code, out, _ := runAgents(t, ops, env, "install", "-config", testCfg, "-yes"); code != exitOK {
		t.Fatalf("install: %d\n%s", code, out)
	}
	want := "/data/profiles/work/skills/dropin-miner/SKILL.md"
	if _, ok := m.files[want]; !ok {
		t.Errorf("HERMES_HOME not honored; skill not at %s\nfiles: %v", want, keysOf(m.files))
	}
	if _, ok := m.files[hermesSkillPath]; ok {
		t.Errorf("also wrote the default ~/.hermes path despite HERMES_HOME")
	}
}

// A single-client install touches only that agent.
func TestPiClientFlagInstallsOnlyPi(t *testing.T) {
	m, ops := newFakeMachine("pi", "hermes")
	if code, out, _ := runAgents(t, ops, nil, "install", "-client", "pi", "-config", testCfg, "-yes"); code != exitOK {
		t.Fatalf("install: %d\n%s", code, out)
	}
	if _, ok := m.files[piSkillPath]; !ok {
		t.Error("Pi skill not written")
	}
	if _, ok := m.files[hermesSkillPath]; ok {
		t.Error("Hermes was touched despite -client pi")
	}
}
