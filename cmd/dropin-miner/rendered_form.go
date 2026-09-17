package main

// What Cursor's shell hook auto-allows.
//
// Cursor asks this binary, before it runs a terminal command, whether to
// allow it. v0.2.9 answered by parsing the command into words with a small
// generic tokenizer and checking the first one — which refused any command
// carrying a control character, and therefore refused the multi-line heredoc
// our own skill teaches: every Cursor search waited for a human, and none
// carried turn or call lineage (#66).
//
// The replacement is not a looser parser. It is the strictest thing
// available: this binary knows what the skill renders, because it renders
// it, so it rebuilds that exact string and compares (H-R3). Nothing before
// it, nothing after it, no second statement, no pipeline of its own — a
// command that is one byte different is not this command and is not allowed.
// The request body is the one part that varies, and it must parse as exactly
// one JSON object with version 1 and nothing after it.
//
// Two things keep this honest. The rendered form is built by the same
// functions the skill uses, so the recognizer cannot drift from what the
// participant's agent was taught; and the binary in the command is still
// identified by os.SameFile, not by its spelling, so a path that reaches this
// same file through a link is recognized while a different program with a
// similar name is not.
//
// This decides whether to auto-allow OUR OWN search and to stamp lineage for
// it. It is a permission answer, so it stays exact: everything it cannot
// rebuild, it refuses, and Cursor asks the participant as it would for any
// other command.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// bodyPlaceholder marks where the request body sits in a rendered search
// command. It cannot occur in a rendered command by accident: the renderers
// never write it, and the body region is located by splitting on it before
// anything is compared.
const bodyPlaceholder = "\x00REQUEST BODY\x00"

// recognizedForm is what the hook learned from a command it allows.
type recognizedForm struct {
	// path is the approved command path: {"search"} or {"agents","prefer"}.
	path []string
	// body is the request body of a search, already checked.
	body string
}

// renderedFormMatch is one rendered form a command matched on grammar
// alone: which declared shell it was rendered for, which command path it is,
// and the two paths and the body it carried, each already read back out of
// the quoting the shell put on it.
type renderedFormMatch struct {
	shell     shellKind
	path      []string
	bin       string
	cfg       string
	body      string
	wantsBody bool
}

// matchedRenderedForms is the grammar half of the recognizer: every form this
// installation's skill renders, for every shell Cursor runs on this OS, that
// command is exactly — in declaration order. It decides only whether the
// command IS one of the strings we teach. Whether the binary and config it
// names are *this* installation's is recognizeRenderedForm's, because that
// needs the filesystem and a permission answer needs both halves.
//
// The two are split so that the hook and the per-OS golden that pins this
// grammar reach the SAME loop over the declared shells. The golden cannot run
// the identity half — it renders Windows and macOS paths that do not exist on
// the runner — so before T1b it rebuilt the loop for itself, and a recognizer
// that stopped after the first declared shell left the whole package green
// while every search in Cursor's second terminal would have prompted (#66
// again, for the second terminal).
func matchedRenderedForms(command, cfg string, shells []shellKind) []renderedFormMatch {
	if command == "" {
		return nil
	}
	command = strings.TrimSuffix(command, "\n")
	var out []renderedFormMatch
	for _, sh := range shells {
		for _, candidate := range renderedFormsForShell(cfg, sh) {
			got, ok := matchRendered(command, candidate)
			if !ok {
				continue
			}
			out = append(out, renderedFormMatch{
				shell:     sh,
				path:      candidate.path,
				bin:       got.bin,
				cfg:       unquoteRendered(got.cfg, candidate.text),
				body:      got.body,
				wantsBody: candidate.wantsBody,
			})
		}
	}
	return out
}

// recognizeRenderedForm decides whether command is exactly one of the
// commands this installation's skill renders, for one of the shells Cursor
// runs on this OS, AND names this installation's own binary and config. cfg
// is the config this hook was started with — the same one the skill's command
// names, because one install wrote both.
func recognizeRenderedForm(command string, executable func() (string, error), cfg string, shells []shellKind) *recognizedForm {
	if executable == nil {
		return nil
	}
	for _, m := range matchedRenderedForms(command, cfg, shells) {
		if !sameBinary(m.bin, executable) {
			continue
		}
		if cfg != "" && !samePath(m.cfg, cfg) {
			continue
		}
		if m.wantsBody {
			if !isOneVersionOneRequest(m.body) {
				continue
			}
			return &recognizedForm{path: m.path, body: m.body}
		}
		return &recognizedForm{path: m.path}
	}
	return nil
}

// renderedCommand is one form the skill teaches, with the binary path and
// (for a search) the body left as placeholders, so a command can be matched
// against it without knowing either in advance.
type renderedCommand struct {
	// prefix and suffix surround the binary path.
	text      string
	path      []string
	wantsBody bool
}

// binPlaceholder and cfgPlaceholder mark the two paths inside a rendered
// form. Both are compared as paths; every other byte is compared as itself.
const (
	binPlaceholder = "\x00BINARY\x00"
	cfgPlaceholder = "\x00CONFIG\x00"
)

// samePath reports whether two spellings name the same file, without asking
// the filesystem: the separators are normalized, and on Windows the
// comparison is case-insensitive as the platform itself is.
func samePath(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// renderedFormsForShell is every command Cursor may auto-allow, rendered
// for one shell, with the binary path and request body as placeholders.
//
// The config path stands as a placeholder, like the binary, and is compared
// as a path rather than as bytes: the hook learns its config from its own
// argv, and the host's hook command may spell it differently from the
// skill's — on Windows a `%q`-quoted hook command hands the process
// `C:\\Users\\…`, doubled separators and all, naming the same file in other
// bytes. Everything outside the placeholders is still compared exactly.
func renderedFormsForShell(cfg string, sh shellKind) []renderedCommand {
	entry := binEntry{command: binPlaceholder}
	if cfg != "" {
		entry.cfg = cfgPlaceholder
	}
	var out []renderedCommand
	if _, script, err := searchBlockForShell(sh, entry, bodyPlaceholder); err == nil {
		out = append(out, renderedCommand{text: script, path: []string{"search"}, wantsBody: true})
	}
	if prefer, err := entry.preferCommandForShell(sh); err == nil {
		for _, arg := range []string{"on", "off", "status"} {
			out = append(out, renderedCommand{text: prefer + " " + arg, path: []string{"agents", "prefer"}})
		}
	}
	return out
}

// matched is what a rendered form yielded: the binary path the command
// names, and the body it carried.
type matched struct {
	bin  string
	cfg  string
	body string
}

// formPart is one piece of a rendered form: a literal run this client wrote,
// or the region a placeholder stands for.
type formPart struct {
	literal string
	marker  string // "" for a literal
}

// splitForm breaks a rendered form into literal runs and the placeholders
// between them, in order.
func splitForm(text string) []formPart {
	var parts []formPart
	for text != "" {
		next, marker, width := -1, "", 0
		for _, candidate := range []string{binPlaceholder, cfgPlaceholder, bodyPlaceholder} {
			at := strings.Index(text, candidate)
			if at < 0 || (next >= 0 && at > next) {
				continue
			}
			next, marker, width = at, candidate, len(candidate)
		}
		if next < 0 {
			parts = append(parts, formPart{literal: text})
			break
		}
		parts = append(parts, formPart{literal: text[:next]}, formPart{marker: marker})
		text = text[next+width:]
	}
	return parts
}

// matchRendered compares command against one rendered form: every literal
// run byte for byte, in order, with nothing left over at the end. What stands
// where a placeholder was is returned.
//
// A variable region ends at the FIRST occurrence of the literal that follows
// it, which is also where the shell itself would end it: a heredoc ends at
// the first line equal to its delimiter, and a here-string at the first line
// beginning with '@. A body that contains its own terminator therefore leaves
// text over and is refused, rather than being silently cut short.
func matchRendered(command string, form renderedCommand) (matched, bool) {
	var got matched
	rest := command
	parts := splitForm(form.text)
	for i, part := range parts {
		if part.marker == "" {
			if !strings.HasPrefix(rest, part.literal) {
				return matched{}, false
			}
			rest = rest[len(part.literal):]
			continue
		}
		// A placeholder runs up to the next literal, or to the end when it is
		// the last part.
		value := rest
		if i+1 < len(parts) {
			next := parts[i+1].literal
			idx := strings.Index(rest, next)
			if idx < 0 {
				return matched{}, false
			}
			value, rest = rest[:idx], rest[idx:]
		} else {
			rest = ""
		}
		switch part.marker {
		case binPlaceholder:
			got.bin = value
		case cfgPlaceholder:
			got.cfg = value
		case bodyPlaceholder:
			got.body = value
		}
	}
	if rest != "" || got.bin == "" {
		return matched{}, false
	}
	got.bin = unquoteRendered(got.bin, form.text)
	return got, true
}

// unquoteRendered reverses the quoting a renderer applied to a path. The
// form's own text says which shell wrote it: a PowerShell command carries
// the call operator, a POSIX one does not.
func unquoteRendered(raw, form string) string {
	if strings.Contains(form, "& "+cfgPlaceholder) || strings.Contains(form, "& "+binPlaceholder) ||
		strings.Contains(form, "& '") {
		return strings.ReplaceAll(raw, "''", "'")
	}
	return strings.ReplaceAll(raw, `'\''`, "'")
}

// sameBinary is the identity check: the command's own path, resolved, is the
// file this process is running from. A different program that happens to be
// spelled the same way is refused; this file reached through a link is not.
func sameBinary(candidate string, executable func() (string, error)) bool {
	if candidate == "" || !filepath.IsAbs(candidate) {
		return false
	}
	own, err := executable()
	if err != nil || !filepath.IsAbs(own) {
		return false
	}
	own, err = filepath.EvalSymlinks(own)
	if err != nil {
		return false
	}
	resolved, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return false
	}
	ownInfo, err := os.Stat(own)
	if err != nil || !ownInfo.Mode().IsRegular() {
		return false
	}
	candidateInfo, err := os.Stat(resolved)
	if err != nil {
		return false
	}
	return os.SameFile(ownInfo, candidateInfo)
}

// isOneVersionOneRequest holds the body to the same contract `search --stdin`
// holds it to: exactly one JSON object, nothing after it, version 1. A second
// object, trailing text, or a body this client would refuse to run is not a
// command to auto-allow.
func isOneVersionOneRequest(body string) bool {
	raw := trimUTF8BOM([]byte(body))
	if singleJSONObject(raw) != nil {
		return false
	}
	var probe struct {
		Version *int `json:"version"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil || probe.Version == nil || *probe.Version != machineRequestVersion {
		return false
	}
	return true
}
