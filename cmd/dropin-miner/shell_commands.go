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
			quoted, ok := hermesQuoteArg(tok.text, false)
			if !ok {
				return "", fmt.Errorf("this path cannot be represented for an argument splitter: %q", tok.text)
			}
			out = append(out, quoted)
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

// preferCommandForShell is what the skill runs for /dropin-miner on|off|status.
func (e binEntry) preferCommandForShell(sh shellKind) (string, error) {
	tokens := append([]cmdToken{pathToken(e.command), literalToken("agents"), literalToken("prefer")}, e.configTokens()...)
	return renderShellCommand(sh, tokens)
}
