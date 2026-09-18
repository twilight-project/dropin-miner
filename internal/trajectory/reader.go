package trajectory

import (
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"
)

// Session is one Claude Code session: its main transcript and the subagent
// runs recorded beside it.
type Session struct {
	// File is slash-separated and relative to the directory the caller
	// named. Empty when only the subagent files of a session were found.
	File string
	// SessionID is the transcript's file name without its extension, which
	// is the host's session id and the raw value the client's trace hashes.
	SessionID string
	Turns     []*Turn
	Subagents []*SubagentRun
	Refusals  []Refusal
	// Versions counts conversation entries by the host version that wrote them.
	Versions map[string]int
	// SessionIDMismatch counts entries whose own sessionId is not the file's.
	SessionIDMismatch int
	// SubstringHits is how many times the search command's text occurs in
	// the raw file. It is the overcount, kept to be compared against.
	SubstringHits int
}

// SubagentRun is one subagent's own record. Its searches stay its own: they
// are never folded into the parent's turn, and the run says which parent
// turn spawned it.
type SubagentRun struct {
	File    string
	AgentID string
	// ParentToolUseID is the Agent tool call that spawned the run, from the
	// host's own sidecar beside the transcript.
	ParentToolUseID string
	// ParentAgentID is set when another subagent, not the main chain, made
	// that call; ParentTurn then indexes that run's turns.
	ParentAgentID string
	// ParentTurn indexes the parent's Turns, or is -1 when the spawning call
	// could not be found — which is refused as subagent_unlinked, not guessed.
	ParentTurn int
	Turns      []*Turn

	agentCalls map[string]int
}

// subagentMeta is the host's sidecar, <transcript>.meta.json.
type subagentMeta struct {
	ToolUseID     string `json:"toolUseId"`
	ParentAgentID string `json:"parentAgentId"`
}

const (
	transcriptExt  = ".jsonl"
	subagentsDir   = "subagents"
	subagentPrefix = "agent-"
	metaExt        = ".meta.json"
)

// Walk reads every session under dir and hands each to visit, one at a time
// and in a stable order, so a caller that only counts never holds more than
// one session. Every open goes through an os.Root on dir: nothing outside it
// is read, by this code or through a link inside it.
func Walk(dir string, opts Options, visit func(*Session)) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return fmt.Errorf("open %s: %w", dir, err)
	}
	defer func() { _ = root.Close() }()

	type group struct {
		main string
		subs []string
	}
	groups := map[string]*group{}
	var stray []string
	groupOf := func(key string) *group {
		if groups[key] == nil {
			groups[key] = &group{}
		}
		return groups[key]
	}
	err = fs.WalkDir(root.FS(), ".", func(p string, d fs.DirEntry, werr error) error {
		if werr != nil || d.IsDir() || !strings.HasSuffix(p, transcriptExt) {
			return nil // an unreadable directory is skipped, not fatal
		}
		parent := path.Dir(p)
		switch {
		case path.Base(parent) != subagentsDir:
			groupOf(strings.TrimSuffix(p, transcriptExt)).main = p
		case strings.HasPrefix(path.Base(p), subagentPrefix):
			groupOf(path.Dir(parent)).subs = append(groupOf(path.Dir(parent)).subs, p)
		default:
			stray = append(stray, p)
		}
		return nil
	})
	if err != nil {
		return err
	}

	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		g := groups[key]
		sort.Strings(g.subs)
		visit(readSession(root, opts, path.Base(key), g.main, g.subs))
	}
	if len(stray) > 0 {
		s := &Session{Versions: map[string]int{}}
		for _, p := range stray {
			s.Refusals = append(s.Refusals, Refusal{File: p, Reason: RefuseUnknownLayout})
		}
		visit(s)
	}
	return nil
}

func readSession(root *os.Root, opts Options, sessionID, mainPath string, subs []string) *Session {
	s := &Session{File: mainPath, SessionID: sessionID, Versions: map[string]int{}}
	mainCalls := map[string]int{}
	if mainPath != "" {
		p := parseFile(root, opts, mainPath, sessionID, false, s.Versions)
		s.Turns, s.SessionIDMismatch, s.SubstringHits = p.turns, p.sessionMismatch, p.substringHits
		s.Refusals = append(s.Refusals, p.refusals...)
		mainCalls = p.agentCalls
	}
	for _, sp := range subs {
		p := parseFile(root, opts, sp, sessionID, true, s.Versions)
		run := &SubagentRun{
			File:       sp,
			AgentID:    strings.TrimSuffix(strings.TrimPrefix(path.Base(sp), subagentPrefix), transcriptExt),
			ParentTurn: -1, Turns: p.turns, agentCalls: p.agentCalls,
		}
		s.SubstringHits += p.substringHits
		s.SessionIDMismatch += p.sessionMismatch
		s.Refusals = append(s.Refusals, p.refusals...)
		if meta, ok := readMeta(root, strings.TrimSuffix(sp, transcriptExt)+metaExt); ok {
			run.ParentToolUseID, run.ParentAgentID = meta.ToolUseID, meta.ParentAgentID
		}
		s.Subagents = append(s.Subagents, run)
	}
	// Link each run to the turn that spawned it, once every run is parsed: a
	// nested subagent's parent is another run, which may sort after it.
	byAgent := map[string]*SubagentRun{}
	for _, run := range s.Subagents {
		byAgent[run.AgentID] = run
	}
	for _, run := range s.Subagents {
		calls := mainCalls
		if run.ParentAgentID != "" {
			calls = nil
			if parent := byAgent[run.ParentAgentID]; parent != nil {
				calls = parent.agentCalls
			}
		}
		if turn, ok := calls[run.ParentToolUseID]; ok && run.ParentToolUseID != "" {
			run.ParentTurn = turn
		} else {
			s.Refusals = append(s.Refusals, Refusal{File: run.File, Reason: RefuseSubagentUnlinked})
		}
	}
	return s
}

func parseFile(root *os.Root, opts Options, name, sessionID string, sidechain bool, versions map[string]int) *turnParser {
	p := &turnParser{opts: opts, file: name, sidechain: sidechain, versions: versions, wantSession: sessionID, agentCalls: map[string]int{}}
	f, err := root.Open(name)
	if err != nil {
		p.refuse(0, RefuseUnreadableFile, "", "")
		return p
	}
	defer func() { _ = f.Close() }()
	if err := p.parse(f); err != nil {
		p.refuse(0, RefuseUnreadableFile, "", "")
	}
	return p
}

func readMeta(root *os.Root, name string) (subagentMeta, bool) {
	var meta subagentMeta
	f, err := root.Open(name)
	if err != nil {
		return meta, false
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, 1<<20))
	if err != nil || json.Unmarshal(data, &meta) != nil {
		return meta, false
	}
	return meta, true
}
