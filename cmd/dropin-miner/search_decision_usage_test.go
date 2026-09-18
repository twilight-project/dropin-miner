package main

// S2: the envelope carries the router's decision, usage and per-provider
// cost — and only when the router actually sent them.
//
// The fixture in testdata/search/balanced_response.json is one real
// `tier: balanced` response, recorded from the router on 2026-09-18
// (request id 01a0b49b-4bf3-7208-8edf-367070fdf6eb; see the stop report
// for the live call record). It carries no private data beyond the query
// chosen for it. The second fixture is the same response with "decision"
// and "usage" stripped, to prove their absence stays absence rather than
// becoming a zeroed block.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readSearchTestdata(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "search", name)) // #nosec G304 -- fixed testdata path this file builds, not an external one
	if err != nil {
		t.Fatalf("reading testdata/search/%s: %v", name, err)
	}
	return raw
}

func decodeFixtureSuccess(t *testing.T, name string) routerSuccess {
	t.Helper()
	success, err := decodeRouterSuccess(readSearchTestdata(t, name), "")
	if err != nil {
		t.Fatalf("decoding %s: %v", name, err)
	}
	return success
}

// TestEnvelopeCarriesTheRoutersDecisionAndUsage is the golden: every field
// S2 adds is present with the fixture's own value.
func TestEnvelopeCarriesTheRoutersDecisionAndUsage(t *testing.T) {
	success := decodeFixtureSuccess(t, "balanced_response.json")
	result := machineResultOf(success, "full")

	got, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	want := readSearchTestdata(t, "balanced_envelope.golden")
	if string(got) != string(want) {
		t.Errorf("envelope differs from the golden:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestAbsentDecisionAndUsageStayAbsent proves the omitempty pointers do
// their job: a router response that never mentions decision or usage
// produces an envelope with neither key, not one with a decision/usage
// object full of zeros.
func TestAbsentDecisionAndUsageStayAbsent(t *testing.T) {
	success := decodeFixtureSuccess(t, "balanced_response_no_decision_usage.json")
	result := machineResultOf(success, "full")
	if result.Decision != nil {
		t.Errorf("decision present when the router sent none: %+v", result.Decision)
	}
	if result.Usage != nil {
		t.Errorf("usage present when the router sent none: %+v", result.Usage)
	}
	if result.LatencyMS != 0 {
		t.Errorf("top-level latency_ms populated from an absent usage block: %d", result.LatencyMS)
	}
	got, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"decision"`, `"usage"`} {
		if strings.Contains(string(got), key) {
			t.Errorf("envelope carries %s despite the router sending none: %s", key, got)
		}
	}
	// Candidates still carry their own cost and latency: those are
	// per-candidate fields on the wire, untouched by stripping the
	// top-level decision/usage blocks.
	if result.Candidates == nil {
		t.Fatal("candidates omitted for the full view")
	}
	var any bool
	for _, c := range *result.Candidates {
		if c.CostMicros != 0 {
			any = true
		}
	}
	if !any {
		t.Error("no candidate carried a cost_micros; the absence fixture should only have stripped the top-level blocks")
	}
}

// TestCandidateCostAndLatencyComeFromTheRightFields pins cost_micros to
// the candidate's own field and latency_ms to latency.total_ms — not
// latency.ttfb_ms, and not the search-wide usage.cost_micros.
func TestCandidateCostAndLatencyComeFromTheRightFields(t *testing.T) {
	success := decodeFixtureSuccess(t, "balanced_response.json")
	result := machineResultOf(success, "full")
	raw := readSearchTestdata(t, "balanced_response.json")
	var wire routerResponse
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	if result.Candidates == nil {
		t.Fatal("candidates omitted for the full view")
	}
	got := *result.Candidates
	if len(got) != len(wire.Candidates) {
		t.Fatalf("candidate count: got %d, want %d", len(got), len(wire.Candidates))
	}
	for i, wc := range wire.Candidates {
		mc := got[i]
		if mc.CostMicros != wc.CostMicros {
			t.Errorf("candidate %d (%s) cost_micros: got %d, want %d", i, wc.Provider, mc.CostMicros, wc.CostMicros)
		}
		wantLatency := int64(0)
		if wc.Latency != nil {
			wantLatency = wc.Latency.TotalMS
		}
		if mc.LatencyMS != wantLatency {
			t.Errorf("candidate %d (%s) latency_ms: got %d, want %d (total_ms)", i, wc.Provider, mc.LatencyMS, wantLatency)
		}
	}
}

// ── the human line ────────────────────────────────────────────────────

func TestFormatCostDollarsIsTheShortestExactForm(t *testing.T) {
	for _, tc := range []struct {
		micros int64
		want   string
	}{
		{0, "$0"},
		{4000, "$0.004"},
		{42000, "$0.042"},
		{1_000_000, "$1"},
		{1_500_000, "$1.5"},
		{1, "$0.000001"},
		{44000, "$0.044"},
	} {
		if got := formatCostDollars(tc.micros); got != tc.want {
			t.Errorf("formatCostDollars(%d) = %q, want %q", tc.micros, got, tc.want)
		}
	}
}

// TestFormatModelPrintsProvidersCostAndPending is the human line for the
// fixture: the exact wording -format model adds, including the dollar
// figure computed from the fixture's own usage.cost_micros (42000, so
// $0.042) and the "still running" note the fixture's pending arm
// (serpapi) earns.
func TestFormatModelPrintsProvidersCostAndPending(t *testing.T) {
	success := decodeFixtureSuccess(t, "balanced_response.json")
	out := renderForModel(success.Response)
	const want = "providers: exa tavily linkup telem brave serpapi parallel-search you*  cost: $0.042  1 arm(s) still running"
	if !strings.Contains(out, want) {
		t.Errorf("human output does not contain the decision line:\nwant substring: %s\ngot:\n%s", want, out)
	}
}

// TestFormatModelOmitsTheDecisionLineWhenAbsent: the absence fixture
// renders without the line at all — no "providers:", no "cost:", and
// critically no zeroed cost standing in for one the router never sent.
func TestFormatModelOmitsTheDecisionLineWhenAbsent(t *testing.T) {
	success := decodeFixtureSuccess(t, "balanced_response_no_decision_usage.json")
	out := renderForModel(success.Response)
	for _, substr := range []string{"providers:", "cost:", "$0"} {
		if strings.Contains(out, substr) {
			t.Errorf("human output carries %q despite no decision/usage on the wire:\n%s", substr, out)
		}
	}
}
