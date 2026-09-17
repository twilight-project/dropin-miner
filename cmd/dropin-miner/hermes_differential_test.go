package main

// A differential over Hermes' config.yaml: many hostile shapes, each through
// the real `agents uninstall`, each judged twice.
//
// hermes_install.go edits YAML by line with no parser, and the way that goes
// wrong is never a crash: it is a file that means something else afterwards.
// L3's review built this — 49 shapes x prefixes x suffixes x LF, CRLF and no
// final newline, 2,875 files — and had PyYAML, the parser Hermes loads the
// file with, say what each meant before and after. It found three classes
// of defect no hand-picked fixture had (content at list depth that is not a
// list entry; a second pre_tool_call: key; live hooks the plan was silent
// about). This is that generator, kept.
//
// Judged here, on every run and every platform, is everything that needs no
// parser: that a surviving line is never altered or reordered; that each
// shape is edited or left as this table says; that a shape left with our
// entry in it gets a sentence, and never "Hermes: not installed" beside it.
// Judged by testdata/hermes/oracle.py, on request, is what the file MEANS:
//
//	HERMES_DIFFERENTIAL_OUT=/tmp/pairs.json go test ./cmd/dropin-miner -run TestHermesDifferential -count=1
//	python3 cmd/dropin-miner/testdata/hermes/oracle.py /tmp/pairs.json
//
// A new shape goes in the table with the outcome the oracle confirmed for
// it, not the outcome the code happened to give.

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
)

type hermesDifferentialPair struct {
	Name   string `json:"name"`
	Cmd    string `json:"cmd"`
	Before string `json:"before"`
	After  string `json:"after"`
	Out    string `json:"out"`
}

type hermesOutcome int

const (
	edited hermesOutcome = iota // our entry goes; the oracle confirms nothing else changes meaning
	left                        // byte-identical, and the plan says our hook is there
	varies                      // depends on what follows the body; only the invariants are judged
)

func TestHermesDifferential(t *testing.T) {
	_, cmd := ourHermesEntry(t)
	ours := entryLines(cmd)
	l := hermesHookLines(cmd)
	cmdLine, matcher := l[2]+"\n", l[3]+"\n"

	prefixes := map[string]string{
		"none":      "",
		"model":     "model: gpt\n",
		"comment":   "# top\nmodel: gpt\n\n",
		"directive": "%YAML 1.2\n---\nmodel: gpt\n",
		// Our whole entry, quoted inside somebody's notes above the real one.
		"blockscalar-above": "notes: |\n  hooks:\n    pre_tool_call:\n  " + strings.ReplaceAll(ours, "\n", "\n  ") + "\nmodel: x\n",
	}
	suffixes := map[string]string{
		"none":     "",
		"database": "database:\n  journal_mode: \"wal\"\n",
		"comment0": "# trailing top comment\n",
		"list":     "plugins:\n  - a\n  - b\n",
	}
	foreign := foreignEntry
	foreignRich := "    - command: 'rich'\n      description: |\n        To add dropin-miner paste:\n" +
		"        " + strings.TrimLeft(cmdLine, " ") + "          matcher: \"terminal\"\n      env:\n        - A=1\n        - B=2\n"
	foreignFlow := "    - {command: 'flow', matcher: \"terminal\"}\n"
	foreignNull := "    - command: 'nullkey'\n      extra:\n"
	foreignMulti := "    - command: 'multi'\n      description: long text\n        continues here\n"
	post := foreignPostTool
	postZero := "  post_tool_call:\n  - command: 'zero-indented'\n    matcher: y\n"
	head := "hooks:\n  pre_tool_call:\n"

	bodies := map[string]struct {
		text string
		want hermesOutcome
	}{
		"only":                 {head + ours, edited},
		"first+foreign":        {head + ours + foreign, edited},
		"foreign+last":         {head + foreign + ours, edited},
		"middle":               {head + foreign + ours + foreign, edited},
		"rich-before":          {head + foreignRich + ours, edited},
		"rich-after":           {head + ours + foreignRich, edited},
		"flow-before":          {head + foreignFlow + ours, edited},
		"flow-after":           {head + ours + foreignFlow, edited},
		"null-before":          {head + foreignNull + ours, edited},
		"multi-before":         {head + foreignMulti + ours, edited},
		"post-after":           {head + ours + post, edited},
		"post-before":          {"hooks:\n" + post + "  pre_tool_call:\n" + ours, edited},
		"postzero-after":       {head + ours + postZero, edited},
		"postzero-before":      {"hooks:\n" + postZero + "  pre_tool_call:\n" + ours, edited},
		"scalar-sibling-key":   {"hooks:\n  enabled: true\n  pre_tool_call:\n" + ours, edited},
		"scalar-sibling-after": {head + ours + "  enabled: true\n", edited},
		"comment-after-ours":   {head + ours + "    # a note about the next thing\n", edited},
		"key-before-in-seq":    {head + "    - matcher: \"x\"\n      command: 'k'\n" + ours, edited},
		"merge-key":            {"base: &b\n  x: 1\nhooks:\n  <<: *b\n  pre_tool_call:\n" + ours, edited},
		"alias-to-hooks":       {head + ours + "copy:\n  ref: 1\n", edited},
		"nested-seq-parent":    {head + "    - - command: 'nested'\n" + ours, edited},

		"comment-inside-ours":    {head + cmdLine + "      # mine\n" + matcher, left},
		"blank-inside-ours":      {head + cmdLine + "\n" + matcher, left},
		"deeper-after":           {head + ours + "      timeout: 5\n", left},
		"deeper-after-comment":   {head + ours + "      # c\n      timeout: 5\n", left},
		"deeper-after-blank":     {head + ours + "\n      timeout: 5\n", left},
		"indent5-after":          {head + ours + "     odd: 1\n", left},
		"indent3-after":          {head + ours + "   odd: 1\n", left},
		"indent1-after":          {head + ours + " odd: 1\n", left},
		"literal-on-next-line":   {head + "    |\n" + ours, left},
		"folded-on-next-line":    {head + "    >-\n" + ours, left},
		"anchor-on-next-line":    {head + "    &a\n" + ours, left},
		"tag-on-next-line":       {head + "    !!seq\n" + ours, left},
		"trailing-space-hooks":   {"hooks: \n  pre_tool_call:\n" + ours, left},
		"trailing-space-event":   {"hooks:\n  pre_tool_call: \n" + ours, left},
		"trailing-space-matcher": {head + cmdLine + strings.TrimRight(matcher, "\n") + " \n", left},
		"hooks-with-anchor":      {"hooks: &h\n  pre_tool_call:\n" + ours, left},
		"dup-event":              {head + ours + "  pre_tool_call:\n" + foreign, left},
		"dup-event-reverse":      {head + foreign + "  pre_tool_call:\n" + ours, left},
		"ours-in-second-of-two":  {head + foreign + "  other:\n    - x\n  pre_tool_call:\n" + ours, left},
		"explicit-key":           {"hooks:\n  ? pre_tool_call\n  :\n" + ours, left},
		"quoted-event":           {"hooks:\n  \"pre_tool_call\":\n" + ours, left},
		"event-with-comment":     {"hooks:\n  pre_tool_call: # mine\n" + ours, left},
		"hooks-with-comment":     {"hooks: # mine\n  pre_tool_call:\n" + ours, left},
		"indent1-event-between":  {"hooks:\n something:\n  pre_tool_call:\n" + ours, left},
		"upper-hooks-too":        {"HOOKS:\n  x: 1\nhooks:\n  pre_tool_call:\n" + ours, left},
		"doc-end":                {head + ours + "...\n", left},

		// With nothing after it the last line is ours without its newline and
		// goes; with a suffix, that suffix is glued to our matcher line.
		"ours-then-eof-noeol": {head + foreign + strings.TrimRight(ours, "\n"), varies},
	}

	var pairs []hermesDifferentialPair
	run := func(name, before string, want hermesOutcome) {
		m, ops := newFakeMachine("hermes")
		m.files[hermesConfigPath] = []byte(before)
		code, out, errOut := runAgents(t, ops, nil, "uninstall", "-config", testCfg, "-yes")
		after := string(m.files[hermesConfigPath])
		pairs = append(pairs, hermesDifferentialPair{Name: name, Cmd: cmd, Before: before, After: after, Out: out})
		if code != exitOK {
			t.Errorf("%s: uninstall exit %d\n%s%s", name, code, out, errOut)
			return
		}
		if !linesSurviveInOrder(after, before) {
			t.Errorf("%s: a line that stayed was altered or moved:\n--- before ---\n%q\n--- after ---\n%q", name, before, after)
		}
		switch {
		case want == edited && after == before:
			t.Errorf("%s: left, want our entry removed\n%s", name, out)
		case want == left && after != before:
			t.Errorf("%s: edited, want the file left exactly as it was:\n--- before ---\n%q\n--- after ---\n%q", name, before, after)
		}
		if after == before {
			if !strings.Contains(out, "pre_tool_call hook") {
				t.Errorf("%s: left with our entry in it, and the plan said nothing about it:\n%s", name, out)
			}
			if strings.Contains(out, "Hermes: not installed") {
				t.Errorf("%s: the plan says our hook is there and that Hermes is not installed:\n%s", name, out)
			}
		}
	}
	for pn, p := range prefixes {
		for bn, b := range bodies {
			for sn, s := range suffixes {
				cfg := p + b.text + s
				name := fmt.Sprintf("%s/%s/%s", pn, bn, sn)
				run(name+"/lf", cfg, b.want)
				run(name+"/crlf", strings.ReplaceAll(cfg, "\n", "\r\n"), b.want)
				if strings.HasSuffix(cfg, "\n") {
					run(name+"/noeol", strings.TrimSuffix(cfg, "\n"), b.want)
				}
			}
		}
	}
	t.Logf("%d files", len(pairs))

	if dst := os.Getenv("HERMES_DIFFERENTIAL_OUT"); dst != "" {
		data, err := json.Marshal(pairs)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dst, data, 0o600); err != nil { // #nosec G703 -- a path the person running the oracle chose
			t.Fatal(err)
		}
	}
}

// linesSurviveInOrder: is every line of after a line of before, in order?
func linesSurviveInOrder(after, before string) bool {
	b := strings.SplitAfter(before, "\n")
	i := 0
	for _, a := range strings.SplitAfter(after, "\n") {
		if a == "" {
			continue // SplitAfter's empty tail after a final newline is not a line
		}
		for i < len(b) && b[i] != a {
			i++
		}
		if i == len(b) {
			return false
		}
		i++
	}
	return true
}
