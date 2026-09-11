package main

// Pi and Hermes: two more skill-only agents. Both consume the same SKILL.md
// dropin already renders, so install is a skill write, status reads it back,
// and uninstall removes exactly it. Hermes' skills dir honors HERMES_HOME.
// All identifiers are synthetic.

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The JavaScript hosts cannot call isSearchCommand, so the shared trace
// source carries a copy of the recognizer's pattern. This guard fails if that
// copy ever drifts from the Go searchCommandRe — the one recognizer the Hermes
// hook, the Cursor hook and Claude Code all go through — so no adapter can fall
// back to a looser (e.g. substring) match without CI noticing. It is checked on
// the shared source because that is now the only place the pattern exists.
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

	// Status reports both installed.
	if _, out, _ := runAgents(t, ops, nil, "status", "-config", testCfg); !strings.Contains(out, "Pi") || !strings.Contains(out, "Hermes") {
		t.Errorf("status omits Pi/Hermes:\n%s", out)
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
