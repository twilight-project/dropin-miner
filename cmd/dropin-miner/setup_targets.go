package main

// setup's coding-agent step. It has its own selection path and calls the
// agents planner directly: agentsMain's explicit resolver is host-only by
// design (an integration must never be reachable through agents -client),
// while setup -with names a target of either kind.
//
//	default            every host, filtered by Detect, and asked about
//	-with <id> ...     exactly those targets, any kind, detection ignored,
//	                   duplicates removed keeping the first occurrence
//	-no-agents         no detection; -with still installs what it names

import (
	"fmt"
	"strings"

	"github.com/twilight-project/dropin-miner/pkg/config"
)

// setupTargets resolves the run's selection. explicit is true when -with
// named the targets: that naming is the participant's answer, so nothing is
// asked about them.
func setupTargets(ops agentOps, paths agentPaths, getenv func(string) string, with []string, noAgents bool) (selected []installTarget, signals map[string]string, explicit bool, err error) {
	if len(with) > 0 {
		resolved, err := targetsByIDs(with)
		if err != nil {
			return nil, nil, true, err
		}
		seen := map[string]bool{}
		for _, t := range resolved {
			if seen[t.ID()] {
				continue
			}
			seen[t.ID()] = true
			selected = append(selected, t)
		}
		return selected, nil, true, nil
	}
	if noAgents {
		return nil, nil, false, nil
	}
	selected, signals = detectHosts(ops, paths, getenv)
	return selected, signals, false, nil
}

// joinLabels is "A", "A and B", "A, B and C".
func joinLabels(ls []string) string {
	switch len(ls) {
	case 0:
		return ""
	case 1:
		return ls[0]
	}
	return strings.Join(ls[:len(ls)-1], ", ") + " and " + ls[len(ls)-1]
}

// agentsParagraph says what answering yes writes, from the registry's own
// labels, and claims nothing about earning: whether a search earns is the
// mining decision's business, not the skill's.
func agentsParagraph() string {
	codex := codexTarget{}.Label()
	return fmt.Sprintf(`Setup knows these coding agents: %s.
Each can get a web-search skill that runs dropin-miner's search through the
Twilight search router, and — where the agent supports one — the hook, plugin
or extension that threads each search into the agent's session (an agent with
no skill directory gets a plugin and a line to paste into its rules instead).
These are written into the agent's own config directory. For %s it also
widens the sandbox in its config.toml:
network access on, and the tokendrop state directory — plus the intake,
sessions and spool directories — made writable, never the config, the stored
key or the wallet, so a search can record itself and the claim can resolve.
Answering yes here accepts all of that.
`, joinLabels(labels(targetsByKind(targetHost))), codex)
}

func (r *setupRun) agentsStep() int {
	r.say("Coding agents")
	if r.leftForOtherInstallation("coding agents", "The coding agents on this machine belong to that one", "setting them up here would repoint your real agents at this installation's config") {
		return exitOK
	}
	ops := r.d.agents
	paths := ops.paths(r.d.getenv)
	later := fmt.Sprintf("%s agents install -config %s", r.displayPath(r.exe), r.displayPath(r.cfgPath))

	selected := r.targets
	if !r.explicitTargets {
		switch {
		case r.noAgents:
			r.printf("Left the agents alone (-no-agents). When you are ready:\n\n    %s\n", later)
			r.skip("coding agents")
			return exitOK
		case len(selected) == 0:
			r.printf("No coding agent found (looked for: %s, each by its command or its config directory). When one is installed:\n\n    %s\n", targetIDs(targetHost), later)
			return exitOK
		case !r.d.interactive && !r.yes && !r.dry:
			r.printf("Not an interactive shell — not touching any agent (pass -yes to set them up). When you are ready:\n\n    %s\n", later)
			r.skip("coding agents")
			return exitOK
		}
	}

	r.printf("%s\n", agentsParagraph())
	entry := binEntry{command: r.exe, cfg: r.cfgPath}
	if r.dry && len(r.configPlanData) > 0 {
		// config() printed what it would write (fresh) or add (migrated)
		// but never published it, so there is nothing at r.cfgPath yet —
		// or nothing with those additions yet — for codexSandboxRoots' own
		// disk read to find. Parse the bytes it would have published
		// instead, through the config package itself, so this sees exactly
		// what the real run's own disk read would.
		if rendered, err := config.LoadBytes(r.configPlanData, r.d.getenv); err == nil {
			entry.rendered = rendered
		}
	}
	plan := buildInstallPlan(ops, paths, selected, entry, r.d.getenv)
	if r.d.agentPlanObserver != nil {
		r.d.agentPlanObserver(plan)
	}
	if r.explicitTargets {
		r.printf("Setting up (named with -with): %s\n", strings.Join(labels(selected), ", "))
	} else {
		r.printf("Found on this machine: %s\n", strings.Join(labelsWithSignals(selected, r.targetSignals), ", "))
	}
	printPlan(&plan, ops.home, r.d.stdout)
	if plan.empty() {
		r.printf("  nothing to write: already set up\n")
		return exitOK
	}
	if r.dry {
		r.printf("(dry run) nothing was written\n")
		return exitOK
	}
	if !r.explicitTargets {
		set, err := r.ask("Set up the coding agents found on this machine now?")
		if err != nil {
			return r.abort("no coding agent was set up")
		}
		if !set {
			r.printf("Left the agents alone. When you change your mind: %s\n", later)
			return exitOK
		}
	}
	failures := commitPlan(ops, &plan, r.d.stdout, r.d.stderr)
	if len(plan.writes) > failures || len(plan.removes) > 0 {
		r.changed = true
	}
	if failures > 0 || len(plan.refused) > 0 {
		r.printf("Some agent could not be set up; see above.\n")
	}
	return exitOK
}
