package main

// The install-target registry: every installable surface behind one
// interface, so adding a seventh target means writing one type, not
// editing five places in agents.go's switches.

import (
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
)

// targetKind distinguishes a coding-agent host — something a participant
// runs interactively, that this client teaches to call `search` — from an
// integration, which reaches `search --stdin` some other way. No
// integration exists yet; the kind exists so one can be added later
// without the host-only commands (agents install|status|uninstall|prefer)
// accidentally picking it up.
type targetKind string

const (
	targetHost        targetKind = "host"
	targetIntegration targetKind = "integration"
)

// targetStatus is one target's install state, as printAgentStatus prints
// it: installed or not, and — when installed — which of its parts are
// present, so a half-installed host (a skill with no lineage channel, or
// the reverse) is reported as itself rather than as plain "installed".
type targetStatus struct {
	installed bool
	detail    string // parenthetical, e.g. "skill+hooks"; empty when not installed
}

// installTarget is everything the agents command needs from one
// installable surface. ID is the stable -client / -with value: it is
// never renamed, because it is how a participant and a script both name
// this target on the command line and it is what an uninstall matches a
// hook or allow-rule entry against.
type installTarget interface {
	ID() string
	Label() string
	Kind() targetKind
	Detect(ops agentOps, paths agentPaths, getenv func(string) string) bool
	PlanInstall(ops agentOps, paths agentPaths, entry binEntry, getenv func(string) string, p *agentPlan)
	PlanUninstall(ops agentOps, paths agentPaths, entry binEntry, p *agentPlan)
	Status(ops agentOps, paths agentPaths, entry binEntry) targetStatus
}

// preferenceTarget is the optional capability: a target whose installed
// skill carries the search-default preference. agentsPrefer needs more
// than rendered bytes — it rewrites the skill only where one is already
// installed — so the host owns the whole step: whether it has such a
// skill, where it lives, whether it exists now, and how to render it.
// opencode is a host without the capability; that fact lives here, in the
// type set, not in an id check anywhere.
type preferenceTarget interface {
	installTarget
	PlanPreference(ops agentOps, paths agentPaths, entry binEntry, prefer string, p *agentPlan)
}

// ── the shell each host runs ─────────────────────────────────────────────
//
// Every string this client renders for a host is run by something: a command
// the skill hands the host, by the shell the host executes tool calls in; a
// hook entry the install writes, by the host's hook runner. v0.2.9 rendered
// all of them with Go's %q and a Bash heredoc, as though every host on every
// OS ran Bash, and the soak's Windows defects (#66–#69) are that one
// assumption failing in four places. So each host declares, per OS, what
// actually runs each kind of string and where that fact comes from.
//
// The fence language our own skill writes is not evidence for either: a
// host that ran `bash` because our skill said `bash` has shown only that it
// follows fences. A cell is established by the host's documentation or
// source, or by a live run of the host.
//
// A cell names a SET of shells, not one (H-R5). Claude Code on Windows runs
// its Bash tool through Git Bash and its PowerShell tool through PowerShell,
// and which one a call uses is the model's choice: the skill teaches a
// runnable form for each. What a set means differs by channel — a tool cell
// lists every shell a call may arrive in, so the skill renders one form per
// shell; a hook cell lists every shell the one rendered command must be
// valid in, because the host picks and we never learn which.
//
// An unknown cell answers an *undeclaredShellError. What a caller does with
// it also differs by channel: a skill keeps v0.2.9's POSIX form and the
// install plan says the shell is not established (toolShellsForSkill),
// because refusing would take away a host that works today. A hook command
// has no such fallback; H3 owns that.

// shellKind is one grammar a rendered string may have to be valid in.
type shellKind string

const (
	// shellPOSIX is sh, bash or zsh running the string as -c text: the POSIX
	// grammar those three share for everything rendered here.
	shellPOSIX shellKind = "posix"
	// shellPowerShell is Windows PowerShell 5.1 and PowerShell 7 (pwsh). A
	// string declared for it must run under both: which one a host starts
	// depends on what is installed, not on anything this client controls.
	shellPowerShell shellKind = "powershell"
	// shellCmd is cmd.exe, as a Node or Win32 host starts it: /d /s /c.
	shellCmd shellKind = "cmd"
	// shellArgv is no shell at all: the host splits the string into an
	// argument vector itself and executes it directly (Hermes'
	// split_command_line, shell=False).
	shellArgv shellKind = "argv"
)

// shellEvidence says how a cell is known.
type shellEvidence string

const (
	// evidenceNone: the host has no such channel on this OS (Codex has no
	// hooks; opencode's and Pi's lineage run in-process, not as commands).
	evidenceNone shellEvidence = "none"
	// evidenceEstablished: the host's documentation or source, or a live run.
	evidenceEstablished shellEvidence = "established"
	// evidenceRuled: not directly observed; a maintainer ruling fixes the set
	// of shells a rendered string must be proven to run in, every one of them.
	evidenceRuled shellEvidence = "ruled"
	// evidenceUnknown: nothing establishes it. Renderers refuse this cell.
	evidenceUnknown shellEvidence = "unknown"
)

// shellCell is one declared fact: for one host on one OS, what runs one kind
// of string. shells lists every grammar a rendered string must be valid in;
// source names where the fact comes from, so a reviewer can check it.
type shellCell struct {
	evidence shellEvidence
	shells   []shellKind
	source   string
}

// hostShells is one host's declaration on one OS.
type hostShells struct {
	// tool runs a command the skill or the rules line hands the host.
	tool shellCell
	// hook runs a hook command the install writes into the host's config.
	hook shellCell
}

// shellDeclaringTarget is the capability every coding-agent host carries:
// what runs its strings on a given GOOS. An integration renders no host
// shell strings and does not implement it; TestEveryHostDeclaresItsShells
// holds every host to it.
type shellDeclaringTarget interface {
	installTarget
	Shells(goos string) hostShells
}

// shellChannel names which of a host's two cells a renderer is asking about.
type shellChannel string

const (
	channelTool shellChannel = "tool"
	channelHook shellChannel = "hook"
)

// undeclaredShellError is the refusal a renderer returns for a cell nothing
// has established: the install plan reports it instead of writing a string
// for a shell nobody has shown is the one that runs it.
type undeclaredShellError struct {
	host    string
	goos    string
	channel shellChannel
}

func (e *undeclaredShellError) Error() string {
	what := "tool calls"
	if e.channel == channelHook {
		what = "hook commands"
	}
	return fmt.Sprintf("%s on %s: which shell runs its %s is not established, so nothing is rendered for it", e.host, e.goos, what)
}

// declaredShells is the one question a renderer asks before writing a
// string for a host: the shells it must be valid in. A channel the host does
// not have on this OS answers nil and no error — there is nothing to render.
// An unknown cell, an OS the host declares nothing for, or a host that
// declares nothing at all is an *undeclaredShellError, never a default.
func declaredShells(t installTarget, goos string, ch shellChannel) ([]shellKind, error) {
	refuse := &undeclaredShellError{host: t.Label(), goos: goos, channel: ch}
	d, ok := t.(shellDeclaringTarget)
	if !ok {
		return nil, refuse
	}
	decl := d.Shells(goos)
	cell := decl.tool
	if ch == channelHook {
		cell = decl.hook
	}
	switch cell.evidence {
	case evidenceNone:
		return nil, nil
	case evidenceEstablished, evidenceRuled:
		if len(cell.shells) > 0 {
			return cell.shells, nil
		}
	}
	return nil, refuse
}

// The cells below are the H1 evidence table. Each source is short; the full
// quotes and links are in the commit that introduced this declaration.
var (
	cellUnknown   = shellCell{evidence: evidenceUnknown}
	cellNoChannel = shellCell{evidence: evidenceNone}
)

func established(source string, shells ...shellKind) shellCell {
	return shellCell{evidence: evidenceEstablished, shells: shells, source: source}
}

// installTargets is every target this binary knows how to install, in the
// order install, status, help and the detected-agents line report them.
// Registry order is part of the contract: claude, codex, cursor, opencode,
// pi, hermes.
var installTargets = []installTarget{
	claudeTarget{}, codexTarget{}, cursorTarget{}, opencodeTarget{}, piTarget{}, hermesTarget{},
}

// targetsByKind is the view a command operates on so it never touches the
// whole slice by accident: agents install|status|uninstall|prefer use
// targetsByKind(targetHost); nothing today asks for targetIntegration,
// because nothing installs through it yet. Registry order is preserved.
func targetsByKind(k targetKind) []installTarget {
	var out []installTarget
	for _, t := range installTargets {
		if t.Kind() == k {
			out = append(out, t)
		}
	}
	return out
}

// targetsByIDs resolves ids of any kind — PR B's setup -with. Explicit
// argument order is preserved and a repeated id is not deduplicated: both
// are exactly what agents -client (hostTargetsByIDs, below) already does,
// frozen here for the resolver PR B builds on. An unknown id names every
// registered id, of either kind.
func targetsByIDs(ids []string) ([]installTarget, error) {
	out := make([]installTarget, 0, len(ids))
	for _, raw := range ids {
		id := strings.ToLower(strings.TrimSpace(raw))
		t, ok := targetByID(installTargets, id)
		if !ok {
			return nil, fmt.Errorf("unknown id %q (%s)", raw, allTargetIDs())
		}
		out = append(out, t)
	}
	return out, nil
}

// hostTargetsByIDs resolves -client ids for the agents command: targetHost
// only, so an integration can never be reached through agents -client by
// accident (there is no -client-selectable integration yet, but the day
// one exists it stays setup -with's, not this command's). This is
// selectSurfaces' unknown-id error, verbatim: an id of the wrong kind is
// unknown from here exactly like an id that does not exist at all.
func hostTargetsByIDs(ids []string) ([]installTarget, error) {
	hosts := targetsByKind(targetHost)
	out := make([]installTarget, 0, len(ids))
	for _, raw := range ids {
		id := strings.ToLower(strings.TrimSpace(raw))
		t, ok := targetByID(hosts, id)
		if !ok {
			return nil, fmt.Errorf("unknown -client %q (%s)", raw, targetIDs(targetHost))
		}
		out = append(out, t)
	}
	return out, nil
}

// targetByID is the one place an id string is compared against user
// input; it is deliberately dumb (linear scan, no memoization) because the
// registry is six entries long and staying dumb is what keeps this the
// only place that comparison happens.
func targetByID(ts []installTarget, id string) (installTarget, bool) {
	for _, t := range ts {
		if t.ID() == id {
			return t, true
		}
	}
	return nil, false
}

// targetIDs is the help and error list for one kind: every -client value,
// in one place, so the help text, the unknown-id error and the
// nothing-detected line all read from the same list the installer itself
// iterates.
func targetIDs(k targetKind) string {
	var ids []string
	for _, t := range installTargets {
		if t.Kind() == k {
			ids = append(ids, t.ID())
		}
	}
	return strings.Join(ids, ", ")
}

// allTargetIDs is every registered id, of either kind — setup -with's
// error text (PR B).
func allTargetIDs() string {
	ids := make([]string, 0, len(installTargets))
	for _, t := range installTargets {
		ids = append(ids, t.ID())
	}
	return strings.Join(ids, ", ")
}

// pathExists is Status's exists() closure, shared across targets.
func pathExists(ops agentOps, path string) bool {
	_, err := ops.stat(path)
	return err == nil
}

// hooksHaveOurs reports whether a host's hook file (Claude Code's
// settings.json, Cursor's hooks.json) already carries an entry for this
// binary, for status's "skill+hooks" vs. "skill only" distinction.
func hooksHaveOurs(ops agentOps, path string, entry binEntry) bool {
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

// ── Claude Code ───────────────────────────────────────────────────────────

type claudeTarget struct{}

func (claudeTarget) ID() string       { return "claude" }
func (claudeTarget) Label() string    { return "Claude Code" }
func (claudeTarget) Kind() targetKind { return targetHost }

// Claude Code runs the Bash tool in the user's shell, and hooks through sh -c
// on macOS and Linux; on Windows both go through Git Bash. Its docs also say
// the Windows PowerShell tool is on by default for claude.ai accounts and is
// then "the primary shell", and that without Git for Windows PowerShell is
// the only one: the soak observed Git Bash, and that cell is a ruling
// question, not a settled one.
func (claudeTarget) Shells(goos string) hostShells {
	switch goos {
	case "darwin", "linux":
		return hostShells{
			tool: established("docs tools-reference (sources ~/.zshrc, ~/.bashrc or ~/.profile); live: soak #57 macOS", shellPOSIX),
			hook: established("docs hooks: \"sh -c on macOS and Linux\"; live: soak #57 macOS", shellPOSIX),
		}
	case "windows":
		return hostShells{
			// Two tools, two shells, and the model chooses per call (#77,
			// H-R5): the Bash tool runs through Git Bash, and the PowerShell
			// tool — on by default for claude.ai and Console accounts, and the
			// only one where Git for Windows is absent — runs through
			// PowerShell. A skill that taught only the heredoc would be wrong
			// for every call the model made with the second.
			tool: established("live: soak #57 Windows (Git Bash); docs setup: \"With Git for Windows, Claude Code uses Git Bash for the Bash tool\"; docs tools-reference: the PowerShell tool is \"on by default for claude.ai and Console accounts\" and \"Claude treats PowerShell as the primary shell\" when enabled", shellPOSIX, shellPowerShell),
			hook: established("docs hooks: \"Git Bash on Windows, or PowerShell when Git Bash isn't installed\"; live: soak #57 Windows", shellPOSIX),
		}
	}
	return hostShells{tool: cellUnknown, hook: cellUnknown}
}

func (claudeTarget) Detect(ops agentOps, _ agentPaths, _ func(string) string) bool {
	_, err := ops.lookPath("claude")
	return err == nil
}

func (t claudeTarget) PlanInstall(ops agentOps, paths agentPaths, entry binEntry, _ func(string) string, p *agentPlan) {
	prefer := readPrefer(ops, entry)
	changed := planSkill(ops, t, paths.claudeSkill, entry, prefer, "", p)
	if planHooksMerge(ops, t.Label(), paths.claudeSettings, p, entry, claudeHooks(entry)) {
		changed = true
	}
	if !changed {
		p.skipped = append(p.skipped, t.Label()+": already installed")
	}
}

func (t claudeTarget) PlanUninstall(ops agentOps, paths agentPaths, entry binEntry, p *agentPlan) {
	removed := false
	if pathExists(ops, filepath.Dir(paths.claudeSkill)) {
		p.removes = append(p.removes, filepath.Dir(paths.claudeSkill))
		removed = true
	}
	if planHooksRemove(ops, t.Label(), paths.claudeSettings, p, entry.command, "hooks") {
		removed = true
	}
	if !removed {
		p.skipped = append(p.skipped, t.Label()+": not installed")
	}
}

func (claudeTarget) Status(ops agentOps, paths agentPaths, entry binEntry) targetStatus {
	skill := pathExists(ops, paths.claudeSkill)
	hooked := hooksHaveOurs(ops, paths.claudeSettings, entry)
	switch {
	case skill && hooked:
		return targetStatus{true, "skill+hooks"}
	case skill:
		return targetStatus{true, "skill only"}
	}
	return targetStatus{}
}

func (t claudeTarget) PlanPreference(ops agentOps, paths agentPaths, entry binEntry, prefer string, p *agentPlan) {
	if !pathExists(ops, paths.claudeSkill) {
		return
	}
	planSkill(ops, t, paths.claudeSkill, entry, prefer, "", p)
}

// ── Codex ─────────────────────────────────────────────────────────────────

type codexTarget struct{}

func (codexTarget) ID() string       { return "codex" }
func (codexTarget) Label() string    { return "Codex" }
func (codexTarget) Kind() targetKind { return targetHost }

// Codex runs a command through the user's default shell on macOS and Linux.
// On Windows its source defaults to PowerShell, but the soak's Codex ran
// `bash` (the WSL launcher) because our fence said bash, and no live run has
// shown what it does with a PowerShell-fenced skill: unknown until one does.
// Codex has no hooks.
func (codexTarget) Shells(goos string) hostShells {
	switch goos {
	case "darwin", "linux":
		return hostShells{
			tool: established("source codex-rs shell_detect.rs default_user_shell (user's shell, else zsh/bash); live: soak #57 macOS", shellPOSIX),
			hook: cellNoChannel,
		}
	case "windows":
		return hostShells{tool: cellUnknown, hook: cellNoChannel}
	}
	return hostShells{tool: cellUnknown, hook: cellNoChannel}
}

func (codexTarget) Detect(ops agentOps, _ agentPaths, _ func(string) string) bool {
	_, err := ops.lookPath("codex")
	return err == nil
}

func (t codexTarget) PlanInstall(ops agentOps, paths agentPaths, entry binEntry, getenv func(string) string, p *agentPlan) {
	prefer := readPrefer(ops, entry)
	if !planSkill(ops, t, paths.codexSkill, entry, prefer, "", p) {
		p.skipped = append(p.skipped, t.Label()+": already installed")
	}
	if roots := codexSandboxRoots(entry, getenv); len(roots) > 0 {
		planCodexSandbox(ops, t.Label(), paths.codexConfig, roots, p)
	} else {
		p.notes = append(p.notes, t.Label()+": shell commands run sandboxed; if searches record nothing, allow this command network access and let it write to your tokendrop home")
	}
}

func (t codexTarget) PlanUninstall(ops agentOps, paths agentPaths, _ binEntry, p *agentPlan) {
	removed := false
	if pathExists(ops, filepath.Dir(paths.codexSkill)) {
		p.removes = append(p.removes, filepath.Dir(paths.codexSkill))
		removed = true
	}
	if existing, mode, err := readWithMode(ops, paths.codexConfig); err == nil && existing != nil {
		if next, had := removeMarkedBlock(existing); had {
			planWrite(ops, t.Label(), paths.codexConfig, next, mode, "remove sandbox block", p)
			removed = true
		}
	}
	if !removed {
		p.skipped = append(p.skipped, t.Label()+": not installed")
	}
}

func (codexTarget) Status(ops agentOps, paths agentPaths, _ binEntry) targetStatus {
	if pathExists(ops, paths.codexSkill) {
		return targetStatus{true, "skill"}
	}
	return targetStatus{}
}

func (t codexTarget) PlanPreference(ops agentOps, paths agentPaths, entry binEntry, prefer string, p *agentPlan) {
	if !pathExists(ops, paths.codexSkill) {
		return
	}
	planSkill(ops, t, paths.codexSkill, entry, prefer, "", p)
}

// ── Cursor ────────────────────────────────────────────────────────────────

type cursorTarget struct{}

func (cursorTarget) ID() string       { return "cursor" }
func (cursorTarget) Label() string    { return "Cursor" }
func (cursorTarget) Kind() targetKind { return targetHost }

// Cursor's agent runs commands in the user's terminal shell on macOS and
// Linux, and in PowerShell on Windows (the CLI's ps-script-*.ps1; the
// editor's agent "defaults to PowerShell no matter what terminal profile you've
// set"). Its hooks ran on macOS; on Linux nothing names the runner; on Windows
// no hook was observed live, and the hooks.json string fails to parse as
// PowerShell and runs under cmd — so the Windows hook cell is ruled rather
// than observed: a hook command must be proven under cmd and both
// PowerShell editions.
func (cursorTarget) Shells(goos string) hostShells {
	switch goos {
	case "darwin":
		return hostShells{
			tool: established("live: soak #57/#66 macOS (Cursor CLI ran the heredoc search)", shellPOSIX),
			hook: established("live: soak #61 macOS (sessionStart and afterAgentThought fired a command beginning with a quoted path)", shellPOSIX),
		}
	case "linux":
		return hostShells{
			tool: established("docs agent/terminal (commands run in your terminal; ~/.zshrc and ~/.bashrc guidance for Cursor sessions)", shellPOSIX),
			hook: cellUnknown,
		}
	case "windows":
		return hostShells{
			tool: established("live: soak #67 Windows (Cursor CLI, ps-script-*.ps1); forum.cursor.com/t/154914 staff: agent shell \"defaults to PowerShell\"", shellPowerShell),
			hook: shellCell{
				evidence: evidenceRuled,
				shells:   []shellKind{shellCmd, shellPowerShell},
				source:   "no hook observed live (#69); the hooks.json string fails as PowerShell and runs under cmd (Windows team, sitting 2); ruled: proven under cmd, PowerShell 5.1 and pwsh",
			},
		}
	}
	return hostShells{tool: cellUnknown, hook: cellUnknown}
}

func (cursorTarget) Detect(ops agentOps, _ agentPaths, _ func(string) string) bool {
	_, err := ops.lookPath("cursor")
	return err == nil
}

func (t cursorTarget) PlanInstall(ops agentOps, paths agentPaths, entry binEntry, _ func(string) string, p *agentPlan) {
	prefer := readPrefer(ops, entry)
	changed := planSkill(ops, t, paths.cursorSkill, entry, prefer, "", p)
	if planHooksMerge(ops, t.Label(), paths.cursorHooks, p, entry, cursorHooks(entry)) {
		changed = true
	}
	if !changed {
		p.skipped = append(p.skipped, t.Label()+": already installed")
	}
}

func (t cursorTarget) PlanUninstall(ops agentOps, paths agentPaths, entry binEntry, p *agentPlan) {
	removed := false
	if pathExists(ops, filepath.Dir(paths.cursorSkill)) {
		p.removes = append(p.removes, filepath.Dir(paths.cursorSkill))
		removed = true
	}
	if planHooksRemove(ops, t.Label(), paths.cursorHooks, p, entry.command, "hooks") {
		removed = true
	}
	if !removed {
		p.skipped = append(p.skipped, t.Label()+": not installed")
	}
}

func (cursorTarget) Status(ops agentOps, paths agentPaths, entry binEntry) targetStatus {
	skill := pathExists(ops, paths.cursorSkill)
	hooked := hooksHaveOurs(ops, paths.cursorHooks, entry)
	switch {
	case skill && hooked:
		return targetStatus{true, "skill+hooks"}
	case skill:
		return targetStatus{true, "skill only"}
	}
	return targetStatus{}
}

func (t cursorTarget) PlanPreference(ops agentOps, paths agentPaths, entry binEntry, prefer string, p *agentPlan) {
	if !pathExists(ops, paths.cursorSkill) {
		return
	}
	planSkill(ops, t, paths.cursorSkill, entry, prefer, "", p)
}

// ── opencode ──────────────────────────────────────────────────────────────
//
// opencode is a host without the preference capability: it has no skill
// directory, only a plugin and an AGENTS.md line, neither of which
// carries preference text. It does not implement preferenceTarget.

type opencodeTarget struct{}

func (opencodeTarget) ID() string       { return "opencode" }
func (opencodeTarget) Label() string    { return "opencode" }
func (opencodeTarget) Kind() targetKind { return targetHost }

// opencode runs its bash tool in $SHELL on macOS and Linux (falling back to
// zsh on macOS, bash, then sh), and on Windows in the first of pwsh,
// powershell, Git Bash and cmd it finds. Its lineage plugin runs in-process:
// there is no hook command.
func (opencodeTarget) Shells(goos string) hostShells {
	switch goos {
	case "darwin", "linux":
		return hostShells{
			tool: established("source packages/core/src/shell.ts select($SHELL), fallback /bin/zsh (darwin), bash, /bin/sh", shellPOSIX),
			hook: cellNoChannel,
		}
	case "windows":
		return hostShells{
			tool: established("live: soak #67/#68 Windows (PowerShell); source shell.ts win(): pwsh, powershell, Git Bash, cmd", shellPowerShell),
			hook: cellNoChannel,
		}
	}
	return hostShells{tool: cellUnknown, hook: cellNoChannel}
}

func (opencodeTarget) Detect(ops agentOps, _ agentPaths, _ func(string) string) bool {
	_, err := ops.lookPath("opencode")
	return err == nil
}

func (t opencodeTarget) PlanInstall(ops agentOps, paths agentPaths, entry binEntry, _ func(string) string, p *agentPlan) {
	js := renderAgentScript(opencodePluginJS)
	if !planWrite(ops, t.Label(), paths.opencodePlugin, []byte(js), 0o600, "lineage plugin", p) {
		p.skipped = append(p.skipped, t.Label()+": already installed")
	}
	shells, shellNote := toolShellsForSkill(t, runtime.GOOS)
	if shellNote != "" {
		p.notes = append(p.notes, shellNote)
	}
	p.notes = append(p.notes, t.Label()+": has no skill directory — add to AGENTS.md:\n"+rulesSnippetFor(entry, shells))
}

func (t opencodeTarget) PlanUninstall(ops agentOps, paths agentPaths, _ binEntry, p *agentPlan) {
	if pathExists(ops, paths.opencodePlugin) {
		p.removes = append(p.removes, paths.opencodePlugin)
		return
	}
	p.skipped = append(p.skipped, t.Label()+": not installed")
}

func (opencodeTarget) Status(ops agentOps, paths agentPaths, _ binEntry) targetStatus {
	if pathExists(ops, paths.opencodePlugin) {
		return targetStatus{true, "plugin"}
	}
	return targetStatus{}
}

// ── Pi ────────────────────────────────────────────────────────────────────

type piTarget struct{}

func (piTarget) ID() string       { return "pi" }
func (piTarget) Label() string    { return "Pi" }
func (piTarget) Kind() targetKind { return targetHost }

// Pi runs its bash tool in /bin/bash (else bash on PATH, else sh), and on
// Windows in Git Bash. Its lineage extension runs in-process: no hook command.
func (piTarget) Shells(goos string) hostShells {
	switch goos {
	case "darwin", "linux":
		return hostShells{
			tool: established("source packages/coding-agent/src/utils/shell.ts getShellConfig: /bin/bash, bash on PATH, sh", shellPOSIX),
			hook: cellNoChannel,
		}
	case "windows":
		return hostShells{
			tool: established("live: soak #57 Windows (Bash); docs coding-agent/docs/windows.md: \"Pi uses Git Bash by default on Windows\"", shellPOSIX),
			hook: cellNoChannel,
		}
	}
	return hostShells{tool: cellUnknown, hook: cellNoChannel}
}

func (piTarget) Detect(ops agentOps, _ agentPaths, _ func(string) string) bool {
	_, err := ops.lookPath("pi")
	return err == nil
}

func (t piTarget) PlanInstall(ops agentOps, paths agentPaths, entry binEntry, _ func(string) string, p *agentPlan) {
	prefer := readPrefer(ops, entry)
	changed := planSkill(ops, t, paths.piSkill, entry, prefer, "", p)
	if planWrite(ops, t.Label(), paths.piExtension, []byte(renderAgentScript(piExtensionTS)), 0o600, "lineage extension", p) {
		changed = true
	}
	if !changed {
		p.skipped = append(p.skipped, t.Label()+": already installed")
	}
}

func (t piTarget) PlanUninstall(ops agentOps, paths agentPaths, _ binEntry, p *agentPlan) {
	removed := false
	if pathExists(ops, filepath.Dir(paths.piSkill)) {
		p.removes = append(p.removes, filepath.Dir(paths.piSkill))
		removed = true
	}
	if pathExists(ops, paths.piExtension) {
		p.removes = append(p.removes, paths.piExtension)
		removed = true
	}
	if !removed {
		p.skipped = append(p.skipped, t.Label()+": not installed")
	}
}

func (piTarget) Status(ops agentOps, paths agentPaths, _ binEntry) targetStatus {
	skill := pathExists(ops, paths.piSkill)
	ext := pathExists(ops, paths.piExtension)
	switch {
	case skill && ext:
		return targetStatus{true, "skill+extension"}
	case skill:
		return targetStatus{true, "skill only"}
	case ext:
		return targetStatus{true, "extension only"}
	}
	return targetStatus{}
}

func (t piTarget) PlanPreference(ops agentOps, paths agentPaths, entry binEntry, prefer string, p *agentPlan) {
	if !pathExists(ops, paths.piSkill) {
		return
	}
	planSkill(ops, t, paths.piSkill, entry, prefer, "", p)
}

// ── Hermes ────────────────────────────────────────────────────────────────

type hermesTarget struct{}

func (hermesTarget) ID() string       { return "hermes" }
func (hermesTarget) Label() string    { return "Hermes" }
func (hermesTarget) Kind() targetKind { return targetHost }

// Hermes runs its terminal tool in bash, and on Windows in Git Bash. Its
// hooks are not run by a shell: split_command_line tokenizes the command and
// it is executed with shell=False, on every OS.
func (hermesTarget) Shells(goos string) hostShells {
	hook := established("source agent/shell_hooks.py: split_command_line, subprocess.Popen(argv, shell=False)", shellArgv)
	switch goos {
	case "darwin", "linux":
		return hostShells{
			tool: established("source tools/environments/local.py _find_bash: bash on PATH, /usr/bin/bash, /bin/bash", shellPOSIX),
			hook: hook,
		}
	case "windows":
		return hostShells{
			tool: established("live: soak #57 Windows (Bash); docs windows-native.md: \"Hermes's terminal tool runs commands through Git Bash\"", shellPOSIX),
			hook: hook,
		}
	}
	return hostShells{tool: cellUnknown, hook: cellUnknown}
}

func (hermesTarget) Detect(ops agentOps, _ agentPaths, _ func(string) string) bool {
	_, err := ops.lookPath("hermes")
	return err == nil
}

func (t hermesTarget) PlanInstall(ops agentOps, paths agentPaths, entry binEntry, _ func(string) string, p *agentPlan) {
	prefer := readPrefer(ops, entry)
	changed := planSkill(ops, t, paths.hermesSkill, entry, prefer, hermesApprovalNote, p)
	if planHermesHook(ops, t.Label(), paths.hermesConfig, entry, p) {
		changed = true
	}
	if !changed {
		p.skipped = append(p.skipped, t.Label()+": already installed")
	}
	p.notes = append(p.notes, t.Label()+": takes effect next session; Hermes asks once to approve the hook the first time it fires — approve it, or launch with --accept-hooks. Its shell tool is in the terminal/coding toolsets.")
}

func (t hermesTarget) PlanUninstall(ops agentOps, paths agentPaths, entry binEntry, p *agentPlan) {
	removed := false
	if pathExists(ops, filepath.Dir(paths.hermesSkill)) {
		p.removes = append(p.removes, filepath.Dir(paths.hermesSkill))
		removed = true
	}
	if existing, mode, err := readWithMode(ops, paths.hermesConfig); err == nil && existing != nil {
		if next, had := hermesRemoveBlock(existing); had {
			planWrite(ops, t.Label(), paths.hermesConfig, next, mode, "remove lineage hook", p)
			removed = true
		}
	}
	if !removed {
		p.skipped = append(p.skipped, t.Label()+": not installed")
	}
}

func (hermesTarget) Status(ops agentOps, paths agentPaths, entry binEntry) targetStatus {
	skill := pathExists(ops, paths.hermesSkill)
	hooked := hermesHookInstalled(ops, paths.hermesConfig, entry)
	switch {
	case skill && hooked:
		return targetStatus{true, "skill+hook"}
	case skill:
		return targetStatus{true, "skill only"}
	case hooked:
		return targetStatus{true, "hook only"}
	}
	return targetStatus{}
}

func (t hermesTarget) PlanPreference(ops agentOps, paths agentPaths, entry binEntry, prefer string, p *agentPlan) {
	if !pathExists(ops, paths.hermesSkill) {
		return
	}
	planSkill(ops, t, paths.hermesSkill, entry, prefer, hermesApprovalNote, p)
}
