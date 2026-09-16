package main

// Rendering a command for the shell that will run it.
//
// v0.2.9 built every command with Go's %q and wrapped the request in a Bash
// heredoc, whatever the host and whatever the OS. %q is Go's quoting, not any
// shell's: it doubles a backslash, escapes a non-ASCII rune to \uXXXX, and
// leaves $ and ` alone inside the double quotes it writes. In Bash that is
// wrong for a path containing $ or a backtick; in PowerShell a command that
// begins with a quoted string is an expression, not an invocation, and the
// next word is a parse error; in cmd there is no heredoc at all.
//
// So every string is rendered for a declared shell (H-R1), and this file owns
// the quoting of every path that goes into one. Each renderer returns an
// error for a shell it has no form for, rather than falling back to one that
// happens to compile.

import (
	"fmt"
	"strings"
)

// cmdToken is one word of a rendered command. A path is quoted for the
// shell; everything else — a subcommand, a flag — is a literal this client
// wrote and is emitted as it is, so the command reads the way the
// documentation says it does.
type cmdToken struct {
	text string
	path bool
}

func literalToken(s string) cmdToken { return cmdToken{text: s} }
func pathToken(s string) cmdToken    { return cmdToken{text: s, path: true} }

// posixQuoteArg wraps a token in single quotes, where every byte is literal;
// an embedded quote ends the run, escapes itself and opens a new one. Single
// quotes, not %q's double quotes: inside double quotes a shell still expands
// $ and `, and a participant whose home directory contains either would have
// had a command that ran something else.
func posixQuoteArg(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// powerShellQuoteArg wraps a token in single quotes, PowerShell's literal
// string, where the only escape is a doubled quote. A single-quoted string
// expands nothing: no $variable, no subexpression, no backtick escape.
func powerShellQuoteArg(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// cmdQuoteArg wraps a token in double quotes, which cmd strips whole. ok is
// false for a token containing a double quote, which no Windows path may
// hold and which cmd cannot represent. % is not escaped: inside double
// quotes cmd expands %NAME% only when NAME is a variable it has, and a
// literal % is left alone — the execution tests carry a path with one.
func cmdQuoteArg(s string) (string, bool) {
	if strings.Contains(s, `"`) {
		return "", false
	}
	return `"` + s + `"`, true
}

// renderShellCommand writes one command for sh: every path quoted its way,
// and — in PowerShell — the call operator in front, because a command
// beginning with a quoted string is otherwise an expression that prints the
// string instead of running it (#69).
func renderShellCommand(sh shellKind, tokens []cmdToken) (string, error) {
	if len(tokens) == 0 {
		return "", fmt.Errorf("no command to render")
	}
	var out []string
	for _, tok := range tokens {
		if !tok.path {
			out = append(out, tok.text)
			continue
		}
		switch sh {
		case shellPOSIX:
			out = append(out, posixQuoteArg(tok.text))
		case shellPowerShell:
			out = append(out, powerShellQuoteArg(tok.text))
		case shellCmd:
			quoted, ok := cmdQuoteArg(tok.text)
			if !ok {
				return "", fmt.Errorf("cmd cannot carry a path containing a double quote: %q", tok.text)
			}
			out = append(out, quoted)
		case shellArgv:
			// Hermes' splitter is the one runner whose quoting depends on the
			// OS it runs on (shlex on POSIX, shlex(posix=False) on Windows),
			// and hermesHookCommand already takes that as a parameter. Deciding
			// it here would mean reading runtime.GOOS in a renderer that is
			// also asked to render for other operating systems — which is how
			// a form rendered for macOS came out with backslashes on a Windows
			// runner. It stays where the caller knows the answer.
			return "", fmt.Errorf("an argument splitter's quoting is hermesHookCommand's, not this renderer's")
		default:
			return "", fmt.Errorf("no command form for the %q shell", sh)
		}
	}
	if sh == shellPowerShell {
		return "& " + strings.Join(out, " "), nil
	}
	return strings.Join(out, " "), nil
}

// The commands every host's skill teaches, each rendered for one shell.

func (e binEntry) configTokens() []cmdToken {
	if e.cfg == "" {
		return nil
	}
	return []cmdToken{literalToken("-config"), pathToken(e.cfg)}
}

// stdinCommandForShell is the machine path: the request arrives as JSON on
// stdin, so the query is never in argv (H-R2).
func (e binEntry) stdinCommandForShell(sh shellKind) (string, error) {
	tokens := append([]cmdToken{pathToken(e.command), literalToken("search")}, e.configTokens()...)
	return renderShellCommand(sh, append(tokens, literalToken("--stdin")))
}

// searchCommandForShell is the human form's prefix, without a query.
func (e binEntry) searchCommandForShell(sh shellKind) (string, error) {
	tokens := append([]cmdToken{pathToken(e.command), literalToken("search")}, e.configTokens()...)
	return renderShellCommand(sh, append(tokens, literalToken("-format"), literalToken("model")))
}

// ── the request body, carried in the shell's own grammar ────────────────
//
// The body has to reach the binary byte for byte: the query is in it, and a
// shell that changes a byte changes the search. Measured on the CI runners
// (the dump is in the commit that added it):
//
//   - POSIX: a quoted heredoc, <<'JSON', expands nothing. Every payload —
//     apostrophes, quotes, backslashes, Latin-1, CJK, an astral emoji —
//     arrives exactly.
//   - PowerShell: a single-quoted here-string piped into the call. On pwsh
//     that alone arrives exactly. On Windows PowerShell 5.1 it does not:
//     without the first line, every UTF-16 unit outside ASCII is replaced by
//     "?" — café becomes caf?, an emoji becomes ??. Setting $OutputEncoding
//     to UTF-8 without a byte-order mark fixes that, and was measured to fix
//     it for every payload on both editions.
//
// What the first line does NOT fix is the byte-order mark 5.1 puts in front
// of the first thing it writes: it is still there with the line, with
// [Console]::OutputEncoding set as well, and twice over with an encoding
// that has its own preamble. The mark appears to be written when the stream
// is created, before any assignment in the script can run, which no rendered
// form can reach — so the binary tolerates one leading mark instead
// (trimUTF8BOM). That tolerance absorbs a mark; it cannot recover a query
// the shell already replaced with question marks, which is why the encoding
// line is not optional.
const psOutputEncodingLine = `$OutputEncoding = [System.Text.UTF8Encoding]::new($false)`

// searchBlockForShell renders the whole command block a skill teaches: the
// fence language the host should read it as, and the command that carries
// body to `search --stdin`.
func searchBlockForShell(sh shellKind, e binEntry, body string) (lang, script string, err error) {
	cmd, err := e.stdinCommandForShell(sh)
	if err != nil {
		return "", "", err
	}
	switch sh {
	case shellPOSIX:
		return "bash", cmd + " <<'JSON'\n" + body + "\nJSON", nil
	case shellPowerShell:
		return "powershell", psOutputEncodingLine + "\n@'\n" + body + "\n'@ | " + cmd, nil
	default:
		return "", "", fmt.Errorf("no search block form for the %q shell", sh)
	}
}

// hookCommandForShell is the command a host writes into its own config and
// runs around a search. It is rendered for the host's declared hook RUNNER,
// which is not always a shell: Hermes splits the string itself and execs it,
// so its command is quoted for that splitter rather than for any grammar.
//
// This is what retires %q from the hook files. A hook command built with Go's
// quoting arrives with doubled backslashes on Windows, which is the fifth
// symptom in #67's family, and made the config path the hook process received
// differ from the one the skill rendered.
func (e binEntry) hookCommandForShell(sh shellKind, sub ...string) (string, error) {
	tokens := append([]cmdToken{pathToken(e.command), literalToken("hook")}, e.configTokens()...)
	for _, s := range sub {
		tokens = append(tokens, literalToken(s))
	}
	return renderShellCommand(sh, tokens)
}

// hookCommandForRunners renders one hook command that every runner in shells
// must be able to execute, and says so when that is impossible.
//
// A hook cell may name more than one runner — Cursor on Windows is ruled
// {cmd, powershell}, because no hook has been observed there and the
// hooks.json string fails to parse as PowerShell while running unchanged
// under cmd. One string has to satisfy all of them, and the measurement (see
// the commit that added this) says none does across the paths a participant
// may have: a quoted first token is an expression in PowerShell, a leading &
// is a syntax error in cmd, `cmd /c call "…"` covers both until the path
// contains a $ (PowerShell expands it inside the double quotes) or a %, and
// a bare path breaks on a space.
//
// So a multi-runner cell keeps the form that the one observed runner accepts
// — v0.2.9's — and answers a reason the install plan prints. Rendering a
// form proven nowhere would be worse than the status quo; removing the hooks
// would take away the lineage Cursor does have on that OS.
func (e binEntry) hookCommandForRunners(shells []shellKind, sub ...string) (cmd, note string, err error) {
	switch len(shells) {
	case 0:
		return "", "", fmt.Errorf("no hook runner is declared")
	case 1:
		cmd, err = e.hookCommandForShell(shells[0], sub...)
		return cmd, "", err
	}
	cmd, err = e.hookCommandForShell(shellCmd, sub...)
	if err != nil {
		return "", "", err
	}
	kinds := make([]string, len(shells))
	for i, sh := range shells {
		kinds[i] = string(sh)
	}
	return cmd, "its hook runner is not established (" + strings.Join(kinds, " or ") +
		"); the command is the one proven under cmd, and no single form runs under all of them", nil
}

// preferCommandForShell is what the skill runs for /dropin-miner on|off|status.
func (e binEntry) preferCommandForShell(sh shellKind) (string, error) {
	tokens := append([]cmdToken{pathToken(e.command), literalToken("agents"), literalToken("prefer")}, e.configTokens()...)
	return renderShellCommand(sh, tokens)
}
