package main

// What the six supported hosts are actually told.
//
// The list is read from agentSurfaces rather than written out here, so a
// seventh host cannot be added without either giving it instruction text
// or failing this file. That is the point: Pi and Hermes were the two that
// arrived last, and a test that hardcoded the original four would have
// passed while saying nothing about them.

import (
	"regexp"
	"strings"
	"testing"
)

// instructionTextFor returns the guidance a host receives, whatever
// mechanism it receives it through. opencode has no skill directory — its
// instructions are the AGENTS.md snippet the installer prints — and
// inventing one for symmetry would install a surface the participant never
// asked for.
func instructionTextFor(t *testing.T, surfaceID string, entry binEntry) string {
	t.Helper()
	if surfaceID == "opencode" {
		return rulesSnippet(entry)
	}
	return string(renderSkill(entry, preferOn, surfaceID))
}

func guidanceEntry() binEntry {
	return binEntry{command: "/usr/local/bin/dropin-miner", cfg: "/home/u/tokendrop.toml"}
}

func TestEverySupportedHostIsToldTheStructuredProtocol(t *testing.T) {
	entry := guidanceEntry()
	if len(agentSurfaces) != 6 {
		t.Fatalf("the supported-surface table has %d entries; this test enumerates what each is told", len(agentSurfaces))
	}
	for _, s := range agentSurfaces {
		t.Run(s.id, func(t *testing.T) {
			text := instructionTextFor(t, s.id, entry)
			if strings.TrimSpace(text) == "" {
				t.Fatalf("%s receives no instruction text at all", s.label)
			}
			for _, want := range []string{"--stdin", `"version"`, "query"} {
				if !strings.Contains(text, want) {
					t.Errorf("%s is not told about %q:\n%s", s.label, want, text)
				}
			}
			// The machine envelope, not prose, is what recovery comes from.
			for _, want := range []string{"ok", "retryable", "action"} {
				if !strings.Contains(text, want) {
					t.Errorf("%s is not told to read %q from the envelope", s.label, want)
				}
			}
		})
	}
}

// The claims that were wrong and are now removed. Each of these was in the
// installed text and each of them would send an agent down a path the
// envelope contradicts.
func TestNoHostIsToldTheSupersededRules(t *testing.T) {
	entry := guidanceEntry()
	forbidden := []struct{ name, pattern string }{
		{"every search earns", `(?i)every search earns`},
		{"the user earns through this one", `(?i)the user earns through this one`},
		{"earns mining rewards", `(?i)earns mining rewards`},
		{"blanket never-retry", `(?i)never retry\b`},
		{"blanket do-not-retry", `(?i)do not retry the search`},
		{"blanket no-fallback", `(?i)do not silently fall back`},
		{"401 means never registered", `(?i)401[^.]{0,40}has not registered`},
	}
	for _, s := range agentSurfaces {
		text := instructionTextFor(t, s.id, entry)
		for _, f := range forbidden {
			if regexp.MustCompile(f.pattern).MatchString(text) {
				t.Errorf("%s is still told %q:\n%s", s.label, f.name, text)
			}
		}
	}
}

// taughtInvocation is the command the guidance actually tells an agent to
// run: the first fenced block of the skill, or the snippet itself for the
// host that has no skill.
//
// Asserting on the whole document is not enough. The text mentions
// -format model further down, as the human form, so a document that
// reverted its taught command to argv while keeping that paragraph still
// contains the string "--stdin" somewhere — and a guard that only searched
// the whole text stayed green through exactly that change.
func taughtInvocation(t *testing.T, surfaceID string, entry binEntry) string {
	t.Helper()
	text := instructionTextFor(t, surfaceID, entry)
	if surfaceID == "opencode" {
		return text
	}
	_, rest, found := strings.Cut(text, "```bash\n")
	if !found {
		t.Fatalf("%s: the skill has no fenced command block:\n%s", surfaceID, text)
	}
	block, _, found := strings.Cut(rest, "```")
	if !found {
		t.Fatalf("%s: the skill's command block is unterminated", surfaceID)
	}
	return block
}

// -format model may still be mentioned — it is the human path and it still
// works — but the command an agent is told to run has to be the structured
// one, or an agent reading top to bottom builds an argv command and stops
// there.
func TestTheTaughtCommandIsTheStructuredOne(t *testing.T) {
	entry := guidanceEntry()
	for _, s := range agentSurfaces {
		t.Run(s.id, func(t *testing.T) {
			taught := taughtInvocation(t, s.id, entry)
			if !strings.Contains(taught, "--stdin") {
				t.Errorf("%s is taught a command that is not the stdin protocol:\n%s", s.label, taught)
			}
			if strings.Contains(taught, "-format model") {
				t.Errorf("%s is taught the argv model form as its command:\n%s", s.label, taught)
			}
			// And the request shape travels with the command.
			if !strings.Contains(taught, `"version"`) || !strings.Contains(taught, `"query"`) {
				t.Errorf("%s is not shown the v1 request alongside the command:\n%s", s.label, taught)
			}
		})
	}
}

func TestTheStructuredPathIsTaughtBeforeTheHumanOne(t *testing.T) {
	entry := guidanceEntry()
	for _, s := range agentSurfaces {
		t.Run(s.id, func(t *testing.T) {
			text := instructionTextFor(t, s.id, entry)
			stdinAt := strings.Index(text, "--stdin")
			if stdinAt < 0 {
				t.Fatalf("%s is never told about --stdin", s.label)
			}
			if modelAt := strings.Index(text, "-format model"); modelAt >= 0 && modelAt < stdinAt {
				t.Errorf("%s is shown -format model (at %d) before --stdin (at %d):\n%s",
					s.label, modelAt, stdinAt, text)
			}
		})
	}
}

// Search success and mining state are separate, and the skill has to say
// which field means mining is on. `configured` reports only that the miner
// block exists; reading it as "mining is on" is the mistake this asserts
// against.
func TestHostsAreToldSearchSuccessIsNotMiningCredit(t *testing.T) {
	entry := guidanceEntry()
	for _, s := range agentSurfaces {
		t.Run(s.id, func(t *testing.T) {
			text := instructionTextFor(t, s.id, entry)
			lower := strings.ToLower(text)
			if !strings.Contains(lower, "does not mean") && !strings.Contains(lower, "not mean anything was earned") {
				t.Errorf("%s is not told that a successful search is not earnings:\n%s", s.label, text)
			}
			if !strings.Contains(text, "mining") || !strings.Contains(text, "state") {
				t.Errorf("%s is not pointed at the mining state:\n%s", s.label, text)
			}
			if regexp.MustCompile(`(?i)configured[^.\n]{0,30}mining is on`).MatchString(text) {
				t.Errorf("%s is told that configured means mining is on:\n%s", s.label, text)
			}
		})
	}
}

func TestHostsAreToldResultTextIsUntrusted(t *testing.T) {
	entry := guidanceEntry()
	for _, s := range agentSurfaces {
		text := strings.ToLower(instructionTextFor(t, s.id, entry))
		if !strings.Contains(text, "untrusted") {
			t.Errorf("%s is not told that result text is untrusted web content", s.label)
		}
	}
}

// §16.2: the note belongs to Hermes, and to Hermes only. It describes that
// host's hook prompt — asserting it for Pi would be claiming a capability
// Pi does not have.
func TestOnlyHermesCarriesTheHookApprovalNote(t *testing.T) {
	entry := guidanceEntry()
	hermes := instructionTextFor(t, "hermes", entry)
	// Compared with whitespace collapsed: the note is prose and wraps, so
	// a line break must not be the thing that decides whether it is there.
	flat := strings.Join(strings.Fields(hermes), " ")
	for _, want := range []string{
		"one-time approval prompt for the installed DropinMiner hook",
		"expected for the installed lineage hook",
	} {
		if !strings.Contains(flat, want) {
			t.Errorf("the Hermes skill does not carry %q:\n%s", want, hermes)
		}
	}
	// And it must not imply the hook authorizes anything.
	if !strings.Contains(hermes, "does not authorize") {
		t.Error("the Hermes note does not say the approval authorizes nothing")
	}
	for _, s := range agentSurfaces {
		if s.id == "hermes" {
			continue
		}
		text := instructionTextFor(t, s.id, entry)
		if strings.Contains(text, "one-time approval prompt") || strings.Contains(text, "--accept-hooks") {
			t.Errorf("%s carries Hermes' hook-approval note, which is not true of it:\n%s", s.label, text)
		}
	}
}

// The skill teaches a command; the lineage adapters have to keep
// recognizing it. This is the join between §16 and the frozen recognizer.
func TestTheTaughtInvocationIsStillRecognizedAsOurSearch(t *testing.T) {
	entry := binEntry{command: "/usr/local/bin/dropin-miner", cfg: "/home/u/tokendrop.toml"}
	invocations := []string{
		entry.stdinCommand(),
		entry.searchCommand() + ` "a query"`,
		// As the skill actually shows it, with the heredoc the host will
		// see in the shell tool's command string.
		entry.stdinCommand() + " <<'JSON'",
		// And the shapes a host might wrap it in.
		"cd /tmp && " + entry.stdinCommand(),
		"(" + entry.stdinCommand() + ")",
	}
	for _, cmd := range invocations {
		if !isSearchCommand(cmd) {
			t.Errorf("the canonical recognizer no longer matches the taught invocation: %q", cmd)
		}
	}
}

// The JS hosts carry their own copy of the recognizer, spliced in from the
// shared trace source. It has to agree with the Go one about the command
// the skill teaches, or a Pi/opencode search loses its lineage.
func TestTheRenderedJSRecognizerAgreesAboutTheTaughtInvocation(t *testing.T) {
	re := regexp.MustCompile(`SEARCH_RE\s*=\s*/(.*?)/\n`).FindStringSubmatch(agentTraceCommonJS)
	if len(re) != 2 {
		t.Fatal("could not find SEARCH_RE in the shared trace source")
	}
	js, err := regexp.Compile(re[1])
	if err != nil {
		t.Fatalf("the JS recognizer is not a Go-compatible regexp (%v); compare it by hand", err)
	}
	entry := binEntry{command: "/usr/local/bin/dropin-miner"}
	for _, cmd := range []string{
		entry.stdinCommand(),
		entry.stdinCommand() + " <<'JSON'",
		entry.searchCommand() + ` "a query"`,
	} {
		if js.MatchString(cmd) != isSearchCommand(cmd) {
			t.Errorf("the JS and Go recognizers disagree about %q: js=%v go=%v",
				cmd, js.MatchString(cmd), isSearchCommand(cmd))
		}
		if !isSearchCommand(cmd) {
			t.Errorf("neither recognizer matches the taught invocation %q", cmd)
		}
	}
}
