package main

// Who is calling a Claude-format hook (#87). Cursor loads Claude Code's hooks
// from ~/.claude/settings.json and runs them with its own payload; the three
// payloads under testdata/hook/cursor-3.20.21-*.json are what it sent, taken
// from Cursor's own hook log and scrubbed. They are inputs, never edited on
// disk: a case that needs a different payload derives it here, in the open,
// by naming the keys it removes or sets.
//
// Everything drives the real hookMain — the decision lives there, and the
// test-local hookMainWith shadow in hook_test.go never sees it.

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
)

// cursorOnlyKeys are the keys every Cursor payload carries and Claude Code's
// never do (#87's measurement). The control removes exactly these.
var cursorOnlyKeys = []string{"cursor_version", "conversation_id", "generation_id"}

type claudeEntryCase struct {
	name    string
	fixture string   // the real Cursor payload that reached this entry point
	args    []string // the entry point, as Claude Code's settings name it
	event   string   // the event Claude Code reports for it
}

var claudeEntryCases = []claudeEntryCase{
	{"lineage", "cursor-3.20.21-preToolUse-runs-claude-lineage.json", []string{"lineage"}, "PreToolUse"},
	{"window session-start", "cursor-3.20.21-sessionStart-runs-claude-window-session-start.json", []string{"window", "session-start"}, "SessionStart"},
	{"flush", "cursor-3.20.21-stop-runs-claude-flush.json", []string{"flush"}, "Stop"},
}

func cursorPayloadFixture(t *testing.T, name string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(readHookFixture(t, name), &m); err != nil {
		t.Fatalf("%s is not a JSON object: %v", name, err)
	}
	for _, k := range append([]string{"hook_event_name"}, cursorOnlyKeys...) {
		if _, ok := m[k]; !ok {
			t.Fatalf("%s carries no %q: it is not the payload #87 measured, and a test run against it proves nothing", name, k)
		}
	}
	return m
}

// asClaudeCode is the control's derivation: the Cursor-only keys removed and
// the event named the way Claude Code names it. For the lineage payload it
// also names a tool Claude Code has and a working directory, because the
// payload in hand has neither (`tool_name: "Shell"`, an empty-window `cwd`)
// and without them the hook is silent for reasons that have nothing to do
// with who is calling — which is the accident this commit stops relying on.
func asClaudeCode(m map[string]any, event string) map[string]any {
	out := map[string]any{}
	for k, v := range m {
		out[k] = v
	}
	for _, k := range cursorOnlyKeys {
		delete(out, k)
	}
	out["hook_event_name"] = event
	if event == "PreToolUse" {
		out["tool_name"] = "Bash"
		out["cwd"] = "/home/u/project"
	}
	return out
}

type hookRun struct {
	stdout  string
	files   []string
	flushes int
}

func runRealHookMain(t *testing.T, fs *fakeHookFS, ops hookOps, cfgPath string, args []string, payload map[string]any) hookRun {
	t.Helper()
	var stdout, stderr bytes.Buffer
	full := append([]string{"-config", cfgPath}, args...)
	if code := hookMain(ops, full, bytes.NewReader(mustJSON(t, payload)), &stdout, &stderr); code != exitOK {
		t.Fatalf("hook %v exited %d (stderr %q); a hook never fails its host", args, code, stderr.String())
	}
	files := keys(fs.files)
	sort.Strings(files)
	return hookRun{stdout: stdout.String(), files: files, flushes: len(fs.flushes)}
}

func (r hookRun) nothing() bool { return r.stdout == "" && len(r.files) == 0 && r.flushes == 0 }

// The three payloads Cursor really sent, each into the entry point Cursor
// really ran it against: no output, no file, no flush.
func TestClaudeFormatHooksStandDownForTheRealCursorPayloads(t *testing.T) {
	cfg := writeHookConfig(t)
	for _, c := range claudeEntryCases {
		t.Run(c.name, func(t *testing.T) {
			fs, ops := newFakeHookOps(nil)
			got := runRealHookMain(t, fs, ops, cfg, c.args, cursorPayloadFixture(t, c.fixture))
			if !got.nothing() {
				t.Fatalf("a Claude Code hook run by Cursor did something: stdout=%q files=%v flushes=%d", got.stdout, got.files, got.flushes)
			}
		})
	}
}

// The control. Without it a hook that does nothing at all passes the test
// above. The same payloads, made Claude Code's, do what they do today: the
// lineage hook rewrites the command and writes the lineage file, the
// session-start hook seeds the window and starts a flush, the stop hook
// starts a flush.
func TestTheSamePayloadsFromClaudeCodeBehaveAsTheyDoToday(t *testing.T) {
	cfg := writeHookConfig(t)
	for _, c := range claudeEntryCases {
		t.Run(c.name, func(t *testing.T) {
			fs, ops := newFakeHookOps(nil)
			got := runRealHookMain(t, fs, ops, cfg, c.args, asClaudeCode(cursorPayloadFixture(t, c.fixture), c.event))
			switch c.event {
			case "PreToolUse":
				var resp struct {
					Out struct {
						Input map[string]any `json:"updatedInput"`
					} `json:"hookSpecificOutput"`
				}
				if err := json.Unmarshal([]byte(got.stdout), &resp); err != nil {
					t.Fatalf("Claude Code's own PreToolUse got no rewrite: %q", got.stdout)
				}
				cmd, _ := resp.Out.Input["command"].(string)
				if env := decodeBridgeFromCommand(t, cmd); env.Harness != "claude-code" {
					t.Errorf("harness %q, want claude-code", env.Harness)
				}
				if want := []string{lineagePath("/sessions", "/home/u/project")}; !slices.Equal(got.files, want) {
					t.Errorf("files %v, want the lineage file %v", got.files, want)
				}
				if got.flushes != 0 {
					t.Errorf("the lineage hook started %d flushes", got.flushes)
				}
			case "SessionStart":
				if want := []string{filepath.Join("/sessions", hookStateFile)}; !slices.Equal(got.files, want) {
					t.Errorf("files %v, want the window state %v", got.files, want)
				}
				if got.flushes != 1 || got.stdout != "" {
					t.Errorf("session-start: flushes=%d stdout=%q, want one flush and no output", got.flushes, got.stdout)
				}
			case "Stop":
				if got.flushes != 1 || got.stdout != "" || len(got.files) != 0 {
					t.Errorf("stop: flushes=%d stdout=%q files=%v, want exactly one flush", got.flushes, got.stdout, got.files)
				}
			}
		})
	}
}

// #87's cost, stated as the thing a participant pays: with both hosts
// installed Cursor runs OUR Cursor hook and OUR Claude Code hook at the same
// event, with the same payload. One turn end is one flush, and one session
// start is one flush.
func TestOneCursorEventStartsOneFlushWithBothHostsInstalled(t *testing.T) {
	cfg := writeHookConfig(t)
	for _, c := range []struct {
		name, fixture  string
		cursor, claude []string
	}{
		{"stop", "cursor-3.20.21-stop-runs-claude-flush.json", []string{"cursor", "stop"}, []string{"flush"}},
		{"sessionStart", "cursor-3.20.21-sessionStart-runs-claude-window-session-start.json", []string{"cursor", "sessionStart"}, []string{"window", "session-start"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			fs, ops := newFakeHookOps(nil)
			payload := cursorPayloadFixture(t, c.fixture)
			runRealHookMain(t, fs, ops, cfg, c.cursor, payload)
			if len(fs.flushes) != 1 {
				t.Fatalf("Cursor's own hook started %d flushes, want 1: the control is broken", len(fs.flushes))
			}
			got := runRealHookMain(t, fs, ops, cfg, c.claude, payload)
			if got.flushes != 1 {
				t.Fatalf("one Cursor %s started %d flushes; the Claude Code hook flushed for a caller that is not Claude Code", c.name, got.flushes)
			}
			if got.stdout != "" {
				t.Errorf("the Claude Code hook answered Cursor: %q", got.stdout)
			}
		})
	}
}

// H2 made the lineage hook silent for `tool_name: "Shell"` — by accident of
// the tool-name rule, not because it knew who was calling. Absent or "Bash",
// the same Cursor payload was still rewritten with a claude-code bridge,
// which is how #91's search reached the router under the wrong harness. The
// stand-down must not depend on the tool name, the working directory, or
// anything else the lineage hook happens to look at.
func TestLineageStandsDownForCursorWhateverTheToolName(t *testing.T) {
	cfg := writeHookConfig(t)
	for _, c := range []struct {
		name string
		edit func(map[string]any)
	}{
		{"tool_name absent", func(m map[string]any) { delete(m, "tool_name") }},
		{"tool_name Bash", func(m map[string]any) { m["tool_name"] = "Bash" }},
		{"tool_name PowerShell", func(m map[string]any) { m["tool_name"] = "PowerShell" }},
		{"tool_name absent, a working directory", func(m map[string]any) { delete(m, "tool_name"); m["cwd"] = "/home/u/project" }},
	} {
		t.Run(c.name, func(t *testing.T) {
			fs, ops := newFakeHookOps(nil)
			payload := cursorPayloadFixture(t, "cursor-3.20.21-preToolUse-runs-claude-lineage.json")
			c.edit(payload)
			if got := runRealHookMain(t, fs, ops, cfg, []string{"lineage"}, payload); !got.nothing() {
				t.Fatalf("Cursor's command was rewritten by the Claude Code hook: stdout=%q files=%v", got.stdout, got.files)
			}
		})
	}
}

// Each signal on its own, and the absence of both. The last two rows are the
// decision about callers nothing is known of: a payload that names no event
// (Copilot CLI's shape) or names Claude Code's own (Codex's) is served as
// before — asserted, so that it is a decision and not an accident.
func TestWhatCountsAsEvidenceOfAnotherHost(t *testing.T) {
	cfg := writeHookConfig(t)
	for _, c := range []struct {
		name      string
		edit      func(map[string]any)
		standDown bool
	}{
		{"cursor_version alone, Claude Code's event name", func(m map[string]any) { m["cursor_version"] = "3.20.21" }, true},
		{"cursor_version empty but present", func(m map[string]any) { m["cursor_version"] = "" }, true},
		{"a translated event name alone", func(m map[string]any) { m["hook_event_name"] = "stop" }, true},
		{"another host's own event name", func(m map[string]any) { m["hook_event_name"] = "AfterAgent" }, true},
		{"an event name that is not a string", func(m map[string]any) { m["hook_event_name"] = 7 }, true},
		// The rule is "the event this entry point is installed under", so an
		// entry somebody re-wired by hand under another Claude Code event
		// stands down too. Nothing installs that, and what it costs is one
		// flush that the next search starts anyway.
		{"a Claude Code event this entry point is not installed under", func(m map[string]any) { m["hook_event_name"] = "PreToolUse" }, true},
		{"no event name at all", func(m map[string]any) { delete(m, "hook_event_name") }, false},
		{"Claude Code's event name", func(map[string]any) {}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			fs, ops := newFakeHookOps(nil)
			payload := asClaudeCode(cursorPayloadFixture(t, "cursor-3.20.21-stop-runs-claude-flush.json"), "Stop")
			c.edit(payload)
			got := runRealHookMain(t, fs, ops, cfg, []string{"flush"}, payload)
			if stoodDown := got.flushes == 0; stoodDown != c.standDown {
				t.Fatalf("flushes=%d, stood down=%v, want %v", got.flushes, stoodDown, c.standDown)
			}
		})
	}
	// Not JSON at all: no evidence of anything, and the flush still runs.
	fs, ops := newFakeHookOps(nil)
	var stdout, stderr bytes.Buffer
	hookMain(ops, []string{"-config", cfg, "flush"}, strings.NewReader("not json"), &stdout, &stderr)
	if len(fs.flushes) != 1 {
		t.Errorf("an unparseable payload stopped the flush: %d", len(fs.flushes))
	}
}

// The two compaction entry points have no Cursor payload in hand — Cursor's
// log records it skipping PostCompact as unknown, and no compaction happened
// in the measured session. They are covered by derivation from the
// sessionStart payload, and this test says so rather than pretending to a
// measurement: same keys, the event name Cursor's loader would report.
func TestCompactionHooksStandDownForADerivedCursorPayload(t *testing.T) {
	cfg := writeHookConfig(t)
	for _, c := range []struct {
		phase, cursorEvent, claudeEvent string
	}{
		{"pre-compact", "preCompact", "PreCompact"},
		{"post-compact", "postCompact", "PostCompact"},
	} {
		t.Run(c.phase, func(t *testing.T) {
			payload := cursorPayloadFixture(t, "cursor-3.20.21-sessionStart-runs-claude-window-session-start.json")
			payload["hook_event_name"] = c.cursorEvent
			fs, ops := newFakeHookOps(nil)
			if got := runRealHookMain(t, fs, ops, cfg, []string{"window", c.phase}, payload); !got.nothing() {
				t.Fatalf("stdout=%q files=%v flushes=%d", got.stdout, got.files, got.flushes)
			}
			fs, ops = newFakeHookOps(nil)
			got := runRealHookMain(t, fs, ops, cfg, []string{"window", c.phase}, asClaudeCode(payload, c.claudeEvent))
			if want := []string{filepath.Join("/sessions", hookStateFile)}; !slices.Equal(got.files, want) {
				t.Fatalf("Claude Code's own %s wrote %v, want %v", c.phase, got.files, want)
			}
		})
	}
}

// Cursor's own hooks are untouched: the same payloads into `hook cursor …`
// still answer the host.
func TestCursorsOwnHooksAreNotGated(t *testing.T) {
	cfg := writeHookConfig(t)
	fs, ops := newFakeHookOps(nil)
	got := runRealHookMain(t, fs, ops, cfg, []string{"cursor", "sessionStart"}, cursorPayloadFixture(t, "cursor-3.20.21-sessionStart-runs-claude-window-session-start.json"))
	if !strings.Contains(got.stdout, `"TOKENDROP_HARNESS":"cursor"`) || got.flushes != 1 {
		t.Fatalf("Cursor's sessionStart: stdout=%q flushes=%d", got.stdout, got.flushes)
	}
	if _, gated := claudeEntryEvent([]string{"cursor", "stop"}); gated {
		t.Error("a Cursor entry point is treated as Claude-format")
	}
	if _, gated := claudeEntryEvent([]string{"hermes", "pre_tool_call"}); gated {
		t.Error("a Hermes entry point is treated as Claude-format")
	}
}

// claudeEntryEvent is a second statement of what claudeHooks installs. Held
// to it here by reading the installer's own output: every event the install
// writes runs an entry point this table maps back to exactly that event, and
// the table names no entry point the install does not write.
func TestTheCallerGateNamesExactlyTheEventsTheInstallWrites(t *testing.T) {
	entry := binEntry{command: "/opt/x/dropin-miner", cfg: "/opt/x/tokendrop.toml"}
	spec, err := claudeHooks(entry, shellPOSIX)
	if err != nil {
		t.Fatal(err)
	}
	entryPoints := [][]string{{"lineage"}, {"window", "session-start"}, {"window", "pre-compact"}, {"window", "post-compact"}, {"flush"}}
	seen := map[string]bool{}
	for _, event := range spec.order {
		group := spec.entries[event]
		hooks, _ := group["hooks"].([]any)
		if len(hooks) != 1 {
			t.Fatalf("%s: %d hook commands, want 1", event, len(hooks))
		}
		installed, _ := hooks[0].(map[string]any)["command"].(string)
		matched := false
		for _, sub := range entryPoints {
			rendered, err := entry.hookCommandForShell(shellPOSIX, sub...)
			if err != nil {
				t.Fatal(err)
			}
			if rendered != installed {
				continue
			}
			matched = true
			seen[strings.Join(sub, " ")] = true
			if got, ok := claudeEntryEvent(sub); !ok || got != event {
				t.Errorf("the install runs `hook %s` at %s, but the gate expects %q (gated=%v): it would stand down for Claude Code itself", strings.Join(sub, " "), event, got, ok)
			}
		}
		if !matched {
			t.Errorf("the install writes %s → %q, an entry point this test does not know: decide whether the gate covers it", event, installed)
		}
	}
	for _, sub := range entryPoints {
		if !seen[strings.Join(sub, " ")] {
			t.Errorf("`hook %s` is gated as Claude-format but the install never writes it", strings.Join(sub, " "))
		}
	}
}
