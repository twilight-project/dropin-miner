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

				// Reading the answer: merged first, found_by as evidence,
				// view:merged to cut tokens, decision, and cost mentioned
				// only if asked.
				for _, want := range []string{
					"result.merged", "found_by", `"view":"merged"`,
					"decision", "usage.cost_micros", "only if the user asks",
				} {
					if !strings.Contains(skill, want) {
						t.Errorf("skill is missing reading-the-answer guidance %q", want)
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
