package trajectory

import (
	"bytes"
	"encoding/json"
	"strings"
)

// The host's entries are decoded permissively on purpose: the format is the
// host's, not ours, and it gains keys between versions. What is refused is an
// unknown *shape* — an entry type, a system subtype, a content block type or
// a prompt source this package has not declared — not an unknown key.

// entryHead is all that is decoded of an entry before its type is known to be
// one the reader parses, so a bookkeeping entry's payload is never retained.
type entryHead struct {
	Type string `json:"type"`
}

type rawEntry struct {
	Type             string          `json:"type"`
	Subtype          string          `json:"subtype"`
	UUID             string          `json:"uuid"`
	SessionID        string          `json:"sessionId"`
	AgentID          string          `json:"agentId"`
	PromptID         string          `json:"promptId"`
	PromptSource     string          `json:"promptSource"`
	Version          string          `json:"version"`
	Timestamp        string          `json:"timestamp"`
	Cwd              string          `json:"cwd"`
	IsMeta           bool            `json:"isMeta"`
	IsCompactSummary bool            `json:"isCompactSummary"`
	IsSidechain      bool            `json:"isSidechain"`
	ToolDenialKind   string          `json:"toolDenialKind"`
	Message          *rawMessage     `json:"message"`
	ToolUseResult    json.RawMessage `json:"toolUseResult"`
}

type rawMessage struct {
	Role       string          `json:"role"`
	Model      string          `json:"model"`
	StopReason *string         `json:"stop_reason"`
	Content    json.RawMessage `json:"content"`
}

type rawBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	IsError   bool            `json:"is_error"`
	Content   json.RawMessage `json:"content"`
}

// blocksOf returns a message's content as blocks. A bare string is the one
// text block it stands for. ok is false when content is neither.
func blocksOf(content json.RawMessage) (blocks []rawBlock, ok bool) {
	content = bytes.TrimSpace(content)
	if len(content) == 0 {
		return nil, false
	}
	switch content[0] {
	case '"':
		var s string
		if json.Unmarshal(content, &s) != nil {
			return nil, false
		}
		return []rawBlock{{Type: blockText, Text: s}}, true
	case '[':
		if json.Unmarshal(content, &blocks) != nil {
			return nil, false
		}
		return blocks, true
	}
	return nil, false
}

// textOf flattens a tool result's content — a string, or a list of blocks of
// which only the text ones contribute — into the text the tool returned.
func textOf(content json.RawMessage) string {
	blocks, ok := blocksOf(content)
	if !ok {
		return ""
	}
	var b strings.Builder
	for _, blk := range blocks {
		if blk.Type == blockText {
			b.WriteString(blk.Text)
		}
	}
	return b.String()
}

// shellResult is the structured form the host keeps beside a shell tool's
// result. stdout is preferred over the flattened result text when present,
// because the flattened text has stderr appended to it.
type shellResult struct {
	Stdout *string `json:"stdout"`
}

func stdoutOf(toolUseResult json.RawMessage) (string, bool) {
	raw := bytes.TrimSpace(toolUseResult)
	if len(raw) == 0 || raw[0] != '{' {
		return "", false
	}
	var r shellResult
	if json.Unmarshal(raw, &r) != nil || r.Stdout == nil {
		return "", false
	}
	return *r.Stdout, true
}

// printableType makes a host-supplied type name safe to print in a count: a
// short identifier or a placeholder, never arbitrary text from the file.
func printableType(s string) string {
	if s == "" {
		return "<absent>"
	}
	if len(s) > 48 {
		return "<unprintable>"
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
		default:
			return "<unprintable>"
		}
	}
	return s
}
