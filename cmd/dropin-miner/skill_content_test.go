package main

// S4: the skill teaches what S1-S3 made true.
//
// The prose sections skill.md carries (Tiers, Request options, Reading the
// answer) do not vary by shell or by OS — only the code blocks inside
// {{CALL}}, {{PREFER}} and {{SEARCH}} do, and those are already pinned
// byte-for-byte per OS by TestRenderedHostStringsGoldenPerOS. There is no
// byte golden for the prose itself; this is the guard that plays that role
// for it — a positive-content check, run for every installed host on every
// OS, so a paragraph quietly dropped from skill.md is caught the same way
// a dropped code block would be.

import (
	"strings"
	"testing"
)

// skillSection extracts the text between a heading and the next one (or
// end of file), heading lines excluded, for a paragraph-scoped assertion
// ("this word belongs in Tiers, not anywhere in the whole document").
func skillSection(skill, heading, nextHeading string) (string, bool) {
	start := strings.Index(skill, heading)
	if start < 0 {
		return "", false
	}
	start += len(heading)
	end := len(skill)
	if nextHeading != "" {
		if i := strings.Index(skill[start:], nextHeading); i >= 0 {
			end = start + i
		}
	}
	return skill[start:end], true
}

func TestSkillTeachesTiersOptionsAndMergedList(t *testing.T) {
	entry := binEntry{command: "/home/u/.tokendrop/bin/dropin-miner", cfg: "/home/u/.tokendrop/tokendrop.toml"}
	for _, id := range goldenHostIDs {
		for _, goos := range hostShellOSes {
			t.Run(id+"/"+goos, func(t *testing.T) {
				skill := renderedSkillFor(id, entry, goos)

				// Tiers: fast is named as the default, on need alone.
				for _, want := range []string{
					"unset for a routine lookup", `"tier":"balanced"`, "several sources",
				} {
					if !strings.Contains(skill, want) {
						t.Errorf("skill is missing tier guidance %q", want)
					}
				}
				tiers, ok := skillSection(skill, "## Tiers", "## Request options")
				if !ok {
					t.Fatal("no ## Tiers section found")
				}
				for _, unwanted := range []string{"cost", "credit", "usage"} {
					if strings.Contains(tiers, unwanted) {
						t.Errorf("the Tiers paragraph mentions %q, which S4 says it must not: %s", unwanted, tiers)
					}
				}

				// Options: all three named, one example JSON shape each.
				for _, want := range []string{
					"`recency`", `"recency":"month"`,
					"`domain_filter`", `"domain_filter":["developer.mozilla.org"]`,
					"`max_results`", `"max_results":3`,
				} {
					if !strings.Contains(skill, want) {
						t.Errorf("skill is missing request-option guidance %q", want)
					}
				}

				// Options: what the router actually promises (#131). An
				// agent that reads domain_filter as a restriction presents
				// off-host results as the named site's, or reports a
				// failure that is not one — which is what the 0.2.12
				// release check watched it do. Asserted inside the section,
				// because "preferences" said anywhere else in the document
				// would not be read at the point the field is chosen.
				options, ok := skillSection(skill, "## Request options", "## What comes back")
				if !ok {
					t.Fatal("no ## Request options section found")
				}
				for _, want := range []string{
					"preferences", "not every provider honors them",
					"other hosts can still come back", "check each",
					"`max_results` is a cap",
				} {
					if !strings.Contains(options, want) {
						t.Errorf("the Request options section does not say what the router promises: missing %q\n%s", want, options)
					}
				}

				// Reading the answer: merged first, found_by as evidence,
				// view:merged to cut tokens, what it costs to ask for it,
				// decision, and cost mentioned only if asked.
				for _, want := range []string{
					"result.merged", "found_by", `"view":"merged"`,
					"decision", "usage.cost_micros", "only if the user asks",
				} {
					if !strings.Contains(skill, want) {
						t.Errorf("skill is missing reading-the-answer guidance %q", want)
					}
				}
				// The merged view is not free and is not the default: it
				// drops the providers' own answer texts with the candidates
				// list. The release check watched an agent ask for it on
				// its very first search, unprompted.
				reading, ok := skillSection(skill, "## Reading the answer", "## Mining is not search")
				if !ok {
					t.Fatal("no ## Reading the answer section found")
				}
				for _, want := range []string{"answer texts", "not the default"} {
					if !strings.Contains(reading, want) {
						t.Errorf("the Reading the answer section does not say what the merged view costs: missing %q\n%s", want, reading)
					}
				}

				// The default tier must not be credited with what only
				// balanced does: the unqualified claim this replaced.
				if strings.Contains(skill, "one call fans out across several search providers and returns provider-attributed results") {
					t.Error("skill still claims the default tier returns several attributed providers")
				}
			})
		}
	}
}

// TestSkillSaysASearchThatDidNotRunIsReported: #135. Cursor 3.21 sandboxed
// the first search and refused the second, and the agent then answered the
// user's question with no search having reached the router. The skill's error
// discipline said to decide from ok, retryable and action; it did not say
// what to do with the question itself. Asserted inside "What comes back",
// where the envelope is described, for every host on every OS.
func TestSkillSaysASearchThatDidNotRunIsReported(t *testing.T) {
	entry := binEntry{command: "/home/u/.tokendrop/bin/dropin-miner", cfg: "/home/u/.tokendrop/tokendrop.toml"}
	for _, id := range goldenHostIDs {
		for _, goos := range hostShellOSes {
			t.Run(id+"/"+goos, func(t *testing.T) {
				section, ok := skillSection(renderedSkillFor(id, entry, goos), "## What comes back", "## Reading the answer")
				if !ok {
					t.Fatal("no ## What comes back section found")
				}
				for _, want := range []string{
					"When `ok` is false, or the command could not run at all",
					"tell the user the search did not run and stop",
					"never answer the question as if the search had run",
				} {
					if !containsFlat(section, want) {
						t.Errorf("What comes back does not say %q:\n%s", want, section)
					}
				}
			})
		}
	}
}
