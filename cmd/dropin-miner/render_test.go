package main

// The human renderer's boundary: remote text in, terminal-safe text out.
//
// The assertion throughout is not "it looks right" — it is that the bytes
// leaving this function cannot steer a terminal, are always valid UTF-8,
// never carry an active link with a scheme that does something other than
// fetch a page, and never exceed the budget however long any one field is.

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// unsafeSamples are the sequences a provider's answer must not be able to
// put on a terminal.
//
// Written as escapes, not literals: a test file that contained a real RLO
// would itself display in an order other than the one it is stored in,
// which is the exact confusion these characters are being refused for.
var unsafeSamples = map[string]string{
	"ESC":                "before \x1b after",
	"CSI color":          "red \x1b[31mtext\x1b[0m done",
	"CSI erase display":  "wipe \x1b[2J\x1b[H screen",
	"OSC window title":   "title \x1b]0;pwned\x07 end",
	"OSC hyperlink":      "link \x1b]8;;http://evil.test\x07click\x1b]8;;\x07 end",
	"carriage return":    "real answer\rFAKE ANSWER",
	"backspace":          "safe\x08\x08\x08\x08unsafe",
	"bell":               "ding \x07 dong",
	"NUL":                "nul \x00 byte",
	"DEL":                "del \x7f byte",
	"C1 CSI":             "eight-bit \u009b31m color",
	"bidi RLO":           "moc.live\u202e/example",
	"bidi isolates":      "a\u2066b\u2067c\u2069d",
	"LRM and RLM":        "a\u200eb\u200fc",
	"Arabic letter mark": "a\u061cb",
	"BOM as joiner":      "a\ufeffb",
	"invalid UTF-8":      "good \xff\xfe bad",
}

func TestRemoteTextNeverReachesTheTerminalAsControlSequences(t *testing.T) {
	for name, sample := range unsafeSamples {
		t.Run(name, func(t *testing.T) {
			got := oneLine(sample, 1000)
			assertTerminalSafe(t, got)
		})
	}
}

// assertTerminalSafe is the shared property: valid UTF-8, and no rune the
// renderer classifies as able to steer a terminal.
func assertTerminalSafe(t *testing.T, s string) {
	t.Helper()
	if !utf8.ValidString(s) {
		t.Fatalf("output is not valid UTF-8: %q", s)
	}
	for i, r := range s {
		// Two runes are expected in output and are not remote text. A
		// newline is the renderer's own line structure — oneLine has
		// already collapsed any that arrived in a field. U+FFFD is the
		// replacement the sanitizer writes: it is a printable glyph and
		// steers nothing, and unsafeForTerminal refuses it on the way IN
		// only because an invalid byte decodes to it.
		if r == '\n' || r == '�' {
			continue
		}
		if unsafeForTerminal(r) {
			t.Errorf("unsafe rune %U at byte %d in %q", r, i, s)
		}
	}
}

// The escape bytes themselves are gone, not merely reclassified.
func TestTheEscapeBytesThemselvesAreGone(t *testing.T) {
	for name, sample := range unsafeSamples {
		t.Run(name, func(t *testing.T) {
			got := oneLine(sample, 1000)
			for _, b := range []string{"\x1b", "\x07", "\x08", "\x00", "\x7f", "\r"} {
				if strings.Contains(got, b) {
					t.Errorf("a raw control byte %q survived: %q", b, got)
				}
			}
		})
	}
}

// Every field of a full response, not just the answer.
func TestEveryRenderedFieldIsSanitized(t *testing.T) {
	for name, sample := range unsafeSamples {
		t.Run(name, func(t *testing.T) {
			out := renderForModel(routerResponse{
				RequestID: "req-" + sample,
				Chosen:    0,
				Session: &struct {
					ID string `json:"id"`
				}{ID: "sess-" + sample},
				Candidates: []routerCandidate{{
					Provider: "prov" + sample,
					Kind:     "kind" + sample,
					Status:   "status" + sample,
					Answer:   "answer " + sample,
					Citations: []routerCitation{{
						URL:     "https://example.test/" + sample,
						Title:   "title " + sample,
						Snippet: "snippet " + sample,
					}},
				}, {
					Provider: "second",
					Error:    "error " + sample,
				}},
			})
			assertTerminalSafe(t, out)
		})
	}
}

// oneLine("éé", 3) is the boundary case §14 names by hand.
func TestTruncationNeverSplitsARune(t *testing.T) {
	multibyte := []string{
		"éé", "ééé", "端口端口", "🌐🌐🌐", "a🌐b", "éé", "ĲĳĲĳ",
	}
	for _, s := range multibyte {
		for max := 0; max <= len(s)+2; max++ {
			got := oneLine(s, max)
			if !utf8.ValidString(got) {
				t.Fatalf("oneLine(%q, %d) = %q, which is not valid UTF-8", s, max, got)
			}
			// The ellipsis is the renderer's, so the content itself must
			// not exceed the budget.
			if body := strings.TrimSuffix(got, "…"); len(body) > max {
				t.Errorf("oneLine(%q, %d) kept %d bytes of content", s, max, len(body))
			}
		}
	}
}

func TestOrdinaryUnicodeSurvivesUnchanged(t *testing.T) {
	for _, s := range []string{
		"¿cómo funcionan los puertos?",
		"端口是如何工作的",
		"ports — em dash, “smart quotes”, ½, ±, ≈",
		"emoji 🌐 stay 🎯 put",
	} {
		if got := oneLine(s, 1000); got != s {
			t.Errorf("ordinary text was altered:\n got %q\nwant %q", got, s)
		}
	}
}

// ── citation schemes ────────────────────────────────────────────────────

func TestOnlyWebSchemesAreRenderedAsLinks(t *testing.T) {
	for _, tc := range []struct {
		name, url string
		rendered  bool
	}{
		{"https", "https://example.test/a?b=c#d", true},
		{"http", "http://example.test/a", true},
		{"https uppercase scheme", "HTTPS://example.test/a", true},
		{"javascript", "javascript:alert(1)", false},
		{"javascript mixed case", "JavaScript:alert(1)", false},
		{"data", "data:text/html;base64,PHNjcmlwdD4=", false},
		{"file", "file:///etc/passwd", false},
		{"vbscript", "vbscript:msgbox(1)", false},
		// These carry an authority, so the empty-host check does not
		// reach them and only the scheme allowlist refuses them. Without
		// them this table passes with no allowlist at all — which is
		// exactly what a mutation of the scheme check proved.
		{"javascript with an authority", "javascript://example.test/%0aalert(1)", false},
		{"file with a host", "file://server/share", false},
		{"ftp", "ftp://example.test/f", false},
		{"gopher", "gopher://example.test/1", false},
		{"ws", "ws://example.test/socket", false},
		{"no scheme", "example.test/a", false},
		{"empty", "", false},
		{"malformed", "ht!tp://%%%", false},
		{"scheme only", "https://", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := citationTarget(tc.url)
			if tc.rendered {
				if got != tc.url {
					t.Errorf("a web URL was not rendered: %q → %q", tc.url, got)
				}
				return
			}
			if !strings.HasPrefix(got, "(link omitted") {
				t.Errorf("%q was rendered as a link: %q", tc.url, got)
			}
			// The payload must not survive into the output where a host
			// might auto-link it.
			if tc.url != "" && strings.Contains(got, tc.url) {
				t.Errorf("the unsafe URL survived into the output: %q", got)
			}
		})
	}
}

func TestUnsafeSchemesAreNotLinkedInAWholeResponse(t *testing.T) {
	out := renderForModel(routerResponse{
		RequestID: "req-1",
		Chosen:    0,
		Candidates: []routerCandidate{{
			Provider: "p",
			Answer:   "see below",
			Citations: []routerCitation{
				{URL: "javascript:alert(document.cookie)", Title: "Click me", Snippet: "harmless looking"},
				{URL: "data:text/html,<script>x</script>", Title: "Also me"},
				{URL: "file:///etc/shadow", Title: "And me"},
				// With an authority, so the allowlist is what refuses it.
				{URL: "javascript://example.test/%0aalert(1)", Title: "Sneaky me"},
				{URL: "ftp://example.test/payload", Title: "Old me"},
				{URL: "https://example.test/real", Title: "Real"},
			},
		}},
	})
	for _, forbidden := range []string{"javascript:", "data:text/html", "file:///", "ftp://"} {
		if strings.Contains(out, forbidden) {
			t.Errorf("%q reached the human output:\n%s", forbidden, out)
		}
	}
	// The titles beside them are still shown: only the link is dropped.
	for _, title := range []string{"Click me", "Also me", "And me", "Sneaky me", "Old me"} {
		if !strings.Contains(out, title) {
			t.Errorf("a safe title was dropped along with its unsafe URL: %q", title)
		}
	}
	if !strings.Contains(out, "https://example.test/real") {
		t.Error("the one safe URL was not rendered")
	}
}

// ── the budget ──────────────────────────────────────────────────────────

// No single field may carry the output past the cap, which is the failure
// a check at the top of the candidate loop could not catch.
func TestNoSingleFieldCanExceedTheOutputBudget(t *testing.T) {
	huge := strings.Repeat("x", renderTotalCap*3)
	for _, tc := range []struct {
		name string
		resp routerResponse
	}{
		{"long provider", routerResponse{RequestID: "r", Candidates: []routerCandidate{{Provider: huge}}}},
		{"long answer", routerResponse{RequestID: "r", Candidates: []routerCandidate{{Provider: "p", Answer: huge}}}},
		{"long error", routerResponse{RequestID: "r", Candidates: []routerCandidate{{Provider: "p", Error: huge}}}},
		{"long request id", routerResponse{RequestID: huge, Candidates: []routerCandidate{{Provider: "p"}}}},
		{
			"long title",
			routerResponse{RequestID: "r", Candidates: []routerCandidate{{
				Provider: "p", Citations: []routerCitation{{URL: "https://a.test/1", Title: huge}},
			}}},
		},
		{
			"long url",
			routerResponse{RequestID: "r", Candidates: []routerCandidate{{
				Provider: "p", Citations: []routerCitation{{URL: "https://a.test/" + huge}},
			}}},
		},
		{
			"long snippet",
			routerResponse{RequestID: "r", Candidates: []routerCandidate{{
				Provider: "p", Citations: []routerCitation{{URL: "https://a.test/1", Snippet: huge}},
			}}},
		},
		{"many candidates", routerResponse{RequestID: "r", Candidates: manyCandidates(huge)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := renderForModel(tc.resp)
			if len(out) > renderTotalCap {
				t.Errorf("output is %d bytes, budget is %d", len(out), renderTotalCap)
			}
			assertTerminalSafe(t, out)
		})
	}
}

func manyCandidates(pad string) []routerCandidate {
	out := make([]routerCandidate, 0, 64)
	for i := 0; i < 64; i++ {
		out = append(out, routerCandidate{Provider: "p", Answer: pad[:1024]})
	}
	return out
}

// A budget reached mid-field still leaves valid UTF-8.
func TestTheBudgetCutIsRuneSafe(t *testing.T) {
	wide := strings.Repeat("端", renderTotalCap)
	out := renderForModel(routerResponse{
		RequestID:  "r",
		Candidates: []routerCandidate{{Provider: "p", Answer: wide}},
	})
	if len(out) > renderTotalCap {
		t.Fatalf("output is %d bytes, budget is %d", len(out), renderTotalCap)
	}
	if !utf8.ValidString(out) {
		t.Fatal("the budget cut produced invalid UTF-8")
	}
}
