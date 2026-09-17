package main

// The hook answers the permission question its own rewrite caused (#98).
//
// `agents install` writes permissions.allow PREFIX rules naming the binary
// first. The PreToolUse hook then rewrites the command to carry the trace
// bridge, so the command Claude Code evaluates no longer begins with the
// binary, no prefix rule matches, and the call falls through to the Bash
// safety heuristics — where the skill's own heredoc is refused as
// "Contains brace with quote character (expansion obfuscation)".
//
// Measured before this was relied on, because whether an allow decision
// survives beside updatedInput in one answer is undocumented: with
// permissions.defaultMode "default" and a PreToolUse hook that answers
// nothing, that heredoc is refused with exactly that message; the same call,
// with a hook answering permissionDecision "allow" AND updatedInput in one
// hookSpecificOutput object, runs, and the rewritten command is the one that
// executes. Both halves survive together.

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// newHookFixture is an installation laid out the way a participant's is: the
// binary is a real regular file named dropin-miner under .tokendrop/bin, and
// the config sits beside it.
//
// It cannot reuse newRecognizerFixture, whose binary is the test's own
// executable: that file is named dropin-miner.test, and isSearchCommand — the
// gate the hook applies before it looks at anything — matches on the binary
// being named dropin-miner. The recognizer's identity check only needs the
// named file to be the same file this process would call itself, which a real
// file plus an executable() that returns it satisfies exactly.
func newHookFixture(t *testing.T, sh shellKind, cfgName string) recognizerFixture {
	t.Helper()
	install := filepath.Join(t.TempDir(), installMarker)
	binDir := filepath.Join(install, "bin")
	if err := os.MkdirAll(binDir, 0o750); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(binDir, "dropin-miner")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	if err := os.WriteFile(bin, []byte("not really a binary"), 0o600); err != nil {
		t.Fatal(err)
	}
	return recognizerFixture{
		entry:  binEntry{command: bin, cfg: filepath.Join(install, cfgName)},
		shells: []shellKind{sh},
		exe:    func() (string, error) { return bin, nil },
	}
}

// hookAnswer is what the lineage hook printed for one payload, decoded.
type hookAnswer struct {
	raw     string
	Output  map[string]any
	command string
}

func (a hookAnswer) decision() string {
	d, _ := a.Output["permissionDecision"].(string)
	return d
}

func (a hookAnswer) rewritten() bool {
	return a.command != "" && strings.Contains(a.command, bridgeEnv)
}

// askHook runs the lineage hook over one Bash tool call and decodes whatever
// it printed. An empty answer is the hook staying out of it.
func askHook(t *testing.T, f recognizerFixture, tool, command string) hookAnswer {
	t.Helper()
	_, ops := newFakeHookOps(nil)
	ops.executable = f.exe
	payload, _ := json.Marshal(map[string]any{
		"session_id": "probe-session", "prompt_id": "probe-prompt", "tool_use_id": "probe-call",
		"tool_name": tool, "tool_input": map[string]any{"command": command},
	})
	var out bytes.Buffer
	hookLineage(ops, hookContext{cfgPath: f.entry.cfg}, payload, &out)
	got := hookAnswer{raw: out.String()}
	if strings.TrimSpace(got.raw) == "" {
		return got
	}
	var body struct {
		HookSpecificOutput map[string]any `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(out.Bytes(), &body); err != nil {
		t.Fatalf("the hook printed something that is not a hook answer: %v (%q)", err, got.raw)
	}
	got.Output = body.HookSpecificOutput
	if ui, ok := body.HookSpecificOutput["updatedInput"].(map[string]any); ok {
		got.command, _ = ui["command"].(string)
	}
	return got
}

// toolFor is the Claude Code tool whose shell is sh.
func toolFor(sh shellKind) string {
	if sh == shellPowerShell {
		return "PowerShell"
	}
	return "Bash"
}

// The rendered search, in each shell Claude Code runs on Windows, comes back
// carrying BOTH the allow decision and the rewritten command. Both in one
// answer is the part that was measured rather than assumed.
func TestTheRenderedSearchIsAllowedAndBridgedInOneAnswer(t *testing.T) {
	for _, sh := range []shellKind{shellPOSIX, shellPowerShell} {
		t.Run(string(sh), func(t *testing.T) {
			f := newHookFixture(t, sh, "tokendrop.toml")
			search := f.renderedSearch(t, sh, `{"version":1,"query":"probe query text"}`)
			got := askHook(t, f, toolFor(sh), search)

			if got.decision() != "allow" {
				t.Errorf("the search this skill renders is answered %q, want allow; without it every search prompts, or is refused outright in a headless session (#98)\nanswer: %s", got.decision(), got.raw)
			}
			if !got.rewritten() {
				t.Errorf("the command carries no trace bridge, so the search would run with no lineage\nanswer: %s", got.raw)
			}
			if _, ok := got.Output["permissionDecisionReason"].(string); !ok {
				t.Errorf("the allow carries no reason, so a participant reading the transcript cannot tell what allowed it\nanswer: %s", got.raw)
			}
		})
	}
}

// A command one byte different from the rendered form carries neither half.
// The allow is a permission answer, so it is exact: it is taken from the
// recognizer that rebuilds the string, never from isSearchCommand, which is
// a pattern and matches any spelling of any search.
func TestACommandThatIsNotTheRenderedSearchIsNeverAllowed(t *testing.T) {
	f := newHookFixture(t, shellPOSIX, "tokendrop.toml")
	rendered := f.renderedSearch(t, shellPOSIX, `{"version":1,"query":"probe query text"}`)

	cases := []struct {
		name    string
		command string
	}{
		{"a second statement after it", rendered + "; echo x"},
		{"one byte more outside the body", strings.Replace(rendered, "--stdin", "--stdin ", 1)},
		{"the heredoc delimiter left unquoted, so the shell would expand the body", strings.Replace(rendered, "<<'JSON'", "<<JSON", 1)},
		{"the human argv form, a search by any pattern", f.entry.searchCommand() + ` "probe query text"`},
		{"our binary, a different subcommand", f.entry.command + " search-nothing"},
		// The shell comes from the tool, so the form rendered for the other
		// shell is not the form this call will run (#77, H-R5).
		{"the PowerShell form arriving through the Bash tool", f.renderedSearch(t, shellPowerShell, `{"version":1,"query":"probe query text"}`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := askHook(t, f, "Bash", tc.command)
			if got.decision() != "" {
				t.Errorf("%q was answered %q; a permission answer is for the exact rendered form only\ncommand: %s\nanswer: %s", tc.name, got.decision(), tc.command, got.raw)
			}
		})
	}
}

// The search of a DIFFERENT installation — our binary, another installation's
// config — is not this hook's to allow, for the same reason uninstall will
// not remove its integrations (#73). Two installations share a binary
// whenever the second was made by running the first.
func TestAnotherInstallationsSearchIsNotAllowed(t *testing.T) {
	f := newHookFixture(t, shellPOSIX, "tokendrop.toml")
	// The SAME binary, another installation's config — which is exactly what
	// `setup -home <dir>` run from an existing installation produces.
	other := f
	other.entry.cfg = filepath.Join(filepath.Dir(f.entry.cfg), "other.toml")
	search := other.renderedSearch(t, shellPOSIX, `{"version":1,"query":"probe query text"}`)
	got := askHook(t, f, "Bash", search)
	if got.decision() != "" {
		t.Errorf("a search naming another installation's config was answered %q\nanswer: %s", got.decision(), got.raw)
	}
}

// A Bash command that is not a search at all is untouched: no answer, so no
// rewrite and no decision. The hook stays out of everything it did not cause.
func TestANonSearchCommandIsUntouched(t *testing.T) {
	f := newHookFixture(t, shellPOSIX, "tokendrop.toml")
	got := askHook(t, f, "Bash", "git status")
	if strings.TrimSpace(got.raw) != "" {
		t.Errorf("the hook answered for a command that is not ours: %s", got.raw)
	}
}
