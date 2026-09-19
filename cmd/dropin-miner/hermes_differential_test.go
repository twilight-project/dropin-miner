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
// entry in it gets a sentence, and never "Hermes: not installed" beside it;
// and that install and uninstall agree about the same file — whatever
// uninstall could only mention, install refuses WITH its warning, and never
// calls already set up (L3d: both were wrong somewhere, and no test knew).
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
	"runtime"
	"strings"
	"testing"
)

type hermesDifferentialPair struct {
	Name   string `json:"name"`
	Cmd    string `json:"cmd"`
	Before string `json:"before"`
	After  string `json:"after"`
	Out    string `json:"out"`
	// Install's verdict on the same file, on a machine of its own.
	InstallOut     string `json:"install_out"`
	InstallChanged bool   `json:"install_changed"`
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
	// Another installation's hook, made by the same renderer from the same
	// entry with another config: this installation's binary, somebody else's
	// installation, which is exactly the pair #73 is about.
	otherEntry, _ := ourHermesEntry(t)
	otherEntry.cfg = "/tmp/disposable/tokendrop.toml"
	otherInstallCmd, ok := hermesHookCommand(otherEntry, runtime.GOOS == "windows")
	if !ok {
		t.Fatal("could not render another installation's command")
	}
	postZero := "  post_tool_call:\n  - command: 'zero-indented'\n    matcher: y\n"
	head := "hooks:\n  pre_tool_call:\n"
	plain := hermesCommandPrefix + hermesWrittenScalar(cmd) + "\n      matcher: terminal\n"

	// #106 and #125: the same generator, with our own markers around the body.
	// A marked block is removed only when it is provably ours and provably
	// only ours, and only when nothing after the end marker continues the
	// mapping it opened — so for every shape here the config must come back
	// untouched by INSTALL, whether uninstall takes the block or leaves it.
	// below is what Hermes' ruamel writer added to our mapping after the end
	// marker; testdata/hermes holds that writer's real output for these two,
	// and this is the same two shapes through every prefix and suffix.
	marked := map[string]struct {
		text  string
		below string
		want  hermesOutcome
	}{
		"ours":              {head + ours, "", edited},
		"foreign-entry":     {head + ours + foreign, "", left},
		"foreign-first":     {head + foreign + ours, "", left},
		"comment-inside":    {head + ours + "# mine\n", "", left},
		"deeper-after-ours": {head + ours + "      timeout: 5\n", "", left},
		"another-install":   {head + entryLines(otherInstallCmd), "", left},
		"sibling-below":     {head + ours, post, left},
		"item-below":        {head + ours, foreign, left},
	}

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

		// L3d: the plain form Hermes writes, and shapes whose command is ours
		// but whose place is not one the structured find reads.
		// #108: the form Hermes leaves, now removed like any other.
		"plain-only":             {head + plain, edited},
		"literal-plain":          {head + "    |\n" + plain, left},
		"folded-plain":           {head + "    >-\n" + plain, left},
		"first-of-two-null":      {head + ours + "  pre_tool_call: null\n", left},
		"trailing-comment-plain": {head + "    - command: " + cmd + " # mine\n      matcher: terminal\n", left},
		"anchored-value":         {head + "    - command: &c " + hermesYAMLSingleQuoted(cmd) + "\n      matcher: terminal\n", left},
		"flow-item":              {head + "    - {command: " + hermesYAMLSingleQuoted(cmd) + ", matcher: terminal}\n", left},
		"command-not-first":      {head + "    - matcher: terminal\n      command: " + hermesYAMLSingleQuoted(cmd) + "\n", left},
		"zero-indented":          {head + "  - command: " + hermesYAMLSingleQuoted(cmd) + "\n    matcher: terminal\n", left},

		// With nothing after it the last line is ours without its newline and
		// goes; with a suffix, that suffix is glued to our matcher line.
		"ours-then-eof-noeol": {head + foreign + strings.TrimRight(ours, "\n"), varies},
	}

	var pairs []hermesDifferentialPair
	run := func(name, before string, want hermesOutcome, isMarked bool) {
		m, ops := newFakeMachine("hermes")
		m.files[hermesConfigPath] = []byte(before)
		code, out, errOut := runAgents(t, ops, nil, "uninstall", "-config", testCfg, "-yes")
		after := string(m.files[hermesConfigPath])

		im, iops := newFakeMachine("hermes")
		im.files[hermesConfigPath] = []byte(before)
		_, installOut, _ := runAgents(t, iops, nil, "install", "-config", testCfg, "-yes")
		installChanged := string(im.files[hermesConfigPath]) != before
		pairs = append(pairs, hermesDifferentialPair{Name: name, Cmd: cmd, Before: before, After: after, Out: out,
			InstallOut: installOut, InstallChanged: installChanged})

		// A marked block is ours to rewrite, so install has one job here and
		// it is to leave the file alone: the block either holds today's entry
		// (nothing to do) or cannot be vouched for (nothing that may be cut).
		// Before #106 install cut every one of these out and wrote its own
		// block in its place, which is #73's defect at install time.
		if isMarked && installChanged {
			t.Errorf("%s: install rewrote a marked block:\n%s", name, installOut)
		}
		// Install and uninstall read the same file with the same finder, and
		// their verdicts must be the same verdict. What uninstall could only
		// MENTION, install may not call set up, may not write beside, and may
		// not advise pasting a second copy of without saying what it saw.
		if strings.Contains(out, "names this installation's pre_tool_call hook command") {
			switch {
			case strings.Contains(installOut, "already set up —"):
				t.Errorf("%s: install says already set up where uninstall could only mention the command:\n%s", name, installOut)
			case installChanged:
				t.Errorf("%s: install wrote beside a command uninstall mentions", name)
			case !strings.Contains(installOut, "already names this installation's pre_tool_call hook command"):
				t.Errorf("%s: install advised pasting a second copy with no warning:\n%s", name, installOut)
			}
		}
		// The same rule for the unmarked form, where install's way of saying
		// it is the #83 note. Our OWN block does not get that note: install's
		// answer to a block of ours it will not rewrite is either silence —
		// there is nothing to do, and the plan's "already installed" says so —
		// or the one sentence naming what it left, which is asserted above by
		// installChanged and in hermes_marked_block_test.go by wording.
		if !isMarked && (after != before || strings.Contains(out, "was left there because")) && !strings.Contains(installOut, "already set up —") {
			t.Errorf("%s: uninstall found our hook and install did not call it set up:\n%s", name, installOut)
		}
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
	// #108: every shape above, again with our entry in the form Hermes leaves
	// when it re-dumps config.yaml — a plain scalar folded at 80 columns and an
	// unquoted matcher. The structural rules do not care which form the entry
	// is in, so each shape keeps its outcome; what changes is which of the two
	// readers has to recognize it.
	//
	// hermesWrittenEntry emulates PyYAML's emitter rather than being its
	// output: the real thing is in testdata/hermes, generated by resave.py and
	// pinned by hermes_resaved_test.go, and this is the generator exercising
	// the reader across every prefix, suffix and line ending. The two
	// properties that matter here are the ones it reproduces — a plain scalar,
	// broken at a space with the continuation indented under the key.
	writtenBodies := map[string]struct {
		text string
		want hermesOutcome
	}{}
	for bn, b := range bodies {
		if !strings.Contains(b.text, ours) {
			continue // a shape that takes our two lines apart; it stays as it is
		}
		writtenBodies[bn] = struct {
			text string
			want hermesOutcome
		}{strings.Replace(b.text, ours, hermesWrittenEntry(cmd), 1), b.want}
	}
	if len(writtenBodies) == 0 {
		t.Fatal("no shape holds our whole entry, so the Hermes-written dimension would test nothing")
	}

	every := func(name, cfg string, want hermesOutcome, isMarked bool) {
		run(name+"/lf", cfg, want, isMarked)
		run(name+"/crlf", strings.ReplaceAll(cfg, "\n", "\r\n"), want, isMarked)
		if strings.HasSuffix(cfg, "\n") {
			run(name+"/noeol", strings.TrimSuffix(cfg, "\n"), want, isMarked)
		}
	}
	for pn, p := range prefixes {
		for sn, s := range suffixes {
			for bn, b := range bodies {
				every(fmt.Sprintf("%s/%s/%s", pn, bn, sn), p+b.text+s, b.want, false)
			}
			for bn, b := range writtenBodies {
				every(fmt.Sprintf("%s/written-%s/%s", pn, bn, sn), p+b.text+s, b.want, false)
			}
			for bn, b := range marked {
				cfg := p + agentsMarkerBegin + "\n" + b.text + agentsMarkerEnd + "\n" + b.below + s
				every(fmt.Sprintf("%s/marked-%s/%s", pn, bn, sn), cfg, b.want, true)
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

// hermesWrittenScalar is cmd in the style a dumper would give it: plain when
// a plain scalar reads back as itself, single-quoted otherwise — the simplest
// form that round-trips, which is the rule PyYAML follows and which
// testdata/hermes shows it following (POSIX plain, Windows single-quoted,
// because a Windows command begins with the double quote of its own argv
// quoting and a scalar that begins with one is a double-quoted scalar).
//
// Typing the command bare instead is what made two Windows-only CI failures
// on this PR: the generated file was not the YAML it was meant to be, the
// reader read a double-quoted scalar and refused it exactly as it should, and
// only the runners that quote paths that way could see it.
func hermesWrittenScalar(cmd string) string {
	if got, ok := hermesDecodeScalar(cmd); ok && got == cmd {
		return cmd
	}
	return hermesYAMLSingleQuoted(cmd)
}

// The rule above, on every runner, for both platforms' commands — the
// generator takes its own command from the runner it is on, so without this
// the Windows shape is only ever seen by a Windows runner.
func TestTheGeneratorWritesAScalarThatReadsBackAsTheCommand(t *testing.T) {
	for name, tc := range hermesResavedEntries {
		cmd, ok := hermesHookCommand(tc.entry, tc.windows)
		if !ok {
			t.Fatalf("%s: could not render the command", name)
		}
		scalar := hermesWrittenScalar(cmd)
		quoted := strings.HasPrefix(scalar, "'")
		if quoted == !tc.windows {
			// POSIX commands are plain, Windows commands single-quoted,
			// exactly as testdata/hermes shows the dumper writing them.
			t.Errorf("%s: single-quoted = %v for a windows = %v command: %s", name, quoted, tc.windows, scalar)
		}
		got, ok := hermesDecodeScalar(scalar)
		if !ok || got != cmd {
			t.Errorf("%s: the scalar does not read back as the command\n got %q, %v\nwant %q", name, got, ok, cmd)
		}
	}
}

// hermesWrittenEntry is our entry as Hermes' PyYAML re-dump leaves it: the
// command a plain scalar broken at a space before column 80 with each
// continuation indented under the key, and the matcher unquoted. An emulation
// of the emitter, not its output — testdata/hermes holds the real thing — kept
// here so the generator can put this form through every shape it builds.
func hermesWrittenEntry(cmd string) string {
	const (
		width = 80
		cont  = "        " // PyYAML indents a folded scalar under its key
	)
	line := hermesCommandPrefix + hermesWrittenScalar(cmd)
	var out []string
	for len(line) > width {
		brk := strings.LastIndex(line[:width+1], " ")
		if brk <= len(cont) {
			break // nowhere to fold: leave the rest on one line
		}
		out = append(out, line[:brk])
		line = cont + line[brk+1:]
	}
	out = append(out, line, "      matcher: terminal")
	return strings.Join(out, "\n") + "\n"
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
