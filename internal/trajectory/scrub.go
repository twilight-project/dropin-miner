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
// Which text each rule reads is itself a rule, and it is not the same for
// all of them.
//
// Everything a rule can only DETECT is detected in the text exactly as it
// arrived. A rewrite can only hide such a value from its rule, never reveal
// one: an environment secret whose value begins with the home directory is
// still that secret after the home directory has been replaced by a tilde,
// and a scrubber that looked only at the rewritten text would keep the event.
// T3b is that fix; at c19f854 such an event was rewritten and kept.
//
// The account name is the one exception, and it is detected AFTER rewriting.
// Inside a path the name is meant to be replaced and the event kept, which is
// what a home path is for; outside one it is a bare mention with no known
// shape around it, and the mention omits the event. Rewriting first takes the
// name out of every path it sits in, so what the bare-mention rule then sees
// is a mention and nothing else. Detecting it first would omit every event
// that named a file under the home directory.
//
// pkg/redact was not reused: it rewrites in place where this must omit, and
// importing it brings net/http into a package whose dependency graph is held
// clear of the network by a test. The credential shapes and their thresholds
// below are taken from it, false-positive measurements included.

// ScrubClass is why content was omitted or what was rewritten in it.
type ScrubClass string

const (
	ScrubCredential  ScrubClass = "credential"
	ScrubEnvSecret   ScrubClass = "env_secret"
	ScrubEnvDump     ScrubClass = "env_dump"
	ScrubEmail       ScrubClass = "email"
	ScrubHostname    ScrubClass = "hostname"
	ScrubTraceBridge ScrubClass = "trace_bridge"
	ScrubTooLarge    ScrubClass = "too_large"

	ScrubHomePath ScrubClass = "home_path" // rewritten, not omitted
	// ScrubAccountName is both: the account name inside a home path is
	// rewritten, and a bare mention of it anywhere else omits the event. One
	// class names what was found; ScrubResult says which of the two happened.
	ScrubAccountName ScrubClass = "account_name"
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
	// secretNamePattern matches a whole underscore-separated SEGMENT of a
	// variable's name, not a substring of it. A substring rule read every
	// TOKENDROP_* variable this client asks a participant to set as a secret,
	// because TOKEN is inside TOKENDROP — so the participant's own config and
	// wallet paths were treated as secret values, and 54 events naming them
	// were omitted whole. The product's name is not a defect in the
	// participant's environment; it was a defect in this pattern.
	secretNamePattern = regexp.MustCompile(`(?i)(^|_)(KEY|TOKEN|SECRET|PASSWORD|PASSWD|CREDENTIAL|AUTH|COOKIE)(_|$)`)
	// envLinePattern is one line of an environment dump. `env` and `printenv`
	// write exactly this, and a shell that echoes its environment writes it
	// with `export ` in front.
	envLinePattern = regexp.MustCompile(`(?m)^(?:export +)?[A-Za-z_][A-Za-z0-9_]*=`)
)

// minSecretLength keeps a short value — "true", "1", a port — from being
// treated as a secret and omitting every event that contains it.
const minSecretLength = 8

// traceBridgeVar is the environment variable the client carries its trace
// envelope in. The envelope is base64 and can hold model prose, so no pattern
// here can see into it; a string that so much as names the variable is
// omitted whole, wherever it appears — a command line, a shell history, a
// tool result quoting either.
const traceBridgeVar = "TOKENDROP_TRACE_BRIDGE="

// envDumpMin is how many NAME=value lines make a string an environment dump.
// One such line is a variable being set and is judged on its value; five are
// somebody's whole environment, where the next line is as likely to hold a
// secret as the one a pattern caught, and the value that gives it away may be
// one this scrubber has never been told about.
const envDumpMin = 5

// Scrubber carries what is known exactly about this machine.
type Scrubber struct {
	home string
	// hostNames is the machine's name and its first label: a tool result
	// prints either, and the label alone is still this machine.
	hostNames []string
	// accounts is the participant's account name: the home directory's base
	// name, and USER and LOGNAME where the environment sets them. On Windows
	// the base name is the account name, which is why USERNAME is not read.
	accounts  []string
	envValues []string
}

// NewScrubber takes the environment as KEY=value pairs, the machine's name
// and the home directory, so a test supplies them and nothing is read from
// the process behind the caller's back.
func NewScrubber(environ []string, hostname, home string) *Scrubber {
	s := &Scrubber{home: strings.TrimRight(home, `/\`)}
	add := func(list []string, v string) []string {
		if v == "" {
			return list
		}
		for _, have := range list {
			if have == v {
				return list
			}
		}
		return append(list, v)
	}
	s.hostNames = add(s.hostNames, hostname)
	if label, _, ok := strings.Cut(hostname, "."); ok {
		s.hostNames = add(s.hostNames, label)
	}
	s.accounts = add(s.accounts, baseName(s.home))
	for _, kv := range environ {
		name, value, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if len(value) >= minSecretLength && secretNamePattern.MatchString(name) {
			s.envValues = append(s.envValues, value)
		}
		if name == "USER" || name == "LOGNAME" {
			s.accounts = add(s.accounts, value)
		}
	}
	return s
}

// baseName is the last element of a path under either separator, since the
// home directory may be a Windows one on any host reading a transcript.
func baseName(p string) string {
	if k := strings.LastIndexAny(p, `/\`); k >= 0 {
		return p[k+1:]
	}
	return p
}

// ScrubResult is what happened to one piece of content.
type ScrubResult struct {
	// Omit is set when the content must not be written at all.
	Omit ScrubClass
	// Rewrites counts whole replacements made, by class.
	Rewrites map[ScrubClass]int
}

// Text scrubs one string. When Omit is set the returned text is empty.
//
// A string past the limit is omitted before anything looks at it: scanning
// its first piece and keeping that is how a credential at the end of a long
// tool result survives its own scrubbing.
func (s *Scrubber) Text(in string) (string, ScrubResult) {
	if len(in) > scrubLimit {
		return "", ScrubResult{Omit: ScrubTooLarge}
	}
	if class := s.detect(in); class != "" {
		return "", ScrubResult{Omit: class}
	}
	out, rewrites := s.rewrite(in)
	if s.namesAccount(out) {
		return "", ScrubResult{Omit: ScrubAccountName}
	}
	return out, ScrubResult{Rewrites: rewrites}
}

// detect reads the text as it arrived. Every rule here is one that can only
// detect, so nothing a rewrite would do to the text could turn one of these
// answers from yes to no without the secret still being there.
func (s *Scrubber) detect(in string) ScrubClass {
	if strings.Contains(in, traceBridgeVar) {
		return ScrubTraceBridge
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
	if len(envLinePattern.FindAllStringIndex(in, envDumpMin)) >= envDumpMin {
		return ScrubEnvDump
	}
	if emailPattern.MatchString(in) {
		return ScrubEmail
	}
	for _, name := range s.hostNames {
		if containsWhole(in, name) {
			return ScrubHostname
		}
	}
	return ""
}

// namesAccount reports a bare mention of the account name. It runs on the
// REWRITTEN text, so a path holding the name has already had it replaced and
// what reaches here is a mention with nothing around it.
func (s *Scrubber) namesAccount(in string) bool {
	for _, name := range s.accounts {
		if containsWhole(in, name) {
			return true
		}
	}
	return false
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
	home := func() { rewrites[ScrubHomePath]++ }
	// The shell's own spelling of the home directory, before the directory
	// itself: ~zebrauser and /Users/zebrauser name the same place, and a
	// transcript holds both.
	for _, name := range s.accounts {
		out = replaceWholePath(out, "~"+name, "~", home)
	}
	if s.home != "" {
		out = replaceWholePath(out, s.home, "~", home)
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
//
// The match ignores ASCII case, because the two filesystems this runs on are
// themselves case-insensitive: /users/zebrauser opens the same directory the
// host named /Users/zebrauser, and a tool result prints whichever spelling it
// was given.
func replaceWholePath(text, prefix, with string, count func()) string {
	if prefix == "" {
		return text
	}
	var b strings.Builder
	for {
		k := indexFoldASCII(text, prefix)
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
			b.WriteString(text[k:end])
		}
		text = text[end:]
	}
}

// indexFoldASCII is strings.Index ignoring case in ASCII only. Folding just
// ASCII keeps every index into one string an index into the other:
// strings.ToLower can change a rune's encoded length, and a participant whose
// home directory holds a non-ASCII character would have every offset after it
// shifted by one.
func indexFoldASCII(text, sub string) int {
	for i := 0; i+len(sub) <= len(text); i++ {
		if equalFoldASCII(text[i:i+len(sub)], sub) {
			return i
		}
	}
	return -1
}

func equalFoldASCII(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		if lowerASCII(a[i]) != lowerASCII(b[i]) {
			return false
		}
	}
	return true
}

func lowerASCII(b byte) byte {
	if b >= 'A' && b <= 'Z' {
		return b + ('a' - 'A')
	}
	return b
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
