package trajectory

import (
	"fmt"
	"io"
	"regexp"
	"strings"
)

// Measuring is what the findings are written from. It answers the questions
// the brief asks with numbers and nothing else: every field is a count, a
// byte total or a member of a closed vocabulary declared in this package. No
// field can hold a message, a query, a path, an id or any other string that
// came out of a transcript, which is what makes it safe to print whole.
//
// It writes nothing anywhere. Measure opens no file for writing, and it never
// opens a file that a transcript merely names — a path found in a tool call's
// parameters is classified as the text it is, never followed.
//
// The numbers are emit's own. The per-level sizes, the scrubber's tallies and
// the outcome labels come from measureTurn, the same function Emit calls for
// the record it goes on to write, so a finding about what a record costs is a
// statement about the records emit would produce and not about a second
// implementation that agrees with it today.

// ConsentCategory names material a turn holds that nobody consented to share.
// The categories overlap on purpose: one event can be several of these at
// once, and each is counted under every category it answers to, because the
// question each answers is different.
type ConsentCategory string

const (
	// ConsentProviderContent: the event is a search result. Every one of them
	// is the search providers' content, arriving under the router's own
	// contracts with them rather than under anything a participant agreed.
	// Confidence: certain — it is what the event is, not a guess about it.
	ConsentProviderContent ConsentCategory = "search_result_provider_content"

	// ConsentFileOutsideWorkspace: a tool result whose call named an absolute
	// path that does not lie under the turn's workspace. Confidence: high
	// that the path is outside — the parameter is the host's own structure —
	// and medium that the CONTENT is foreign, since a participant's own notes
	// live outside their workspace as readily as an employer's code does.
	ConsentFileOutsideWorkspace ConsentCategory = "tool_result_file_outside_workspace"

	// ConsentOtherAccountHome: the event names a home directory belonging to
	// an account that is not this machine's. Confidence: medium — a colleague
	// on a shared host looks exactly like an example path in documentation,
	// and this rule cannot tell them apart.
	ConsentOtherAccountHome ConsentCategory = "other_account_home_directory"

	// ConsentEmail: the event carries an email address. Confidence: high for
	// the shape, medium for the person — an address in a code sample, a
	// license header or a commit trailer is somebody's address either way.
	ConsentEmail ConsentCategory = "email_address"

	// ConsentAnotherWorkspace: the event names a path under a workspace this
	// corpus has seen a turn run in, other than this turn's own. Confidence:
	// high — both ends are the host's own structure, and the other workspace
	// is one this participant demonstrably works in.
	ConsentAnotherWorkspace ConsentCategory = "path_under_another_workspace"

	// ConsentWorkspaceSibling: the event names a path sharing the workspace's
	// parent directory but lying outside the workspace and outside every
	// workspace this corpus saw. Confidence: medium — a sibling directory is
	// where a second checkout sits, and it is also where anything else does.
	ConsentWorkspaceSibling ConsentCategory = "path_beside_the_workspace"
)

// AllConsentCategories is the order the report prints them in.
var AllConsentCategories = []ConsentCategory{
	ConsentProviderContent, ConsentFileOutsideWorkspace, ConsentOtherAccountHome,
	ConsentEmail, ConsentAnotherWorkspace, ConsentWorkspaceSibling,
}

// ConsentCounts counts events, the turns they fall in, and their sizes. Size
// is the event's own byte count, recorded by the reader at every setting, so
// a category can be weighed without its content being held.
type ConsentCounts struct {
	Events map[ConsentCategory]int
	Turns  map[ConsentCategory]int
	Bytes  map[ConsentCategory]int64
	// TurnsAny is turns answering to at least one category.
	TurnsAny int
}

func newConsentCounts() *ConsentCounts {
	return &ConsentCounts{
		Events: map[ConsentCategory]int{}, Turns: map[ConsentCategory]int{}, Bytes: map[ConsentCategory]int64{},
	}
}

// Measurement is one run of measure.
type Measurement struct {
	Counts  *Counts
	Emit    *EmitStats
	Consent *ConsentCounts
	// Records is the turns that would produce a record — emit's denominator.
	// Nothing was written for any of them.
	Records int
	// Workspaces is how many distinct workspaces the corpus ran turns in. It
	// is the set the "another workspace" rule is decided against.
	Workspaces int
}

// Measure reads every transcript under dir twice and writes nothing.
//
// The first pass keeps no content at all and collects only the workspaces the
// corpus ran turns in; the second pass does the measuring. Two passes, rather
// than one and a guess, because "a path under another project" is worth
// saying only when the other project is one this participant demonstrably
// works in, and that set is not known until the whole corpus has been seen.
func Measure(dir string, idx *LineageIndex, scrub *Scrubber) (*Measurement, error) {
	roots, err := workspaceRoots(dir)
	if err != nil {
		return nil, err
	}
	m := &Measurement{
		Counts: NewCounts(idx), Emit: newEmitStats(), Consent: newConsentCounts(), Workspaces: len(roots),
	}
	seen := map[string]bool{}
	var turnErr error
	one := func(t *Turn, ctx emitContext) {
		if turnErr != nil || len(t.Searches) == 0 {
			return
		}
		if id := t.startUUID(); id != "" {
			if seen[id] {
				return
			}
			seen[id] = true
		}
		if _, _, err := measureTurn(t, ctx, scrub, m.Emit); err != nil {
			turnErr = err
			return
		}
		m.Records++
		m.Consent.add(t, roots, scrub)
	}
	err = Walk(dir, Options{KeepContent: true}, func(s *Session) {
		m.Counts.Add(s, idx)
		for _, t := range s.Turns {
			one(t, emitContext{sessionID: s.SessionID})
		}
		for _, run := range s.Subagents {
			for _, t := range run.Turns {
				one(t, emitContext{sessionID: s.SessionID, agentID: run.AgentID})
			}
		}
	})
	if err != nil {
		return nil, err
	}
	return m, turnErr
}

// workspaceRoots is the set of workspaces the corpus ran turns in. It reads
// with content off: a workspace is structure, and nothing else is wanted here.
func workspaceRoots(dir string) (map[string]bool, error) {
	roots := map[string]bool{}
	add := func(turns []*Turn) {
		for _, t := range turns {
			if t.Cwd != "" {
				roots[t.Cwd] = true
			}
		}
	}
	err := Walk(dir, Options{}, func(s *Session) {
		add(s.Turns)
		for _, run := range s.Subagents {
			add(run.Turns)
		}
	})
	return roots, err
}

// add folds one turn into the counts. It reads the turn's content as it
// stands, before the scrubber, because the question is what the material IS,
// not what would survive being written down.
func (c *ConsentCounts) add(t *Turn, roots map[string]bool, scrub *Scrubber) {
	// A tool result carries what its call asked for, so the path that decides
	// the result is on the call. The two are joined by the host's own id.
	outside := map[string]bool{}
	for i := range t.Events {
		ev := &t.Events[i]
		if ev.Kind == KindToolCall && ev.ToolUseID != "" && namesOutsidePath(string(ev.Input), t.Cwd) {
			outside[ev.ToolUseID] = true
		}
	}
	touched := map[ConsentCategory]bool{}
	for i := range t.Events {
		for _, cat := range categoriesOf(&t.Events[i], t, roots, outside, scrub) {
			c.Events[cat]++
			c.Bytes[cat] += int64(t.Events[i].Bytes)
			touched[cat] = true
		}
	}
	for cat := range touched {
		c.Turns[cat]++
	}
	if len(touched) > 0 {
		c.TurnsAny++
	}
}

func categoriesOf(ev *Event, t *Turn, roots, outside map[string]bool, scrub *Scrubber) []ConsentCategory {
	var cats []ConsentCategory
	if ev.Kind == KindSearchResult {
		cats = append(cats, ConsentProviderContent)
	}
	if ev.Kind == KindToolResult && ev.ToolUseID != "" && outside[ev.ToolUseID] {
		cats = append(cats, ConsentFileOutsideWorkspace)
	}
	text := ev.Text
	if ev.Kind == KindToolCall || ev.Kind == KindSearchCall {
		text = string(ev.Input)
	}
	if text == "" {
		return cats
	}
	if namesOtherAccountHome(text, scrub) {
		cats = append(cats, ConsentOtherAccountHome)
	}
	if emailPattern.MatchString(text) {
		cats = append(cats, ConsentEmail)
	}
	switch {
	case namesAnotherRoot(text, t.Cwd, roots):
		cats = append(cats, ConsentAnotherWorkspace)
	case namesWorkspaceSibling(text, t.Cwd, roots):
		cats = append(cats, ConsentWorkspaceSibling)
	}
	return cats
}

// namesOtherAccountHome reports whether text holds a home directory whose
// account is not one of this machine's.
func namesOtherAccountHome(text string, scrub *Scrubber) bool {
	for _, re := range []*regexp.Regexp{unixHomePattern, windowsHomePattern} {
		for _, m := range re.FindAllString(text, -1) {
			if account := baseName(m); account != "" && !scrub.isOwnAccount(account) {
				return true
			}
		}
	}
	return false
}

// isOwnAccount reports whether name is an account name of this machine's.
func (s *Scrubber) isOwnAccount(name string) bool {
	for _, own := range s.accounts {
		if equalFoldASCII(own, name) {
			return true
		}
	}
	return false
}

// namesAnotherRoot reports whether text holds a path under a workspace this
// corpus has seen, other than this turn's own.
func namesAnotherRoot(text, cwd string, roots map[string]bool) bool {
	for root := range roots {
		if root != cwd && containsPathPrefix(text, root) {
			return true
		}
	}
	return false
}

// namesWorkspaceSibling reports whether text holds a path that shares the
// workspace's parent directory but is neither under the workspace nor under
// any workspace the corpus saw.
func namesWorkspaceSibling(text, cwd string, roots map[string]bool) bool {
	parent, sep := parentDir(cwd)
	if parent == "" || parent == sep {
		return false
	}
	prefix := parent + sep
	for from := 0; ; {
		k := indexFoldASCII(text[from:], prefix)
		if k < 0 {
			return false
		}
		start := from + k
		from = start + len(prefix)
		if start > 0 && !pathBoundaryByte(text[start-1]) {
			continue // the parent is itself inside a longer name
		}
		candidate := prefix + firstSegment(text[from:], sep)
		if candidate == prefix || candidate == cwd || roots[candidate] {
			continue
		}
		return true
	}
}

// firstSegment is what follows up to the next separator or to anything that
// cannot continue a path segment.
func firstSegment(text, sep string) string {
	for i := 0; i < len(text); i++ {
		if text[i] == sep[0] || pathBoundaryByte(text[i]) {
			return text[:i]
		}
	}
	return text
}

// pathBoundaryByte reports whether a byte ends a path where it stands: a
// quote, a space, a bracket, a comma — anything a path is not made of.
func pathBoundaryByte(b byte) bool {
	switch b {
	case ' ', '\t', '\n', '\r', '"', '\'', '`', ',', ';', ')', '(', ']', '[', '}', '{', '>', '<', '|', '*', '?', ':', '=':
		return true
	}
	return false
}

// parentDir is the path's parent and the separator it is written with.
func parentDir(p string) (string, string) {
	sep := "/"
	if strings.LastIndex(p, `\`) > strings.LastIndex(p, "/") {
		sep = `\`
	}
	k := strings.LastIndex(p, sep)
	if k <= 0 {
		return "", sep
	}
	return p[:k], sep
}

// containsPathPrefix reports whether text holds prefix as a whole path
// prefix: at a boundary, and followed by a separator or by nothing that could
// continue the name. It is replaceWholePath's rule without the replacement.
func containsPathPrefix(text, prefix string) bool {
	if prefix == "" {
		return false
	}
	for from := 0; ; {
		k := indexFoldASCII(text[from:], prefix)
		if k < 0 {
			return false
		}
		start := from + k
		end := start + len(prefix)
		before := start == 0 || !tokenByte(text[start-1])
		after := end == len(text) || (!tokenByte(text[end]) && text[end] != '-' && text[end] != '.')
		if before && after {
			return true
		}
		from = start + 1
	}
}

// namesOutsidePath reports whether a tool call's parameters name an absolute
// path that does not lie under cwd. A relative path is not considered: it is
// resolved against the workspace by every tool that takes one.
func namesOutsidePath(input, cwd string) bool {
	if cwd == "" {
		return false
	}
	for _, p := range absolutePathsIn(input) {
		if !containsPathPrefix(p, cwd) {
			return true
		}
	}
	return false
}

// absolutePathsIn pulls the absolute paths out of a tool call's parameters.
// It reads the parameters as the text they are rather than decoding them,
// because the shape differs per tool and only the paths are wanted.
func absolutePathsIn(input string) []string {
	var out []string
	for i := 0; i < len(input); i++ {
		if !startsAbsolutePath(input, i) {
			continue
		}
		j := i
		for j < len(input) && !pathBoundaryByte(input[j]) {
			j++
		}
		if j-i > 1 {
			out = append(out, strings.TrimRight(input[i:j], `\/.`))
		}
		i = j
	}
	return out
}

// startsAbsolutePath reports whether an absolute path begins at i: a slash at
// a boundary, or a drive letter and a backslash.
func startsAbsolutePath(s string, i int) bool {
	if s[i] == '/' {
		return i == 0 || !tokenByte(s[i-1]) && s[i-1] != '/' && s[i-1] != '.' && s[i-1] != '~'
	}
	if i+2 < len(s) && s[i+1] == ':' && (s[i+2] == '\\' || s[i+2] == '/') {
		return (s[i] >= 'A' && s[i] <= 'Z') || (s[i] >= 'a' && s[i] <= 'z')
	}
	return false
}

// WriteTo prints the measurement. Counts, byte totals and this package's own
// vocabularies; there is no code path from a transcript's content to here.
func (m *Measurement) WriteTo(w io.Writer) (int64, error) {
	var n int64
	p := func(format string, args ...any) {
		k, _ := fmt.Fprintf(w, format, args...)
		n += int64(k)
	}
	p("measure writes nothing and prints counts only.\n\n")

	p("== the material, and the anchoring gap ==\n")
	k, err := m.Counts.WriteTo(w)
	n += k
	if err != nil {
		return n, err
	}

	p("\n== what a record costs ==\n")
	p("turns that would produce a record         %d (nothing was written for any of them)\n", m.Records)
	base := m.Emit.MeasuredBytes[1]
	for _, level := range []int{1, 2, 3} {
		per := int64(0)
		if m.Records > 0 {
			per = m.Emit.MeasuredBytes[level] / int64(m.Records)
		}
		ratio := ""
		if base > 0 {
			ratio = fmt.Sprintf(", %.1f x level 1", float64(m.Emit.MeasuredBytes[level])/float64(base))
		}
		p("  level %d                                %d bytes total, %d per record%s\n",
			level, m.Emit.MeasuredBytes[level], per, ratio)
	}

	p("\n== what the scrubber caught, over all %d items of content ==\n", m.Emit.ScrubbedItems)
	p("omitted whole\n")
	for _, class := range sortedKeys(m.Emit.ScrubOmitted) {
		p("  %-24s %6d\n", class, m.Emit.ScrubOmitted[class])
	}
	p("rewritten in place\n")
	for _, class := range sortedKeys(m.Emit.ScrubRewritten) {
		p("  %-24s %6d\n", class, m.Emit.ScrubRewritten[class])
	}

	p("\n== outcome labels derivable ==\n")
	for _, typ := range sortedKeys(m.Emit.Labels) {
		p("  %-30s %d\n", typ, m.Emit.Labels[typ])
	}
	p("  searches whose result offered no citation to label against: %d\n", m.Emit.SearchesNoURLs)
	p("  searches whose envelope shape carried no citations at all:   %d\n", m.Emit.SearchesCitationsUnavailable)

	p("\n== what a turn holds that nobody consented to share ==\n")
	p("workspaces this corpus ran turns in       %d\n", m.Workspaces)
	p("record-producing turns holding any of it  %d of %d\n", m.Consent.TurnsAny, m.Records)
	p("%-38s %8s %8s %12s\n", "category", "events", "turns", "bytes")
	for _, cat := range AllConsentCategories {
		p("  %-36s %8d %8d %12d\n", cat, m.Consent.Events[cat], m.Consent.Turns[cat], m.Consent.Bytes[cat])
	}
	return n, nil
}
