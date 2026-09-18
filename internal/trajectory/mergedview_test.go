package trajectory

import (
	"bytes"
	"strings"
	"testing"
)

const mergedFixtureDir = "testdata/mergedview"

// TestTheMergedViewEnvelopeIsNamedRatherThanParsedIntoSilence.
//
// `"view":"merged"` is the one view whose envelope omits `candidates`, and
// `candidates[].citations[].url` is the only place citations are read from.
// Before T3b such a result parsed, yielded no citations and said nothing: the
// corpus lost its outcome labels with no refusal counted anywhere, which is
// the opposite of the rule T1 set for a shape the reader does not handle.
//
// It is not refused — it is a documented shape of a contract this client owns
// — and it is not a loss, because the request id is still at the top and the
// search is anchored. It is named, and counted.
func TestTheMergedViewEnvelopeIsNamedRatherThanParsedIntoSilence(t *testing.T) {
	merged := `{"version":1,"command":"search","ok":true,"request_id":"req_m",` +
		`"result":{"request_id":"req_m","merged":[` +
		`{"url":"https://zebra.example.test/a","title":"ZEBRA-TITLE","snippet":"ZEBRA-SNIPPET"}]}}`
	full := `{"version":1,"command":"search","ok":true,"request_id":"req_f",` +
		`"result":{"request_id":"req_f","merged":[{"url":"https://zebra.example.test/a"}],` +
		`"candidates":[{"citations":[{"url":"https://zebra.example.test/a"}]}]}}`
	topLevel := `{"request_id":"req_t","merged":[{"url":"https://zebra.example.test/a"}]}`

	for name, c := range map[string]struct {
		text      string
		wantShape ResultShape
		wantCites int
		wantID    string
	}{
		"merged and no candidates":        {merged, ShapeMergedView, 0, "req_m"},
		"merged beside candidates":        {full, "", 1, "req_f"},
		"merged at the top of the result": {topLevel, ShapeMergedView, 0, "req_t"},
	} {
		t.Run(name, func(t *testing.T) {
			sc := scrapeRequestIDs(c.text)
			if sc.shape != c.wantShape {
				t.Errorf("shape = %q, want %q", sc.shape, c.wantShape)
			}
			if len(sc.citations) != c.wantCites {
				t.Errorf("citations = %d, want %d", len(sc.citations), c.wantCites)
			}
			// Anchored either way: the shape decides the citations, never the id.
			if len(sc.ids) != 1 || sc.ids[0] != c.wantID {
				t.Errorf("ids = %v, want [%s]: a merged-view result is still anchored", sc.ids, c.wantID)
			}
			// Nothing of a merged page is bound, at any shape.
			for _, s := range []string{"ZEBRA-TITLE", "ZEBRA-SNIPPET"} {
				for _, cit := range sc.citations {
					if strings.Contains(cit.URL, s) {
						t.Errorf("a merged page's %s reached a citation", s)
					}
				}
			}
		})
	}
}

// TestAMergedViewSearchIsCountedThroughToTheReport proves the naming reaches
// the numbers: it is counted as its own shape, it is NOT counted as a loss,
// and it is kept apart from a search whose result simply offered no citation.
func TestAMergedViewSearchIsCountedThroughToTheReport(t *testing.T) {
	m, err := Measure(mergedFixtureDir, nil, testScrubber())
	if err != nil {
		t.Fatalf("Measure: %v", err)
	}
	if m.Records != 1 || m.Counts.Searches != 1 {
		t.Fatalf("records = %d, searches = %d; the fixture holds one of each and this test is not looking at it",
			m.Records, m.Counts.Searches)
	}
	if got := m.Counts.SearchesByShape[ShapeMergedView]; got != 1 {
		t.Errorf("searches of the merged-view shape = %d, want 1", got)
	}
	// Anchored, and therefore not a loss of any kind.
	if m.Counts.AnchoredPath1 != 1 {
		t.Errorf("anchored by path 1 = %d, want 1: the request id is still at the top of the envelope", m.Counts.AnchoredPath1)
	}
	if len(m.Counts.LossByReason) != 0 {
		t.Errorf("losses = %v, want none: a shape that carries no citations is not a lost anchor", m.Counts.LossByReason)
	}
	// Counted as unavailable, not as "offered none".
	if m.Emit.SearchesCitationsUnavailable != 1 || m.Emit.SearchesNoURLs != 0 {
		t.Errorf("citations unavailable = %d, offered none = %d; want 1 and 0 — they are different facts",
			m.Emit.SearchesCitationsUnavailable, m.Emit.SearchesNoURLs)
	}
	// And it reaches the report a person reads.
	var report bytes.Buffer
	if _, err := m.WriteTo(&report); err != nil {
		t.Fatal(err)
	}
	out := report.String()
	if !strings.Contains(out, string(ShapeMergedView)) {
		t.Errorf("the report never names the shape:\n%s", out)
	}
	if !strings.Contains(out, "carried no citations at all:   1") {
		t.Errorf("the report does not count the unavailable citations:\n%s", out)
	}
	for _, leak := range []string{"ZEBRA-", "zebra.example.test", "req_synthetic"} {
		if strings.Contains(out, leak) {
			t.Errorf("the report carries %q:\n%s", leak, out)
		}
	}
}
