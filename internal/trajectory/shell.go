package trajectory

import (
	"encoding/json"
	"strings"
)

// A search is recognized by parsing the tool call's command line, never by
// finding the search command's text in it. The lexer below is deliberately
// small: it splits a command line into simple commands at unquoted control
// operators, splits each into words honoring quotes, and skips the body of a
// here-document or a PowerShell here-string so that the JSON request a search
// is fed is never read as a command. It does not expand, substitute or
// evaluate anything.

// SearchInvocation is what the argv of a recognized search says about it.
type SearchInvocation struct {
	// Stdin is true for `search --stdin`, the machine path, whose output is
	// this client's JSON envelope.
	Stdin bool
	// Format is the value of -format when one was given, lowercased.
	Format string
	// OutputElsewhere is true when the search's standard output was piped
	// into another command or redirected to a file: whatever came back to
	// the transcript is then that command's output, or nothing, and the
	// request id went wherever the output went.
	OutputElsewhere bool
}

// ExpectsJSON reports whether the invocation asked for output that carries a
// request id at all. The human rendering does not print one, so a search run
// that way loses its anchor by design rather than by accident. No -format at
// all is JSON: that is the flag's default in search.go, so only a format that
// was named, and is not json, is the human rendering.
func (s SearchInvocation) ExpectsJSON() bool { return s.Stdin || s.Format == "" || s.Format == "json" }

const clientBinary = "dropin-miner"

// commandWrappers may stand in front of the binary without changing what runs.
var commandWrappers = map[string]bool{"env": true, "command": true, "exec": true, "time": true, "nohup": true}

// parseSearchInvocations returns one entry per simple command in line that
// runs this client's search.
func parseSearchInvocations(line string) []SearchInvocation {
	var found []SearchInvocation
	for _, p := range parseSearches(line) {
		found = append(found, p.inv)
	}
	return found
}

// parsedSearch is an invocation and, when the command line shows it, the
// query it sent. The query is what the model wrote, so it is held only to
// compare one search of a turn with the next and is emitted behind a gate or
// not at all; hasQuery is false when the request came from somewhere the
// command line does not show, such as a file.
type parsedSearch struct {
	inv      SearchInvocation
	query    string
	hasQuery bool
}

func parseSearches(line string) []parsedSearch {
	var found []parsedSearch
	cmds := splitSimpleCommands(line)
	for k, sc := range cmds {
		inv, args, ok := searchInvocationOf(sc.words)
		if !ok {
			continue
		}
		inv.OutputElsewhere = inv.OutputElsewhere || sc.piped
		p := parsedSearch{inv: inv}
		if inv.Stdin {
			if body, ok := stdinOf(cmds, k); ok {
				p.query, p.hasQuery = queryOfRequest(body)
			}
		} else {
			p.query, p.hasQuery = queryOfArgs(args)
		}
		found = append(found, p)
	}
	return found
}

// simpleCommand is one command of a line: its words, whether its output was
// piped into the next command, and the literal text the line itself feeds it
// — a here-document's body, or a PowerShell here-string it is made of.
type simpleCommand struct {
	words      []string
	piped      bool
	hereDoc    string
	hasHereDoc bool
	hereString string
	hasHereStr bool
}

// stdinOf finds the text the command line feeds to command k: its own
// here-document, or the here-string or echo piped into it.
func stdinOf(cmds []simpleCommand, k int) (string, bool) {
	if cmds[k].hasHereDoc {
		return cmds[k].hereDoc, true
	}
	if k == 0 || !cmds[k-1].piped {
		return "", false
	}
	prev := cmds[k-1]
	switch {
	case prev.hasHereStr:
		return prev.hereString, true
	case len(prev.words) >= 2 && (prev.words[0] == "echo" || prev.words[0] == "printf"):
		return prev.words[len(prev.words)-1], true
	}
	return "", false
}

// queryOfRequest reads the query out of a version-1 stdin request.
func queryOfRequest(body string) (string, bool) {
	var req struct {
		Query *string `json:"query"`
	}
	if json.Unmarshal([]byte(body), &req) != nil || req.Query == nil {
		return "", false
	}
	return *req.Query, true
}

// searchValueFlags are the search flags that take a value (search.go).
var searchValueFlags = map[string]bool{"config": true, "tier": true, "format": true, "timeout": true}

// queryOfArgs joins the positional words after the flags, the way the flag
// package the client uses would see them.
func queryOfArgs(args []string) (string, bool) {
	i := 0
	for i < len(args) && strings.HasPrefix(args[i], "-") && args[i] != "-" && args[i] != "--" {
		name := strings.TrimLeft(args[i], "-")
		if !strings.Contains(name, "=") && searchValueFlags[name] {
			i++
		}
		i++
	}
	if i < len(args) && args[i] == "--" {
		i++
	}
	var words []string
	for ; i < len(args); i++ {
		if a := args[i]; strings.HasPrefix(a, ">") || strings.HasPrefix(a, "<") || strings.HasPrefix(a, "1>") || strings.HasPrefix(a, "2>") {
			break
		}
		words = append(words, args[i])
	}
	if len(words) == 0 {
		return "", false
	}
	return strings.Join(words, " "), true
}

// searchInvocationOf also returns the search's own arguments, after the
// subcommand, for the query to be read from.
func searchInvocationOf(words []string) (SearchInvocation, []string, bool) {
	i := 0
	for i < len(words) && (isAssignment(words[i]) || commandWrappers[words[i]]) {
		i++
	}
	if i >= len(words) || !isClientBinary(words[i]) {
		return SearchInvocation{}, nil, false
	}
	args := words[i+1:]
	// The only global flag that may precede the subcommand takes a value.
	if len(args) >= 2 && (args[0] == "-config" || args[0] == "--config") {
		args = args[2:]
	} else if len(args) >= 1 && (strings.HasPrefix(args[0], "-config=") || strings.HasPrefix(args[0], "--config=")) {
		args = args[1:]
	}
	if len(args) == 0 || args[0] != "search" {
		return SearchInvocation{}, nil, false
	}
	var inv SearchInvocation
	args = args[1:]
	for j := 0; j < len(args); j++ {
		switch a := args[j]; {
		case a == "--stdin" || a == "-stdin":
			inv.Stdin = true
		case a == "-format" || a == "--format":
			if j+1 < len(args) {
				inv.Format = strings.ToLower(args[j+1])
				j++
			}
		case strings.HasPrefix(a, "-format=") || strings.HasPrefix(a, "--format="):
			inv.Format = strings.ToLower(a[strings.IndexByte(a, '=')+1:])
		case strings.HasPrefix(a, ">") || strings.HasPrefix(a, "1>"):
			// Standard output to a file. 2> is standard error and is not this.
			inv.OutputElsewhere = true
		}
	}
	return inv, args, true
}

// isClientBinary: the word's final path element, under either separator,
// with an optional .exe, is exactly the client's name.
func isClientBinary(word string) bool {
	if k := strings.LastIndexAny(word, `/\`); k >= 0 {
		word = word[k+1:]
	}
	if len(word) > 4 && strings.EqualFold(word[len(word)-4:], ".exe") {
		word = word[:len(word)-4]
	}
	return word == clientBinary
}

func isAssignment(word string) bool {
	eq := strings.IndexByte(word, '=')
	if eq <= 0 {
		return false
	}
	for k, r := range word[:eq] {
		switch {
		case r == '_' || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z'):
		case k > 0 && r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return true
}

// splitSimpleCommands lexes line into simple commands, each a list of words
// with quotes removed.
func splitSimpleCommands(line string) []simpleCommand {
	var (
		cmds   []simpleCommand
		words  []string
		cur    strings.Builder
		inWord bool
		// pending are here-documents awaiting their bodies: the delimiter,
		// and the index of the command each one feeds.
		pending []hereDocument
		// hereStr is a PowerShell here-string seen in the command under way.
		hereStr    string
		hasHereStr bool
	)
	endWord := func() {
		if inWord {
			words = append(words, cur.String())
			cur.Reset()
			inWord = false
		}
	}
	endCmd := func(piped bool) {
		endWord()
		if len(words) > 0 {
			cmds = append(cmds, simpleCommand{words: words, piped: piped, hereString: hereStr, hasHereStr: hasHereStr})
			words = nil
		}
		hereStr, hasHereStr = "", false
	}
	n := len(line)
	for i := 0; i < n; {
		c := line[i]
		switch {
		case c == '\\' && i+1 < n && !inPathWord(cur.String(), inWord):
			// A backslash escapes the next byte — except inside a word that
			// is already a Windows path, where it is a separator.
			inWord = true
			if line[i+1] != '\n' {
				cur.WriteByte(line[i+1])
			}
			i += 2
		case c == '\'':
			inWord = true
			j := strings.IndexByte(line[i+1:], '\'')
			if j < 0 {
				cur.WriteString(line[i+1:])
				i = n
				break
			}
			cur.WriteString(line[i+1 : i+1+j])
			i += j + 2
		case c == '"':
			inWord = true
			i++
			for i < n && line[i] != '"' {
				if line[i] == '\\' && i+1 < n && strings.IndexByte("\"\\$`", line[i+1]) >= 0 {
					i++
				}
				cur.WriteByte(line[i])
				i++
			}
			i++
		case c == '@' && !inWord && i+1 < n && (line[i+1] == '\'' || line[i+1] == '"'):
			// PowerShell here-string: @' … '@ on its own lines. The body is data.
			closer := "\n" + string(line[i+1]) + "@"
			if j := strings.Index(line[i+2:], closer); j >= 0 {
				hereStr, hasHereStr = strings.TrimPrefix(strings.TrimPrefix(line[i+2:i+2+j], "\r"), "\n"), true
				i += 2 + j + len(closer)
			} else {
				i = n
			}
			inWord = true // it stands as one (empty) word in its pipeline
		case c == '<' && i+1 < n && line[i+1] == '<' && (i+2 >= n || line[i+2] != '<'):
			// << opens a here-document; <<< is a here-string and has no body.
			endWord()
			i += 2
			if i < n && line[i] == '-' {
				i++
			}
			for i < n && (line[i] == ' ' || line[i] == '\t') {
				i++
			}
			start := i
			for i < n && strings.IndexByte(" \t\n;&|()<>", line[i]) < 0 {
				i++
			}
			if delim := strings.Trim(line[start:i], `'"\`); delim != "" {
				// The command under way is not in cmds yet; len(cmds) is the
				// index it takes when it ends, which it does before its body.
				pending = append(pending, hereDocument{delim: delim, feeds: len(cmds)})
			}
		case c == '\n':
			endCmd(false)
			i++
			for _, h := range pending {
				var body string
				i, body = skipHereDocument(line, i, h.delim)
				if h.feeds < len(cmds) {
					cmds[h.feeds].hereDoc, cmds[h.feeds].hasHereDoc = body, true
				}
			}
			pending = nil
		case c == '|':
			// One bar pipes this command's output onward; two is "or".
			or := i+1 < n && line[i+1] == '|'
			endCmd(!or)
			i++
			if or {
				i++
			}
		case c == ';' || c == '&' || c == '(' || c == ')':
			endCmd(false)
			i++
		case c == '$' && i+1 < n && line[i+1] == '(':
			endCmd(false)
			i += 2
		case c == ' ' || c == '\t' || c == '\r':
			endWord()
			i++
		default:
			inWord = true
			cur.WriteByte(c)
			i++
		}
	}
	endCmd(false)
	return cmds
}

// inPathWord reports whether the word under construction already looks like a
// drive-letter path (C:…), in which a backslash separates rather than escapes.
func inPathWord(word string, inWord bool) bool {
	return inWord && len(word) >= 2 && word[1] == ':' &&
		((word[0] >= 'A' && word[0] <= 'Z') || (word[0] >= 'a' && word[0] <= 'z'))
}

// hereDocument is a here-document whose body has not been reached yet.
type hereDocument struct {
	delim string
	feeds int
}

// skipHereDocument returns the index just past the line that equals delim —
// or the end of input when the body is unterminated — and the body it passed.
func skipHereDocument(line string, i int, delim string) (int, string) {
	start := i
	for i < len(line) {
		end := strings.IndexByte(line[i:], '\n')
		var row string
		if end < 0 {
			row, end = line[i:], len(line)-i
		} else {
			row = line[i : i+end]
			end++
		}
		if strings.TrimLeft(strings.TrimRight(row, "\r"), "\t") == delim {
			return i + end, line[start:i]
		}
		i += end
	}
	return i, line[start:i]
}
