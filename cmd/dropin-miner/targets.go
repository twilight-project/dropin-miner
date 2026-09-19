package main

// The install-target registry: every installable surface behind one
// interface, so adding a seventh target means writing one type, not
// editing five places in agents.go's switches.

import (
	"fmt"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
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
//
// Detect answers with the SIGNAL that says the host is in use on this
// machine — "cursor-agent on PATH", "~/.cursor" — and the empty string when
// nothing does. It is a signal rather than a bool because the participant is
// entitled to know why a host was or was not offered: setup's "Found on this
// machine" and `agents status` both print it, and #61 was reported as
// "Cursor not on PATH" on a machine where Cursor was plainly installed.
type installTarget interface {
	ID() string
	Label() string
	Kind() targetKind
	Detect(ops agentOps, paths agentPaths, getenv func(string) string) string
	PlanInstall(ops agentOps, paths agentPaths, entry binEntry, getenv func(string) string, p *agentPlan)
	PlanUninstall(ops agentOps, paths agentPaths, entry binEntry, getenv func(string) string, p *agentPlan)
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

// shellChoice says who decides which of a cell's shells runs a given call.
// It matters only where a cell names more than one, and only to the words
// the skill puts above each block: whoever reads that block has to recognize
// their own situation in it, and the two situations are not the same one.
type shellChoice string

const (
	// chosenPerCall: the host offers several tools, the model picks one per
	// call, and the call itself says which — Claude Code's PreToolUse payload
	// names the tool. The label can therefore speak about the call.
	chosenPerCall shellChoice = "per-call"
	// chosenByParticipant: one shell runs every call, and which one is the
	// participant's own configuration — Cursor's editor runs its terminal
	// according to terminal.integrated.defaultProfile.windows (#96). Nothing
	// in the call says which it is and nothing this client installs can, so
	// the label has to speak about the machine instead.
	chosenByParticipant shellChoice = "participant"
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
	// choice says who picks, and is required of every cell naming more than
	// one tool shell (TestEveryMultiShellToolCellSaysWhoChooses). A cell
	// naming one shell leaves it empty: there is nothing to pick between.
	choice shellChoice
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

// establishedChosenBy is established() for a cell that names more than one
// shell, where who picks between them is part of the fact being declared.
func establishedChosenBy(choice shellChoice, source string, shells ...shellKind) shellCell {
	c := established(source, shells...)
	c.choice = choice
	return c
}

// toolShellChoice is who picks, among the tool shells t declares on goos.
// A cell that names one shell, or none this client can render for, answers
// "": there is nothing to pick between, and nothing to label.
func toolShellChoice(t installTarget, goos string) shellChoice {
	d, ok := t.(shellDeclaringTarget)
	if !ok {
		return ""
	}
	return d.Shells(goos).tool.choice
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

// detectCommand is the signal every host but Cursor still answers with: the
// command it is launched by, found on PATH. The names are tried in order and
// the first that resolves is the signal, so a host reachable under two names
// reports the one the participant actually has.
func detectCommand(ops agentOps, names ...string) string {
	for _, n := range names {
		if _, err := ops.lookPath(n); err == nil {
			return n + " on PATH"
		}
	}
	return ""
}

// detectConfigDir is the second kind of signal: the directory the host keeps
// its own configuration in. A populated config directory is the participant
// using the host; the shell command is incidental, and for two of Cursor's
// three configurations it is simply absent (#61).
func detectConfigDir(ops agentOps, dir string) string {
	if !pathExists(ops, dir) {
		return ""
	}
	return tilde(ops.home, dir)
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
				if entryIsOurs(e, refFor(entry)) {
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
			tool: establishedChosenBy(chosenPerCall, "live: soak #57 Windows (Git Bash); docs setup: \"With Git for Windows, Claude Code uses Git Bash for the Bash tool\"; docs tools-reference: the PowerShell tool is \"on by default for claude.ai and Console accounts\" and \"Claude treats PowerShell as the primary shell\" when enabled", shellPOSIX, shellPowerShell),
			hook: established("docs hooks: \"Git Bash on Windows, or PowerShell when Git Bash isn't installed\"; live: soak #57 Windows", shellPOSIX),
		}
	}
	return hostShells{tool: cellUnknown, hook: cellUnknown}
}

func (claudeTarget) Detect(ops agentOps, _ agentPaths, _ func(string) string) string {
	return detectCommand(ops, "claude")
}

func (t claudeTarget) PlanInstall(ops agentOps, paths agentPaths, entry binEntry, _ func(string) string, p *agentPlan) {
	prefer := readPrefer(ops, entry)
	changed, left := planSkill(ops, t, paths.claudeSkill, entry, prefer, "", p)
	if spec, err := claudeHooksFor(t, entry, runtime.GOOS); err != nil {
		p.refused = append(p.refused, fmt.Sprintf("%s: %v", t.Label(), err))
	} else if planHooksMerge(ops, t.Label(), paths.claudeSettings, p, entry, spec) {
		changed = true
	}
	if !changed && !left {
		p.skipped = append(p.skipped, t.Label()+": already installed")
	}
}

func (t claudeTarget) PlanUninstall(ops agentOps, paths agentPaths, entry binEntry, getenv func(string) string, p *agentPlan) {
	removed := false
	if pathExists(ops, filepath.Dir(paths.claudeSkill)) {
		planRemove(p, t.Label(), filepath.Dir(paths.claudeSkill))
		removed = true
	}
	if planHooksRemove(ops, t.Label(), paths.claudeSettings, p, entry, "hooks") {
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
// On Windows it runs Windows PowerShell 5.1: the soak's Codex ran `bash` —
// the WSL launcher, which fails — only because our fence said bash, and the
// follow-up run settled it by fencing the other way. That is the cell's
// whole content: what the host does with the block we actually render, not
// what its source defaults to. Codex has no hooks.
func (codexTarget) Shells(goos string) hostShells {
	switch goos {
	case "darwin", "linux":
		return hostShells{
			tool: established("source codex-rs shell_detect.rs default_user_shell (user's shell, else zsh/bash); live: soak #57 macOS", shellPOSIX),
			hook: cellNoChannel,
		}
	case "windows":
		return hostShells{
			tool: established("live: Windows soak follow-up 2026-09-16, codex-cli 0.154.0, both codex exec and the TUI", shellPowerShell),
			hook: cellNoChannel,
		}
	}
	return hostShells{tool: cellUnknown, hook: cellNoChannel}
}

func (codexTarget) Detect(ops agentOps, _ agentPaths, _ func(string) string) string {
	return detectCommand(ops, "codex")
}

// Codex has two halves — a skill and the sandbox block — and "already
// installed" is a claim about both. It used to be decided by the skill alone
// and printed before the block was even planned, so a host whose block is
// another installation's was reported as already installed AND left in place,
// in one plan, for one host. Both halves answer now, and the line is printed
// only when neither of them had anything to do.
func (t codexTarget) PlanInstall(ops agentOps, paths agentPaths, entry binEntry, getenv func(string) string, p *agentPlan) {
	prefer := readPrefer(ops, entry)
	skillChanged, skillLeft := planSkill(ops, t, paths.codexSkill, entry, prefer, "", p)
	blockChanged, blockLeft := false, false
	if roots := codexSandboxRoots(entry, getenv); len(roots) > 0 {
		blockChanged, blockLeft = planCodexSandbox(ops, t.Label(), paths.codexConfig, roots, entry, getenv, p)
	} else {
		p.notes = append(p.notes, t.Label()+": shell commands run sandboxed; if searches record nothing, allow this command network access and let it write to your tokendrop home")
	}
	if !skillChanged && !skillLeft && !blockChanged && !blockLeft {
		p.skipped = append(p.skipped, t.Label()+": already installed")
	}
}

func (t codexTarget) PlanUninstall(ops agentOps, paths agentPaths, entry binEntry, getenv func(string) string, p *agentPlan) {
	removed := false
	if pathExists(ops, filepath.Dir(paths.codexSkill)) {
		planRemove(p, t.Label(), filepath.Dir(paths.codexSkill))
		removed = true
	}
	if existing, mode, err := readWithMode(ops, paths.codexConfig); err == nil && existing != nil {
		switch r := removeOurSandboxBlock(existing, entry, getenv); {
		case r.had && r.ours:
			if len(r.kept) > 0 {
				p.notes = append(p.notes, fmt.Sprintf("%s: keeping %s in %s that dropin-miner did not write: %s",
					t.Label(), tables(len(r.kept)), paths.codexConfig, strings.Join(r.kept, ", ")))
			}
			if len(r.dropped) > 0 {
				p.notes = append(p.notes, droppedKeysNote(t.Label(), paths.codexConfig, r.dropped))
			}
			planWrite(ops, t.Label(), paths.codexConfig, r.next, mode, "remove sandbox block", p)
			removed = true
		case r.had:
			p.notes = append(p.notes, t.Label()+": left the sandbox block in "+paths.codexConfig+": "+r.why)
		}
	}
	if !removed {
		p.skipped = append(p.skipped, t.Label()+": not installed")
	}
}

// sandboxRemoval is what uninstall concluded about Codex's config.toml.
type sandboxRemoval struct {
	next []byte   // the file with our own tables gone
	had  bool     // a marked block was there at all
	ours bool     // it is this installation's, and next may be written
	kept []string // tables inside the markers this client did not write
	why  string   // why it was left, when ours is false
	// dropped is the keys inside OUR table the renderer does not write,
	// which go with the table and are named in the plan (keysWeDidNotWrite).
	dropped []string
}

// removeOurSandboxBlock takes out the marked [sandbox_workspace_write] table
// only when it is this installation's, and only that table.
//
// Attribution is H5's, unchanged: the block names no binary and no config —
// it names DIRECTORIES — so its writable roots are read, and roots that do
// not lie under this installation's home belong to another installation
// whose searches would go silent if this one removed them (#73).
//
// What is new is #82. Codex appends its own tables to the end of
// config.toml, which put them INSIDE our markers whenever our block was last
// — which install made it — and v0.2.9 deleted the marker-to-marker byte
// range. The tester's uninstall left a 0-byte file: folder trust and
// `[windows] sandbox = "unelevated"` gone, and the following setup restored
// only our own block. So the tables inside the markers are separated by who
// wrote them, ours go, and every other one survives in its original bytes,
// appended below where the block was.
func removeOurSandboxBlock(existing []byte, entry binEntry, getenv func(string) string) sandboxRemoval {
	stripped, had := removeMarkedBlock(existing)
	if !had {
		return sandboxRemoval{next: existing}
	}
	_, region, _, _ := markedRegion(existing)
	contents, readable := splitCodexBlock(region)
	if !readable {
		return sandboxRemoval{next: existing, had: true,
			why: "it cannot be read as TOML tables, so which of them are ours cannot be decided; remove it by hand"}
	}
	// The one reading install refuses by (codexBlockOwner), so a block this
	// installation's install left cannot be one its uninstall then removes.
	// Where the owner can be named, the reason is the sentence the skill's
	// own refusal uses; where it cannot, it says what it could not read.
	if ours, other, why := codexBlockOwner(contents.oursText(), entry, getenv); !ours {
		if other != "" {
			why = belongsTo(other)
		}
		return sandboxRemoval{next: existing, had: true, why: why}
	}
	return sandboxRemoval{
		next:    appendTables(stripped, contents.foreignText()),
		had:     true,
		ours:    true,
		kept:    contents.foreignNames(),
		dropped: keysWeDidNotWrite(contents.oursText()),
	}
}

// markedSandboxRoots reads the writable_roots out of OUR table inside the
// marked block, in the one spelling sandboxSettings writes them: a single
// line of %q-quoted paths. It is given our table's text rather than the
// whole block, so a writable_roots line in a table somebody else appended
// into the block cannot be read as ours (#82).
func markedSandboxRoots(block string) []string {
	m := sandboxRootsLine.FindStringSubmatch(block)
	if m == nil {
		return nil
	}
	var out []string
	for _, q := range sandboxRootQuoted.FindAllString(m[1], -1) {
		if p, err := strconv.Unquote(q); err == nil {
			out = append(out, p)
		}
	}
	return out
}

var (
	sandboxRootsLine  = regexp.MustCompile(`(?m)^writable_roots\s*=\s*\[([^\]]*)\]`)
	sandboxRootQuoted = regexp.MustCompile(`"(?:[^"\\]|\\.)*"`)
)

// pathUnder: is p inside dir, or dir itself? Both are compared the way
// samePath compares, so Windows case differences do not make an installation
// look foreign to itself.
func pathUnder(p, dir string) bool {
	if samePath(p, dir) {
		return true
	}
	rel, err := filepath.Rel(filepath.Clean(dir), filepath.Clean(p))
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
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
// Linux. On Windows it runs them in PowerShell by default (the CLI's
// ps-script-*.ps1) or in whatever terminal.integrated.defaultProfile.windows
// names — Git Bash on the machine #96 was found on — so that cell declares
// both and the skill teaches a form for each. Its hooks ran on macOS; on
// Linux nothing names the runner; on Windows
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
			// Ruled, not observed, on the same grounds H-R5 ruled the tool
			// cell: no host was run live on Linux, macOS's hook runner was
			// proven POSIX live, PowerShell is not a Linux default and cmd
			// does not exist there. Left unknown, a hook command — which has
			// no v0.2.9 fallback, since one written for the wrong runner
			// fails silently — would mean Cursor on Linux losing the hooks it
			// has today, which is the regression H-R5 forbids.
			hook: shellCell{
				evidence: evidenceRuled,
				shells:   []shellKind{shellPOSIX},
				source:   "no live Linux run; macOS proven live (#61); PowerShell is not a Linux default and cmd does not exist there; ruled by the same argument as H-R5's tool cell",
			},
		}
	case "windows":
		return hostShells{
			// Two shells, and the participant picks — not per call, once, in
			// terminal.integrated.defaultProfile.windows (#96). The CLI and a
			// default editor install use PowerShell, which is why it is first
			// and why 0.2.10 declared it alone; an editor whose profile is Git
			// Bash wrapped that PowerShell form in powershell.exe -Command and
			// bash expanded $OutputEncoding out of it before PowerShell ever
			// saw it, so the query reached the router as caf? ?? and the search
			// answered a different question. A single declared shell cannot
			// describe a host whose shell the participant selects.
			tool: establishedChosenBy(chosenByParticipant, "live: soak #67 Windows (Cursor CLI, ps-script-*.ps1) and forum.cursor.com/t/154914 staff: agent shell \"defaults to PowerShell\"; live: 0.2.10 release check Windows 11 row R5 (#96), editor with terminal.integrated.defaultProfile.windows = Git Bash ran the Bash heredoc byte-exact and mangled the PowerShell form", shellPowerShell, shellPOSIX),
			hook: shellCell{
				evidence: evidenceRuled,
				shells:   []shellKind{shellCmd, shellPowerShell},
				source:   "no hook observed live (#69); the hooks.json string fails as PowerShell and runs under cmd (Windows team, sitting 2); ruled: proven under cmd, PowerShell 5.1 and pwsh",
			},
		}
	}
	return hostShells{tool: cellUnknown, hook: cellUnknown}
}

// Cursor is the one host reached under two command names and installable
// without either. The editor provides a `cursor` shim only after the user
// runs "Install 'cursor' command in PATH" from its palette; the Agent CLI is
// `cursor-agent`; and an editor whose participant never ran the palette
// command has neither, while keeping a populated ~/.cursor the whole time.
// v0.2.9 looked for `cursor` alone, so it saw a Cursor user in exactly one of
// those three configurations — #61, where setup reported "Claude Code, Codex"
// on a machine with /Applications/Cursor.app, ~/.cursor and cursor-agent.
//
// The config directory is last because a command is the better answer to
// print: it says which Cursor is here. But it is not weaker evidence. It is
// where this host's own skill and hooks are written, so if it exists we are
// reading and writing there either way.
func (cursorTarget) Detect(ops agentOps, paths agentPaths, _ func(string) string) string {
	if cmd := detectCommand(ops, "cursor", "cursor-agent"); cmd != "" {
		return cmd
	}
	return detectConfigDir(ops, cursorConfigDir(paths))
}

// cursorConfigDir is ~/.cursor (%USERPROFILE%\.cursor), taken from the paths
// this host already writes into rather than rebuilt from the home directory:
// the directory detection reports and the directory install uses are then the
// same one by construction.
func cursorConfigDir(paths agentPaths) string {
	return filepath.Dir(paths.cursorHooks)
}

func (t cursorTarget) PlanInstall(ops agentOps, paths agentPaths, entry binEntry, _ func(string) string, p *agentPlan) {
	prefer := readPrefer(ops, entry)
	changed, left := planSkill(ops, t, paths.cursorSkill, entry, prefer, "", p)
	spec, note, err := cursorHooksFor(t, entry, runtime.GOOS)
	switch {
	case err != nil:
		p.refused = append(p.refused, fmt.Sprintf("%s: %v", t.Label(), err))
	default:
		if note != "" {
			p.notes = append(p.notes, t.Label()+": "+note)
		}
		if planHooksMerge(ops, t.Label(), paths.cursorHooks, p, entry, spec) {
			changed = true
		}
	}
	if !changed && !left {
		p.skipped = append(p.skipped, t.Label()+": already installed")
	}
}

func (t cursorTarget) PlanUninstall(ops agentOps, paths agentPaths, entry binEntry, getenv func(string) string, p *agentPlan) {
	removed := false
	if pathExists(ops, filepath.Dir(paths.cursorSkill)) {
		planRemove(p, t.Label(), filepath.Dir(paths.cursorSkill))
		removed = true
	}
	if planHooksRemove(ops, t.Label(), paths.cursorHooks, p, entry, "hooks") {
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

func (opencodeTarget) Detect(ops agentOps, _ agentPaths, _ func(string) string) string {
	return detectCommand(ops, "opencode")
}

func (t opencodeTarget) PlanInstall(ops agentOps, paths agentPaths, entry binEntry, _ func(string) string, p *agentPlan) {
	if changed, left := planAgentScript(ops, t, paths.opencodePlugin, opencodePluginJS, "lineage plugin", entry, p); !changed && !left {
		p.skipped = append(p.skipped, t.Label()+": already installed")
	}
	shells, shellNote := toolShellsForSkill(t, runtime.GOOS)
	if shellNote != "" {
		p.notes = append(p.notes, shellNote)
	}
	p.notes = append(p.notes, t.Label()+": has no skill directory — add to AGENTS.md:\n"+rulesSnippetFor(entry, shells))
}

func (t opencodeTarget) PlanUninstall(ops agentOps, paths agentPaths, _ binEntry, getenv func(string) string, p *agentPlan) {
	if pathExists(ops, paths.opencodePlugin) {
		planRemove(p, t.Label(), paths.opencodePlugin)
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

func (piTarget) Detect(ops agentOps, _ agentPaths, _ func(string) string) string {
	return detectCommand(ops, "pi")
}

func (t piTarget) PlanInstall(ops agentOps, paths agentPaths, entry binEntry, _ func(string) string, p *agentPlan) {
	prefer := readPrefer(ops, entry)
	changed, left := planSkill(ops, t, paths.piSkill, entry, prefer, "", p)
	extChanged, extLeft := planAgentScript(ops, t, paths.piExtension, piExtensionTS, "lineage extension", entry, p)
	changed, left = changed || extChanged, left || extLeft
	if !changed && !left {
		p.skipped = append(p.skipped, t.Label()+": already installed")
	}
}

func (t piTarget) PlanUninstall(ops agentOps, paths agentPaths, _ binEntry, getenv func(string) string, p *agentPlan) {
	removed := false
	if pathExists(ops, filepath.Dir(paths.piSkill)) {
		planRemove(p, t.Label(), filepath.Dir(paths.piSkill))
		removed = true
	}
	if pathExists(ops, paths.piExtension) {
		planRemove(p, t.Label(), paths.piExtension)
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

func (hermesTarget) Detect(ops agentOps, _ agentPaths, _ func(string) string) string {
	return detectCommand(ops, "hermes")
}

func (t hermesTarget) PlanInstall(ops agentOps, paths agentPaths, entry binEntry, _ func(string) string, p *agentPlan) {
	prefer := readPrefer(ops, entry)
	changed, left := planSkill(ops, t, paths.hermesSkill, entry, prefer, hermesApprovalNote, p)
	if planHermesHook(ops, t.Label(), paths.hermesConfig, entry, p) {
		changed = true
	}
	if !changed && !left {
		p.skipped = append(p.skipped, t.Label()+": already installed")
	}
	p.notes = append(p.notes, t.Label()+": takes effect next session; Hermes asks once to approve the hook the first time it fires — approve it, or launch with --accept-hooks. Its shell tool is in the terminal/coding toolsets.")
}

func (t hermesTarget) PlanUninstall(ops agentOps, paths agentPaths, entry binEntry, getenv func(string) string, p *agentPlan) {
	removed := false
	// noted: something of this installation's was seen and left, and said so.
	// "not installed" beside that sentence would be false (and was printed).
	noted := false
	if pathExists(ops, filepath.Dir(paths.hermesSkill)) {
		planRemove(p, t.Label(), filepath.Dir(paths.hermesSkill))
		removed = true
	}
	unhooked, said := planHermesUnhook(ops, t.Label(), paths.hermesConfig, entry, p)
	removed, noted = removed || unhooked, said
	if !removed && !noted {
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
