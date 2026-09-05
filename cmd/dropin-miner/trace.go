package main

// The trace envelope: lineage that rides INSIDE a /v1/search request so the
// search router can group one task's searches. Ported from Telem's design
// (trajectory threading via a bridge parameter), adapted to this proxy.
//
// Privacy shape, stated once: hard invariant 2 forbids the PROXY retaining
// or logging prompt/completion content of proxied traffic. This envelope is
// content the CLIENT (the mcp command, at the participant's direction)
// chooses to send upstream in its own request body — the same category as
// the query itself. The proxy forwards it byte-for-byte, never logs it, and
// the observer reads response metadata only. The participant's controls:
// TOKENDROP_TRACE=off disables it entirely, and raw host session ids are
// hashed before they leave the machine — the router sees stable identifiers,
// never the host's own ids.

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
)

const (
	traceVersion = 1
	// The visible-text cap: what may ride as history, after truncation.
	traceHistoryCap = 32 << 10
	// The whole-envelope cap (compact JSON). Above it, history is dropped
	// first; an envelope that is still too big is dropped whole. A trace
	// must never be the reason a search fails.
	traceEnvelopeCap = 48 << 10
)

type traceHistory struct {
	Role string `json:"role"`
	Text string `json:"text"`
}

type traceEnvelope struct {
	V         int             `json:"v"`
	Harness   string          `json:"harness,omitempty"`
	SessionID string          `json:"session_id,omitempty"`
	TurnID    string          `json:"turn_id,omitempty"`
	CallID    string          `json:"call_id,omitempty"`
	Window    string          `json:"window,omitempty"`
	Seq       int             `json:"seq,omitempty"`
	History   []traceHistory  `json:"history,omitempty"`
	HostMeta  json.RawMessage `json:"host_meta,omitempty"`
}

// traceHash derives the identifier that travels from an identifier that must
// not: a keyed sha256, hex, 32 chars. Deterministic per raw id (the router
// can group), irreversible (the host's own id never leaves the machine).
func traceHash(raw string) string {
	if raw == "" {
		return ""
	}
	sum := sha256.Sum256([]byte("tokendrop-trace-v1|" + raw))
	return hex.EncodeToString(sum[:16])
}

// traceRandomID is the per-process fallback session identity for hosts with
// no hook channel: one id per mcp process, which is one agent session.
func traceRandomID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	return hex.EncodeToString(b[:])
}

// encodeTraceBridge / decodeTraceBridge carry an envelope through the one
// channel a hook has: a string parameter on the tool call.
func encodeTraceBridge(env *traceEnvelope) (string, error) {
	b, err := json.Marshal(env)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func decodeTraceBridge(s string) *traceEnvelope {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil
	}
	var env traceEnvelope
	if err := json.Unmarshal(b, &env); err != nil || env.V != traceVersion {
		return nil
	}
	return &env
}

// capTrace enforces the size discipline: truncate history text (keeping the
// END — the words nearest the search are the ones that explain it), then
// drop history, then drop the envelope. Returns nil when nothing may ride.
func capTrace(env *traceEnvelope) *traceEnvelope {
	if env == nil {
		return nil
	}
	for i := range env.History {
		if len(env.History[i].Text) > traceHistoryCap {
			env.History[i].Text = env.History[i].Text[len(env.History[i].Text)-traceHistoryCap:]
		}
	}
	if b, err := json.Marshal(env); err == nil && len(b) <= traceEnvelopeCap {
		return env
	}
	env.History = nil
	if b, err := json.Marshal(env); err == nil && len(b) <= traceEnvelopeCap {
		return env
	}
	return nil
}
