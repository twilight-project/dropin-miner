package main

// The agents command: make the miner's search the web search of every
// coding agent on this machine — with a skill and hooks, never a tool
// server.
//
//	agents install     detect Claude Code, Codex, Cursor and opencode on PATH
//	                   and give each a skill naming `dropin-miner search`,
//	                   plus the hooks that host supports
//	agents status      what is installed where
//	agents uninstall   take it all back out, and nothing else
//
// The shape is a staged plan: detection and file reads build a list of
// writes and removals, the plan is printed, and only then — after -yes or
// a prompt — is anything committed. -dry-run is the plan without the
// commit. Detection is a PATH lookup and nothing more; no agent is
// executed to find out whether it exists.
//
// What each host gets:
//
//	Claude Code   ~/.claude/skills/dropin-miner/SKILL.md, and five hook
//	              entries merged into ~/.claude/settings.json: PreToolUse on
//	              Bash (lineage), SessionStart / PreCompact / PostCompact
//	              (window), Stop (flush).
//	Codex         ~/.codex/skills/dropin-miner/SKILL.md.
//	Cursor        ~/.cursor/skills/dropin-miner/SKILL.md, and six entries
//	              merged into ~/.cursor/hooks.json: sessionStart,
//	              beforeShellExecution, afterAgentThought,
//	              afterAgentResponse, preCompact, stop.
//	opencode      an in-process plugin that prefixes our search command with
//	              the bridge, the way the Claude hook does, plus a line to
//	              paste into AGENTS.md (opencode has no skill directory).
//
// Every config edit is a JSON merge that adds our entries and nothing
// else, refuses a file that is not plain JSON rather than rewrite it
// without its comments, and removes on uninstall only entries whose
// command names this binary. The API key is in none of it: `search`
// resolves it at call time (environment, then the credentials file `login`
// wrote).

import (
	"bufio"
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

//go:embed skill.md
var skillMD string

//go:embed opencode_plugin.js
var opencodePluginJS string

const (
	agentsName        = "dropin-miner"
	agentsMarkerBegin = "# >>> dropin-miner agents install >>>"
	agentsMarkerEnd   = "# <<< dropin-miner agents install <<<"
)

type agentOps struct {
	home       string
	lookPath   func(string) (string, error)
	executable func() (string, error)
	readFile   func(string) ([]byte, error)
	writeFile  func(string, []byte, os.FileMode) error
	mkdirAll   func(string, os.FileMode) error
	stat       func(string) (os.FileInfo, error)
	removeAll  func(string) error
	isTerminal func() bool
}

func realAgentOps() agentOps {
	home, _ := os.UserHomeDir()
	return agentOps{
		home:       home,
		lookPath:   exec.LookPath,
		executable: os.Executable,
		readFile:   os.ReadFile,
		writeFile:  os.WriteFile,
		mkdirAll:   os.MkdirAll,
		stat:       os.Stat,
		removeAll:  os.RemoveAll,
		isTerminal: func() bool {
			fi, err := os.Stdin.Stat()
			return err == nil && fi.Mode()&os.ModeCharDevice != 0
		},
	}
}

type agentSurface struct{ id, label, probe string }

var agentSurfaces = []agentSurface{
	{"claude", "Claude Code", "claude"},
	{"codex", "Codex", "codex"},
	{"cursor", "Cursor", "cursor"},
	{"opencode", "opencode", "opencode"},
}

func surfaceByID(id string) (agentSurface, bool) {
	for _, s := range agentSurfaces {
		if s.id == id {
			return s, true
		}
	}
	return agentSurface{}, false
}

type agentPaths struct {
	claudeSkill    string
	claudeSettings string
	codexSkill     string
	cursorSkill    string
	cursorHooks    string
	opencodePlugin string
}

func (o agentOps) paths(getenv func(string) string) agentPaths {
	codexHome := getenv("CODEX_HOME")
	if codexHome == "" {
		codexHome = filepath.Join(o.home, ".codex")
	}
	xdg := getenv("XDG_CONFIG_HOME")
	if xdg == "" {
		xdg = filepath.Join(o.home, ".config")
	}
	claudeDir := getenv("CLAUDE_CONFIG_DIR")
	if claudeDir == "" {
		claudeDir = filepath.Join(o.home, ".claude")
	}
	return agentPaths{
		claudeSkill:    filepath.Join(claudeDir, "skills", agentsName, "SKILL.md"),
		claudeSettings: filepath.Join(claudeDir, "settings.json"),
		codexSkill:     filepath.Join(codexHome, "skills", agentsName, "SKILL.md"),
		cursorSkill:    filepath.Join(o.home, ".cursor", "skills", agentsName, "SKILL.md"),
		cursorHooks:    filepath.Join(o.home, ".cursor", "hooks.json"),
		opencodePlugin: filepath.Join(xdg, "opencode", "plugins", agentsName+".js"),
	}
}

// binEntry is how every host reaches the binary: its absolute path (agents
// do not inherit the user's PATH) and the config it should read.
type binEntry struct {
	command string
	cfg     string // absolute config path, or "" for discovery
}

// searchCommand is the exact invocation the skill teaches.
func (e binEntry) searchCommand() string {
	cmd := fmt.Sprintf("%q search", e.command)
	if e.cfg != "" {
		cmd += fmt.Sprintf(" -config %q", e.cfg)
	}
	return cmd + " -format model"
}

// hookCommand is what a host runs for one hook event.
func (e binEntry) hookCommand(sub ...string) string {
	cmd := fmt.Sprintf("%q hook", e.command)
	if e.cfg != "" {
		cmd += fmt.Sprintf(" -config %q", e.cfg)
	}
	return cmd + " " + strings.Join(sub, " ")
}

type agentWrite struct {
	surface  string
	path     string
	contents []byte
	mode     os.FileMode
	why      string
}

type agentPlan struct {
	writes  []agentWrite
	removes []string
	skipped []string
	refused []string
	notes   []string
}

func (p *agentPlan) empty() bool { return len(p.writes) == 0 && len(p.removes) == 0 }

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func cmdAgents(args []string, stdin io.Reader, stdout, stderr io.Writer, getenv func(string) string) int {
	return agentsMain(realAgentOps(), args, stdin, stdout, stderr, getenv)
}

const agentsUsage = `usage: dropin-miner agents install|status|uninstall [-config file] [-client name]... [-dry-run] [-yes]
  install     detect coding agents on PATH and give each the search skill and hooks
  status      what is installed where
  uninstall   remove exactly what install wrote
  -client     act on this agent only (claude, codex, cursor, opencode); repeatable
  -dry-run    print the plan, change nothing
  -yes        do not ask before writing
`

func agentsMain(ops agentOps, args []string, stdin io.Reader, stdout, stderr io.Writer, getenv func(string) string) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, agentsUsage)
		return exitUsage
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "install", "status", "uninstall":
	default:
		fmt.Fprintf(stderr, "dropin-miner agents: unknown subcommand %q\n%s", sub, agentsUsage)
		return exitUsage
	}
	fs := newFlagSet("agents "+sub, stderr)
	cfgPath := fs.String("config", "", "path to TOML config file the search and hooks should read")
	var clients multiFlag
	fs.Var(&clients, "client", "act on this agent only; repeatable")
	dryRun := fs.Bool("dry-run", false, "print the plan and change nothing")
	yes := fs.Bool("yes", false, "do not ask before writing")
	if err := fs.Parse(rest); err != nil {
		return exitUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "dropin-miner agents: unexpected argument %q\n", fs.Arg(0))
		return exitUsage
	}

	paths := ops.paths(getenv)
	selected, detected, err := selectSurfaces(ops, clients)
	if err != nil {
		fmt.Fprintln(stderr, "dropin-miner agents:", err)
		return exitUsage
	}
	entry, cfgNote, err := resolveEntry(ops, *cfgPath, getenv)
	if err != nil {
		fmt.Fprintln(stderr, "dropin-miner agents:", err)
		return exitTransport
	}

	if sub == "status" {
		printAgentStatus(ops, paths, entry, detected, stdout)
		return exitOK
	}

	var plan agentPlan
	if sub == "install" {
		plan = buildInstallPlan(ops, paths, selected, entry)
	} else {
		plan = buildUninstallPlan(ops, paths, selected, entry)
	}

	fmt.Fprintf(stdout, "dropin-miner agents %s\n", sub)
	if len(detected) == 0 && len(clients) == 0 {
		fmt.Fprintln(stdout, "  no coding agent found on PATH (looked for: claude, codex, cursor, opencode)")
	} else {
		fmt.Fprintf(stdout, "  agents: %s\n", strings.Join(labels(selected), ", "))
	}
	if sub == "install" {
		fmt.Fprintf(stdout, "  search: %s%s\n", entry.searchCommand(), cfgNote)
	}
	printPlan(&plan, ops.home, stdout)
	if len(selected) == 0 {
		fmt.Fprintln(stdout, "\nFor any other agent, add to its rules or AGENTS.md:")
		fmt.Fprintln(stdout, rulesSnippet(entry))
	}
	if plan.empty() {
		fmt.Fprintln(stdout, "\nnothing to do")
		return refusedExit(&plan)
	}
	if *dryRun {
		fmt.Fprintln(stdout, "\n(dry run: nothing was changed)")
		return exitOK
	}
	if !*yes {
		if !ops.isTerminal() {
			fmt.Fprintln(stderr, "dropin-miner agents: not a terminal, and -yes was not given; nothing was changed")
			return exitUsage
		}
		fmt.Fprint(stdout, "\nProceed? [Y/n]: ")
		line, _ := bufio.NewReader(stdin).ReadString('\n')
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "", "y", "yes":
		default:
			fmt.Fprintln(stdout, "left everything as it was")
			return exitOK
		}
	}

	failures := commitPlan(ops, &plan, stdout, stderr)
	if failures > 0 {
		return exitTransport
	}
	if sub == "install" {
		fmt.Fprintln(stdout, "\ndone. Restart any agent that is already open; check with: dropin-miner agents status")
	} else {
		fmt.Fprintln(stdout, "\ndone")
	}
	return refusedExit(&plan)
}

func refusedExit(p *agentPlan) int {
	if len(p.refused) > 0 {
		return exitTransport
	}
	return exitOK
}

func selectSurfaces(ops agentOps, clients []string) (selected, detected []agentSurface, err error) {
	for _, s := range agentSurfaces {
		if _, e := ops.lookPath(s.probe); e == nil {
			detected = append(detected, s)
		}
	}
	if len(clients) == 0 {
		return detected, detected, nil
	}
	for _, c := range clients {
		s, ok := surfaceByID(strings.ToLower(strings.TrimSpace(c)))
		if !ok {
			return nil, detected, fmt.Errorf("unknown -client %q (claude, codex, cursor, opencode)", c)
		}
		selected = append(selected, s)
	}
	return selected, detected, nil
}

func resolveEntry(ops agentOps, cfgPath string, getenv func(string) string) (binEntry, string, error) {
	bin, err := ops.executable()
	if err != nil {
		return binEntry{}, "", fmt.Errorf("cannot determine my own path: %w", err)
	}
	entry := binEntry{command: bin}
	src := cfgPath
	if src == "" {
		src = describeConfigSource("", getenv)
	}
	if src == "" {
		return entry, "  (no config file: defaults and TOKENDROP_* env)", nil
	}
	abs, err := filepath.Abs(src)
	if err != nil {
		return binEntry{}, "", fmt.Errorf("%s: %w", src, err)
	}
	entry.cfg = abs
	return entry, "  (config: " + abs + ")", nil
}

func labels(ss []agentSurface) []string {
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		out = append(out, s.label)
	}
	return out
}

func rulesSnippet(entry binEntry) string {
	return "  For public-web search, run: " + entry.searchCommand() + " \"<query>\"\n" +
		"  It prints provider-attributed results. Every search earns mining rewards for this machine.\n" +
		"  Needs the sr- key stored by `dropin-miner login` (or TOKENDROP_API_KEY in the environment)."
}

// ── install ─────────────────────────────────────────────────────────────

func renderSkill(entry binEntry) []byte {
	return []byte(strings.ReplaceAll(skillMD, "{{SEARCH}}", entry.searchCommand()))
}

func buildInstallPlan(ops agentOps, paths agentPaths, selected []agentSurface, entry binEntry) agentPlan {
	var p agentPlan
	for _, s := range selected {
		switch s.id {
		case "claude":
			changed := planWrite(ops, s.label, paths.claudeSkill, renderSkill(entry), 0o600, "skill", &p)
			if planHooksMerge(ops, s.label, paths.claudeSettings, &p, entry, claudeHooks(entry)) {
				changed = true
			}
			if !changed {
				p.skipped = append(p.skipped, s.label+": already installed")
			}
		case "codex":
			if !planWrite(ops, s.label, paths.codexSkill, renderSkill(entry), 0o600, "skill", &p) {
				p.skipped = append(p.skipped, s.label+": already installed")
			}
			p.notes = append(p.notes, s.label+": shell commands run sandboxed with no network by default; allow network for this command or searches fail silently")
		case "cursor":
			changed := planWrite(ops, s.label, paths.cursorSkill, renderSkill(entry), 0o600, "skill", &p)
			if planHooksMerge(ops, s.label, paths.cursorHooks, &p, entry, cursorHooks(entry)) {
				changed = true
			}
			if !changed {
				p.skipped = append(p.skipped, s.label+": already installed")
			}
		case "opencode":
			js := strings.ReplaceAll(opencodePluginJS, "{{BINARY}}", entry.command)
			if !planWrite(ops, s.label, paths.opencodePlugin, []byte(js), 0o600, "lineage plugin", &p) {
				p.skipped = append(p.skipped, s.label+": already installed")
			}
			p.notes = append(p.notes, s.label+": has no skill directory — add to AGENTS.md:\n"+rulesSnippet(entry))
		}
	}
	return p
}

// hooksSpec is one host's hook file, as the entries we want present:
// event name -> the group to append when no group of ours is there.
type hooksSpec struct {
	// root is the key the events live under ("hooks" on both hosts).
	root string
	// version, when non-zero, is written at the top level (Cursor).
	version int
	entries map[string]map[string]any
	// order keeps the plan deterministic.
	order []string
}

func claudeHooks(entry binEntry) hooksSpec {
	cmd := func(sub ...string) map[string]any {
		return map[string]any{"type": "command", "command": entry.hookCommand(sub...)}
	}
	group := func(matcher string, h map[string]any) map[string]any {
		g := map[string]any{"hooks": []any{h}}
		if matcher != "" {
			g["matcher"] = matcher
		}
		return g
	}
	return hooksSpec{
		root: "hooks",
		entries: map[string]map[string]any{
			"PreToolUse":   group("Bash", cmd("lineage")),
			"SessionStart": group("", cmd("window", "session-start")),
			"PreCompact":   group("", cmd("window", "pre-compact")),
			"PostCompact":  group("", cmd("window", "post-compact")),
			"Stop":         group("", cmd("flush")),
		},
		order: []string{"PreToolUse", "SessionStart", "PreCompact", "PostCompact", "Stop"},
	}
}

func cursorHooks(entry binEntry) hooksSpec {
	events := []string{"sessionStart", "beforeShellExecution", "afterAgentThought", "afterAgentResponse", "preCompact", "stop"}
	entries := map[string]map[string]any{}
	for _, ev := range events {
		entries[ev] = map[string]any{"command": entry.hookCommand("cursor", ev)}
	}
	return hooksSpec{root: "hooks", version: 1, entries: entries, order: events}
}

// entryIsOurs: does this hook entry (a Claude group or a Cursor entry)
// run this binary? Matching on the binary path is what makes uninstall
// exact and idempotent install cheap.
func entryIsOurs(e any, bin string) bool {
	m, ok := e.(map[string]any)
	if !ok {
		return false
	}
	if c, ok := m["command"].(string); ok && strings.Contains(c, bin) {
		return true
	}
	if hs, ok := m["hooks"].([]any); ok {
		for _, h := range hs {
			if hm, ok := h.(map[string]any); ok {
				if c, ok := hm["command"].(string); ok && strings.Contains(c, bin) {
					return true
				}
			}
		}
	}
	return false
}

// planHooksMerge adds our entries to a host's hook file, event by event,
// leaving everything else byte-for-byte as it was in the decoded object.
func planHooksMerge(ops agentOps, label, path string, p *agentPlan, entry binEntry, spec hooksSpec) bool {
	existing, mode, err := readWithMode(ops, path)
	if err != nil {
		p.refused = append(p.refused, fmt.Sprintf("%s: cannot read %s: %v", label, path, err))
		return false
	}
	m, err := decodeJSONObject(existing)
	if err != nil {
		p.refused = append(p.refused, fmt.Sprintf("%s: %s is not plain JSON (%v); add the hooks by hand", label, path, err))
		return false
	}
	if spec.version != 0 {
		if _, ok := m["version"]; !ok {
			m["version"] = spec.version
		}
	}
	hooks := child(m, spec.root)
	changed := false
	for _, ev := range spec.order {
		list, _ := hooks[ev].([]any)
		present := false
		for _, e := range list {
			if entryIsOurs(e, entry.command) {
				present = true
				break
			}
		}
		if present {
			continue
		}
		hooks[ev] = append(list, spec.entries[ev])
		changed = true
	}
	if !changed {
		return false
	}
	next, _ := json.MarshalIndent(m, "", "  ")
	return planWrite(ops, label, path, append(next, '\n'), mode, "hooks", p)
}

// planHooksRemove drops our entries and nothing else; an event left empty
// is removed, a file left with only an empty hooks object keeps it (the
// host may have created the file).
func planHooksRemove(ops agentOps, label, path string, p *agentPlan, bin, root string) bool {
	existing, mode, err := readWithMode(ops, path)
	if err != nil || existing == nil {
		return false
	}
	m, err := decodeJSONObject(existing)
	if err != nil {
		p.refused = append(p.refused, fmt.Sprintf("%s: %s is not plain JSON (%v); remove the hooks by hand", label, path, err))
		return false
	}
	hooks, ok := m[root].(map[string]any)
	if !ok {
		return false
	}
	changed := false
	for ev, v := range hooks {
		list, ok := v.([]any)
		if !ok {
			continue
		}
		kept := make([]any, 0, len(list))
		for _, e := range list {
			if entryIsOurs(e, bin) {
				changed = true
				continue
			}
			kept = append(kept, e)
		}
		if len(kept) == 0 {
			delete(hooks, ev)
		} else {
			hooks[ev] = kept
		}
	}
	if !changed {
		return false
	}
	next, _ := json.MarshalIndent(m, "", "  ")
	return planWrite(ops, label, path, append(next, '\n'), mode, "remove hooks", p)
}

// ── uninstall / status ──────────────────────────────────────────────────

func buildUninstallPlan(ops agentOps, paths agentPaths, selected []agentSurface, entry binEntry) agentPlan {
	var p agentPlan
	for _, s := range selected {
		removed := false
		rm := func(path string) {
			if _, err := ops.stat(path); err == nil {
				p.removes = append(p.removes, path)
				removed = true
			}
		}
		switch s.id {
		case "claude":
			rm(filepath.Dir(paths.claudeSkill))
			if planHooksRemove(ops, s.label, paths.claudeSettings, &p, entry.command, "hooks") {
				removed = true
			}
		case "codex":
			rm(filepath.Dir(paths.codexSkill))
		case "cursor":
			rm(filepath.Dir(paths.cursorSkill))
			if planHooksRemove(ops, s.label, paths.cursorHooks, &p, entry.command, "hooks") {
				removed = true
			}
		case "opencode":
			rm(paths.opencodePlugin)
		}
		if !removed {
			p.skipped = append(p.skipped, s.label+": not installed")
		}
	}
	return p
}

func printAgentStatus(ops agentOps, paths agentPaths, entry binEntry, detected []agentSurface, stdout io.Writer) {
	isDetected := map[string]bool{}
	for _, s := range detected {
		isDetected[s.id] = true
	}
	exists := func(path string) bool { _, err := ops.stat(path); return err == nil }
	hooked := func(path string) bool {
		b, _, err := readWithMode(ops, path)
		if err != nil || b == nil {
			return false
		}
		m, err := decodeJSONObject(b)
		if err != nil {
			return false
		}
		hooks, _ := m["hooks"].(map[string]any)
		for _, v := range hooks {
			if list, ok := v.([]any); ok {
				for _, e := range list {
					if entryIsOurs(e, entry.command) {
						return true
					}
				}
			}
		}
		return false
	}
	fmt.Fprintln(stdout, "dropin-miner agents status")
	for _, s := range agentSurfaces {
		state := "not installed"
		switch s.id {
		case "claude":
			switch {
			case exists(paths.claudeSkill) && hooked(paths.claudeSettings):
				state = "installed (skill+hooks)"
			case exists(paths.claudeSkill):
				state = "installed (skill only)"
			}
		case "codex":
			if exists(paths.codexSkill) {
				state = "installed (skill)"
			}
		case "cursor":
			switch {
			case exists(paths.cursorSkill) && hooked(paths.cursorHooks):
				state = "installed (skill+hooks)"
			case exists(paths.cursorSkill):
				state = "installed (skill only)"
			}
		case "opencode":
			if exists(paths.opencodePlugin) {
				state = "installed (plugin)"
			}
		}
		found := "not on PATH"
		if isDetected[s.id] {
			found = "on PATH"
		}
		fmt.Fprintf(stdout, "  %-12s %-12s %s\n", s.label, found, state)
	}
}

// ── plan mechanics ──────────────────────────────────────────────────────

func planWrite(ops agentOps, surface, path string, contents []byte, mode os.FileMode, why string, p *agentPlan) bool {
	if existing, err := ops.readFile(path); err == nil && bytes.Equal(existing, contents) {
		return false
	}
	p.writes = append(p.writes, agentWrite{surface: surface, path: path, contents: contents, mode: mode, why: why})
	return true
}

func readWithMode(ops agentOps, path string) ([]byte, os.FileMode, error) {
	b, err := ops.readFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, 0o600, nil
		}
		return nil, 0, err
	}
	mode := os.FileMode(0o600)
	if info, err := ops.stat(path); err == nil && info != nil {
		mode = info.Mode().Perm()
	}
	return b, mode, nil
}

func decodeJSONObject(b []byte) (map[string]any, error) {
	if len(bytes.TrimSpace(b)) == 0 {
		return map[string]any{}, nil
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, nil
}

func child(m map[string]any, key string) map[string]any {
	if c, ok := m[key].(map[string]any); ok {
		return c
	}
	c := map[string]any{}
	m[key] = c
	return c
}

func printPlan(p *agentPlan, home string, w io.Writer) {
	for _, s := range p.skipped {
		fmt.Fprintf(w, "  %s\n", s)
	}
	last := ""
	for _, wr := range p.writes {
		if wr.surface != last {
			fmt.Fprintf(w, "  %s\n", wr.surface)
			last = wr.surface
		}
		fmt.Fprintf(w, "    write  %s  (%s)\n", tilde(home, wr.path), wr.why)
	}
	for _, r := range p.removes {
		fmt.Fprintf(w, "    remove %s\n", tilde(home, r))
	}
	for _, r := range p.refused {
		fmt.Fprintf(w, "  refused: %s\n", r)
	}
	for _, n := range p.notes {
		fmt.Fprintf(w, "  %s\n", n)
	}
}

func commitPlan(ops agentOps, p *agentPlan, stdout, stderr io.Writer) int {
	failures := 0
	for _, wr := range p.writes {
		if err := ops.mkdirAll(filepath.Dir(wr.path), 0o700); err != nil {
			fmt.Fprintf(stderr, "dropin-miner agents: %s: %v\n", filepath.Dir(wr.path), err)
			failures++
			continue
		}
		if err := ops.writeFile(wr.path, wr.contents, wr.mode); err != nil {
			fmt.Fprintf(stderr, "dropin-miner agents: %s: %v\n", wr.path, err)
			failures++
			continue
		}
		fmt.Fprintf(stdout, "wrote %s\n", tilde(ops.home, wr.path))
	}
	for _, r := range p.removes {
		if err := ops.removeAll(r); err != nil {
			fmt.Fprintf(stderr, "dropin-miner agents: remove %s: %v\n", r, err)
			failures++
			continue
		}
		fmt.Fprintf(stdout, "removed %s\n", tilde(ops.home, r))
	}
	return failures
}

func tilde(home, path string) string {
	if home != "" && strings.HasPrefix(path, home+string(filepath.Separator)) {
		return "~" + path[len(home):]
	}
	return path
}
