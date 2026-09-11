package main

// Hermes' config.yaml is the participant's own privileged host
// configuration, edited without a YAML parser. Two properties carry that
// decision, and both are tested here rather than argued:
//
//   - we never mutate a config whose structure we cannot establish
//     conservatively, and a refusal leaves the file byte-identical;
//   - what we do append comes back off exactly, so install→uninstall
//     returns the bytes we were given.
//
// Plus the quoting: the command we write has to survive a YAML scalar AND
// Hermes' own argv splitter, for the paths people actually have.
// All paths and identifiers here are synthetic.

import (
	"strings"
	"testing"
)

func hermesEntry(bin, cfg string) binEntry { return binEntry{command: bin, cfg: cfg} }

// mustHermesYAML is the hook block as it would be installed here, for
// tests that need a config that already carries ours.
func mustHermesYAML(t *testing.T) string {
	t.Helper()
	body, ok := hermesHookYAML(binEntry{command: "/home/u/.tokendrop/bin/dropin-miner", cfg: testCfg})
	if !ok {
		t.Fatal("could not render the Hermes hook YAML")
	}
	return body
}

// ── conservative structure detection ────────────────────────────────────

// PyYAML keeps the LAST of two duplicate top-level keys and reports
// nothing, so appending a second `hooks:` to a config that already has one
// would silently delete every hook the participant wrote. That is the
// asymmetry this detector is built around: refusing a config we could
// safely have edited costs four lines of pasting, and editing one we
// should have refused costs them their hooks with no error message.
func TestHermesRefusesAnyPlausibleExistingHooksKey(t *testing.T) {
	refuse := map[string]string{
		"plain":              "hooks:\n  post_tool_call:\n    - command: mine\n",
		"space before colon": "hooks :\n  post_tool_call: []\n",
		"single quoted":      "'hooks':\n  post_tool_call: []\n",
		"double quoted":      "\"hooks\":\n  post_tool_call: []\n",
		"trailing spaces":    "hooks:   \n  post_tool_call: []\n",
		"tab after colon":    "hooks:\t\n  post_tool_call: []\n",
		"trailing comment":   "hooks: # mine\n  post_tool_call: []\n",
		"empty mapping":      "hooks: {}\n",
		"empty value":        "hooks:\n",
		"after other keys":   "database:\n  journal_mode: wal\nhooks:\n  post_tool_call: []\n",
		"after a comment":    "# my config\nhooks:\n  post_tool_call: []\n",
		"uppercase":          "HOOKS:\n  post_tool_call: []\n",
		"crlf":               "database: x\r\nhooks:\r\n  post_tool_call: []\r\n",
		// Structures a top-level `hooks:` mapping cannot simply be appended to.
		"top-level sequence": "- one\n- two\n",
		"flow mapping":       "{database: x}\n",
		"explicit key":       "? database\n: x\n",
		"two documents":      "database: x\n---\nother: y\n",
		"document end":       "database: x\n...\n",
		"bare scalar":        "just some text\n",
		"key without space":  "database:x\n",
		"escaped key":        "\"data\\u0062ase\": x\n",
		"partial block":      agentsMarkerBegin + "\ntruncated\n",
	}
	for name, cfg := range refuse {
		t.Run(name, func(t *testing.T) {
			if reason := hermesConfigRefusal([]byte(cfg)); reason == "" {
				t.Errorf("edited a config it should have refused:\n%s", cfg)
			}
		})
	}

	// …and the configs we must NOT refuse, or the feature never installs.
	accept := map[string]string{
		"empty":              "",
		"only comments":      "# nothing here yet\n",
		"ordinary":           "database:\n  journal_mode: \"wal\"\nmodel: gpt\n",
		"document start":     "---\ndatabase: x\n",
		"yaml directive":     "%YAML 1.2\n---\ndatabase: x\n",
		"nested hooks key":   "agent:\n  hooks:\n    - one\n",
		"hooks in a comment": "# hooks: are documented elsewhere\ndatabase: x\n",
		"hooks in a value":   "note: \"see hooks: in the docs\"\n",
		"hooks as a suffix":  "webhooks:\n  - one\n",
		"hooks_auto_accept":  "hooks_auto_accept: false\n",
		"indented text":      "prompt: |\n  hooks:\n    are not a key here\n",
		"no final newline":   "database: x",
		"blank lines":        "\n\ndatabase: x\n\n",
	}
	for name, cfg := range accept {
		t.Run(name, func(t *testing.T) {
			if reason := hermesConfigRefusal([]byte(cfg)); reason != "" {
				t.Errorf("refused a config it could safely extend (%s):\n%s", reason, cfg)
			}
		})
	}
}

// A refusal is only conservative if it also leaves the file alone.
func TestHermesRefusalLeavesTheConfigByteIdentical(t *testing.T) {
	foreign := "hooks:\n  post_tool_call:\n    - command: \"my-own-hook\"\n"
	m, ops := newFakeMachine("hermes")
	m.files[hermesConfigPath] = []byte(foreign)

	code, out, _ := runAgents(t, ops, nil, "install", "-config", testCfg, "-yes")
	if code != exitTransport {
		t.Fatalf("expected a refusal exit, got %d\n%s", code, out)
	}
	if !strings.Contains(out, "already declares a top-level hooks: key") {
		t.Errorf("expected a refusal naming the reason, got:\n%s", out)
	}
	if !strings.Contains(out, "pre_tool_call:") {
		t.Errorf("a refusal must print the snippet to paste, got:\n%s", out)
	}
	if got := string(m.files[hermesConfigPath]); got != foreign {
		t.Errorf("the participant's own config was modified:\n%q", got)
	}
}

// ── exact round trip ────────────────────────────────────────────────────

// install → uninstall must return the bytes we were handed. The final
// newline is the case that makes this non-trivial: without recording what
// we found, uninstall cannot tell a newline we added from one that was
// always there.
func TestHermesInstallUninstallRoundTripsExactly(t *testing.T) {
	body := mustHermesYAML(t)
	for name, original := range map[string]string{
		"empty":             "",
		"trailing newline":  "database:\n  journal_mode: \"wal\"\n",
		"no final newline":  "database:\n  journal_mode: \"wal\"",
		"blank line at eof": "database: x\n\n",
		"many blank lines":  "database: x\n\n\n\n",
		"only comments":     "# just a comment\n",
		"crlf":              "database: x\r\n",
	} {
		t.Run(name, func(t *testing.T) {
			installed := hermesAppendBlock([]byte(original), body)
			if !strings.Contains(string(installed), "pre_tool_call:") {
				t.Fatalf("the hook was not added:\n%q", installed)
			}
			if !strings.HasPrefix(string(installed), original) {
				t.Fatalf("the original bytes were not preserved verbatim:\n%q", installed)
			}
			// Idempotent: a second install of the same config is a no-op.
			stripped, had := hermesRemoveBlock(installed)
			if !had {
				t.Fatal("our own block was not found for removal")
			}
			if again := hermesAppendBlock(stripped, body); string(again) != string(installed) {
				t.Errorf("a second install produced different bytes:\n%q\n%q", again, installed)
			}
			if string(stripped) != original {
				t.Errorf("uninstall did not restore the original bytes:\n got %q\nwant %q", stripped, original)
			}
		})
	}
}

func TestHermesInstallRegistersHookAndUninstallRemovesIt(t *testing.T) {
	m, ops := newFakeMachine("hermes")
	// Hermes already has a config the tool must preserve.
	original := "database:\n  journal_mode: \"wal\"\n"
	m.files[hermesConfigPath] = []byte(original)

	if code, out, _ := runAgents(t, ops, nil, "install", "-config", testCfg, "-yes"); code != exitOK {
		t.Fatalf("install: %d\n%s", code, out)
	}
	got := string(m.files[hermesConfigPath])
	for _, want := range []string{
		"journal_mode",             // the user's config preserved
		agentsMarkerBegin,          // our block
		"hooks:", "pre_tool_call:", // the hook
		"hermes pre_tool_call",  // our subcommand
		"matcher: \"terminal\"", // fires on the shell tool
		agentsMarkerEnd,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("config.yaml missing %q:\n%s", want, got)
		}
	}
	// We do not fail the participant's shell command when our own hook
	// breaks: fail_closed would turn a crash here into a blocked tool call.
	if strings.Contains(got, "fail_closed") || strings.Contains(got, "failClosed") {
		t.Errorf("the installed hook opts into fail-closed:\n%s", got)
	}
	// Idempotent.
	before := got
	if code, _, _ := runAgents(t, ops, nil, "install", "-config", testCfg, "-yes"); code != exitOK {
		t.Fatal("second install failed")
	}
	if string(m.files[hermesConfigPath]) != before {
		t.Error("second install was not idempotent")
	}
	// Uninstall removes only our block; the user's config survives exactly.
	if code, _, _ := runAgents(t, ops, nil, "uninstall", "-config", testCfg, "-yes"); code != exitOK {
		t.Fatal("uninstall failed")
	}
	if left := string(m.files[hermesConfigPath]); left != original {
		t.Errorf("uninstall did not restore the config exactly:\n got %q\nwant %q", left, original)
	}
}

// ── quoting: YAML, then Hermes' own splitter ────────────────────────────

// Hermes does not run the hook through a shell: it tokenizes the string and
// execs argv directly. So the command has to survive two layers — a YAML
// single-quoted scalar, then shlex — for the paths people actually have.
// Go's %q, which is what this used to use, survives neither: it escapes a
// backslash to \\ and a non-ASCII rune to \uXXXX, and nothing downstream
// undoes either.
func TestHermesHookCommandSurvivesYAMLAndTheArgvSplitter(t *testing.T) {
	for name, tc := range map[string]struct {
		bin, cfg string
		windows  bool
	}{
		"ordinary":         {"/home/u/.tokendrop/bin/dropin-miner", "/home/u/.tokendrop/tokendrop.toml", false},
		"spaces":           {"/home/u/My Tools/dropin-miner", "/home/u/My Tools/tokendrop.toml", false},
		"apostrophe":       {"/Users/O'Neil/bin/dropin-miner", "/Users/O'Neil/tokendrop.toml", false},
		"double quote":     {`/home/u/say "hi"/dropin-miner`, "/home/u/tokendrop.toml", false},
		"backslash":        {`/home/u/odd\path/dropin-miner`, "/home/u/tokendrop.toml", false},
		"non-ascii":        {"/home/ユーザー/bin/dropin-miner", "/home/ユーザー/tokendrop.toml", false},
		"dollar and tick":  {"/home/u/$HOME`x`/dropin-miner", "/home/u/tokendrop.toml", false},
		"hash":             {"/home/u/#1/dropin-miner", "/home/u/tokendrop.toml", false},
		"no config":        {"/home/u/bin/dropin-miner", "", false},
		"windows":          {`C:\Program Files\Dropin Miner\dropin-miner.exe`, `C:\Users\u\tokendrop.toml`, true},
		"windows apostro":  {`C:\Users\O'Neil\dropin-miner.exe`, `C:\Users\O'Neil\tokendrop.toml`, true},
		"windows non-asci": {`C:\Users\ユーザー\dropin-miner.exe`, `C:\Users\ユーザー\tokendrop.toml`, true},
	} {
		t.Run(name, func(t *testing.T) {
			cmd, ok := hermesHookCommand(hermesEntry(tc.bin, tc.cfg), tc.windows)
			if !ok {
				t.Fatal("could not represent this path for the splitter")
			}
			scalar := hermesYAMLSingleQuoted(cmd)
			// Layer 1: YAML gives back the command string exactly.
			if got := unquoteYAMLSingle(t, scalar); got != cmd {
				t.Fatalf("YAML round trip changed the command:\n got %q\nwant %q", got, cmd)
			}
			// Layer 2: the splitter gives back the exact argv we meant.
			want := []string{tc.bin, "hook"}
			if tc.cfg != "" {
				want = append(want, "-config", tc.cfg)
			}
			want = append(want, "hermes", "pre_tool_call")
			got := splitCommandLine(t, cmd, tc.windows)
			if len(got) != len(want) {
				t.Fatalf("argv = %q, want %q", got, want)
			}
			for i := range want {
				if got[i] != want[i] {
					t.Fatalf("argv[%d] = %q, want %q (full: %q)", i, got[i], want[i], got)
				}
			}
		})
	}

	// The one case with no representation: a double quote on Windows, which
	// no legal Windows path may contain. Refused, never guessed at.
	if _, ok := hermesHookCommand(hermesEntry(`C:\a"b\dropin-miner.exe`, ""), true); ok {
		t.Error("a Windows path containing a double quote was quoted anyway")
	}
}

// The installed YAML is what a YAML parser will actually read.
func TestHermesHookYAMLIsTheCommandWeMeant(t *testing.T) {
	body, ok := hermesHookYAML(hermesEntry("/home/u/O'Neil/dropin-miner", "/home/u/c.toml"))
	if !ok {
		t.Fatal("could not render")
	}
	line := ""
	for _, l := range strings.Split(body, "\n") {
		if strings.Contains(l, "- command:") {
			line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(l), "- command:"))
		}
	}
	if line == "" {
		t.Fatalf("no command entry in:\n%s", body)
	}
	cmd := unquoteYAMLSingle(t, line)
	if !strings.Contains(cmd, "hermes pre_tool_call") {
		t.Errorf("the command does not run our hook: %q", cmd)
	}
	argv := splitCommandLine(t, cmd, false)
	if argv[0] != "/home/u/O'Neil/dropin-miner" {
		t.Errorf("argv[0] = %q, want the exact binary path", argv[0])
	}
}

// ── models of the two consumers ─────────────────────────────────────────

// unquoteYAMLSingle reads a YAML single-quoted scalar: everything is
// literal and a doubled quote is the only escape.
func unquoteYAMLSingle(t *testing.T, s string) string {
	t.Helper()
	if len(s) < 2 || s[0] != '\'' || s[len(s)-1] != '\'' {
		t.Fatalf("not a single-quoted scalar: %s", s)
	}
	body := s[1 : len(s)-1]
	var b strings.Builder
	for i := 0; i < len(body); i++ {
		if body[i] == '\'' {
			if i+1 >= len(body) || body[i+1] != '\'' {
				t.Fatalf("unescaped quote inside a single-quoted scalar: %s", s)
			}
			i++
		}
		b.WriteByte(body[i])
	}
	return b.String()
}

// splitCommandLine models Hermes' own splitter for the token forms we
// emit: POSIX shlex.split, or on Windows shlex.split(posix=False) with one
// layer of matching quotes stripped per token — deliberately different, so
// that Windows path backslashes survive.
func splitCommandLine(t *testing.T, line string, windows bool) []string {
	t.Helper()
	var out []string
	var cur strings.Builder
	started := false
	quote := byte(0)
	flush := func() {
		if started {
			out = append(out, cur.String())
			cur.Reset()
			started = false
		}
	}
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case quote != 0 && c == quote:
			quote = 0
			if windows {
				cur.WriteByte(c) // non-POSIX keeps the quotes on the token
			}
		case quote == '\'':
			cur.WriteByte(c) // literal, always
		case quote == '"':
			if !windows && c == '\\' && i+1 < len(line) && (line[i+1] == '"' || line[i+1] == '\\') {
				i++
			}
			cur.WriteByte(line[i])
		case c == '\'' || c == '"':
			quote = c
			started = true
			if windows {
				cur.WriteByte(c)
			}
		case !windows && c == '\\' && i+1 < len(line):
			i++
			cur.WriteByte(line[i])
			started = true
		case c == ' ' || c == '\t':
			flush()
		default:
			cur.WriteByte(c)
			started = true
		}
	}
	if quote != 0 {
		t.Fatalf("unbalanced quotes in %q", line)
	}
	flush()
	if windows {
		for i, tok := range out {
			if len(tok) >= 2 && tok[0] == tok[len(tok)-1] && (tok[0] == '\'' || tok[0] == '"') {
				out[i] = tok[1 : len(tok)-1]
			}
		}
	}
	return out
}
