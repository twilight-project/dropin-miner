package main

// Human search output, rendered safely.
//
// Everything in a search result — provider names, titles, URLs, snippets,
// answers — is remote text this client did not write. Machine output keeps
// it as data, because JSON already makes a control byte inert. Terminal
// output cannot: an escape sequence written to a terminal is not text, it
// is an instruction, and the two things a provider's answer must never be
// able to do are steer the terminal and be mistaken for this program's own
// output.
//
// So this file is the boundary. Three rules, applied to every field
// without exception:
//
//   - every rune that could steer a terminal, and every byte that is not
//     valid UTF-8, is replaced by U+FFFD — one policy, visibly;
//   - every truncation lands on a rune boundary, so no output is ever
//     invalid UTF-8 because it was cut;
//   - the total budget is enforced after every append, not checked before
//     a candidate and then blown by the fields inside it.
//
// Sanitizing presentation is all this does. It is not prompt-injection
// defense: the text still says whatever it says, and whether an agent
// treats web content as instructions is the agent's policy, stated in the
// installed skill. What this guarantees is narrower and worth having —
// that displaying a result cannot itself do something.

import (
	"fmt"
	"net/url"
	"strings"
	"unicode/utf8"
)

const (
	// renderTruncationNotice is counted against the budget, not added on
	// top of it, so the final string never exceeds renderTotalCap.
	renderTruncationNotice = "\n…(output cap reached; use -format json, or search --stdin, for the rest)\n"
	renderProviderCap      = 64
	renderTitleCap         = 200
	renderURLCap           = 500
	renderIDCap            = 128
)

// unsafeForTerminal reports whether r must not reach a terminal verbatim.
//
// The C0 controls carry ESC (which starts CSI and OSC), CR (which moves
// the cursor to the start of the line and lets later text overwrite what
// was printed), backspace and BEL. C1 is the eight-bit form of the same
// escapes. The bidi and invisible formatting characters do not steer the
// cursor but reorder what a reader sees, which is the same problem wearing
// a different hat: text that displays as something other than its bytes.
//
// Ordinary whitespace is not listed. oneLine collapses it first, so by the
// time a rune reaches here a tab or newline is already a space.
func unsafeForTerminal(r rune) bool {
	switch {
	case r == utf8.RuneError: // an invalid byte already decoded to this
		return true
	case r < 0x20, r == 0x7f: // C0 and DEL
		return true
	case r >= 0x80 && r <= 0x9f: // C1
		return true
	case r >= 0x200e && r <= 0x200f: // LRM, RLM
		return true
	case r >= 0x202a && r <= 0x202e: // the embedding/override set
		return true
	case r >= 0x2066 && r <= 0x2069: // the isolate set
		return true
	case r == 0x061c: // Arabic letter mark
		return true
	case r == 0xfeff: // zero-width no-break space, used as an invisible joiner
		return true
	default:
		return false
	}
}

// sanitizeForTerminal replaces every unsafe rune, and every byte that is
// not valid UTF-8, with U+FFFD. One policy, applied everywhere: a reader
// who sees � knows something was removed, and no caller has to remember
// which fields were cleaned.
func sanitizeForTerminal(s string) string {
	clean := true
	for _, r := range s {
		if unsafeForTerminal(r) {
			clean = false
			break
		}
	}
	if clean && utf8.ValidString(s) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		// An invalid byte decodes to RuneError, which unsafeForTerminal
		// already refuses, so malformed input and escape sequences take
		// the same path.
		if unsafeForTerminal(r) {
			b.WriteRune('�')
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// truncateRunes cuts s to at most max bytes without splitting a rune.
func truncateRunes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// oneLine is the single entry point for remote text: collapse whitespace,
// sanitize, then truncate on a rune boundary. The order matters — cutting
// first could leave half an escape sequence, and sanitizing after cutting
// would let a truncation split a rune that was about to be replaced.
func oneLine(s string, max int) string {
	return truncateRunes(sanitizeForTerminal(strings.Join(strings.Fields(s), " ")), max)
}

// citationTarget decides what to print where a citation's URL goes.
//
// Only http and https are rendered as the URL. A javascript:, data: or
// file: URL, or one that does not parse, is replaced by an inert note
// naming the scheme — the reader still learns a link was there, and the
// title and snippet beside it are still shown, but nothing a host might
// auto-link or a person might click carries a scheme that does something
// other than fetch a web page.
func citationTarget(raw string) string {
	trimmed := strings.TrimSpace(raw)
	u, err := url.Parse(trimmed)
	if err != nil || u.Host == "" {
		return "(link omitted: malformed URL)"
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		return oneLine(trimmed, renderURLCap)
	default:
		return "(link omitted: unsupported scheme " + oneLine(u.Scheme, 32) + ")"
	}
}

// cappedBuilder enforces the output budget after every write rather than
// before every candidate. The difference is the whole bug it replaces: a
// check at the top of a loop bounds how many candidates are appended, not
// how long each one is, so one long answer or one long title could carry
// the output past the budget however carefully the loop was written.
type cappedBuilder struct {
	b    strings.Builder
	max  int
	full bool
}

func newCappedBuilder(total int) *cappedBuilder {
	max := total - len(renderTruncationNotice)
	if max < 0 {
		max = 0
	}
	return &cappedBuilder{max: max}
}

func (c *cappedBuilder) writeString(s string) {
	if c.full {
		return
	}
	if c.b.Len()+len(s) <= c.max {
		c.b.WriteString(s)
		return
	}
	if room := c.max - c.b.Len(); room > 0 {
		cut := room
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		c.b.WriteString(s[:cut])
	}
	c.b.WriteString(renderTruncationNotice)
	c.full = true
}

func (c *cappedBuilder) printf(format string, args ...any) {
	c.writeString(fmt.Sprintf(format, args...))
}

func (c *cappedBuilder) String() string { return c.b.String() }

// renderRouterFailure is what -format model prints when the router
// refused the search.
//
// The router's error body is remote text exactly as an answer is, and it
// arrives on the one path where remote text is most likely to be hostile.
// Model output goes to a terminal, so it gets the same treatment
// everything else does: bounded, sanitized, and never echoed raw. The
// compatibility path (-format json) still prints the router's own bytes,
// which is what a caller asking for the router's JSON is asking for.
//
// A valid flat envelope contributes its code and message, sanitized. A
// malformed or oversized one contributes nothing but its own description
// — there is no safe way to quote bytes that did not parse, and a caller
// who wants them has -format json.
func renderRouterFailure(out searchOutcome) string {
	b := newCappedBuilder(renderTotalCap)
	b.printf("search failed: HTTP %d\n", out.HTTPStatus)
	if out.HasRouterErr {
		if code := oneLine(out.RouterErr.Code, renderProviderCap); code != "" {
			b.printf("  code: %s\n", code)
		}
		if msg := oneLine(out.RouterErr.Error, renderAnswerCap); msg != "" {
			b.printf("  router: %s\n", msg)
		}
		return b.String()
	}
	if len(out.RawBody) > 0 {
		b.writeString("  the router's error body was not a JSON object this client understands;\n" +
			"  re-run with -format json to see it verbatim\n")
	}
	return b.String()
}

// renderForModel is the compact text an agent reads: the chosen candidate
// first, then the rest, each with its citations. Budgets keep one search
// inside what a host shows of a command's output; `search --stdin` is the
// path for an agent that wants the whole structured result.
func renderForModel(r routerResponse) string {
	b := newCappedBuilder(renderTotalCap)
	b.printf("search %s", oneLine(r.RequestID, renderIDCap))
	if r.Session != nil && r.Session.ID != "" {
		b.printf("  session %s", oneLine(r.Session.ID, renderIDCap))
	}
	b.printf("  %d candidates\n", len(r.Candidates))

	order := make([]int, 0, len(r.Candidates))
	if r.Chosen >= 0 && r.Chosen < len(r.Candidates) {
		order = append(order, r.Chosen)
	}
	for i := range r.Candidates {
		if i != r.Chosen {
			order = append(order, i)
		}
	}
	for _, i := range order {
		if b.full {
			break
		}
		c := r.Candidates[i]
		mark := ""
		if i == r.Chosen {
			mark = " (chosen)"
		}
		b.printf("\n[%s]%s", oneLine(c.Provider, renderProviderCap), mark)
		if c.Kind != "" {
			b.printf(" %s", oneLine(c.Kind, renderProviderCap))
		}
		if c.Status != "" && c.Status != "ok" {
			b.printf(" status=%s", oneLine(c.Status, renderProviderCap))
		}
		b.writeString("\n")
		if c.Error != "" {
			b.printf("  error: %s\n", oneLine(c.Error, renderSnippetCap))
			continue
		}
		if c.Answer != "" {
			b.printf("  answer: %s\n", oneLine(c.Answer, renderAnswerCap))
		}
		for n, cit := range c.Citations {
			if n >= renderCitationCap {
				b.printf("  …(%d more)\n", len(c.Citations)-renderCitationCap)
				break
			}
			b.printf("  %d. %s", n+1, citationTarget(cit.URL))
			if t := oneLine(cit.Title, renderTitleCap); t != "" {
				b.printf(" — %s", t)
			}
			b.writeString("\n")
			if s := oneLine(cit.Snippet, renderSnippetCap); s != "" {
				b.printf("     %s\n", s)
			}
		}
	}
	return b.String()
}
