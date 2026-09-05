package observe

// The search-router profile: a JSON response whose metadata sits at
// different paths from a completion's, read by the same scanner under the
// same bounds. The cases that matter are the ones where "different shape"
// could quietly become "different retention posture" or "invented number".

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// searchObserve drives the search parser exactly as the observer loop would:
// small chunks, an abort check after each, finalize at EOF.
func searchObserve(t *testing.T, body, headerID string) (*Observation, Outcome) {
	t.Helper()
	p := newSearchParser(32, 256, headerID)
	obsv := &Observation{StatusCode: 200}
	const chunk = 7
	for i := 0; i < len(body); i += chunk {
		p.consume([]byte(body[i:min(i+chunk, len(body))]))
		if reason, aborted := p.abortReason(); aborted {
			return obsv, Abandoned(reason)
		}
	}
	return obsv, p.finalize(true, nil, obsv)
}

func loadJSON(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "fixtures", "json", name)) // #nosec G304 -- test-owned fixture names
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

const fixtureRequestID = "fx-search-0e02b2c3d479"

// The happy path: identity from the header, the answering provider resolved
// through chosen, a terminal COMPLETE outcome.
func TestSearchFullResponse(t *testing.T) {
	o, out := searchObserve(t, loadJSON(t, "search_full.json"), fixtureRequestID)

	if term, ok := out.Termination(); !ok || term != TerminationDone {
		t.Fatalf("outcome %v, want complete:done", out)
	}
	if o.GenerationID != fixtureRequestID {
		t.Errorf("generation id: %q", o.GenerationID)
	}
	if o.ResolvedModel != "synthetic-search-b" {
		t.Errorf("chosen candidate's provider: %q", o.ResolvedModel)
	}
	if !o.SawDone || o.ParseDegraded || o.MetadataConflict || o.Streaming {
		t.Errorf("flags: %+v", o)
	}
}

// The invariant that keeps this API's money out of the token fields: it
// prices in micro-dollars and reports no token count, so the counters stay
// zero and usage stays absent. A cost converted into tokens would be a
// locally invented number.
func TestSearchUsageStaysEmpty(t *testing.T) {
	o, _ := searchObserve(t, loadJSON(t, "search_full.json"), fixtureRequestID)
	if o.Usage != nil || o.SawUsage {
		t.Errorf("cost_micros must not become provider-reported usage: %+v", o.Usage)
	}
	if o.FinishReason != "" || o.SawFinishReason {
		t.Errorf("this shape has no finish reason to report: %+v", o)
	}
}

// chosen == -1: the router answered but picked nothing. A recorded fact, not
// a parse failure — the observation is still terminal and still identified.
func TestSearchChosenNoneLeavesProviderEmpty(t *testing.T) {
	o, out := searchObserve(t, loadJSON(t, "search_no_choice.json"), "")
	if term, ok := out.Termination(); !ok || term != TerminationDone {
		t.Fatalf("outcome %v, want complete:done", out)
	}
	if o.ResolvedModel != "" {
		t.Errorf("chosen == -1 must resolve to no provider: %q", o.ResolvedModel)
	}
	if o.GenerationID != "fx-search-nochoice-1a2b" {
		t.Errorf("id must fall back to the body's request_id: %q", o.GenerationID)
	}
	if o.ParseDegraded {
		t.Error("choosing nothing is not a degraded parse")
	}
}

// candidates may arrive before chosen. Resolution is positional and must not
// depend on key order, which JSON does not guarantee.
func TestSearchKeyOrderIndependent(t *testing.T) {
	body := `{"candidates":[{"provider":"prov-a"},{"provider":"prov-b"},{"provider":"prov-c"}],` +
		`"chosen":2,"request_id":"req-order"}`
	o, out := searchObserve(t, body, "")
	if term, ok := out.Termination(); !ok || term != TerminationDone {
		t.Fatalf("outcome %v", out)
	}
	if o.ResolvedModel != "prov-c" || o.GenerationID != "req-order" {
		t.Errorf("late chosen not resolved: %+v", o)
	}
}

// The header is preferred, and a body that disagrees is recorded as a
// conflict rather than silently discarded.
func TestSearchHeaderPreferredAndConflictRecorded(t *testing.T) {
	body := `{"request_id":"req-from-body","chosen":0,"candidates":[{"provider":"prov-a"}]}`

	o, _ := searchObserve(t, body, "req-from-header")
	if o.GenerationID != "req-from-header" || !o.MetadataConflict {
		t.Errorf("header should win and record the disagreement: %+v", o)
	}

	o, _ = searchObserve(t, body, "req-from-body")
	if o.GenerationID != "req-from-body" || o.MetadataConflict {
		t.Errorf("agreement is not a conflict: %+v", o)
	}

	o, _ = searchObserve(t, body, "")
	if o.GenerationID != "req-from-body" || o.MetadataConflict {
		t.Errorf("absent header should fall back silently: %+v", o)
	}
}

// A header is upstream input and gets the same screen a body field gets.
func TestRequestIDHeaderScreened(t *testing.T) {
	cases := []struct {
		name, value, want string
	}{
		{"plain uuid", "f47ac10b-58cc-4372-a567-0e02b2c3d479", "f47ac10b-58cc-4372-a567-0e02b2c3d479"},
		{"absent", "", ""},
		{"whitespace", "req id", ""},
		{"control character", "req\tid", ""},
		{"non-ascii", "req-café", ""},
		{"oversized", strings.Repeat("x", capIDBytes+1), ""},
		{"credential shaped", "sk-or-v1-" + strings.Repeat("0", 8), ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := http.Header{}
			if c.value != "" {
				h.Set("X-Request-Id", c.value)
			}
			if got := requestIDHeader(h); got != c.want {
				t.Errorf("requestIDHeader = %q, want %q", got, c.want)
			}
		})
	}
	// Header names are case-insensitive on the wire; the screen must see
	// the value however the upstream spelled the name.
	h := http.Header{}
	h.Set("x-request-id", "req-lowercase")
	if got := requestIDHeader(h); got != "req-lowercase" {
		t.Errorf("case-insensitive header lookup: %q", got)
	}
}

// A body cut short is abandoned, so nothing about it is promotable — even
// though the header handed the observation a perfectly good identity. The
// identity is not the claim; finishing the observation is.
func TestSearchTruncatedBodyAbandons(t *testing.T) {
	full := loadJSON(t, "search_full.json")
	o, out := searchObserve(t, full[:len(full)/2], fixtureRequestID)

	if reason, ok := out.Reason(); !ok || reason != AbandonMalformedBody {
		t.Fatalf("outcome %v, want abandoned:malformed_body", out)
	}
	if _, complete := out.Termination(); complete {
		t.Error("an abandoned observation must not read as complete")
	}
	if o.GenerationID != fixtureRequestID {
		t.Errorf("the header identity is still recorded: %q", o.GenerationID)
	}
}

// Garbage where the document should be: the profile does not guess.
func TestSearchNonJSONBodyAbandons(t *testing.T) {
	for _, body := range []string{
		"data: {\"request_id\":\"req-1\"}\n\n",
		"<html>upstream error page</html>",
		`{"request_id":"req-1"} trailing`,
	} {
		_, out := searchObserve(t, body, "")
		if reason, ok := out.Reason(); !ok || reason != AbandonMalformedBody {
			t.Errorf("%q: outcome %v, want abandoned:malformed_body", body, out)
		}
	}
}

// The retention canary for this shape: an answer is completion content, and
// no field the observer keeps may contain it.
func TestSearchAnswerNeverCaptured(t *testing.T) {
	const canary = "canary-answer-3f8b-fictional"
	body := `{"request_id":"req-canary","chosen":0,"candidates":[{"provider":"prov-a",` +
		`"answer":"` + canary + `","citations":["https://example.invalid/` + canary + `"]}]}`
	o, out := searchObserve(t, body, "")
	if term, ok := out.Termination(); !ok || term != TerminationDone {
		t.Fatalf("outcome %v", out)
	}
	surfaces := o.GenerationID + " " + o.ResolvedModel + " " + o.FinishReason + " " + o.ErrorType + " " + o.ErrorCode
	if strings.Contains(surfaces, canary) {
		t.Error("answer content reached a retained field")
	}
	if o.ResolvedModel != "prov-a" {
		t.Errorf("provider capture broken: %q", o.ResolvedModel)
	}
}

// Past the fixed candidate table: the chosen row is a dropped field, which
// degrades. A chosen row still inside the table does not, however many
// candidates followed it.
func TestSearchCandidateTableBounded(t *testing.T) {
	var b strings.Builder
	b.WriteString(`{"request_id":"req-many","chosen":%d,"candidates":[`)
	for i := 0; i < maxCandidateSlots+4; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"provider":"prov-`)
		b.WriteByte(byte('a' + i))
		b.WriteString(`"}`)
	}
	b.WriteString(`]}`)
	tmpl := b.String()

	beyond := strings.Replace(tmpl, "%d", "11", 1)
	o, _ := searchObserve(t, beyond, "")
	if o.ResolvedModel != "" || !o.ParseDegraded {
		t.Errorf("a chosen row past the table must degrade, not guess: %+v", o)
	}

	inside := strings.Replace(tmpl, "%d", "1", 1)
	o, _ = searchObserve(t, inside, "")
	if o.ResolvedModel != "prov-b" || o.ParseDegraded {
		t.Errorf("a chosen row inside the table must resolve cleanly: %+v", o)
	}
}

// Per-field caps apply here too: an over-cap provider is dropped and
// degraded, never truncated and kept.
func TestSearchOversizedProviderDropped(t *testing.T) {
	long := strings.Repeat("p", capProviderBytes+1)
	body := `{"request_id":"req-long","chosen":0,"candidates":[{"provider":"` + long + `"}]}`
	o, _ := searchObserve(t, body, "")
	if o.ResolvedModel != "" || !o.ParseDegraded {
		t.Errorf("over-cap provider kept: %+v", o)
	}
}

// chosen is the one signed field. Everything that is not a plain integer
// degrades to "nothing chosen" rather than coercing.
func TestSearchChosenIntegerHygiene(t *testing.T) {
	for _, c := range []struct {
		chosen       string
		wantProvider string
		wantDegraded bool
	}{
		{"0", "prov-a", false},
		{"-1", "", false},
		{"-0", "", false},
		{"1.5", "", true},
		{"99999999999999999999999", "", true},
		{"null", "", false},
		{`"0"`, "", false},
	} {
		body := `{"request_id":"req-1","chosen":` + c.chosen + `,"candidates":[{"provider":"prov-a"}]}`
		o, out := searchObserve(t, body, "")
		if _, ok := out.Termination(); !ok {
			t.Errorf("chosen=%s: outcome %v, want a complete outcome", c.chosen, out)
			continue
		}
		if o.ResolvedModel != c.wantProvider || o.ParseDegraded != c.wantDegraded {
			t.Errorf("chosen=%s: provider %q degraded=%v", c.chosen, o.ResolvedModel, o.ParseDegraded)
		}
	}
}

// The profiles do not bleed into each other: a completion body read as a
// search response captures nothing, and vice versa. Wrong shape means no
// metadata, never wrong metadata.
func TestSearchProfileIsolation(t *testing.T) {
	o, out := searchObserve(t, loadJSON(t, "full_metadata.json"), "")
	if term, ok := out.Termination(); !ok || term != TerminationDone {
		t.Fatalf("outcome %v", out)
	}
	if o.GenerationID != "" || o.ResolvedModel != "" || o.Usage != nil {
		t.Errorf("completion metadata read through the search profile: %+v", o)
	}

	r := scanDoc(t, loadJSON(t, "search_full.json"))
	if r.ID != "" || r.Model != "" || r.SawUsage {
		t.Errorf("search metadata read through the completion profile: %+v", r)
	}
	if !r.Complete || r.Degraded {
		t.Errorf("an unknown shape is still structurally scannable: %+v", r)
	}
}
