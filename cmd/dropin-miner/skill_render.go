package main

// The skill, rendered for the shells its host actually runs.
//
// One host can run tool calls in more than one shell on one OS — Claude Code
// on Windows runs its Bash tool through Git Bash and its PowerShell tool
// through PowerShell, and which one a call uses is the model's choice, not
// ours (H-R5). So the skill teaches one runnable form per declared shell,
// labeled, rather than one form that is wrong for half the calls.
//
// A host whose shell is not established keeps v0.2.9's POSIX form and the
// install says so in its plan: refusing would take away a host that works
// today (Codex on Windows), which is a regression 0.2.10 must not ship.

import (
	"fmt"
	"strings"
)

// exampleRequest is the request body the skill shows.
const exampleRequest = `{"version":1,"query":"exact query text"}`

// shellLabel names a shell the way a participant's host does.
func shellLabel(sh shellKind) string {
	switch sh {
	case shellPOSIX:
		return "Bash (on Windows, Git Bash)"
	case shellPowerShell:
		return "PowerShell"
	case shellCmd:
		return "cmd"
	}
	return string(sh)
}

// toolShellsForSkill is the shells a host's skill renders for on this OS,
// and the note the install plan prints when nothing established them.
//
// The fallback is v0.2.9's POSIX form, deliberately: a host nobody has
// watched keeps working exactly as it does today, and the participant is
// told which host and OS that applies to instead of finding out from a
// search that never runs.
func toolShellsForSkill(t installTarget, goos string) (shells []shellKind, note string) {
	declared, err := declaredShells(t, goos, channelTool)
	if err != nil {
		return []shellKind{shellPOSIX}, fmt.Sprintf(
			"%s: which shell runs its tool calls on %s is not established, so the skill keeps the Bash form; a search that does not run here is why",
			t.Label(), goos)
	}
	if len(declared) == 0 {
		return []shellKind{shellPOSIX}, ""
	}
	return declared, ""
}

// fenced wraps a rendered script in its fence.
func fenced(lang, script string) string {
	return "```" + lang + "\n" + script + "\n```"
}

// perShellSection renders one block per shell, labeled only when the host
// runs more than one: a single-shell host's skill reads exactly as it did.
func perShellSection(shells []shellKind, lead string, render func(shellKind) (string, error)) (string, error) {
	var b strings.Builder
	multiple := len(shells) > 1
	if multiple && lead != "" {
		b.WriteString("\n" + lead + "\n")
	}
	for _, sh := range shells {
		block, err := render(sh)
		if err != nil {
			return "", err
		}
		if multiple {
			fmt.Fprintf(&b, "\nIf the tool you are calling runs %s:\n\n%s\n", shellLabel(sh), block)
			continue
		}
		b.WriteString("\n" + block + "\n")
	}
	return b.String(), nil
}

// callSection is the skill's "How to call it": the search command, and the
// sentence about the quoting that carries the request body.
func callSection(entry binEntry, shells []shellKind) (string, error) {
	body, err := perShellSection(shells,
		"This host runs tool calls in more than one shell. Use the form that matches the tool you are calling with; both reach the same search.",
		func(sh shellKind) (string, error) {
			lang, script, err := searchBlockForShell(sh, entry, exampleRequest)
			if err != nil {
				return "", err
			}
			return fenced(lang, script) + "\n" + quotingNote(sh), nil
		})
	if err != nil {
		return "", err
	}
	return body, nil
}

// quotingNote explains, per shell, what keeps the request intact — the part
// an agent has to preserve when it substitutes its own query.
func quotingNote(sh shellKind) string {
	switch sh {
	case shellPOSIX:
		return `
The heredoc delimiter is quoted (` + "`<<'JSON'`" + `) so the shell expands
nothing inside it, and every path is single-quoted for the same reason. Keep
both when you substitute your own query.`
	case shellPowerShell:
		return `
The here-string is single-quoted (` + "`@'` … `'@`" + `) so PowerShell expands
nothing inside it; only a line that begins with ` + "`'@`" + ` ends it. The first line
sets this shell's output encoding to UTF-8 with no byte-order mark, which is
what makes a query carrying an apostrophe, a quotation mark or any non-ASCII
character arrive exactly as written on Windows PowerShell 5.1. Keep both lines
when you substitute your own query.`
	}
	return ""
}

// preferSection is the skill's on/off command, per shell.
func preferSection(entry binEntry, shells []shellKind) (string, error) {
	return perShellSection(shells, "", func(sh shellKind) (string, error) {
		cmd, err := entry.preferCommandForShell(sh)
		if err != nil {
			return "", err
		}
		lang := "bash"
		if sh == shellPowerShell {
			lang = "powershell"
		}
		return fenced(lang, cmd+" <argument>"), nil
	})
}

// humanSection is the human terminal form, per shell. It keeps the query in
// argv, which H-R2 leaves alone: it is the form a person types, and it is
// never what an agent or a machine caller uses.
func humanSection(entry binEntry, shells []shellKind) (string, error) {
	return perShellSection(shells, "", func(sh shellKind) (string, error) {
		cmd, err := entry.searchCommandForShell(sh)
		if err != nil {
			return "", err
		}
		lang := "bash"
		if sh == shellPowerShell {
			lang = "powershell"
		}
		return fenced(lang, cmd+` "<query>"`), nil
	})
}
