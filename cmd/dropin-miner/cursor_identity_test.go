package main

// Cursor's identity, carried on the command by its preToolUse hook (#118).
//
// Cursor applies a sessionStart hook's `env` to the session's later hooks and
// to nothing its agent's shell inherits — measured on macOS and Windows, and
// what its documentation says. So the preToolUse hook, which does receive
// those variables, writes them onto the one command that is ours, and
// beforeShellExecution must still recognize that command as ours.
//
// Every OS's forms are asserted on every runner: the shells are parameters
// of cursorPreToolUse and recognizeCursorCommand, and the expected strings
// are written out here rather than rebuilt with the code under test.

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// cursorPreToolUseFixture is the preToolUse INPUT Cursor sends for a Shell
// call. RECONSTRUCTED, not captured: the field set is the K-review capture
// from Cursor 3.20.21 (testdata/hook/cursor-3.20.21-preToolUse-runs-claude-
// lineage.json) with cursor_version set to 3.21.16 and hook_event_name kept
// as Cursor reports it. It is replaced by a real 3.21 capture when the
// Windows tester sends one.
const cursorPreToolUseFixture = "testdata/hook/cursor-3.21.16-preToolUse-shell.reconstructed.json"

// cursorPreToolUsePayload is the fixture with its command replaced.
func cursorPreToolUsePayload(t *testing.T, command string) []byte {
	t.Helper()
	raw, err := os.ReadFile(cursorPreToolUseFixture)
	if err != nil {
		t.Fatal(err)
	}
	var p map[string]any
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatal(err)
	}
	p["tool_input"].(map[string]any)["command"] = command
	return mustJSON(t, p)
}

// cursorIdentityEnv is what sessionStart exported for conversation conv.
func cursorIdentityEnv(sessionsDir, conv string) map[string]string {
	return map[string]string{
		"TOKENDROP_HARNESS": "cursor",
		lineageEnv:          lineagePath(sessionsDir, "/w/proj"),
		sessionEnv:          traceHash(conv),
	}
}

// renderedCursorSearch is the search the skill renders for sh, naming this
// test process's own executable so the identity half of the recognizer holds
// on every runner.
func renderedCursorSearch(t *testing.T, sh shellKind, cfg, body string) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	_, script, err := searchBlockForShell(sh, binEntry{command: exe, cfg: cfg}, body)
	if err != nil {
		t.Fatal(err)
	}
	return script
}

// expectedCursorCommand writes out, by hand, the command preToolUse must
// hand back: POSIX assignments in front, or PowerShell's after its encoding
// line. q is the shell's single-quoting, spelled here and not borrowed.
func expectedCursorCommand(t *testing.T, sh shellKind, env map[string]string, rendered string) string {
	t.Helper()
	switch sh {
	case shellPOSIX:
		q := func(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
		return "TOKENDROP_HARNESS='cursor' TOKENDROP_LINEAGE=" + q(env[lineageEnv]) + " TOKENDROP_SESSION=" + q(env[sessionEnv]) + " " + rendered
	case shellPowerShell:
		q := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
		head := "$OutputEncoding = [System.Text.UTF8Encoding]::new($false)\n"
		if !strings.HasPrefix(rendered, head) {
			t.Fatalf("the PowerShell search no longer begins with its encoding line:\n%s", rendered)
		}
		return head + "$env:TOKENDROP_HARNESS='cursor'; $env:TOKENDROP_LINEAGE=" + q(env[lineageEnv]) + "; $env:TOKENDROP_SESSION=" + q(env[sessionEnv]) + "; " + rendered[len(head):]
	}
	t.Fatalf("no expected form for %s", sh)
	return ""
}

// cursorAnswer is preToolUse's output, decoded.
type cursorAnswer struct {
	Permission   string                     `json:"permission"`
	UpdatedInput map[string]json.RawMessage `json:"updated_input"`
}

func runCursorPreToolUse(t *testing.T, env map[string]string, hc hookContext, payload []byte, shells, runners []shellKind) (string, string) {
	t.Helper()
	_, ops := newFakeHookOps(env)
	var out, errOut bytes.Buffer
	cursorPreToolUse(ops, hc, payload, shells, runners, &out, &errOut)
	return out.String(), errOut.String()
}

func cursorShellsFor(t *testing.T, goos string) (tool, hook []shellKind) {
	t.Helper()
	tool, err := declaredShells(cursorTarget{}, goos, channelTool)
	if err != nil {
		t.Fatalf("Cursor's tool shells on %s: %v", goos, err)
	}
	hook, err = declaredShells(cursorTarget{}, goos, channelHook)
	if err != nil {
		t.Fatalf("Cursor's hook runners on %s: %v", goos, err)
	}
	return tool, hook
}

// TestCursorPreToolUseCarriesTheIdentityOnEveryRenderedSearch: for every OS
// and every shell Cursor runs there, the skill's own search comes back as
// the same command with the three assignments in that shell's syntax, every
// other field of the tool input echoed as it came.
func TestCursorPreToolUseCarriesTheIdentityOnEveryRenderedSearch(t *testing.T) {
	hc := hookContext{cfgPath: "/c.toml", sessionsDir: "/sessions"}
	env := cursorIdentityEnv(hc.sessionsDir, "conv-1")
	for _, goos := range []string{"darwin", "linux", "windows"} {
		tool, runners := cursorShellsFor(t, goos)
		for _, sh := range tool {
			t.Run(goos+"/"+string(sh), func(t *testing.T) {
				rendered := renderedCursorSearch(t, sh, hc.cfgPath, `{"version":1,"query":"exact query text"}`)
				out, errOut := runCursorPreToolUse(t, env, hc, cursorPreToolUsePayload(t, rendered), tool, runners)
				var got cursorAnswer
				if err := json.Unmarshal([]byte(out), &got); err != nil {
					t.Fatalf("no answer for the rendered search (%v): %q stderr %q", err, out, errOut)
				}
				var command string
				_ = json.Unmarshal(got.UpdatedInput["command"], &command)
				if want := expectedCursorCommand(t, sh, env, rendered); command != want {
					t.Fatalf("updated command:\n got %q\nwant %q", command, want)
				}
				if got.Permission != "allow" {
					t.Errorf("permission %q, want allow", got.Permission)
				}
				if string(got.UpdatedInput["cwd"]) != `""` || string(got.UpdatedInput["timeout"]) != "30000" || len(got.UpdatedInput) != 3 {
					t.Errorf("the rest of the tool input was not echoed as it came: %v", got.UpdatedInput)
				}
				if strings.ContainsFunc(out, func(r rune) bool { return r >= 0x80 }) {
					t.Errorf("the answer is not ASCII: %q", out)
				}
			})
		}
	}
}

// TestCursorPreToolUseLeavesEverythingElseAlone: any command that is not
// exactly the rendered search, any identity that is not exactly what
// sessionStart exports, and any payload Cursor did not send gets no answer at
// all — the agent's command is untouched and nothing is denied.
func TestCursorPreToolUseLeavesEverythingElseAlone(t *testing.T) {
	hc := hookContext{cfgPath: "/c.toml", sessionsDir: "/sessions"}
	good := cursorIdentityEnv(hc.sessionsDir, "conv-1")
	without := func(k string) map[string]string {
		m := map[string]string{}
		for kk, v := range good {
			if kk != k {
				m[kk] = v
			}
		}
		return m
	}
	with := func(k, v string) map[string]string {
		m := without(k)
		m[k] = v
		return m
	}
	for _, goos := range []string{"darwin", "windows"} {
		tool, runners := cursorShellsFor(t, goos)
		for _, sh := range tool {
			rendered := renderedCursorSearch(t, sh, hc.cfgPath, `{"version":1,"query":"exact query text"}`)
			fixture := func(cmd string) []byte { return cursorPreToolUsePayload(t, cmd) }
			withoutVersion := func(cmd string) []byte {
				var p map[string]any
				_ = json.Unmarshal(fixture(cmd), &p)
				delete(p, "cursor_version")
				return mustJSON(t, p)
			}
			emptyVersion := func(cmd string) []byte {
				var p map[string]any
				_ = json.Unmarshal(fixture(cmd), &p)
				p["cursor_version"] = ""
				return mustJSON(t, p)
			}
			cases := []struct {
				name    string
				env     map[string]string
				payload []byte
			}{
				{"a foreign command", good, fixture("ls -la")},
				{"one byte off: a trailing space", good, fixture(rendered + " ")},
				{"one byte off: a flag misspelled", good, fixture(strings.Replace(rendered, "--stdin", "--stdim", 1))},
				{"the search with a second command after it", good, fixture(rendered + "\necho appended")},
				{"the search with a request that is not one version-1 object", good, fixture(strings.Replace(rendered, `{"version":1,`, `{"version":2,`, 1))},
				{"already carrying the identity", good, fixture(expectedCursorCommand(t, sh, good, rendered))},
				{"no harness", without("TOKENDROP_HARNESS"), fixture(rendered)},
				{"no lineage", without(lineageEnv), fixture(rendered)},
				{"no session", without(sessionEnv), fixture(rendered)},
				{"another harness", with("TOKENDROP_HARNESS", "claude-code"), fixture(rendered)},
				{"a session that is not a hashed id", with(sessionEnv, "conv-1"), fixture(rendered)},
				{"a lineage path outside the sessions directory", with(lineageEnv, filepath.Join("/elsewhere", filepath.Base(good[lineageEnv]))), fixture(rendered)},
				{"a lineage path that is not a lineage file", with(lineageEnv, filepath.Join(hc.sessionsDir, "window.json")), fixture(rendered)},
				{"no cursor_version", good, withoutVersion(rendered)},
				{"an empty cursor_version", good, emptyVersion(rendered)},
				{"not JSON", good, []byte("{")},
			}
			for _, c := range cases {
				t.Run(goos+"/"+string(sh)+"/"+c.name, func(t *testing.T) {
					if out, _ := runCursorPreToolUse(t, c.env, hc, c.payload, tool, runners); out != "" {
						t.Fatalf("answered %q; want no answer", out)
					}
				})
			}
		}
	}
}

// TestCursorPreToolUseNeverEchoesTextItsRunnerReencoded: on an OS where
// Cursor's hook runner re-encodes non-ASCII text before this hook reads it
// (#113), a command carrying any is not rewritten — the rewrite would hand
// Cursor back a different query. Where the runner is POSIX, the bytes are the
// agent's own and the search is labeled like any other.
func TestCursorPreToolUseNeverEchoesTextItsRunnerReencoded(t *testing.T) {
	hc := hookContext{cfgPath: "/c.toml", sessionsDir: "/sessions"}
	env := cursorIdentityEnv(hc.sessionsDir, "conv-1")
	body := `{"version":1,"query":"café 東京 naïve"}`
	for _, goos := range []string{"darwin", "linux", "windows"} {
		tool, runners := cursorShellsFor(t, goos)
		for _, sh := range tool {
			t.Run(goos+"/"+string(sh), func(t *testing.T) {
				rendered := renderedCursorSearch(t, sh, hc.cfgPath, body)
				out, errOut := runCursorPreToolUse(t, env, hc, cursorPreToolUsePayload(t, rendered), tool, runners)
				if hookInputIntact(runners) {
					var got cursorAnswer
					var command string
					if json.Unmarshal([]byte(out), &got) != nil || json.Unmarshal(got.UpdatedInput["command"], &command) != nil ||
						command != expectedCursorCommand(t, sh, env, rendered) {
						t.Fatalf("a POSIX runner's intact non-ASCII search was not labeled: %q", out)
					}
					if strings.ContainsFunc(out, func(r rune) bool { return r >= 0x80 }) {
						t.Errorf("the answer is not ASCII: %q", out)
					}
					return
				}
				if out != "" {
					t.Fatalf("a command this hook may have received re-encoded was echoed back: %q", out)
				}
				if !strings.Contains(errOut, "#113") {
					t.Errorf("no note on stderr: %q", errOut)
				}
			})
		}
	}
}

// TestCursorIdentityQuotingRoundTrips: a path with a space and a path with an
// apostrophe, quoted each shell's own way; and a value no quoting carries
// intact means no rewrite and a note, never part of a prefix.
func TestCursorIdentityQuotingRoundTrips(t *testing.T) {
	hc := hookContext{cfgPath: "/c.toml", sessionsDir: "/tmp/it's a dir/sessions"}
	env := cursorIdentityEnv(hc.sessionsDir, "conv-1")
	lineage := env[lineageEnv]
	session := env[sessionEnv]
	want := map[shellKind]string{
		shellPOSIX:      "TOKENDROP_HARNESS='cursor' TOKENDROP_LINEAGE='/tmp/it'\\''s a dir/sessions/" + filepath.Base(lineage) + "' TOKENDROP_SESSION='" + session + "' ",
		shellPowerShell: "$env:TOKENDROP_HARNESS='cursor'; $env:TOKENDROP_LINEAGE='/tmp/it''s a dir/sessions/" + filepath.Base(lineage) + "'; $env:TOKENDROP_SESSION='" + session + "'; ",
	}
	if runtime.GOOS == "windows" {
		// lineagePath joins with the runner's separator; the quoting is what
		// is under test, so the expectation follows the value.
		want[shellPOSIX] = "TOKENDROP_HARNESS='cursor' TOKENDROP_LINEAGE='" + strings.ReplaceAll(lineage, "'", `'\''`) + "' TOKENDROP_SESSION='" + session + "' "
		want[shellPowerShell] = "$env:TOKENDROP_HARNESS='cursor'; $env:TOKENDROP_LINEAGE='" + strings.ReplaceAll(lineage, "'", "''") + "'; $env:TOKENDROP_SESSION='" + session + "'; "
	}
	for sh, w := range want {
		got, ok := cursorIdentityPrefix(sh, cursorIdentity{lineage: lineage, session: session})
		if !ok || got != w {
			t.Errorf("%s prefix:\n got %q (%v)\nwant %q", sh, got, ok, w)
		}
	}

	_, runners := cursorShellsFor(t, "darwin")
	for _, c := range []struct {
		sh  shellKind
		dir string
	}{
		{shellPOSIX, "/tmp/line\nbreak/sessions"},
		{shellPowerShell, "/tmp/line\nbreak/sessions"},
		{shellPowerShell, "/tmp/it’s typographic/sessions"},
	} {
		hc := hookContext{cfgPath: "/c.toml", sessionsDir: c.dir}
		env := cursorIdentityEnv(c.dir, "conv-1")
		rendered := renderedCursorSearch(t, c.sh, hc.cfgPath, `{"version":1,"query":"q"}`)
		out, errOut := runCursorPreToolUse(t, env, hc, cursorPreToolUsePayload(t, rendered), []shellKind{c.sh}, runners)
		if out != "" || !strings.Contains(errOut, "cannot be quoted") {
			t.Errorf("%s with %q: answered %q, stderr %q; want no answer and a note", c.sh, c.dir, out, errOut)
		}
	}
	if _, ok := cursorIdentityPrefix(shellPOSIX, cursorIdentity{lineage: "/tmp/it’s/x.json", session: session}); !ok {
		t.Error("POSIX single quotes carry a typographic apostrophe as itself; it was refused")
	}
}

// TestCursorShellHookAllowsTheSearchCarryingItsIdentity: beforeShellExecution
// sees the command preToolUse rewrote and must still allow it — this prefix,
// with this session's values, in front of the rendered search, and nothing
// else.
func TestCursorShellHookAllowsTheSearchCarryingItsIdentity(t *testing.T) {
	hc := hookContext{cfgPath: "/c.toml", sessionsDir: "/sessions"}
	env := cursorIdentityEnv(hc.sessionsDir, "conv-1")
	_, ops := newFakeHookOps(env)
	for _, goos := range []string{"darwin", "linux", "windows"} {
		tool, _ := cursorShellsFor(t, goos)
		for _, sh := range tool {
			t.Run(goos+"/"+string(sh), func(t *testing.T) {
				rendered := renderedCursorSearch(t, sh, hc.cfgPath, `{"version":1,"query":"q"}`)
				prefixed := expectedCursorCommand(t, sh, env, rendered)
				if f := recognizeCursorCommand(ops, hc, prefixed, tool); !isSearchForm(f) {
					t.Fatalf("the search carrying its identity is not recognized:\n%s", prefixed)
				}
				if f := recognizeCursorCommand(ops, hc, rendered, tool); !isSearchForm(f) {
					t.Fatalf("the plain search is no longer recognized:\n%s", rendered)
				}

				other := cursorIdentityEnv(hc.sessionsDir, "conv-2")
				foreign := map[string]string{
					"another session's values":        expectedCursorCommand(t, sh, other, rendered),
					"an assignment of any other name": strings.Replace(prefixed, "TOKENDROP_HARNESS=", "TOKENDROP_HARNES=", 1),
					"a fourth assignment":             strings.Replace(prefixed, "TOKENDROP_SESSION=", "X='1' TOKENDROP_SESSION=", 1),
					"the prefix twice":                expectedCursorCommand(t, sh, env, prefixed),
					"the prefix on a foreign command": strings.Replace(prefixed, rendered[strings.Index(rendered, "'"):], "'/bin/echo' hi", 1),
				}
				if sh == shellPowerShell {
					foreign["an assignment of any other name"] = strings.Replace(prefixed, "$env:TOKENDROP_HARNESS=", "$env:TOKENDROP_HARNES=", 1)
					foreign["a fourth assignment"] = strings.Replace(prefixed, "$env:TOKENDROP_SESSION=", "$env:X='1'; $env:TOKENDROP_SESSION=", 1)
					foreign["the prefix before the encoding line"] = strings.Replace(prefixed, "$OutputEncoding", "$env:TOKENDROP_HARNESS='cursor'; $OutputEncoding", 1)
				}
				for name, cmd := range foreign {
					if cmd == prefixed || cmd == rendered {
						t.Fatalf("%s: the mutation did not change the command", name)
					}
					if f := recognizeCursorCommand(ops, hc, cmd, tool); f != nil {
						t.Errorf("%s was recognized as ours:\n%s", name, cmd)
					}
				}
				prefer, err := binEntry{command: mustExecutable(t), cfg: hc.cfgPath}.preferCommandForShell(sh)
				if err != nil {
					t.Fatal(err)
				}
				if sh == shellPOSIX {
					preferPrefixed := expectedCursorCommand(t, sh, env, prefer+" status")
					if f := recognizeCursorCommand(ops, hc, preferPrefixed, tool); f != nil {
						t.Errorf("the identity prefix on the preference command was recognized: it is only ever written on the search")
					}
				}
			})
		}
	}
}

func mustExecutable(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return exe
}

// TestCursorHooksAnswerTheRewrittenSearchEndToEnd drives hookCursor itself,
// for this runner's OS: sessionStart's own answer is the environment the
// later hooks get, preToolUse rewrites the search, and beforeShellExecution
// allows what preToolUse handed back and stamps the call.
func TestCursorHooksAnswerTheRewrittenSearchEndToEnd(t *testing.T) {
	if _, err := declaredShells(cursorTarget{}, runtime.GOOS, channelTool); err != nil {
		t.Skipf("Cursor declares no tool shell on %s", runtime.GOOS)
	}
	hc := hookContext{cfgPath: "/c.toml", sessionsDir: "/sessions"}
	base := map[string]any{"conversation_id": "conv-1", "generation_id": "gen-1", "workspace_roots": []string{"/w/proj"}, "cursor_version": "3.21.16"}
	fs, ops := newFakeHookOps(nil)
	var out bytes.Buffer
	hookCursor(ops, hc, "sessionStart", mustJSON(t, base), &out, io.Discard)
	var start struct {
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal(out.Bytes(), &start); err != nil {
		t.Fatalf("sessionStart: %q", out.String())
	}
	// The later hooks of the session: the same files, and sessionStart's
	// `env` in their environment, as Cursor starts them.
	ops.getenv = func(k string) string { return start.Env[k] }

	search := cursorTestSearch(t, hc.cfgPath)
	out.Reset()
	hookCursor(ops, hc, "preToolUse", cursorPreToolUsePayload(t, search), &out, io.Discard)
	var got cursorAnswer
	var command string
	if json.Unmarshal(out.Bytes(), &got) != nil || json.Unmarshal(got.UpdatedInput["command"], &command) != nil || command == search {
		t.Fatalf("preToolUse did not rewrite the search: %q", out.String())
	}

	out.Reset()
	payload := map[string]any{}
	for k, v := range base {
		payload[k] = v
	}
	payload["command"] = command
	hookCursor(ops, hc, "beforeShellExecution", mustJSON(t, payload), &out, io.Discard)
	if strings.TrimSpace(out.String()) != `{"permission":"allow"}` {
		t.Fatalf("beforeShellExecution did not allow the rewritten search: %q", out.String())
	}
	l, ok := loadLineage(ops, start.Env[lineageEnv])
	if !ok || l.CallID == "" || l.Seq != 1 || l.SessionID != start.Env[sessionEnv] {
		t.Fatalf("the rewritten search's call was not stamped into the declared file: %+v (files %v)", l, keys(fs.files))
	}
}

// TestCursorRewrittenSearchReachesTheRouterAsCursor runs the whole chain on
// this runner with its real shells: sessionStart in process, preToolUse in
// process, and the command preToolUse handed back run in the shell it was
// rendered for, from an installation whose path holds a space and an
// apostrophe. What decides it is the trace the router received.
func TestCursorRewrittenSearchReachesTheRouterAsCursor(t *testing.T) {
	tool, err := declaredShells(cursorTarget{}, runtime.GOOS, channelTool)
	if err != nil {
		t.Skipf("Cursor declares no tool shell on %s", runtime.GOOS)
	}
	runners, err := declaredShells(cursorTarget{}, runtime.GOOS, channelHook)
	if err != nil {
		runners = unknownCellCandidates["cursor "+runtime.GOOS+" hook"]
	}
	for _, sh := range renderedShellsOnThisOS(t, "cursor") {
		t.Run(sh.name, func(t *testing.T) {
			requireExecTool(t, sh)
			in := newExecInstallationIn(t, "it's a dir")
			hc := hookContext{cfgPath: in.cfg, sessionsDir: in.sessions}
			ops := realHookOps()
			ops.executable = func() (string, error) { return in.bin, nil }
			ops.spawnFlush = nil
			ops.getenv = func(string) string { return "" }
			var out bytes.Buffer
			hookCursor(ops, hc, "sessionStart", mustJSON(t, map[string]any{"conversation_id": "exec-conversation", "workspace_roots": []string{in.root}, "cursor_version": "3.21.16"}), &out, io.Discard)
			var start struct {
				Env map[string]string `json:"env"`
			}
			if err := json.Unmarshal(out.Bytes(), &start); err != nil {
				t.Fatalf("sessionStart: %q", out.String())
			}
			ops.getenv = func(k string) string { return start.Env[k] }

			search := skillBlockFor(t, in.renderedSkill("cursor"), sh.kind, "search").body
			out.Reset()
			cursorPreToolUse(ops, hc, cursorPreToolUsePayload(t, search), tool, runners, &out, io.Discard)
			var got cursorAnswer
			var command string
			if json.Unmarshal(out.Bytes(), &got) != nil || json.Unmarshal(got.UpdatedInput["command"], &command) != nil {
				t.Fatalf("preToolUse did not rewrite the %s search: %q", sh.kind, out.String())
			}
			res := runInShell(t, sh, command, nil, in.env)
			req := requireOneRequest(t, in, res, "exact query text")
			if req.Trace == nil || req.Trace.Harness != "cursor" || req.Trace.SessionID != traceHash("exec-conversation") {
				t.Fatalf("the router received trace %+v, want harness cursor and the conversation's session\n%s", req.Trace, req.Raw)
			}
			if l, ok := loadLineage(ops, start.Env[lineageEnv]); !ok || l.Seq != 1 {
				t.Fatalf("the search did not read and advance the declared lineage file: %+v", l)
			}
		})
	}
}
