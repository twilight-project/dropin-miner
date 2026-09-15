package main

// Every string a host is handed, per OS, as a golden document.
//
// A shell string is rendered in several places — the skill's command blocks,
// the rules line, hook entries in two JSON files and one YAML file, Claude
// Code's allow rules, four bridge prefixes — and the soak's Windows defects
// are all the same mistake made in some of them. This file renders every one
// of them for a representative installation on each OS and compares the
// result with testdata/hosts/<goos>.golden. All three documents are checked
// on every runner: nothing in them depends on the OS the test runs on, only
// on the OS it renders for.
//
// H1 characterizes: the goldens hold what v0.2.9 renders, doubled Windows
// backslashes, Bash heredocs for PowerShell hosts and all. H2 and H3 change
// them deliberately, as reviewed diffs. There is no flag that rewrites them.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// hostStringsEntry is the representative installation each golden renders
// for: the default layout under an ordinary home directory on that OS.
func hostStringsEntry(goos string) binEntry {
	switch goos {
	case "windows":
		return binEntry{command: `C:\Users\u\.tokendrop\bin\dropin-miner.exe`, cfg: `C:\Users\u\.tokendrop\tokendrop.toml`}
	case "darwin":
		return binEntry{command: "/Users/u/.tokendrop/bin/dropin-miner", cfg: "/Users/u/.tokendrop/tokendrop.toml"}
	}
	return binEntry{command: "/home/u/.tokendrop/bin/dropin-miner", cfg: "/home/u/.tokendrop/tokendrop.toml"}
}

// renderedSkillFor is the SKILL.md the host's own install writes.
func renderedSkillFor(id string, entry binEntry) string {
	note := ""
	if id == "hermes" {
		note = hermesApprovalNote
	}
	return string(renderSkill(entry, preferOn, note))
}

// skillBlock is one fenced block of a rendered skill: its fence language and
// its body, exactly as written between the fences.
type skillBlock struct {
	lang, body string
}

// installMarker is the path component every installation these tests render
// for lives under. Command blocks are found by it rather than by the binary
// path's spelling, because how a path is quoted is exactly what H2 and H3
// change, and the skill's prose names `dropin-miner` in places that run
// nothing.
const installMarker = ".tokendrop"

// skillCommandBlocks are the fenced blocks of a rendered skill that run this
// binary, in order: the search, then the preference command.
func skillCommandBlocks(skill string) []skillBlock {
	var out []skillBlock
	lines := strings.Split(skill, "\n")
	for i := 0; i < len(lines); i++ {
		if !strings.HasPrefix(lines[i], "```") {
			continue
		}
		lang := strings.TrimPrefix(lines[i], "```")
		var body []string
		for i++; i < len(lines) && !strings.HasPrefix(lines[i], "```"); i++ {
			body = append(body, lines[i])
		}
		text := strings.Join(body, "\n")
		if strings.Contains(text, installMarker) {
			out = append(out, skillBlock{lang: lang, body: text})
		}
	}
	return out
}

// skillSearchBlock is the skill's search: the first command block.
func skillSearchBlock(t *testing.T, skill string) skillBlock {
	t.Helper()
	blocks := skillCommandBlocks(skill)
	if len(blocks) < 2 {
		t.Fatalf("the rendered skill has %d command blocks, want the search and the preference command:\n%s", len(blocks), skill)
	}
	return blocks[0]
}

// skillPreferBlock is the skill's preference command block.
func skillPreferBlock(t *testing.T, skill string) skillBlock {
	t.Helper()
	blocks := skillCommandBlocks(skill)
	if len(blocks) < 2 {
		t.Fatalf("the rendered skill has %d command blocks:\n%s", len(blocks), skill)
	}
	return blocks[1]
}

// skillProseCommandLines are the lines outside any fence that name the
// binary: the human form.
func skillProseCommandLines(skill string) []string {
	var out []string
	fenced := false
	for _, l := range strings.Split(skill, "\n") {
		if strings.HasPrefix(l, "```") {
			fenced = !fenced
			continue
		}
		if !fenced && strings.Contains(l, installMarker) {
			out = append(out, l)
		}
	}
	return out
}

// installedHookFile is the hook file an install writes on an empty machine,
// as bytes.
func installedHookFile(t *testing.T, spec hooksSpec, entry binEntry) string {
	t.Helper()
	_, ops := newFakeMachine()
	var p agentPlan
	if !planHooksMerge(ops, "golden", "/golden/hooks.json", &p, entry, spec) || len(p.writes) != 1 {
		t.Fatalf("hook merge planned %d writes, refused %v", len(p.writes), p.refused)
	}
	return string(p.writes[0].contents)
}

// installedHookCommands reads every hook command back out of the file the
// install wrote, event by event in the spec's order — what a host reads.
func installedHookCommands(t *testing.T, file string, spec hooksSpec) []struct{ event, command string } {
	t.Helper()
	m, err := decodeJSONObject([]byte(file))
	if err != nil {
		t.Fatal(err)
	}
	hooks, _ := m[spec.root].(map[string]any)
	var out []struct{ event, command string }
	for _, ev := range spec.order {
		list, _ := hooks[ev].([]any)
		for _, e := range list {
			em, _ := e.(map[string]any)
			if c, ok := em["command"].(string); ok {
				out = append(out, struct{ event, command string }{ev, c})
			}
			hs, _ := em["hooks"].([]any)
			for _, h := range hs {
				hm, _ := h.(map[string]any)
				if c, ok := hm["command"].(string); ok {
					out = append(out, struct{ event, command string }{ev, c})
				}
			}
		}
	}
	if len(out) != len(spec.order) {
		t.Fatalf("read %d hook commands back, want %d", len(out), len(spec.order))
	}
	return out
}

var bridgeValueRe = regexp.MustCompile(bridgeEnv + `=[A-Za-z0-9_-]+`)

// withBridgePlaceholder replaces a generated bridge value, which carries a
// random call id, with a stable placeholder.
func withBridgePlaceholder(cmd string) string {
	return bridgeValueRe.ReplaceAllString(cmd, bridgeEnv+"=<BRIDGE>")
}

// claudeBridgedCommand is the command Claude Code's PreToolUse hook hands
// back for a Bash call running command — hookLineage itself, in process.
func claudeBridgedCommand(t *testing.T, command string) string {
	t.Helper()
	_, ops := newFakeHookOps(nil)
	payload, _ := json.Marshal(map[string]any{
		"session_id": "synthetic-session", "prompt_id": "synthetic-prompt", "tool_use_id": "synthetic-call",
		"tool_name": "Bash", "tool_input": map[string]any{"command": command},
	})
	var out bytes.Buffer
	hookLineage(ops, hookContext{}, payload, &out)
	var got struct {
		HookSpecificOutput struct {
			UpdatedInput map[string]any `json:"updatedInput"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("the Claude Code hook rewrote nothing for %q: %v (%q)", command, err, out.String())
	}
	cmd, _ := got.HookSpecificOutput.UpdatedInput["command"].(string)
	return cmd
}

// hermesBridgedCommand is the command Hermes' pre_tool_call hook hands back.
func hermesBridgedCommand(t *testing.T, command string) string {
	t.Helper()
	payload, _ := json.Marshal(map[string]any{
		"hook_event_name": "pre_tool_call", "tool_name": "terminal", "session_id": "synthetic-session",
		"tool_input": map[string]any{"command": command}, "extra": map[string]any{"tool_call_id": "synthetic-call"},
	})
	var out bytes.Buffer
	hookHermes("pre_tool_call", payload, &out)
	var got hermesModifyDirective
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("the Hermes hook rewrote nothing for %q: %v (%q)", command, err, out.String())
	}
	cmd, _ := got.ToolInput["command"].(string)
	return cmd
}

// jsBridgedCommand runs the rendered opencode plugin or Pi extension — the
// artifact the install writes — in Node against one synthetic bash call and
// returns the command it hands the host.
func jsBridgedCommand(t *testing.T, host, command string) string {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal("node is required to run the rendered opencode plugin and Pi extension")
	}
	source := renderAgentScript(opencodePluginJS)
	if host == "pi" {
		source = renderAgentScript(piExtensionTS)
	}
	script := `
 const fs = await import('node:fs');
 const input = JSON.parse(fs.readFileSync(0,'utf8'));
 const mod = await import('data:text/javascript;base64,'+Buffer.from(input.source).toString('base64'));
 let command = input.command;
 if (input.host === 'opencode') {
  const hooks = await mod.DropinMinerLineage({client:{session:{messages:async()=>({data:[]})}}});
  const output = {args:{command}};
  await hooks['tool.execute.before']({tool:'bash',sessionID:'synthetic-session',callID:'synthetic-call'}, output);
  command = output.args.command;
 } else {
  let handler;
  mod.default({on:(event, fn)=>{ if (event === 'tool_call') handler = fn }});
  const event = {toolName:'bash', toolCallId:'synthetic-call', input:{command}};
  await handler(event, {sessionManager:{getSessionId:()=>'synthetic-session', getBranch:()=>[]}});
  command = event.input.command;
 }
 process.stdout.write(JSON.stringify({command}));`
	input, _ := json.Marshal(map[string]string{"host": host, "source": source, "command": command})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, node, "--input-type=module", "-e", script) // #nosec G204 -- fixed test script and local Node runtime; synthetic input on stdin
	cmd.Stdin = bytes.NewReader(input)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s adapter: %v\n%s", host, err, out)
	}
	var got struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("%s adapter output %q: %v", host, out, err)
	}
	return got.Command
}

// bridgedCommand is the command host's lineage adapter hands the host for
// command, or "" for a host with no bridge (Codex; Cursor, whose lineage is
// a file its hooks write).
func bridgedCommand(t *testing.T, host, command string) string {
	t.Helper()
	switch host {
	case "claude":
		return claudeBridgedCommand(t, command)
	case "hermes":
		return hermesBridgedCommand(t, command)
	case "opencode", "pi":
		return jsBridgedCommand(t, host, command)
	}
	return ""
}

// recognizerVerdict is what Cursor's command recognizer makes of s on goos,
// grammar only: the argv it parses, or its refusal. The identity check needs
// a real file and is exercised against the built binary in the execution
// tests.
func recognizerVerdict(s, goos string) string {
	argv, ok := simpleCommandArgsForPlatform(s, goos == "windows")
	if !ok {
		return "refused"
	}
	quoted := make([]string, len(argv))
	for i, a := range argv {
		quoted[i] = fmt.Sprintf("%q", a)
	}
	return "argv [" + strings.Join(quoted, " ") + "]"
}

// renderedHostStrings is the golden document for goos.
func renderedHostStrings(t *testing.T, goos string) string {
	t.Helper()
	entry := hostStringsEntry(goos)
	var b strings.Builder
	w := func(format string, args ...any) { fmt.Fprintf(&b, format, args...) }
	section := func(title string) { w("\n== %s\n", title) }
	value := func(label, v string) { w("-- %s\n%s\n", label, v) }

	w("# Every string each host is handed on %s, as this binary renders it.\n", goos)
	w("# Characterization (H1): v0.2.9's renderings; H2 and H3 change them as reviewed diffs.\n")
	w("binary: %s\nconfig: %s\n", entry.command, entry.cfg)

	section("declaration")
	for _, tg := range targetsByKind(targetHost) {
		d, ok := tg.(shellDeclaringTarget)
		if !ok {
			t.Fatalf("%s declares no shells", tg.ID())
		}
		decl := d.Shells(goos)
		for _, c := range []struct {
			name string
			cell shellCell
		}{{"tool", decl.tool}, {"hook", decl.hook}} {
			shells := make([]string, len(c.cell.shells))
			for i, s := range c.cell.shells {
				shells[i] = string(s)
			}
			w("%s\n", strings.TrimRight(fmt.Sprintf("%-9s %s %-11s %s", tg.ID(), c.name, c.cell.evidence, strings.Join(shells, "+")), " "))
		}
	}

	section("any host: the search line agents install prints")
	value("searchCommand", entry.searchCommand())
	section("any host without a skill (opencode's AGENTS.md note, the rules line for any other agent)")
	value("rulesSnippet", rulesSnippet(entry))

	skill := renderedSkillFor("claude", entry)
	stdinSearch := skillSearchBlock(t, skill)
	for _, id := range goldenHostIDs {
		switch id {
		case "claude", "codex", "cursor", "pi", "hermes":
			skill := renderedSkillFor(id, entry)
			section(id + ": skill command blocks")
			for _, blk := range skillCommandBlocks(skill) {
				value("fence "+blk.lang, blk.body)
			}
			for _, l := range skillProseCommandLines(skill) {
				value("prose", l)
			}
		}
		switch id {
		case "claude":
			spec := claudeHooks(entry)
			file := installedHookFile(t, spec, entry)
			section("claude: settings.json hook commands, as read back from the file")
			for _, h := range installedHookCommands(t, file, spec) {
				literal, _ := json.Marshal(h.command)
				value(h.event, h.command+"\n"+string(literal))
			}
			section("claude: settings.json permissions.allow rules")
			for _, r := range spec.allow {
				literal, _ := json.Marshal(r)
				value("rule", r+"\n"+string(literal))
			}
		case "cursor":
			spec := cursorHooks(entry)
			file := installedHookFile(t, spec, entry)
			section("cursor: hooks.json commands, as read back from the file")
			for _, h := range installedHookCommands(t, file, spec) {
				literal, _ := json.Marshal(h.command)
				value(h.event, h.command+"\n"+string(literal))
			}
			section("cursor: beforeShellExecution recognizer grammar on " + goos)
			value("skill search block", recognizerVerdict(stdinSearch.body, goos))
			value("stdin command alone", recognizerVerdict(entry.stdinCommand(), goos))
			value("preference command", recognizerVerdict(entry.preferCommand()+" status", goos))
			value("human form", recognizerVerdict(entry.searchCommand()+` "exact query text"`, goos))
		case "hermes":
			section("hermes: config.yaml hook command")
			cmd, ok := hermesHookCommand(entry, goos == "windows")
			if !ok {
				t.Fatalf("hermes: no hook command for %s", goos)
			}
			value("command", cmd)
			value("yaml line", "    - command: "+hermesYAMLSingleQuoted(cmd))
		}
		switch id {
		case "claude", "hermes", "opencode", "pi":
			section(id + ": bridge prefix its lineage adapter writes")
			value("skill search block", withBridgePlaceholder(bridgedCommand(t, id, stdinSearch.body)))
			value("stdin command", withBridgePlaceholder(bridgedCommand(t, id, entry.stdinCommand())))
		}
	}
	return b.String()
}

func TestRenderedHostStringsGoldenPerOS(t *testing.T) {
	requireGoldenSequence(t)
	for _, goos := range hostShellOSes {
		t.Run(goos, func(t *testing.T) {
			got := renderedHostStrings(t, goos)
			path := filepath.Join("testdata", "hosts", goos+".golden")
			want, err := os.ReadFile(path) // #nosec G304 -- a fixed testdata path this test builds
			if err != nil {
				t.Fatalf("reading golden %s: %v", path, err)
			}
			if string(want) != got {
				t.Errorf("rendered host strings differ from %s:\n--- got ---\n%s\n--- want ---\n%s", path, got, want)
			}
		})
	}
}
