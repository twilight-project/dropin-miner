package trajectory

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
)

// Anchoring path 2: recompute the ids the client put in its trace envelope
// and join them to ids already stored. It reads no message text at all.
//
// The client derives every envelope id from the host's own id (the lineage
// hook in cmd/dropin-miner/hook.go): session_id from the host session id —
// or from session|agent for a subagent's lane — turn_id from
// session|prompt_id, and call_id from session|tool_use_id. Claude Code names
// each transcript after that session id, stamps the prompt id on the turn's
// entries and the tool-use id on the call, so a transcript alone reproduces
// all three. That is what makes the join possible, and it is also why these
// ids are not anonymous: whoever holds the transcript can recompute them.

// tracePrefix restates the client's domain separator. cmd/dropin-miner is
// package main and cannot be imported, so the derivation is repeated here and
// pinned to the original by a test that reads trace.go.
const tracePrefix = "tokendrop-trace-v1|"

// TraceHash is the client's traceHash: SHA-256 over the public prefix and the
// raw id, first sixteen bytes, hex.
func TraceHash(raw string) string {
	if raw == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(tracePrefix + raw))
	return hex.EncodeToString(sum[:16])
}

// TraceIDs are the envelope ids one search would have carried.
type TraceIDs struct{ Session, Turn, Call string }

// TraceIDsFor recomputes them. agentID is empty on the main chain.
func TraceIDsFor(sessionID, agentID, promptID, toolUseID string) TraceIDs {
	ids := TraceIDs{Session: TraceHash(sessionID)}
	if agentID != "" {
		ids.Session = TraceHash(sessionID + "|" + agentID)
	}
	if promptID != "" {
		ids.Turn = TraceHash(sessionID + "|" + promptID)
	}
	if toolUseID != "" {
		ids.Call = TraceHash(sessionID + "|" + toolUseID)
	}
	return ids
}

// LineageIndex holds the ids found in the client's local lineage files, and
// nothing else from them.
type LineageIndex struct {
	Sessions, Turns, Calls map[string]bool
	// Files is how many lineage files were indexed; Skipped is how many
	// .json files in the directory were not lineage files this reads.
	Files, Skipped int
}

// lineageIDs is the only part of a lineage file that is decoded. The file
// also holds a history of assistant text; it has no field here to land in.
type lineageIDs struct {
	V         int    `json:"v"`
	SessionID string `json:"session_id"`
	TurnID    string `json:"turn_id"`
	CallID    string `json:"call_id"`
}

const lineageVersion = 1

// LoadLineage indexes the lineage files directly inside dir, through an
// os.Root like every other read here. A lineage file is one per workspace
// and keeps only that workspace's most recent ids, so this index is a floor
// on what the router could join, never the whole of it.
func LoadLineage(dir string) (*LineageIndex, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", dir, err)
	}
	defer func() { _ = root.Close() }()
	idx := &LineageIndex{Sessions: map[string]bool{}, Turns: map[string]bool{}, Calls: map[string]bool{}}
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		var l lineageIDs
		if !readJSON(root, e.Name(), &l) || l.V != lineageVersion || l.SessionID == "" {
			idx.Skipped++
			continue
		}
		idx.Files++
		idx.Sessions[l.SessionID] = true
		if l.TurnID != "" {
			idx.Turns[l.TurnID] = true
		}
		if l.CallID != "" {
			idx.Calls[l.CallID] = true
		}
	}
	return idx, nil
}

func readJSON(root *os.Root, name string, v any) bool {
	f, err := root.Open(name)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, 4<<20))
	return err == nil && json.Unmarshal(data, v) == nil
}

// LineageMatch says which of a search's recomputed ids the index holds.
type LineageMatch struct{ Session, Turn, Call bool }

// Match joins recomputed ids to the index. A nil index matches nothing.
func (idx *LineageIndex) Match(ids TraceIDs) LineageMatch {
	if idx == nil {
		return LineageMatch{}
	}
	return LineageMatch{
		Session: ids.Session != "" && idx.Sessions[ids.Session],
		Turn:    ids.Turn != "" && idx.Turns[ids.Turn],
		Call:    ids.Call != "" && idx.Calls[ids.Call],
	}
}
