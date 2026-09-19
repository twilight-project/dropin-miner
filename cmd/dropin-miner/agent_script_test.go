package main

// What the installer actually writes into a JavaScript adapter.
//
// The adapter is the only place the bridge syntax is decided. A plugin
// running inside opencode cannot ask the declaration — it is a JavaScript
// file, not this binary — so renderAgentScript splices the answer in at
// install time, and a wrong value there is #68 again on every traced
// search: a POSIX prefix handed to PowerShell is looked up as a program
// name and the search does not run at all.
//
// Nothing observed that until this file. The per-OS host golden recomputes
// the line from declaredShells in test code, and the install-plan golden
// replaces it with a placeholder, so an installer that rendered every
// adapter with one hardcoded shell would have passed both.

import (
	"regexp"
	"runtime"
	"testing"
)

// hostShellLineRe reads back the one line renderAgentScript splices in.
var hostShellLineRe = regexp.MustCompile(`(?m)^const HOST_SHELL = "([^"]*)"$`)

func hostShellInScript(t *testing.T, script []byte) string {
	t.Helper()
	m := hostShellLineRe.FindSubmatch(script)
	if m == nil {
		t.Fatalf("no HOST_SHELL line in the rendered adapter")
	}
	return string(m[1])
}

// powerShellDeclaringHost declares powershell for its tool calls on every
// OS. It exists so the assertion below has teeth on a POSIX runner too:
// opencode's real declaration IS posix on macOS and Linux, so an installer
// that ignored the declaration and hardcoded posix would agree with it
// there and be caught only on Windows.
type powerShellDeclaringHost struct{}

func (powerShellDeclaringHost) ID() string       { return "fake-powershell-host" }
func (powerShellDeclaringHost) Label() string    { return "Fake PowerShell Host" }
func (powerShellDeclaringHost) Kind() targetKind { return targetHost }
func (powerShellDeclaringHost) Detect(agentOps, agentPaths, func(string) string) string {
	return ""
}
func (powerShellDeclaringHost) PlanInstall(agentOps, agentPaths, binEntry, func(string) string, *agentPlan) {
}
func (powerShellDeclaringHost) PlanUninstall(agentOps, agentPaths, binEntry, func(string) string, *agentPlan) {
}
func (powerShellDeclaringHost) Status(agentOps, agentPaths, binEntry) targetStatus {
	return targetStatus{}
}
func (powerShellDeclaringHost) Shells(string) hostShells {
	return hostShells{tool: shellCell{
		evidence: evidenceRuled,
		shells:   []shellKind{shellPowerShell},
		source:   "a test fixture, so this assertion does not depend on the runner's own answer",
	}}
}

// TestTheInstallerWritesTheDeclaredShellIntoEveryJavaScriptAdapter reads the
// adapter bytes the INSTALL PLAN writes and requires the shell in them to be
// the one the host declares.
func TestTheInstallerWritesTheDeclaredShellIntoEveryJavaScriptAdapter(t *testing.T) {
	entry := goldenEntry()
	for _, tc := range []struct{ id, path string }{
		{"opencode", "/home/u/.config/opencode/plugins/dropin-miner.js"},
		{"pi", "/home/u/.pi/agent/extensions/dropin-miner.ts"},
	} {
		t.Run(tc.id, func(t *testing.T) {
			tg, ok := surfaceByID(tc.id)
			if !ok {
				t.Fatalf("no target %q", tc.id)
			}
			want, err := declaredShells(tg, runtime.GOOS, channelTool)
			if err != nil {
				t.Fatalf("%s declares no tool shell on %s: %v", tc.id, runtime.GOOS, err)
			}
			_, ops := newFakeMachine()
			plan := buildInstallPlan(ops, ops.paths(noEnv), []installTarget{tg}, entry, noEnv)
			var script []byte
			for _, w := range plan.writes {
				if slash(w.path) == tc.path {
					script = w.contents
				}
			}
			if script == nil {
				t.Fatalf("the plan wrote no adapter at %s", tc.path)
			}
			if got := hostShellInScript(t, script); got != string(want[0]) {
				t.Errorf("the installed adapter says %q, the declaration for %s says %q", got, runtime.GOOS, want[0])
			}
		})
	}

	// The same assertion where the answer cannot be the runner's own default.
	t.Run("a host that declares powershell gets powershell on any runner", func(t *testing.T) {
		_, ops := newFakeMachine()
		var p agentPlan
		if changed, _ := planAgentScript(ops, powerShellDeclaringHost{}, "/home/u/adapter.js", opencodePluginJS, "lineage plugin", goldenEntry(), &p); !changed {
			t.Fatalf("the plan refused a host that declares exactly one tool shell: %v", p.refused)
		}
		if len(p.writes) != 1 {
			t.Fatalf("writes: %d, want 1", len(p.writes))
		}
		if got := hostShellInScript(t, p.writes[0].contents); got != string(shellPowerShell) {
			t.Errorf("the installed adapter says %q, the declaration says %q", got, shellPowerShell)
		}
	})
}
