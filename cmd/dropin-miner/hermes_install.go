package main

// Hermes' config.yaml, edited without a YAML parser.
//
// Hermes has no per-workspace lineage file and no in-process plugin we can
// drop in, but it does run declared shell hooks: a `hooks:` block in
// config.yaml, each entry a command that receives the tool call as JSON on
// stdin and may return a directive. `agents install` registers one
// pre_tool_call entry running `dropin-miner hook hermes pre_tool_call`.
//
// config.yaml is the participant's own privileged host configuration and
// the dependency budget here is stdlib + toml — there is no YAML parser and
// this is not the place to grow one. That asymmetry decides the posture:
// **a false-positive refusal is cheap and an ambiguous mutation is not.**
// Refusing prints the snippet and leaves the file byte-identical; the
// participant pastes four lines. Guessing wrong corrupts the configuration
// of the agent we are asking them to trust us with. So we only ever APPEND
// our own marked block, we establish conservatively that the file is a
// plain top-level mapping with no hooks: key of its own, and anything we
// cannot establish that way is refused rather than edited.

import (
	"bytes"
	"fmt"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
)

// hermesHomeDir mirrors Hermes' own resolution (hermes_constants.py):
// HERMES_HOME wins; otherwise the platform default — %LOCALAPPDATA%\hermes on
// Windows, ~/.hermes elsewhere. Installs land on machines we do not control,
// so we honor the override rather than hardcode a single path.
func hermesHomeDir(home string, getenv func(string) string) string {
	if h := strings.TrimSpace(getenv("HERMES_HOME")); h != "" {
		return h
	}
	if runtime.GOOS == "windows" {
		base := strings.TrimSpace(getenv("LOCALAPPDATA"))
		if base == "" {
			base = filepath.Join(home, "AppData", "Local")
		}
		return filepath.Join(base, "hermes")
	}
	return filepath.Join(home, ".hermes")
}

// hermesSkillsDir is <home>/skills; the config is <home>/config.yaml.
func hermesSkillsDir(home string, getenv func(string) string) string {
	return filepath.Join(hermesHomeDir(home, getenv), "skills")
}

// hermesNoEOLNote records, inside our own block, that the config it was
// appended to did not end with a newline. Without it uninstall cannot tell
// the newline we added from one the participant's file already had, and
// "install then uninstall returns the original bytes" would be off by one
// byte. It is a comment, so Hermes never sees it as configuration.
const hermesNoEOLNote = "# the configuration above ended without a final newline; uninstall restores that"

// planHermesHook appends (or refreshes) our marked hooks: block in Hermes'
// config.yaml. Returns whether it planned a write.
func planHermesHook(ops agentOps, label, path string, entry binEntry, p *agentPlan) bool {
	return planHermesHookFor(ops, label, path, entry, runtime.GOOS == "windows", p)
}

// planHermesHookFor is planHermesHook with the splitter named, so that both
// platforms' fixtures can be driven on every runner.
func planHermesHookFor(ops agentOps, label, path string, entry binEntry, windows bool, p *agentPlan) bool {
	existing, mode, err := readWithMode(ops, path)
	if err != nil {
		p.refused = append(p.refused, fmt.Sprintf("%s: cannot read %s: %v", label, path, err))
		return false
	}
	cmd, ok := hermesHookCommand(entry, windows)
	if !ok {
		p.refused = append(p.refused, fmt.Sprintf(
			"%s: cannot write a hook entry for %s — the path cannot be quoted for this platform's command splitter",
			label, entry.command))
		return false
	}
	body := strings.Join(hermesHookLines(cmd), "\n") + "\n"
	if hermesMarkedBlockIsCurrent(existing, cmd) {
		// #105: our block, holding today's entry in whatever bytes Hermes'
		// round-trip writer left it in. Writing it back would be undone by
		// Hermes' next save, and would make install report a change on every
		// run of a host where nothing is wrong.
		return false
	}
	stripped, hadBlock := hermesRemoveBlock(existing)
	var own hermesOwnEntry
	if !hadBlock {
		own = findHermesOwnEntry(existing, refFor(entry))
	}
	if own.found {
		// #83: a hooks: block this client did not write, already holding this
		// installation's entry. That is not a refusal — the hook is there and
		// will fire — and it is not something to rewrite into a marked block
		// either, since the block around it is the participant's.
		p.notes = append(p.notes, fmt.Sprintf(
			"%s: already set up — the pre_tool_call hook is in %s at %s, under a hooks: block dropin-miner did not write; left as it is",
			label, path, own.where()))
		return false
	}
	if reason := hermesConfigRefusal(stripped); reason != "" {
		if own.mention > 0 {
			// The paste advice would put a second copy beside one that may
			// already be live. This client cannot vouch for where that line
			// sits, so it still refuses — but it says what it saw.
			p.refused = append(p.refused, fmt.Sprintf(
				"%s: %s %s, and line %d of it already names this installation's pre_tool_call hook command in a place or a form dropin-miner cannot read reliably; check whether it is already set up before adding this entry by hand:\n%s",
				label, path, reason, own.mention, indentBlock(body)))
			return false
		}
		p.refused = append(p.refused, fmt.Sprintf(
			"%s: %s %s; add this pre_tool_call entry by hand:\n%s",
			label, path, reason, indentBlock(body)))
		return false
	}
	return planWrite(ops, label, path, hermesAppendBlock(stripped, body), mode, "pre_tool_call lineage hook", p)
}

// hermesHookInstalled reports whether OUR block is in this config and names
// this binary and config — what `agents status` needs in order to tell a
// complete install from a skill sitting there with no lineage behind it.
// Read-only.
//
// It compares against the hook we WOULD write for this entry, rather than
// looking for the binary path as a substring. The path does not survive
// into the file unchanged: quoting splits an apostrophe into a
// quote-escape-quote run, so a path like /Users/O'Neil/bin/dropin-miner
// never appears contiguously in the YAML, and a substring test would report
// a complete install as "skill only" — the installer having worked
// perfectly, and somebody sent to debug an installation that is fine. Asking
// the one serializer what this entry looks like also answers the question
// status is really asking, which is not "is some hook of ours here" but
// "is the hook for THIS binary and THIS config here": a block left behind
// by a different install is not this installation being complete.
func hermesHookInstalled(ops agentOps, path string, entry binEntry) bool {
	return hermesHookInstalledFor(ops, path, entry, runtime.GOOS == "windows")
}

// hermesHookInstalledFor is hermesHookInstalled with the splitter named, so
// that both platforms' fixtures can be judged on every runner.
func hermesHookInstalledFor(ops agentOps, path string, entry binEntry, windows bool) bool {
	cmd, ok := hermesHookCommand(entry, windows)
	if !ok {
		return false
	}
	b, _, err := readWithMode(ops, path)
	if err != nil || b == nil {
		return false
	}
	s := string(b)
	i, j, marked := hermesMarkedSpan(s)
	if !marked {
		// No block of ours: the hook may still be there under a hooks: block
		// the participant wrote (#83), and then this installation is complete.
		return findHermesOwnEntry(b, refFor(entry)).found
	}
	// The renderer's own bytes, as before — a block that has come to hold
	// something more beside them still runs our hook, and what that something
	// is belongs to removal (#106), not to status. Or the same entry in the
	// bytes Hermes' round-trip writer leaves it in (#105).
	body := strings.Join(hermesHookLines(cmd), "\n") + "\n"
	return strings.Contains(s[i:j], body) || hermesMarkedBlockIsCurrent(b, cmd)
}

// ── our own block, after Hermes has been at it ──────────────────────────
//
// #105. Hermes has more than one writer for config.yaml, and they do
// different things to us (utils.py at NousResearch/hermes-agent d150fc202463,
// each caller read there rather than assumed):
//
//   - atomic_yaml_write re-dumps the file through PyYAML. Comments go, so our
//     markers go; that is the unmarked form findHermesOwnEntry reads. Its
//     callers are save_config, the setup wizard and `hermes config set`
//     (set_config_value -> _write_user_config).
//   - atomic_roundtrip_yaml_update and atomic_roundtrip_yaml_save are ruamel
//     round trips sharing one loader (_roundtrip_load: preserve_quotes,
//     indent 2/4/2, ruamel's default 80-column width). Comments and quotes
//     are KEPT, so our markers survive and our matcher keeps its quotes — and
//     the command, being longer than 80 columns, is folded onto a second line
//     inside them. Their callers are a model switch (persist_model_selection),
//     an in-session setting (cli.save_config_value), a personality change and
//     the TUI gateway's _save_cfg.
//
// A folded scalar is the same YAML and different bytes, and the question this
// used to ask was about bytes: is the rendered entry, byte for byte, between
// the markers. After one model switch the answer was no. Status said the hook
// was missing while Hermes ran it on every terminal call, and install wrote
// the block back on every run until Hermes folded it again.
//
// So the block is read, with the reader #83 built for the unmarked form. What
// it is compared with is deliberately NOT H5's rule, which answers whose an
// entry is and accepts every spelling this client ever wrote — v0.2.9's %q
// among them, which neither of Hermes' splitters undoes. The block is ours to
// rewrite, so the question here is narrower and costs nothing to keep narrow:
// does it decode to exactly the command the renderer writes today. A block
// that is this installation's in a spelling that is not is still refreshed.

// hermesMarkedSpan finds our block: s[i:j] runs from the begin marker to the
// start of the end marker. The first of each, as hermesRemoveBlock reads them.
func hermesMarkedSpan(s string) (i, j int, ok bool) {
	i = strings.Index(s, agentsMarkerBegin)
	if i < 0 {
		return 0, 0, false
	}
	n := strings.Index(s[i:], agentsMarkerEnd)
	if n < 0 {
		return 0, 0, false
	}
	return i, i + n, true
}

// hermesMarkedBlockIsCurrent: does what lies between our markers decode to
// the entry the renderer writes for cmd?
func hermesMarkedBlockIsCurrent(b []byte, cmd string) bool {
	s := string(b)
	i, j, ok := hermesMarkedSpan(s)
	if !ok {
		return false
	}
	got, ok := hermesReadMarkedBlock(s[i+len(agentsMarkerBegin) : j])
	return ok && got == cmd
}

// hermesReadMarkedBlock decodes what lies between our markers, and answers
// only when that is the mapping the renderer writes and nothing besides:
// hooks:, pre_tool_call:, one entry whose command is a scalar in any of the
// three styles with its folding undone, and its matcher. Blank lines and
// comments are passed over between those lines — our own note is one — but
// not inside the command, where hermesItemCommand stops at the first line
// that is not a continuation and an unterminated scalar then fails to decode.
func hermesReadMarkedBlock(inside string) (cmd string, ok bool) {
	lines := hermesLines([]byte(inside))
	var content []int
	for i, l := range lines {
		if l.hasTabs {
			return "", false
		}
		if l.indent >= 0 {
			content = append(content, i)
		}
	}
	want := hermesHookLines("")
	if len(content) < 4 || lines[content[0]].text != want[0] || lines[content[1]].text != want[1] {
		return "", false
	}
	at := content[2]
	if lines[at].indent != 4 {
		return "", false
	}
	cmd, ok = hermesItemCommand(lines, at, len(lines))
	if !ok {
		return "", false
	}
	// What follows the command's own continuation lines is the matcher, and
	// after the matcher there is nothing.
	k := at + 1
	for k < len(lines) && lines[k].indent > 6 {
		k++
	}
	if next := hermesNextContent(lines, k); next < 0 || next != content[len(content)-1] || !hermesIsOurMatcher(lines[next]) {
		return "", false
	}
	return cmd, true
}

// hermesIsOurMatcher: `      matcher: ` and a scalar that decodes to the tool
// name the renderer writes. ruamel keeps the quotes we wrote and PyYAML drops
// them; both are the same matcher.
func hermesIsOurMatcher(l hermesLine) bool {
	if l.indent != 6 {
		return false
	}
	rest, ok := strings.CutPrefix(l.text[6:], "matcher:")
	if !ok || rest == "" || rest[0] != ' ' {
		return false
	}
	v, ok := hermesDecodeScalar(strings.TrimSpace(rest))
	return ok && v == hermesMatcher
}

const hermesMatcher = "terminal"

// hermesConfigRefusal reports, in a phrase that completes "config.yaml …",
// why our block must not be appended to this configuration — or "" when
// appending it is safe.
//
// Safe means one thing: the file is a single YAML document whose top level
// is a block mapping written at column zero, and none of its keys is
// `hooks`. We accept only the forms we can recognize with certainty — a
// plain or quoted key followed by a colon. Every other column-zero line is
// a structure we did not expect (a top-level sequence, a flow mapping, an
// explicit key, a second document, a plain scalar continuation), and an
// unexpected structure is a refusal. Comments are skipped wherever they
// appear, so a line merely MENTIONING hooks is not a hooks section; a
// `hooks` key is matched whatever its spelling, including `hooks :`,
// `'hooks':` and `"hooks":`, and case-insensitively, because a config
// strange enough to carry `HOOKS:` is one a person should look at rather
// than a program.
//
// Column zero is not merely where we look for keys — it is a property the
// file has to have before we may append anything. A YAML root mapping may
// itself be written indented, every line of it, consistently: a config
// whose first content line is two spaces and then `hooks:`, with its own
// entries indented further under that. A scan that reads only column zero
// sees no keys at all in such a file, calls it empty, and appends a
// column-zero `hooks:` — landing a second root key in a document whose
// root is somewhere else entirely, and taking the participant's hooks with
// it. We cannot establish that file's indentation model without a parser,
// and this is not the place to grow one, so content appearing before any
// column-zero key is a refusal whatever it says. That costs us the odd
// indented-but-ordinary config, which is the direction of error we chose.
// (The refusal cases in hermes_install_test.go carry the literal shapes;
// they are the readable version of this paragraph.)
func hermesConfigRefusal(b []byte) string {
	reason, hooks := hermesConfigScan(b)
	if reason != "" {
		return reason
	}
	if hooks > 0 {
		return "already declares a top-level hooks: key"
	}
	return ""
}

// hermesConfigScan is the scan above with the hooks: key counted rather than
// refused, for the one caller that has to look inside a hooks: block it did
// not write (findHermesOwnEntry). Everything else it establishes is the same
// and is established first: a file whose structure cannot be vouched for is
// not one whose hooks: block can be.
func hermesConfigScan(b []byte) (reason string, hooks int) {
	if bytes.Contains(b, []byte(agentsMarkerBegin)) || bytes.Contains(b, []byte(agentsMarkerEnd)) {
		return "carries a partial dropin-miner block we cannot read back", 0
	}
	document, content, rooted := false, false, false
	for _, raw := range strings.Split(string(b), "\n") {
		line := strings.TrimRight(raw, " \t\r")
		if line == "" {
			continue
		}
		if bare := strings.TrimLeft(line, " \t"); strings.HasPrefix(bare, "#") {
			continue // a comment, at any indent
		}
		if line[0] == ' ' || line[0] == '\t' {
			if !rooted {
				// Indented content with no column-zero key above it: the
				// root of this document is not where our block would go.
				return "does not put its top-level keys in the first column, so we cannot tell where its root mapping is", 0
			}
			content = true // part of a top-level key we have already read
			continue
		}
		if line[0] == '%' {
			continue // a directive, ahead of the document itself
		}
		if line == "---" || strings.HasPrefix(line, "--- ") {
			if document || content {
				return "holds more than one YAML document", 0
			}
			document = true
			continue
		}
		if line == "..." {
			return "holds more than one YAML document", 0
		}
		content = true
		key, ok := hermesTopLevelKey(line)
		if !ok {
			return "is not a plain top-level mapping we can safely extend", 0
		}
		if strings.EqualFold(key, "hooks") {
			hooks++
		}
		rooted = true // a top-level key at column zero: this root is ours to extend
	}
	return "", hooks
}

// hermesTopLevelKey reads the key of a column-zero block-mapping entry —
// `key:`, `key :`, `'key':`, `"key":`, with or without a value or trailing
// comment. ok is false for anything it cannot read with certainty, which
// the caller treats as a refusal rather than a guess.
func hermesTopLevelKey(line string) (string, bool) {
	var key, rest string
	switch line[0] {
	case '\'', '"':
		q := line[0]
		if q == '"' && strings.Contains(line, `\`) {
			return "", false // escapes in a key: not worth decoding by hand
		}
		end := strings.IndexByte(line[1:], q)
		if end < 0 {
			return "", false
		}
		key, rest = line[1:1+end], line[1+end+1:]
	default:
		colon := strings.IndexByte(line, ':')
		if colon < 0 {
			return "", false
		}
		key, rest = line[:colon], line[colon:]
		// A plain YAML key holds none of the indicator characters; one
		// that does is something other than the mapping entry it resembles.
		if strings.ContainsAny(key, "#{}[]&*!|>'\"%@`,") {
			return "", false
		}
	}
	rest = strings.TrimLeft(rest, " \t")
	if !strings.HasPrefix(rest, ":") {
		return "", false
	}
	// `key:value` with no space is a plain scalar in YAML, not a mapping.
	if after := rest[1:]; after != "" && after[0] != ' ' && after[0] != '\t' {
		return "", false
	}
	return strings.TrimRight(key, " \t"), true
}

// hermesAppendBlock puts our block at the end of the configuration, with
// exactly the separator hermesRemoveBlock knows how to take back off. The
// pair is an exact inverse — install then uninstall returns the bytes it
// started from, final newline or not — which is the contract for editing a
// file somebody else owns.
func hermesAppendBlock(existing []byte, body string) []byte {
	switch {
	case len(existing) == 0:
		return hermesHookBlock(body, false)
	case bytes.HasSuffix(existing, []byte("\n")):
		return append(append([]byte{}, existing...), append([]byte("\n"), hermesHookBlock(body, false)...)...)
	default:
		return append(append([]byte{}, existing...), append([]byte("\n\n"), hermesHookBlock(body, true)...)...)
	}
}

// hermesRemoveBlock strips our block and the separator hermesAppendBlock
// put in front of it, restoring the surrounding configuration exactly.
func hermesRemoveBlock(b []byte) ([]byte, bool) {
	s := string(b)
	i, j, ok := hermesMarkedSpan(s)
	if !ok {
		return b, false
	}
	end := j + len(agentsMarkerEnd)
	if end < len(s) && s[end] == '\n' {
		end++
	}
	pre, block, post := s[:i], s[i:end], s[end:]
	drop := 1
	if strings.Contains(block, hermesNoEOLNote) {
		drop = 2
	}
	for ; drop > 0 && strings.HasSuffix(pre, "\n"); drop-- {
		pre = pre[:len(pre)-1]
	}
	return []byte(pre + post), true
}

// ── the hook entry itself ───────────────────────────────────────────────
//
// Two quoting layers sit between the path on this machine and the process
// Hermes eventually starts, and getting either wrong silently installs a
// hook that never runs:
//
//   - YAML. The command is one single-quoted scalar, where the only escape
//     is a doubled quote. Backslashes and non-ASCII are literal, which is
//     what a Windows path and a non-ASCII home directory both need.
//   - Hermes' own splitter. The command is NOT run through a shell:
//     split_command_line tokenizes it and execs argv directly (shell=False).
//     On POSIX that is exactly shlex.split; on Windows it is
//     shlex.split(posix=False) with one layer of matching quotes stripped
//     per token — deliberately, so that Windows path backslashes survive,
//     since POSIX shlex would eat them.
//
// Those two splitters want different quoting, and the installer knows which
// one will run because Hermes runs on the machine we are installing on. So
// the shell layer is chosen per platform rather than guessed: POSIX single
// quotes, which can carry any byte including an apostrophe; Windows double
// quotes, which the non-POSIX splitter strips whole and which no legal
// Windows path can contain.
//
// Go's %q was what this used to be, and it is wrong for both: it escapes a
// backslash to \\ and a non-ASCII rune to \uXXXX, neither of which either
// splitter undoes. A participant whose home directory is not ASCII would
// have had a hook pointing at a binary that does not exist.

// hermesHookYAML is the hooks: mapping we install, and the snippet printed
// when we refuse to install it. ok is false when the command cannot be
// represented for this platform's splitter at all, in which case there is
// no honest snippet to print either.
func hermesHookYAML(entry binEntry) (string, bool) {
	cmd, ok := hermesHookCommand(entry, runtime.GOOS == "windows")
	if !ok {
		return "", false
	}
	return strings.Join(hermesHookLines(cmd), "\n") + "\n", true
}

// hermesHookLines is the hooks: mapping for one command, a line at a time.
// The one renderer: install writes these, and findHermesOwnEntry compares a
// participant's file against these same lines before it will remove any, so
// what may be deleted is by construction what this function produces.
func hermesHookLines(cmd string) []string {
	return []string{
		"hooks:",
		"  pre_tool_call:",
		hermesCommandPrefix + hermesYAMLSingleQuoted(cmd),
		// matcher is a regex fullmatched against the tool name, and
		// `terminal` is Hermes' shell tool. Least privilege: this hook is
		// handed only the tool calls whose arguments it is meant to read.
		`      matcher: "terminal"`,
	}
}

const hermesCommandPrefix = "    - command: "

// hermesHookCommand is the argv Hermes should exec, as one shell-quoted
// string. windows selects which splitter it has to survive.
func hermesHookCommand(entry binEntry, windows bool) (string, bool) {
	bin, ok := hermesQuoteArg(entry.command, windows)
	if !ok {
		return "", false
	}
	cmd := bin + " hook"
	if entry.cfg != "" {
		cfg, ok := hermesQuoteArg(entry.cfg, windows)
		if !ok {
			return "", false
		}
		cmd += " -config " + cfg
	}
	return cmd + " hermes pre_tool_call", true
}

// hermesQuoteArg quotes one argv token for Hermes' command splitter. ok is
// false only on Windows for a token containing a double quote — a character
// no Windows path may hold, and the one case the non-POSIX splitter cannot
// represent.
//
// An ordinary POSIX path is left bare, because this string is also the
// snippet a participant is asked to paste by hand when we refuse to edit
// their config, and three layers of nested quotes around a path that needed
// none is a snippet people mistype. Bare is safe only for the characters
// below, which no splitter treats as anything but themselves; everything
// else — a space, a backslash, a quote, any non-ASCII byte — takes the
// single quotes.
func hermesQuoteArg(s string, windows bool) (string, bool) {
	if windows {
		// Always quoted: spaces are ordinary in Windows paths, and the
		// non-POSIX splitter strips exactly one layer of matching quotes.
		if strings.Contains(s, `"`) {
			return "", false
		}
		return `"` + s + `"`, true
	}
	if s != "" && strings.IndexFunc(s, func(r rune) bool { return !hermesBareArgRune(r) }) < 0 {
		return s, true
	}
	// POSIX single quotes carry every byte literally; an embedded quote
	// ends the run, escapes itself, and opens a new one.
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'", true
}

// hermesBareArgRune: characters that survive POSIX word splitting unquoted
// and carry no meaning to a shell either, so the same token is safe in the
// snippet a person pastes into a shell of their own.
func hermesBareArgRune(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return true
	}
	return strings.ContainsRune("_-./:=+@%", r)
}

// hermesYAMLSingleQuoted renders s as a YAML single-quoted scalar, where a
// doubled quote is the one and only escape.
func hermesYAMLSingleQuoted(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

func hermesHookBlock(body string, noEOL bool) []byte {
	note := ""
	if noEOL {
		note = hermesNoEOLNote + "\n"
	}
	return []byte(agentsMarkerBegin + "\n" + note + body + agentsMarkerEnd + "\n")
}

// ── our entry under a hooks: block this client did not write ────────────
//
// #83. hermesConfigRefusal refuses any file with a top-level hooks: key, so
// a machine whose hooks: block already held our pre_tool_call entry — with
// no markers around it — got "Some agent could not be set up" on every run.
// And because setup had not written it, nothing tracked it: uninstall
// neither listed nor removed it, and Hermes went on invoking a binary that
// might no longer exist, on every terminal call.
//
// There is no YAML parser here and this is still not the place to grow one,
// so the posture is the file's own, applied to deletion: a false-positive
// refusal is cheap and an ambiguous mutation is not. Recognizing our entry
// is allowed to be generous about nothing. It is ours only when the file's
// structure can be vouched for, the entry sits exactly where the renderer
// puts one, its command is this installation's under H5's rule, and the
// lines to go are — byte for byte — lines the renderer produces for that
// command. A boundary this scan misses must cost a refusal, never someone
// else's hook (L2b's lesson, carried from TOML to YAML).

// hermesOwnEntry is what was found.
type hermesOwnEntry struct {
	found bool
	// start, end: the lines [start, end) whose removal takes out our entry
	// and leaves every other line of the file byte-identical. Both zero
	// when the entry was found but the edit cannot be expressed that way.
	start, end int
	why        string // why not, in a phrase that completes "… because it"
	// first, last: the 1-based lines the entry occupies, so a note that
	// asks a participant to remove it by hand can say where it is.
	first, last int
	// mention: nothing was found where a hook lives and in a form this
	// client can vouch for, but this 1-based line names this installation's
	// hook command. Grounds for a sentence, never for an edit.
	mention int
}

func (e hermesOwnEntry) removable() bool { return e.found && e.end > e.start }

func (e hermesOwnEntry) where() string {
	if e.last <= e.first {
		return fmt.Sprintf("line %d", e.first)
	}
	return fmt.Sprintf("lines %d-%d", e.first, e.last)
}

// hermesLine is one line of the file, with what the scan needs of it.
type hermesLine struct {
	raw     string // the original bytes, line ending included
	text    string // without the line ending
	indent  int    // leading spaces; -1 for a blank or comment line
	hasTabs bool   // a tab in the indentation: not something to reason about
}

func hermesLines(b []byte) []hermesLine {
	var out []hermesLine
	for _, raw := range strings.SplitAfter(string(b), "\n") {
		if raw == "" {
			continue
		}
		l := hermesLine{raw: raw, text: strings.TrimRight(raw, "\r\n")}
		body := strings.TrimLeft(l.text, " \t")
		switch {
		case body == "" || strings.HasPrefix(body, "#"):
			l.indent = -1
		default:
			lead := l.text[:len(l.text)-len(body)]
			l.indent, l.hasTabs = len(lead), strings.Contains(lead, "\t")
		}
		out = append(out, l)
	}
	return out
}

// findHermesOwnEntry looks for this installation's pre_tool_call entry in a
// config that carries no marked block of ours. Three answers, in descending
// order of what may be done about them:
//
//   - found and removable: the renderer's own form, provably. Uninstall takes
//     the lines out.
//   - found: a live hook of ours where a hook lives, in a form this client
//     will not edit. Install and status count it; uninstall names its lines
//     and leaves it. This is the form HERMES writes: save_config reloads
//     config.yaml as data and dumps it with PyYAML, so after Hermes' first
//     save our marker comments are gone and our entry is a plain or
//     single-quoted scalar folded at 80 columns. Without this tier every
//     symptom of #83 came straight back after one save: status "not
//     installed", install refusing with a second copy to paste, uninstall
//     leaving the hook without a word.
//   - mention: the command is named somewhere this scan cannot vouch for.
//     A sentence on uninstall and on install's refusal; never an edit, and
//     never "already set up", because a command quoted in somebody's notes
//     is not a hook.
func findHermesOwnEntry(b []byte, ref installationRef) hermesOwnEntry {
	if e := hermesFindStructured(b, ref); e.found {
		return e
	}
	return hermesOwnEntry{mention: hermesMention(b, ref)}
}

func hermesFindStructured(b []byte, ref installationRef) hermesOwnEntry {
	// The structure first, by the same scan install trusts: one document, a
	// plain mapping at column zero, no partial block of ours. Exactly one
	// hooks: key, because with two there is no saying which one Hermes reads.
	if reason, hooks := hermesConfigScan(b); reason != "" || hooks != 1 {
		return hermesOwnEntry{}
	}
	lines := hermesLines(b)
	for _, l := range lines {
		// Not redundant with the exact-text checks below: a tab can sit in
		// the indentation of a line that is none of ours — a sibling entry —
		// and every depth comparison made about that line would then be
		// about a number that means nothing.
		if l.hasTabs {
			return hermesOwnEntry{}
		}
	}
	want := hermesHookLines("")

	// The hooks: line, spelled exactly as the renderer spells it. `hooks :`,
	// `"hooks":`, `hooks: &h` or `hooks: # mine` are all a hooks key to the
	// scan above, and none of them is a block this function knows how to
	// read; they fall to the mention tier.
	top := -1
	for i, l := range lines {
		if l.indent == 0 && l.text == want[0] {
			top = i
			break
		}
	}
	if top < 0 {
		return hermesOwnEntry{}
	}
	blockEnd := len(lines)
	for i := top + 1; i < len(lines); i++ {
		if lines[i].indent == 0 {
			blockEnd = i
			break
		}
	}

	var hits []int
	for i := top + 1; i < blockEnd; i++ {
		if hermesEntryIsOurs(lines, i, top, blockEnd, ref) {
			hits = append(hits, i)
		}
	}
	if len(hits) == 0 {
		return hermesOwnEntry{}
	}
	at := hits[0]
	parent := hermesParent(lines, at, top)
	listEnd := hermesListEnd(lines, parent, blockEnd)

	// Two rules about the block itself come BEFORE "found", because when
	// either trips this scan does not know what a YAML parser makes of the
	// lines it matched — and "found" is a claim install turns into "already
	// set up" and status into "installed". Review's oracle caught both
	// claimed where PyYAML read no live hook at all. So these answer at the
	// mention tier: a sentence that is true whichever way the parser reads
	// it, a refusal with a warning from install, nothing counted by status.
	//
	// Exactly one pre_tool_call: key, for the reason there must be exactly
	// one hooks: key, one level down. YAML keeps the last of two — a second
	// `pre_tool_call: null` included — so ours under the first is dead text,
	// and taking ours out of the second would un-shadow the first.
	events := 0
	for k := top + 1; k < blockEnd; k++ {
		if lines[k].indent != 2 {
			continue
		}
		if key, ok := hermesTopLevelKey(lines[k].text[2:]); ok && strings.EqualFold(key, "pre_tool_call") {
			events++
		}
	}
	if events != 1 {
		return hermesOwnEntry{}
	}
	// Everything at list depth must BE a list entry. A `|` or a `>-` alone
	// on the line under pre_tool_call: makes our two lines below it the TEXT
	// of a block scalar, not a hook; an anchor or a tag there is content at
	// the right depth that is not a sibling. Counted as one, it took our two
	// lines out from under a block scalar, a null and a file that then would
	// not parse.
	for k := parent + 1; k < listEnd; k++ {
		if lines[k].indent < 0 || lines[k].indent > 4 {
			continue
		}
		if item := lines[k].text[lines[k].indent:]; lines[k].indent != 4 || (item != "-" && !strings.HasPrefix(item, "- ")) {
			return hermesOwnEntry{}
		}
	}
	// And the block must be one a parser can load at all, as far as that can
	// be said of our own entry without being one. The oracle, pointed at
	// install, found "already set up" claimed on files PyYAML rejects
	// outright: a line at depth one or five after our entry, and our matcher
	// line with the next key glued to it where a final newline was missing.
	// Hermes cannot load those, so no hook in them is live.
	for k := top + 1; k < blockEnd; k++ {
		if lines[k].indent == 1 || lines[k].indent == 3 {
			return hermesOwnEntry{}
		}
	}
	if !hermesItemReadable(lines, at, listEnd) {
		return hermesOwnEntry{}
	}

	// Ours, and from here on the question is only whether it can be taken
	// out. An entry that cannot be still runs our binary on every terminal
	// call, so it is reported and left rather than not seen at all: silence
	// about a hook that outlives the installation is what #83 was.
	entry := hermesOwnEntry{found: true, first: at + 1, last: at + 1}
	for k := at + 1; k < listEnd; k++ {
		if lines[k].indent >= 0 && lines[k].indent <= 4 {
			break
		}
		if lines[k].indent > 4 {
			entry.last = k + 1
		}
	}
	leave := func(why string) hermesOwnEntry { entry.why = why; return entry }

	if len(hits) > 1 {
		return leave("appears there more than once")
	}
	if _, ok := hermesDecodeCommandLine(lines[at].text); !ok {
		return leave("is not written the way dropin-miner writes it — Hermes rewrites config.yaml in its own style when it saves it")
	}
	if at+1 >= len(lines) || lines[at+1].text != want[3] {
		return leave("does not have the matcher line dropin-miner writes under it, so it has been edited")
	}
	if next := hermesNextContent(lines, at+2); next >= 0 && lines[next].indent > 4 {
		return leave("has a further key inside it that dropin-miner did not write")
	}

	// How much goes. Our two lines when another entry shares the list; the
	// pre_tool_call: line with them when ours was its only entry, since a key
	// left with no value is a null where Hermes expects a list; and hooks:
	// too when that was its only key, for the same reason. Each is a suffix
	// of what the renderer writes, and each must be contiguous: a comment or
	// a blank line in between belongs to the participant and cannot be
	// stepped over.
	start := at
	if !hermesHasOther(lines, parent+1, listEnd, at, at+2, 4) {
		if parent != at-1 {
			return leave("is the only pre_tool_call entry and is separated from its pre_tool_call: line, so removing it cleanly cannot be done by line")
		}
		start = parent
		if !hermesHasOther(lines, top+1, blockEnd, parent, at+2, 2) {
			if top != parent-1 {
				return leave("is the only hook and is separated from its hooks: line, so removing it cleanly cannot be done by line")
			}
			start = top
		}
	}
	entry.start, entry.end = start, at+2
	if !hermesRunIsRendered(lines, entry, at) {
		entry.start, entry.end = 0, 0
		return leave("is not written exactly as dropin-miner writes it")
	}
	return entry
}

// hermesEntryIsOurs: does a list entry begin at i whose command is this
// installation's hook, sitting where a pre_tool_call hook lives?
func hermesEntryIsOurs(lines []hermesLine, i, top, blockEnd int, ref installationRef) bool {
	if lines[i].indent != 4 {
		return false
	}
	cmd, ok := hermesItemCommand(lines, i, blockEnd)
	if !ok || !hermesCommandIsOurHook(cmd, ref) {
		return false
	}
	// Under `  pre_tool_call:`, itself under the hooks: line found above.
	parent := hermesParent(lines, i, top)
	if parent < 0 || lines[parent].text != hermesHookLines("")[1] {
		return false
	}
	for k := parent - 1; k > top; k-- {
		if lines[k].indent >= 0 && lines[k].indent < 2 {
			return false
		}
	}
	return true
}

// hermesItemReadable: below its command, is our entry made only of lines a
// YAML parser would accept where they stand — `key: value` at the entry's own
// depth, with a value that is empty, plain, or one complete quoted scalar, and
// deeper lines only under such a key? Not a parser and not trying to be one:
// it judges the few lines of OUR entry, and only ever to withdraw a claim.
func hermesItemReadable(lines []hermesLine, at, listEnd int) bool {
	k := at + 1
	for k < listEnd && lines[k].indent > 6 {
		k++ // the command's own folded continuation lines
	}
	underKey := false
	for ; k < listEnd; k++ {
		switch l := lines[k]; {
		case l.indent < 0:
			continue
		case l.indent <= 4:
			return true
		case l.indent == 6:
			if !hermesKeyValueLine.MatchString(l.text[6:]) {
				return false
			}
			underKey = true
		case l.indent > 6 && underKey:
		default:
			return false // depth five, or deeper with no key above it
		}
	}
	return true
}

// hermesKeyValueLine is `key:` or `key: value`, the value plain or one whole
// quoted scalar with nothing after it.
var hermesKeyValueLine = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]*:(?: +(?:"(?:[^"\\]|\\.)*"|'(?:[^']|'')*'|[^\s"'#&*!|>{}\[\]%@` + "`" + `][^#"']*?))? *$`)

// hermesItemCommand reads the command scalar of the entry at i: the rest of
// its `    - command:` line and, for a scalar folded over several lines the
// way PyYAML folds a long one, the deeper lines under it, joined by single
// spaces as YAML joins them.
func hermesItemCommand(lines []hermesLine, i, blockEnd int) (string, bool) {
	key := strings.TrimRight(hermesCommandPrefix, " ")
	rest, ok := strings.CutPrefix(lines[i].text, key)
	if !ok || (rest != "" && rest[0] != ' ') {
		return "", false
	}
	value := strings.TrimSpace(rest)
	for k := i + 1; k < blockEnd && lines[k].indent > 6; k++ {
		value += " " + strings.TrimSpace(lines[k].text)
	}
	return hermesDecodeScalar(strings.TrimSpace(value))
}

// hermesDecodeScalar reads a scalar in the three styles a command can be
// written in. Anything it is not sure of is not a command.
func hermesDecodeScalar(v string) (string, bool) {
	switch {
	case v == "":
		return "", false
	case v[0] == '\'':
		return hermesUnquoteSingle(v)
	case v[0] == '"':
		s, err := strconv.Unquote(v)
		return s, err == nil
	case strings.ContainsRune("|>&*!{}[]%@`#,?-:", rune(v[0])):
		return "", false // an indicator: a block scalar, an anchor, a flow collection
	case strings.Contains(v, " #") || strings.Contains(v, ": "):
		return "", false // a comment or a mapping inside it: not a plain scalar
	}
	return v, true
}

// hermesUnquoteSingle reads a YAML single-quoted scalar, and accepts only one
// that the renderer's own encoder reproduces exactly from what was decoded.
// A lone quote inside — which a lenient reading would decode to the very same
// command — is not a scalar any YAML parser reads that way.
func hermesUnquoteSingle(q string) (string, bool) {
	if len(q) < 2 || q[0] != '\'' || q[len(q)-1] != '\'' {
		return "", false
	}
	s := strings.ReplaceAll(q[1:len(q)-1], "''", "'")
	if hermesYAMLSingleQuoted(s) != q {
		return "", false
	}
	return s, true
}

// hermesParent is the nearest line above i that is less indented than the
// item, or -1.
func hermesParent(lines []hermesLine, i, top int) int {
	for k := i - 1; k > top; k-- {
		if lines[k].indent >= 0 && lines[k].indent < 4 {
			return k
		}
	}
	return -1
}

// hermesListEnd is where the list under parent stops: the next line at or
// above the parent's own depth.
func hermesListEnd(lines []hermesLine, parent, blockEnd int) int {
	for k := parent + 1; k < blockEnd; k++ {
		if lines[k].indent >= 0 && lines[k].indent <= 2 {
			return k
		}
	}
	return blockEnd
}

// hermesHasOther: is there a content line at exactly depth in [from, to),
// outside [skipFrom, skipTo)? At depth 4 that is a sibling entry — the
// caller has already established that everything at that depth is one — and
// at depth 2 a sibling event.
func hermesHasOther(lines []hermesLine, from, to, skipFrom, skipTo, depth int) bool {
	for k := from; k < to; k++ {
		if k >= skipFrom && k < skipTo {
			continue
		}
		if lines[k].indent == depth {
			return true
		}
	}
	return false
}

// hermesNextContent is the first line at or after i that is neither blank
// nor a comment, or -1.
func hermesNextContent(lines []hermesLine, i int) int {
	for k := i; k < len(lines); k++ {
		if lines[k].indent >= 0 {
			return k
		}
	}
	return -1
}

// hermesDecodeCommandLine reads `    - command: '<scalar>'` and returns the
// scalar: the renderer's own form, on one line. A folded scalar, a plain
// one, a trailing comment or a different quote style is not this.
func hermesDecodeCommandLine(text string) (string, bool) {
	if !strings.HasPrefix(text, hermesCommandPrefix) {
		return "", false
	}
	return hermesUnquoteSingle(text[len(hermesCommandPrefix):])
}

// hermesCommandIsOurHook: H5's rule — this installation's binary, this
// installation's config, in any spelling hookCommandIsOurs knows — and then
// exactly the words the renderer puts after them. Another subcommand of this
// same binary is somebody's own hook, not ours.
func hermesCommandIsOurHook(cmd string, ref installationRef) bool {
	if !hookCommandIsOurs(cmd, ref) {
		return false
	}
	rest, ok := ref.afterBinary(cmd)
	if !ok {
		return false
	}
	words := renderedWords(rest)
	switch len(words) {
	case 3:
		return words[0] == "hook" && words[1] == "hermes" && words[2] == "pre_tool_call"
	case 5:
		return words[0] == "hook" && words[1] == "-config" && words[3] == "hermes" && words[4] == "pre_tool_call"
	}
	return false
}

// hermesRunIsRendered is the net. Whatever the scan above concluded, the
// lines about to go must be, byte for byte, the last lines of what the
// renderer writes for this command. If they are not, the scan was wrong
// about something, and the only safe reading of that is to touch nothing.
func hermesRunIsRendered(lines []hermesLine, e hermesOwnEntry, at int) bool {
	cmd, ok := hermesDecodeCommandLine(lines[at].text)
	if !ok {
		return false
	}
	rendered := hermesHookLines(cmd)
	n := e.end - e.start
	if n < 2 || n > len(rendered) {
		return false
	}
	want := rendered[len(rendered)-n:]
	for k := 0; k < n; k++ {
		if lines[e.start+k].text != want[k] {
			return false
		}
	}
	// The run must also BEGIN under what the renderer puts above it. A run
	// of two lines carries no heading of its own to compare, so without this
	// our two lines under `  post_tool_call:` pass every comparison above:
	// walk up from the run, and each ancestor must be the rendered line for
	// its depth.
	for level, idx := lines[e.start].indent, e.start; level > 0; {
		p := idx - 1
		for p >= 0 && (lines[p].indent < 0 || lines[p].indent >= level) {
			p--
		}
		if p < 0 {
			return false
		}
		switch lines[p].indent {
		case 2:
			if lines[p].text != rendered[1] {
				return false
			}
		case 0:
			if lines[p].text != rendered[0] {
				return false
			}
		default:
			return false
		}
		level, idx = lines[p].indent, p
	}
	// And the run must END where the thing it removes ends. Lines that are
	// ours can still be the head of something that is not: take the two
	// lines of an entry that has a `timeout:` under them and the timeout is
	// left dangling under the entry above; take `pre_tool_call:` while
	// another entry sits under it and that entry is re-parented or orphaned.
	// Every line compared above would have been rendered, and the file
	// would still be broken. So whatever follows may be no deeper than the
	// run's own first line — nothing after it belongs to what went.
	if next := hermesNextContent(lines, e.end); next >= 0 && lines[next].indent > lines[e.start].indent {
		return false
	}
	return true
}

// removeHermesOwnEntry takes the run out. Every other line is copied as it
// was read, line ending and all.
func removeHermesOwnEntry(b []byte, e hermesOwnEntry) []byte {
	var out strings.Builder
	for i, l := range hermesLines(b) {
		if i >= e.start && i < e.end {
			continue
		}
		out.WriteString(l.raw)
	}
	return []byte(out.String())
}

// hermesMention is the last tier: does this file name this installation's
// hook command anywhere outside a comment? It returns the 1-based line, or 0.
//
// It exists for the shapes the structured find deliberately will not read —
// `hooks: ` with a trailing space, `hooks: &h`, `  "pre_tool_call":`, the
// explicit `? pre_tool_call` key, a closing `...`, `HOOKS:` beside `hooks:` —
// every one of which a YAML parser reads as a live hook of ours while the
// plan said "Hermes: not installed". It reads words, not structure: the
// file's non-comment text split on whitespace, which is also exactly what
// survives YAML's line folding, searched for the renderer's closing words
// and then backwards for a command that decodes, in any of the three scalar
// styles, to this installation's hook under H5's rule. Knowing nothing about
// where the words sit is what lets it see all of those shapes, and is why it
// may only ever produce a sentence.
func hermesMention(b []byte, ref installationRef) int {
	type word struct {
		text string
		line int
	}
	var words []word
	for i, l := range hermesLines(b) {
		if l.indent < 0 {
			continue
		}
		for _, f := range strings.Fields(l.text) {
			words = append(words, word{f, i + 1})
		}
	}
	const closers = `'",}]`
	for e := 1; e < len(words); e++ {
		if strings.TrimRight(words[e].text, closers) != "pre_tool_call" || words[e-1].text != "hermes" {
			continue
		}
		for s := e - 2; s >= 0 && s >= e-24; s-- {
			parts := make([]string, 0, e-s+1)
			for _, w := range words[s : e+1] {
				parts = append(parts, w.text)
			}
			cand := strings.TrimRight(strings.Join(parts, " "), ",}]")
			for _, cmd := range hermesScalarReadings(cand) {
				if hermesCommandIsOurHook(cmd, ref) {
					return words[s].line
				}
			}
		}
	}
	return 0
}

// hermesScalarReadings is every command a run of words might spell: as it
// stands, and unquoted in either YAML quote style. Lenient on purpose, which
// is affordable only because nothing is ever edited on its say-so.
func hermesScalarReadings(cand string) []string {
	out := []string{cand}
	if len(cand) >= 2 {
		switch cand[0] {
		case '\'':
			out = append(out, strings.ReplaceAll(strings.TrimSuffix(cand[1:], "'"), "''", "'"))
		case '"':
			if s, err := strconv.Unquote(cand); err == nil {
				out = append(out, s)
			}
		}
	}
	return out
}
