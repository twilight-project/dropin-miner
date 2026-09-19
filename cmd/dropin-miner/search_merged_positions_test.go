package main

// #126: a merged page names the (candidate, citation) positions it was
// merged from.
//
// With "view":"merged" the candidates list is omitted, so found_in is the
// only thing in that envelope that maps a page back into the result the
// router stores under request_id. Anything that refers to a result by
// position rather than by copying its text — the router's own
// result.fetched and result.cited feedback events — loses its join
// without it.

import (
	"encoding/json"
	"net/http"
	"slices"
	"testing"
)

// positionsOf is a page's found_in as plain pairs, so a case can write out
// the positions it expects instead of building structs for them.
func positionsOf(p machineMergedPage) [][2]int {
	out := make([][2]int, len(p.FoundIn))
	for i, pos := range p.FoundIn {
		out[i] = [2]int{pos.Candidate, pos.Citation}
	}
	return out
}

func samePositions(got [][2]int, want ...[2]int) bool {
	return slices.Equal(got, want)
}

// TestMergedFoundInIndexesTheFixturesOwnCitations is the claim checked
// against the real recorded response rather than a constructed one: every
// position in every page's found_in indexes a candidate of that response
// whose citation at that index normalizes to the same page, the providers
// are the found_by providers in the same order, and the citation named is
// that provider's FIRST citation of the page.
//
// Read off the fixture, not written down: a literal list of 38 pages'
// positions would be a transcript of whatever the code produced the day it
// was written, which is the thing a golden cannot check.
func TestMergedFoundInIndexesTheFixturesOwnCitations(t *testing.T) {
	success := decodeFixtureSuccess(t, "balanced_response.json")
	wire := success.Response
	result := machineResultOf(success, "full")

	if len(result.Merged) == 0 {
		t.Fatal("the fixture produced no merged pages")
	}
	for _, page := range result.Merged {
		key, ok := normalizeMergeKey(page.URL)
		if !ok {
			t.Errorf("a merged page's own URL does not normalize: %q", page.URL)
			continue
		}
		if len(page.FoundIn) != len(page.FoundBy) {
			t.Errorf("%s: found_in has %d entries and found_by %d; they must be parallel",
				page.URL, len(page.FoundIn), len(page.FoundBy))
			continue
		}
		for n, pos := range page.FoundIn {
			if pos.Candidate < 0 || pos.Candidate >= len(wire.Candidates) {
				t.Errorf("%s: found_in[%d] names candidate %d of %d", page.URL, n, pos.Candidate, len(wire.Candidates))
				continue
			}
			c := wire.Candidates[pos.Candidate]
			if c.Provider != page.FoundBy[n] {
				t.Errorf("%s: found_in[%d] names candidate %d (%s) but found_by[%d] is %s; the two lists must agree entry for entry",
					page.URL, n, pos.Candidate, c.Provider, n, page.FoundBy[n])
			}
			if c.Status != "ok" {
				t.Errorf("%s: found_in[%d] names candidate %d, whose status is %q; only ok candidates contribute pages",
					page.URL, n, pos.Candidate, c.Status)
			}
			if pos.Citation < 0 || pos.Citation >= len(c.Citations) {
				t.Errorf("%s: found_in[%d] names citation %d of candidate %d's %d",
					page.URL, n, pos.Citation, pos.Candidate, len(c.Citations))
				continue
			}
			got, ok := normalizeMergeKey(c.Citations[pos.Citation].URL)
			if !ok || got != key {
				t.Errorf("%s: found_in[%d] points at candidate %d citation %d, which is %q — a different page",
					page.URL, n, pos.Candidate, pos.Citation, c.Citations[pos.Citation].URL)
				continue
			}
			// And it is that candidate's first citation of the page: an
			// earlier one naming the same page would mean found_in was
			// built from a later occurrence than found_by counted.
			for earlier := 0; earlier < pos.Citation; earlier++ {
				if k, ok := normalizeMergeKey(c.Citations[earlier].URL); ok && k == key {
					t.Errorf("%s: found_in[%d] names citation %d, but candidate %d cites the page earlier at %d",
						page.URL, n, pos.Citation, pos.Candidate, earlier)
					break
				}
			}
		}
	}
}

// TestViewMergedCarriesFoundInWithoutCandidates is the view the field
// exists for: the envelope an agent asking for "merged" actually receives,
// decoded as JSON rather than read off the Go struct, carries found_in on
// every page and no candidates key at all.
func TestViewMergedCarriesFoundInWithoutCandidates(t *testing.T) {
	_, cfg, root := newFakeRouter(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(readSearchTestdata(t, "balanced_response.json"))
	})
	h := fixedSearchOps(root)

	_, out, _ := runSearchStdin(t, h, map[string]string{"TOKENDROP_API_KEY": "k"},
		`{"version":1,"query":"q","view":"merged"}`, "-config", cfg)
	env := decodeEnvelope(t, out)
	result, _ := env["result"].(map[string]any)
	if result == nil {
		t.Fatalf("no result object: %s", out)
	}
	if _, ok := result["candidates"]; ok {
		t.Fatalf("view:merged carries a candidates key: %s", out)
	}
	merged, _ := result["merged"].([]any)
	if len(merged) == 0 {
		t.Fatalf("view:merged carries no pages: %s", out)
	}
	for i, raw := range merged {
		page, _ := raw.(map[string]any)
		foundBy, _ := page["found_by"].([]any)
		foundIn, hasFoundIn := page["found_in"].([]any)
		if !hasFoundIn {
			t.Fatalf("merged[%d] has no found_in key, so nothing in this envelope names a position: %v", i, page)
		}
		if len(foundIn) != len(foundBy) {
			t.Errorf("merged[%d]: found_in has %d entries and found_by %d", i, len(foundIn), len(foundBy))
		}
		for n, rawPos := range foundIn {
			pos, _ := rawPos.(map[string]any)
			if _, ok := pos["candidate"].(float64); !ok {
				t.Errorf("merged[%d].found_in[%d] has no candidate index: %v", i, n, rawPos)
			}
			if _, ok := pos["citation"].(float64); !ok {
				t.Errorf("merged[%d].found_in[%d] has no citation index: %v", i, n, rawPos)
			}
		}
	}

	// And the merged list is the same list the full view carries: the
	// field is additive, not a second shape for the merged-only view.
	_, fullOut, _ := runSearchStdin(t, h, map[string]string{"TOKENDROP_API_KEY": "k"},
		`{"version":1,"query":"q","view":"full"}`, "-config", cfg)
	fullEnv := decodeEnvelope(t, fullOut)
	fullResult, _ := fullEnv["result"].(map[string]any)
	a, err1 := json.Marshal(fullResult["merged"])
	b, err2 := json.Marshal(result["merged"])
	if err1 != nil || err2 != nil {
		t.Fatalf("marshal: %v %v", err1, err2)
	}
	if string(a) != string(b) {
		t.Errorf("merged differs between the views:\nfull:   %s\nmerged: %s", a, b)
	}
}
