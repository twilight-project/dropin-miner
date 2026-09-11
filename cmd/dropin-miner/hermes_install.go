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
	"runtime"
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
	existing, mode, err := readWithMode(ops, path)
	if err != nil {
		p.refused = append(p.refused, fmt.Sprintf("%s: cannot read %s: %v", label, path, err))
		return false
	}
	body, ok := hermesHookYAML(entry)
	if !ok {
		p.refused = append(p.refused, fmt.Sprintf(
			"%s: cannot write a hook entry for %s — the path cannot be quoted for this platform's command splitter",
			label, entry.command))
		return false
	}
	stripped, _ := hermesRemoveBlock(existing)
	if reason := hermesConfigRefusal(stripped); reason != "" {
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
	body, ok := hermesHookYAML(entry)
	if !ok {
		return false
	}
	b, _, err := readWithMode(ops, path)
	if err != nil || b == nil {
		return false
	}
	i := bytes.Index(b, []byte(agentsMarkerBegin))
	if i < 0 {
		return false
	}
	j := bytes.Index(b[i:], []byte(agentsMarkerEnd))
	if j < 0 {
		return false
	}
	return bytes.Contains(b[i:i+j], []byte(body))
}

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
	if bytes.Contains(b, []byte(agentsMarkerBegin)) || bytes.Contains(b, []byte(agentsMarkerEnd)) {
		return "carries a partial dropin-miner block we cannot read back"
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
				return "does not put its top-level keys in the first column, so we cannot tell where its root mapping is"
			}
			content = true // part of a top-level key we have already read
			continue
		}
		if line[0] == '%' {
			continue // a directive, ahead of the document itself
		}
		if line == "---" || strings.HasPrefix(line, "--- ") {
			if document || content {
				return "holds more than one YAML document"
			}
			document = true
			continue
		}
		if line == "..." {
			return "holds more than one YAML document"
		}
		content = true
		key, ok := hermesTopLevelKey(line)
		if !ok {
			return "is not a plain top-level mapping we can safely extend"
		}
		if strings.EqualFold(key, "hooks") {
			return "already declares a top-level hooks: key"
		}
		rooted = true // a top-level key at column zero: this root is ours to extend
	}
	return ""
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
	i := strings.Index(s, agentsMarkerBegin)
	if i < 0 {
		return b, false
	}
	j := strings.Index(s[i:], agentsMarkerEnd)
	if j < 0 {
		return b, false
	}
	end := i + j + len(agentsMarkerEnd)
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
	return "hooks:\n" +
		"  pre_tool_call:\n" +
		"    - command: " + hermesYAMLSingleQuoted(cmd) + "\n" +
		// matcher is a regex fullmatched against the tool name, and
		// `terminal` is Hermes' shell tool. Least privilege: this hook is
		// handed only the tool calls whose arguments it is meant to read.
		"      matcher: \"terminal\"\n", true
}

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
