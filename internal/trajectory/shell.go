package trajectory

import "strings"

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
// that way loses its anchor by design rather than by accident.
func (s SearchInvocation) ExpectsJSON() bool { return s.Stdin || s.Format == "json" }

const clientBinary = "dropin-miner"

// commandWrappers may stand in front of the binary without changing what runs.
var commandWrappers = map[string]bool{"env": true, "command": true, "exec": true, "time": true, "nohup": true}

// parseSearchInvocations returns one entry per simple command in line that
// runs this client's search.
func parseSearchInvocations(line string) []SearchInvocation {
	var found []SearchInvocation
	for _, sc := range splitSimpleCommands(line) {
		if inv, ok := searchInvocationOf(sc.words); ok {
			inv.OutputElsewhere = inv.OutputElsewhere || sc.piped
			found = append(found, inv)
		}
	}
	return found
}

// simpleCommand is one command of a line: its words, and whether its output
// was piped into the next command.
type simpleCommand struct {
	words []string
	piped bool
}

func searchInvocationOf(words []string) (SearchInvocation, bool) {
	i := 0
	for i < len(words) && (isAssignment(words[i]) || commandWrappers[words[i]]) {
		i++
	}
	if i >= len(words) || !isClientBinary(words[i]) {
		return SearchInvocation{}, false
	}
	args := words[i+1:]
	// The only global flag that may precede the subcommand takes a value.
	if len(args) >= 2 && (args[0] == "-config" || args[0] == "--config") {
		args = args[2:]
	} else if len(args) >= 1 && (strings.HasPrefix(args[0], "-config=") || strings.HasPrefix(args[0], "--config=")) {
		args = args[1:]
	}
	if len(args) == 0 || args[0] != "search" {
		return SearchInvocation{}, false
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
	return inv, true
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
		cmds    []simpleCommand
		words   []string
		cur     strings.Builder
		inWord  bool
		pending []string // here-document delimiters awaiting their bodies
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
			cmds = append(cmds, simpleCommand{words: words, piped: piped})
			words = nil
		}
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
				pending = append(pending, delim)
			}
		case c == '\n':
			endCmd(false)
			i++
			for _, delim := range pending {
				i = skipHereDocument(line, i, delim)
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

// skipHereDocument returns the index just past the line that equals delim,
// or the end of input when the body is unterminated.
func skipHereDocument(line string, i int, delim string) int {
	for i < len(line) {
		end := strings.IndexByte(line[i:], '\n')
		var row string
		if end < 0 {
			row, end = line[i:], len(line)-i
		} else {
			row = line[i : i+end]
			end++
		}
		i += end
		if strings.TrimLeft(strings.TrimRight(row, "\r"), "\t") == delim {
			return i
		}
	}
	return i
}
