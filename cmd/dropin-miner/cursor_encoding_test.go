package main

// A payload Cursor's Windows wrapper double-encoded is stored as the text it
// was (#113), and only the stored text: never the command.
//
// The fixtures are the bytes the issues record. The 19-byte intact form is
// the dump in #117's 2026-09-21 comment, verbatim, taken from Cursor's own
// hook log. The 29-byte double-encoded form is assembled from the
// per-character bytes #113's body states for the lineage record (`é` c3 83 c2
// a9, `東京` c3 a6 c2 9d c2 b1 c3 a4 c2 ba c2 ac, `ï` c3 83 c2 af); #113's
// 2026-09-21 comment gives its length and its first seven bytes, and both are
// checked here, since the comment elides the rest.

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
)

func issue113Bytes(t *testing.T, name string, wantLen int) string {
	t.Helper()
	raw, err := os.ReadFile("testdata/hook/" + name) // #nosec G304 -- a fixture name this test file spells
	if err != nil {
		t.Fatal(err)
	}
	b, err := hex.DecodeString(strings.ReplaceAll(strings.TrimSpace(string(raw)), " ", ""))
	if err != nil {
		t.Fatal(err)
	}
	if len(b) != wantLen {
		t.Fatalf("%s is %d bytes; the issue records %d", name, len(b), wantLen)
	}
	return string(b)
}

func issue113Fixtures(t *testing.T) (intact, doubled string) {
	t.Helper()
	intact = issue113Bytes(t, "issue-113-intact-19-bytes.hex", 19)
	doubled = issue113Bytes(t, "issue-113-double-encoded-29-bytes.hex", 29)
	if intact != "café 東京 naïve" {
		t.Fatalf("the intact fixture reads %q", intact)
	}
	if !strings.HasPrefix(doubled, "\x63\x61\x66\xc3\x83\xc2\xa9") {
		t.Fatalf("the double-encoded fixture does not begin as #113's comment records: % x", doubled[:7])
	}
	return intact, doubled
}

// TestTheDoubleEncodedReasoningLineStoresIntact: the reasoning and the
// response, as Cursor's wrapper hands them over, are stored as the sentence
// Cursor logged, and the repair is counted on stderr.
func TestTheDoubleEncodedReasoningLineStoresIntact(t *testing.T) {
	intact, doubled := issue113Fixtures(t)
	for _, event := range []string{"afterAgentThought", "afterAgentResponse"} {
		t.Run(event, func(t *testing.T) {
			_, ops := newFakeHookOps(nil)
			hc := hookContext{sessionsDir: "/sessions"}
			payload := mustJSON(t, map[string]any{"conversation_id": "conv-1", "workspace_roots": []string{"/w/proj"},
				"text": "The user wants " + doubled + " looked up; I will search for it."})
			var errOut bytes.Buffer
			hookCursor(ops, hc, event, payload, io.Discard, &errOut)
			l, ok := loadLineage(ops, conversationLineagePath("/sessions", "/w/proj", "conv-1"))
			if !ok || len(l.History) != 1 {
				t.Fatalf("nothing stored: %+v", l)
			}
			if want := "The user wants " + intact + " looked up; I will search for it."; l.History[0].Text != want {
				t.Fatalf("stored % x\nwant   % x", l.History[0].Text, want)
			}
			if !strings.Contains(errOut.String(), "1 double-encoded text repaired") {
				t.Errorf("the repair was not counted on stderr: %q", errOut.String())
			}
		})
	}
}

// TestTextThatIsNotDoubleEncodedIsStoredAsItCame covers every other shape:
// intact multibyte text, ASCII, Latin-1-only prose (what the second reading
// exists for — its bytes read back are not UTF-8), a string that is intact
// but whose every character is under U+0100, and cp1252's own characters used
// as themselves.
func TestTextThatIsNotDoubleEncodedIsStoredAsItCame(t *testing.T) {
	intact, _ := issue113Fixtures(t)
	for name, text := range map[string]string{
		"the intact line":                        intact,
		"ASCII":                                  "Let me check the docs.",
		"café, then 東京, intact":                  "café 東京",
		"Latin-1-only prose":                     "café naïve résumé",
		"an em dash and curly quotes as written": "it’s “quoted” — here",
		"a genuine Ã followed by a space":        "Ã ",
	} {
		t.Run(name, func(t *testing.T) {
			if got, ok := undoDoubleEncoding(text); ok || got != text {
				t.Fatalf("%q was taken as double-encoded and became %q", text, got)
			}
			_, ops := newFakeHookOps(nil)
			var errOut bytes.Buffer
			hookCursor(ops, hookContext{sessionsDir: "/sessions"}, "afterAgentThought",
				mustJSON(t, map[string]any{"conversation_id": "conv-1", "workspace_roots": []string{"/w/proj"}, "text": text}), io.Discard, &errOut)
			l, _ := loadLineage(ops, conversationLineagePath("/sessions", "/w/proj", "conv-1"))
			if l == nil || len(l.History) != 1 || l.History[0].Text != prepareTraceText(text) {
				t.Fatalf("stored %+v, want the text as it came", l)
			}
			if errOut.Len() != 0 {
				t.Errorf("a repair was reported for text that needed none: %q", errOut.String())
			}
		})
	}
}

// TestCP1252DoubleEncodingIsRepaired: the characters models write most — the
// em dash and the typographic quotes — pass through cp1252's 0x80–0x9F range,
// which reads as characters above U+00FF (#88's `â€”`).
func TestCP1252DoubleEncodingIsRepaired(t *testing.T) {
	for doubled, want := range map[string]string{
		"itâ€™s":            "it’s",
		"one â€” two":       "one — two",
		"â€œquotedâ€\u009d": "“quoted”",
		"cafÃ© â€¦ naÃ¯ve":  "café … naïve",
	} {
		if got, ok := undoDoubleEncoding(doubled); !ok || got != want {
			t.Errorf("%q repaired to %q (%v), want %q", doubled, got, ok, want)
		}
	}
}

// TestTheCommandIsNeverRepaired: the repair is for text the hooks store. A
// command is handed back to Cursor to run, and a guess there would change the
// search, so preToolUse echoes a double-encoded-looking body exactly as it
// received it (on a runner whose bytes are intact, where it rewrites at all).
func TestTheCommandIsNeverRepaired(t *testing.T) {
	_, doubled := issue113Fixtures(t)
	hc := hookContext{cfgPath: "/c.toml", sessionsDir: "/sessions"}
	env := cursorIdentityEnv(hc.sessionsDir, "conv-1")
	tool, runners := cursorShellsFor(t, "darwin")
	body := `{"version":1,"query":"` + doubled + `"}`
	rendered := renderedCursorSearch(t, shellPOSIX, hc.cfgPath, body)
	out, errOut := runCursorPreToolUse(t, env, hc, cursorPreToolUsePayload(t, rendered), tool, runners)
	var got cursorAnswer
	var command string
	if json.Unmarshal([]byte(out), &got) != nil || json.Unmarshal(got.UpdatedInput["command"], &command) != nil {
		t.Fatalf("no answer: %q %q", out, errOut)
	}
	if command != expectedCursorCommand(t, shellPOSIX, env, rendered) {
		t.Fatalf("the command was changed beyond the prefix:\n got %q", command)
	}
}
