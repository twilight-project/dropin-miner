package trajectory

import (
	"encoding/json"
	"regexp"
	"strings"
)

// The scrubber runs on every string before it can be written, and it is not
// optional: emit has no switch that turns it off.
//
// It does two different things, and the difference is the rule. What it can
// only DETECT — a credential-shaped string, a secret's value, an email
// address, this machine's name — causes the whole event's content to be
// omitted. It is not cut out with the rest kept: a pattern that found one
// secret in a block of text says nothing about the one beside it, and a cut
// is how half a secret survives. What it KNOWS exactly — the home directory,
// and the account name in a home-directory path — it replaces whole, because
// replacing every occurrence of a known string leaves nothing of it behind.
// A string too large to scan in one piece is omitted whole, never sliced.
//
// pkg/redact was not reused: it rewrites in place where this must omit, and
// importing it brings net/http into a package whose dependency graph is held
// clear of the network by a test. The credential shapes and their thresholds
// below are taken from it, false-positive measurements included.

// ScrubClass is why content was omitted or what was rewritten in it.
type ScrubClass string

const (
	ScrubCredential ScrubClass = "credential"
	ScrubEnvSecret  ScrubClass = "env_secret"
	ScrubEmail      ScrubClass = "email"
	ScrubHostname   ScrubClass = "hostname"
	ScrubTooLarge   ScrubClass = "too_large"

	ScrubHomePath    ScrubClass = "home_path"    // rewritten, not omitted
	ScrubAccountName ScrubClass = "account_name" // rewritten, not omitted
)

// scrubLimit is the most text scrubbed as one piece.
const scrubLimit = 256 << 10

var credentialPatterns = []*regexp.Regexp{
	regexp.MustCompile(`\b(?:sk|sr)-[A-Za-z0-9_-]{16,}`),
	regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/=-]{16,}`),
	regexp.MustCompile(`[a-zA-Z][a-zA-Z0-9+.-]*://[^/@\s:]+:[^/@\s]+@`),
	regexp.MustCompile(`\bgh[opsur]_[A-Za-z0-9]{20,}\b|\bgithub_pat_[A-Za-z0-9_]{20,}\b`),
	regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`),
	regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{4,}\.[A-Za-z0-9_-]{4,}\.[A-Za-z0-9_-]{4,}\b`),
	regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}`),
	regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`),
}

var (
	emailPattern = regexp.MustCompile(`\b[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}\b`)
	// Any account's home directory, not only this one's: a tool result can
	// show a colleague's path as easily as the participant's.
	unixHomePattern    = regexp.MustCompile(`(/Users/|/home/)[^/\s"']+`)
	windowsHomePattern = regexp.MustCompile(`(?i)([A-Z]:\\Users\\)[^\\\s"']+`)
	secretNamePattern  = regexp.MustCompile(`(?i)(KEY|TOKEN|SECRET|PASSWORD|PASSWD|CREDENTIAL|AUTH|COOKIE)`)
)

// minSecretLength keeps a short value — "true", "1", a port — from being
// treated as a secret and omitting every event that contains it.
const minSecretLength = 8

// Scrubber carries what is known exactly about this machine.
type Scrubber struct {
	home      string
	hostname  string
	envValues []string
}

// NewScrubber takes the environment as KEY=value pairs, the machine's name
// and the home directory, so a test supplies them and nothing is read from
// the process behind the caller's back.
func NewScrubber(environ []string, hostname, home string) *Scrubber {
	s := &Scrubber{home: strings.TrimRight(home, `/\`), hostname: hostname}
	for _, kv := range environ {
		name, value, ok := strings.Cut(kv, "=")
		if ok && len(value) >= minSecretLength && secretNamePattern.MatchString(name) {
			s.envValues = append(s.envValues, value)
		}
	}
	return s
}

// ScrubResult is what happened to one piece of content.
type ScrubResult struct {
	// Omit is set when the content must not be written at all.
	Omit ScrubClass
	// Rewrites counts whole replacements made, by class.
	Rewrites map[ScrubClass]int
}

// Text scrubs one string. When Omit is set the returned text is empty.
func (s *Scrubber) Text(in string) (string, ScrubResult) {
	if class := s.detect(in); class != "" {
		return "", ScrubResult{Omit: class}
	}
	out, rewrites := s.rewrite(in)
	return out, ScrubResult{Rewrites: rewrites}
}

func (s *Scrubber) detect(in string) ScrubClass {
	if len(in) > scrubLimit {
		return ScrubTooLarge
	}
	for _, re := range credentialPatterns {
		if re.MatchString(in) {
			return ScrubCredential
		}
	}
	for _, value := range s.envValues {
		if containsWhole(in, value) {
			return ScrubEnvSecret
		}
	}
	if emailPattern.MatchString(in) {
		return ScrubEmail
	}
	if s.hostname != "" && containsWhole(in, s.hostname) {
		return ScrubHostname
	}
	return ""
}

// containsWhole reports whether value occurs in text as a whole value: not
// flanked by a character that would make it part of a longer token.
func containsWhole(text, value string) bool {
	for from := 0; ; {
		k := strings.Index(text[from:], value)
		if k < 0 {
			return false
		}
		start, end := from+k, from+k+len(value)
		if (start == 0 || !tokenByte(text[start-1])) && (end == len(text) || !tokenByte(text[end])) {
			return true
		}
		from = start + 1
	}
}

func tokenByte(b byte) bool {
	return b == '_' || (b >= '0' && b <= '9') || (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z')
}

func (s *Scrubber) rewrite(in string) (string, map[ScrubClass]int) {
	rewrites := map[ScrubClass]int{}
	out := in
	if s.home != "" {
		out = replaceWholePath(out, s.home, "~", func() { rewrites[ScrubHomePath]++ })
	}
	for _, re := range []*regexp.Regexp{unixHomePattern, windowsHomePattern} {
		out = re.ReplaceAllStringFunc(out, func(m string) string {
			rewrites[ScrubAccountName]++
			return re.ReplaceAllString(m, "${1}<user>")
		})
	}
	return out, rewrites
}

// replaceWholePath replaces prefix where it is a whole path prefix: followed
// by a separator, or by nothing that could continue a name. /Users/ann must
// not match inside /Users/annabel.
func replaceWholePath(text, prefix, with string, count func()) string {
	var b strings.Builder
	for {
		k := strings.Index(text, prefix)
		if k < 0 {
			b.WriteString(text)
			return b.String()
		}
		end := k + len(prefix)
		b.WriteString(text[:k])
		if end == len(text) || (!tokenByte(text[end]) && text[end] != '-' && text[end] != '.') {
			b.WriteString(with)
			count()
		} else {
			b.WriteString(prefix)
		}
		text = text[end:]
	}
}

// JSON scrubs every string inside a JSON value, keys included, and
// re-encodes it. One string that must be omitted omits the whole value: a
// tool call's parameters are one item. Scrubbing the decoded strings, not
// the raw bytes, is what makes a Windows path visible — in the raw JSON its
// separators are doubled.
func (s *Scrubber) JSON(raw json.RawMessage) (json.RawMessage, ScrubResult) {
	if len(raw) > scrubLimit {
		return nil, ScrubResult{Omit: ScrubTooLarge}
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		// Not JSON after all: treat it as the text it is.
		text, res := s.Text(string(raw))
		if res.Omit != "" {
			return nil, res
		}
		out, _ := json.Marshal(text)
		return out, res
	}
	res := ScrubResult{Rewrites: map[ScrubClass]int{}}
	v = s.walk(v, &res)
	if res.Omit != "" {
		return nil, ScrubResult{Omit: res.Omit}
	}
	out, err := json.Marshal(v)
	if err != nil {
		return nil, ScrubResult{Omit: ScrubTooLarge}
	}
	return out, res
}

func (s *Scrubber) walk(v any, res *ScrubResult) any {
	if res.Omit != "" {
		return nil
	}
	switch t := v.(type) {
	case string:
		return s.walkString(t, res)
	case []any:
		for i := range t {
			t[i] = s.walk(t[i], res)
		}
		return t
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[s.walkString(k, res)] = s.walk(val, res)
		}
		return out
	}
	return v
}

func (s *Scrubber) walkString(in string, res *ScrubResult) string {
	out, r := s.Text(in)
	if r.Omit != "" {
		res.Omit = r.Omit
		return ""
	}
	for class, n := range r.Rewrites {
		res.Rewrites[class] += n
	}
	return out
}
