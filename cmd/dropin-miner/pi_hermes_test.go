package main

// Pi and Hermes: two more skill-only agents. Both consume the same SKILL.md
// dropin already renders, so install is a skill write, status reads it back,
// and uninstall removes exactly it. Hermes' skills dir honors HERMES_HOME.
// All identifiers are synthetic.

import (
	"strings"
	"testing"
)

const (
	piSkillPath     = "/home/u/.pi/agent/skills/dropin-miner/SKILL.md"
	hermesSkillPath = "/home/u/.hermes/skills/dropin-miner/SKILL.md"
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
		// It teaches the CLI-as-tool invocation, pointed at this config.
		if !strings.Contains(s, "search") || !strings.Contains(s, testCfg) {
			t.Errorf("%s: skill does not name the search command with the config", p)
		}
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
	for _, p := range []string{piSkillPath, hermesSkillPath} {
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
