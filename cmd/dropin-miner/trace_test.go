package main

// The trace envelope: hashing, the bridge encoding, and the size discipline.
// All identifiers are synthetic.

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

func TestTraceHashIsStableIrreversibleAndNeverTheRawID(t *testing.T) {
	raw := "canary-session-raw-id"
	h1, h2 := traceHash(raw), traceHash(raw)
	if h1 != h2 {
		t.Errorf("not deterministic: %q vs %q", h1, h2)
	}
	if h1 == raw || strings.Contains(h1, "canary") {
		t.Errorf("raw id leaked into %q", h1)
	}
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(h1) {
		t.Errorf("shape: %q", h1)
	}
	if traceHash("other") == h1 {
		t.Error("distinct inputs collide")
	}
	if traceHash("") != "" {
		t.Error("empty input must stay empty")
	}
}

func TestTraceBridgeRoundTripsAndRejectsGarbage(t *testing.T) {
	env := &traceEnvelope{V: traceVersion, Harness: "claude-code", SessionID: "abc", TurnID: "t", Seq: 3,
		History: []traceHistory{{Role: "assistant", Text: "thinking about ports"}}}
	bridge, err := encodeTraceBridge(env)
	if err != nil {
		t.Fatal(err)
	}
	got := decodeTraceBridge(bridge)
	if got == nil || got.SessionID != "abc" || got.History[0].Text != "thinking about ports" {
		t.Errorf("round trip: %+v", got)
	}
	for _, bad := range []string{"", "not base64 ///", "bm90IGpzb24", encodeMust(t, &traceEnvelope{V: 99})} {
		if decodeTraceBridge(bad) != nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func encodeMust(t *testing.T, env *traceEnvelope) string {
	t.Helper()
	s, err := encodeTraceBridge(env)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestCapTraceTruncatesKeepingTheEndThenDropsHistoryThenTheEnvelope(t *testing.T) {
	long := strings.Repeat("x", traceHistoryCap) + "THE-END"
	env := capTrace(&traceEnvelope{V: traceVersion, History: []traceHistory{{Role: "assistant", Text: long}}})
	if env == nil || len(env.History[0].Text) != traceHistoryCap || !strings.HasSuffix(env.History[0].Text, "THE-END") {
		t.Fatalf("truncation kept the wrong end")
	}
	// An envelope whose non-history part alone exceeds the cap is dropped whole.
	if got := capTrace(&traceEnvelope{V: traceVersion, SessionID: strings.Repeat("s", traceEnvelopeCap+1)}); got != nil {
		t.Errorf("oversized envelope survived: %d bytes", envelopeLen(t, got))
	}
	// One that only exceeds it because of history keeps everything else.
	big := &traceEnvelope{V: traceVersion, SessionID: "keep-me",
		History: []traceHistory{{Role: "assistant", Text: strings.Repeat("h", traceHistoryCap)}, {Role: "assistant", Text: strings.Repeat("h", traceHistoryCap)}}}
	got := capTrace(big)
	if got == nil || got.SessionID != "keep-me" || got.History != nil {
		t.Errorf("history not shed: %+v", got)
	}
}

// TestCapTraceRedactsHistoryText is a direct, isolated check of capTrace's
// own contribution to Batch-1 T3 — called on a bare envelope, nothing else
// in the pipeline involved. This matters: an end-to-end test that goes
// through hookLineage also writes the lineage sidecar via saveLineage
// first, and saveLineage's in-place redaction mutates the same backing
// array env.History already points to (l.History = env.History aliases
// it), so that kind of test can pass even with capTrace's own redaction
// disabled — a vacuous pass for THIS chokepoint specifically, caught by
// injection-checking it before trusting the end-to-end test alone.
func TestCapTraceRedactsHistoryText(t *testing.T) {
	secret := "sk-or-v1-" + strings.Repeat("a", 24) + "SECRET"
	env := capTrace(&traceEnvelope{V: traceVersion, History: []traceHistory{{Role: "assistant", Text: "key: " + secret}}})
	if env == nil || len(env.History) != 1 {
		t.Fatalf("history lost: %+v", env)
	}
	if strings.Contains(env.History[0].Text, secret) {
		t.Fatalf("capTrace did not redact: %q", env.History[0].Text)
	}
	if !strings.Contains(env.History[0].Text, "[REDACTED]") {
		t.Fatalf("expected a scrub marker: %q", env.History[0].Text)
	}
}

// TestCapTraceRedactsBeforeTruncatingSoABoundaryStraddleDoesNotSurvive is
// PR #1 review: a secret positioned so a truncate-THEN-redact order would
// cut off its identifying prefix (sk-/sr-) at the traceHistoryCap
// boundary, leaving an unmatchable, unredacted bare tail. Constructed so
// the cut point (the number of leading bytes truncation removes) falls
// partway through the secret rather than before or after it.
func TestCapTraceRedactsBeforeTruncatingSoABoundaryStraddleDoesNotSurvive(t *testing.T) {
	secret := "sk-or-v1-" + strings.Repeat("a", 24) + "TAILMARKER"
	tailFillerLen := traceHistoryCap - len(secret)/2
	text := secret + strings.Repeat("x", tailFillerLen)
	if len(text) <= traceHistoryCap {
		t.Fatalf("test setup: text (%d bytes) must exceed traceHistoryCap (%d)", len(text), traceHistoryCap)
	}
	cutRemoves := len(text) - traceHistoryCap
	if cutRemoves <= 0 || cutRemoves >= len(secret) {
		t.Fatalf("test setup: cut point (%d) does not straddle the secret (len %d) — fix the fixture, not the assertion", cutRemoves, len(secret))
	}

	env := capTrace(&traceEnvelope{V: traceVersion, History: []traceHistory{{Role: "assistant", Text: text}}})
	if env == nil || len(env.History) != 1 {
		t.Fatalf("history lost entirely: %+v", env)
	}
	if strings.Contains(env.History[0].Text, "TAILMARKER") {
		t.Errorf("a fragment of the secret (its identifying prefix truncated away) survived the boundary straddle unredacted (%d bytes in the result)", len(env.History[0].Text))
	}
}

func envelopeLen(t *testing.T, env *traceEnvelope) int {
	t.Helper()
	b, _ := json.Marshal(env)
	return len(b)
}
