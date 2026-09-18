package main

// The rendered skill's frontmatter has to be valid YAML for every host on
// every OS: it is what Claude Code, Cursor and every other consumer parses
// with their own YAML front-matter reader (js-yaml, gray-matter) before any
// of the prose below it means anything. A description that happens to
// contain a double quote or a colon is not a hypothetical here — it is
// exactly what descriptionOn carries once it names a JSON field like
// "tier":"balanced".

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// skillFrontmatter splits the leading "---\n...\n---\n" block off a
// rendered skill and returns its raw YAML, or false if the fences are not
// there at all — a document with no frontmatter is its own separate bug,
// not one this test conflates with "the frontmatter parsed."
func skillFrontmatter(skill string) (string, bool) {
	const fence = "---\n"
	if !strings.HasPrefix(skill, fence) {
		return "", false
	}
	rest := skill[len(fence):]
	end := strings.Index(rest, "\n---\n")
	if end < 0 {
		return "", false
	}
	return rest[:end], true
}

// TestSkillFrontmatterIsValidYAML renders every installed host's skill on
// every OS and parses its frontmatter with the same library family a real
// consumer uses (yaml.v3 here; gray-matter and js-yaml are both YAML 1.1/1.2
// parsers with the same rule that decides this: an unescaped `"` inside a
// double-quoted scalar ends the scalar right there).
func TestSkillFrontmatterIsValidYAML(t *testing.T) {
	entry := binEntry{command: "/home/u/.tokendrop/bin/dropin-miner", cfg: "/home/u/.tokendrop/tokendrop.toml"}
	for _, id := range goldenHostIDs {
		for _, goos := range hostShellOSes {
			t.Run(id+"/"+goos, func(t *testing.T) {
				skill := renderedSkillFor(id, entry, goos)
				fm, ok := skillFrontmatter(skill)
				if !ok {
					preview := skill
					if len(preview) > 200 {
						preview = preview[:200]
					}
					t.Fatalf("no frontmatter fences found:\n%s", preview)
				}
				var doc struct {
					Name        string `yaml:"name"`
					Description string `yaml:"description"`
				}
				if err := yaml.Unmarshal([]byte(fm), &doc); err != nil {
					t.Fatalf("frontmatter is not valid YAML: %v\nfrontmatter:\n%s", err, fm)
				}
				if doc.Name != "dropin-miner" {
					t.Errorf("name round-trip: got %q, want %q", doc.Name, "dropin-miner")
				}
				// renderedSkillFor always renders with preferOn, so
				// descriptionOn is the value that went in.
				if doc.Description != descriptionOn {
					t.Errorf("description did not round-trip through YAML:\ngot:  %q\nwant: %q", doc.Description, descriptionOn)
				}
			})
		}
	}
}

// TestSkillFrontmatterIsValidYAMLWhenOff covers the other description the
// template ever substitutes: descriptionOff, rendered when the search is
// turned off as the default. renderedSkillFor only ever renders "on", so
// this calls renderSkill directly for the one case that matters — the
// value that actually goes in the frontmatter — rather than every host, on
// per-host command rendering.
func TestSkillFrontmatterIsValidYAMLWhenOff(t *testing.T) {
	tg, ok := targetByID(installTargets, "claude")
	if !ok {
		t.Fatal("no claude target")
	}
	shells, _ := toolShellsForSkill(tg, "linux")
	skill, err := renderSkill(binEntry{command: "/home/u/.tokendrop/bin/dropin-miner", cfg: "/home/u/.tokendrop/tokendrop.toml"}, preferOff, "", shells)
	if err != nil {
		t.Fatal(err)
	}
	fm, ok := skillFrontmatter(string(skill))
	if !ok {
		t.Fatal("no frontmatter fences found")
	}
	var doc struct {
		Name        string `yaml:"name"`
		Description string `yaml:"description"`
	}
	if err := yaml.Unmarshal([]byte(fm), &doc); err != nil {
		t.Fatalf("frontmatter is not valid YAML: %v\nfrontmatter:\n%s", err, fm)
	}
	if doc.Description != descriptionOff {
		t.Errorf("description did not round-trip through YAML:\ngot:  %q\nwant: %q", doc.Description, descriptionOff)
	}
}
