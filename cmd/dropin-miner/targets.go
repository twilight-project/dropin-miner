package main

// The install-target registry: every installable surface behind one
// interface, so adding a seventh target means writing one type, not
// editing five places in agents.go's switches.

import (
	"fmt"
	"path/filepath"
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

func (claudeTarget) Detect(ops agentOps, _ agentPaths, _ func(string) string) bool {
	_, err := ops.lookPath("claude")
	return err == nil
}

func (t claudeTarget) PlanInstall(ops agentOps, paths agentPaths, entry binEntry, _ func(string) string, p *agentPlan) {
	prefer := readPrefer(ops, entry)
	changed := planWrite(ops, t.Label(), paths.claudeSkill, renderSkill(entry, prefer, ""), 0o600, "skill", p)
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
	planWrite(ops, t.Label(), paths.claudeSkill, renderSkill(entry, prefer, ""), 0o600, "skill", p)
}

// ── Codex ─────────────────────────────────────────────────────────────────

type codexTarget struct{}

func (codexTarget) ID() string       { return "codex" }
func (codexTarget) Label() string    { return "Codex" }
func (codexTarget) Kind() targetKind { return targetHost }

func (codexTarget) Detect(ops agentOps, _ agentPaths, _ func(string) string) bool {
	_, err := ops.lookPath("codex")
	return err == nil
}

func (t codexTarget) PlanInstall(ops agentOps, paths agentPaths, entry binEntry, getenv func(string) string, p *agentPlan) {
	prefer := readPrefer(ops, entry)
	if !planWrite(ops, t.Label(), paths.codexSkill, renderSkill(entry, prefer, ""), 0o600, "skill", p) {
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
	planWrite(ops, t.Label(), paths.codexSkill, renderSkill(entry, prefer, ""), 0o600, "skill", p)
}

// ── Cursor ────────────────────────────────────────────────────────────────

type cursorTarget struct{}

func (cursorTarget) ID() string       { return "cursor" }
func (cursorTarget) Label() string    { return "Cursor" }
func (cursorTarget) Kind() targetKind { return targetHost }

func (cursorTarget) Detect(ops agentOps, _ agentPaths, _ func(string) string) bool {
	_, err := ops.lookPath("cursor")
	return err == nil
}

func (t cursorTarget) PlanInstall(ops agentOps, paths agentPaths, entry binEntry, _ func(string) string, p *agentPlan) {
	prefer := readPrefer(ops, entry)
	changed := planWrite(ops, t.Label(), paths.cursorSkill, renderSkill(entry, prefer, ""), 0o600, "skill", p)
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
	planWrite(ops, t.Label(), paths.cursorSkill, renderSkill(entry, prefer, ""), 0o600, "skill", p)
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

func (opencodeTarget) Detect(ops agentOps, _ agentPaths, _ func(string) string) bool {
	_, err := ops.lookPath("opencode")
	return err == nil
}

func (t opencodeTarget) PlanInstall(ops agentOps, paths agentPaths, entry binEntry, _ func(string) string, p *agentPlan) {
	js := renderAgentScript(opencodePluginJS)
	if !planWrite(ops, t.Label(), paths.opencodePlugin, []byte(js), 0o600, "lineage plugin", p) {
		p.skipped = append(p.skipped, t.Label()+": already installed")
	}
	p.notes = append(p.notes, t.Label()+": has no skill directory — add to AGENTS.md:\n"+rulesSnippet(entry))
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

func (piTarget) Detect(ops agentOps, _ agentPaths, _ func(string) string) bool {
	_, err := ops.lookPath("pi")
	return err == nil
}

func (t piTarget) PlanInstall(ops agentOps, paths agentPaths, entry binEntry, _ func(string) string, p *agentPlan) {
	prefer := readPrefer(ops, entry)
	changed := planWrite(ops, t.Label(), paths.piSkill, renderSkill(entry, prefer, ""), 0o600, "skill", p)
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
	planWrite(ops, t.Label(), paths.piSkill, renderSkill(entry, prefer, ""), 0o600, "skill", p)
}

// ── Hermes ────────────────────────────────────────────────────────────────

type hermesTarget struct{}

func (hermesTarget) ID() string       { return "hermes" }
func (hermesTarget) Label() string    { return "Hermes" }
func (hermesTarget) Kind() targetKind { return targetHost }

func (hermesTarget) Detect(ops agentOps, _ agentPaths, _ func(string) string) bool {
	_, err := ops.lookPath("hermes")
	return err == nil
}

func (t hermesTarget) PlanInstall(ops agentOps, paths agentPaths, entry binEntry, _ func(string) string, p *agentPlan) {
	prefer := readPrefer(ops, entry)
	changed := planWrite(ops, t.Label(), paths.hermesSkill, renderSkill(entry, prefer, hermesApprovalNote), 0o600, "skill", p)
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
	planWrite(ops, t.Label(), paths.hermesSkill, renderSkill(entry, prefer, hermesApprovalNote), 0o600, "skill", p)
}
