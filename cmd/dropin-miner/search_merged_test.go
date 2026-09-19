package main

// S3: result.merged — one deduplicated list of pages across every "ok"
// candidate — and view, which lets a caller ask for that list alone.

import (
	"encoding/json"
	"net/http"
	"testing"
)

// TestMergedListMatchesTheFixtureCounts pins the merge against numbers
// computed by hand from testdata/search/balanced_response.json (an
// independent Python re-implementation of the same rule cross-checked
// these against the fixture before this test was written): 5 of the 8
// candidates are "ok" (exa timed out, tavily errored, serpapi is still
// pending), contributing 47 citations that collapse to 38 unique pages —
// 2 pages found by 3 providers, 5 by 2, and 31 by exactly 1.
func TestMergedListMatchesTheFixtureCounts(t *testing.T) {
	success := decodeFixtureSuccess(t, "balanced_response.json")
	result := machineResultOf(success, "full")

	if got := len(result.Merged); got != 38 {
		t.Fatalf("merged pages: got %d, want 38", got)
	}
	dist := map[int]int{}
	for _, p := range result.Merged {
		dist[len(p.FoundBy)]++
	}
	want := map[int]int{3: 2, 2: 5, 1: 31}
	for n, count := range want {
		if dist[n] != count {
			t.Errorf("pages found by exactly %d provider(s): got %d, want %d", n, dist[n], count)
		}
	}
	// The distribution above should also account for every page: nothing
	// found by 0 or by more than 3 providers in this fixture.
	if dist[0] != 0 {
		t.Errorf("a page with no found_by entries: %d", dist[0])
	}
	for n := range dist {
		if n < 1 || n > 3 {
			t.Errorf("unexpected found_by count %d present in the distribution: %v", n, dist)
		}
	}
	// With a single ok candidate the merged list would just be that
	// candidate's own citations in order — sanity-checked here by the
	// fixture's top page: found by the most providers (3), and among
	// those the smallest best_rank, exactly as S3's ordering states.
	top := result.Merged[0]
	if len(top.FoundBy) != 3 {
		t.Errorf("top merged page found_by = %d, want 3: %+v", len(top.FoundBy), top)
	}
}

// routerCandidateOK is a small builder for the constructed fixtures below:
// an "ok" candidate naming one provider and its citations, in the shape
// mergedPagesOf reads (rank = position in this slice).
func routerCandidateOK(provider string, citations ...routerCitation) routerCandidate {
	return routerCandidate{Provider: provider, Status: "ok", Citations: citations}
}

// TestMergedListNormalizesURLs proves the exact identity rule: scheme
// dropped, HOST lowercased (path case is untouched — the rule names only
// the host), one leading "www." dropped, a trailing slash dropped,
// fragment dropped — while the query string is kept, so a differing
// query stays a distinct page rather than merging into one.
func TestMergedListNormalizesURLs(t *testing.T) {
	r := routerResponse{
		Chosen: 0,
		Candidates: []routerCandidate{
			routerCandidateOK("alpha",
				routerCitation{URL: "https://Example.com/Docs/", Title: "canonical"},
			),
			routerCandidateOK("beta",
				routerCitation{URL: "http://www.EXAMPLE.com/Docs#section-2", Title: "same page, different dress"},
			),
			routerCandidateOK("gamma",
				routerCitation{URL: "https://example.com/Docs?v=2", Title: "a distinct page: query kept"},
			),
		},
	}
	pages := mergedPagesOf(r)
	if len(pages) != 2 {
		t.Fatalf("pages: got %d, want 2 (one merged pair, one distinct query): %+v", len(pages), pages)
	}
	merged := pages[0]
	if len(merged.FoundBy) != 2 {
		t.Fatalf("merged page found_by: got %v, want [alpha beta]", merged.FoundBy)
	}
	if merged.FoundBy[0] != "alpha" || merged.FoundBy[1] != "beta" {
		t.Errorf("found_by order should follow candidate order: %v", merged.FoundBy)
	}
	if merged.URL != "https://Example.com/Docs/" {
		t.Errorf("url should come from the first candidate that found the page: %q", merged.URL)
	}
	// found_in follows found_by entry for entry: alpha cited it as
	// candidate 0's rank 0, beta as candidate 1's (#126).
	if got := positionsOf(merged); !samePositions(got, [2]int{0, 0}, [2]int{1, 0}) {
		t.Errorf("found_in: got %v, want [[0 0] [1 0]]", got)
	}
	distinct := pages[1]
	if len(distinct.FoundBy) != 1 || distinct.FoundBy[0] != "gamma" {
		t.Errorf("the differing-query page merged when it should not have: %+v", distinct)
	}
	if got := positionsOf(distinct); !samePositions(got, [2]int{2, 0}) {
		t.Errorf("the distinct page's found_in: got %v, want [[2 0]]", got)
	}
}

// TestMergedListDropsOpaqueURLs: mailto:, tel: and javascript: URLs have no
// host and no path — they are not fetchable pages — so three providers
// each citing one must produce zero merged pages, not one page keyed to
// the empty string all three collide on.
func TestMergedListDropsOpaqueURLs(t *testing.T) {
	r := routerResponse{
		Candidates: []routerCandidate{
			routerCandidateOK("alpha", routerCitation{URL: "mailto:someone@example.com"}),
			routerCandidateOK("beta", routerCitation{URL: "tel:+15551234567"}),
			routerCandidateOK("gamma", routerCitation{URL: "javascript:alert(1)"}),
		},
	}
	pages := mergedPagesOf(r)
	if len(pages) != 0 {
		t.Fatalf("pages: got %d, want 0: %+v", len(pages), pages)
	}
}

// TestMergedListKeepsPortDistinct: normalizeMergeKey keys on u.Host (which
// carries the port), not u.Hostname() (which strips it) — two different
// endpoints must not collapse into one page just because they share a
// hostname.
func TestMergedListKeepsPortDistinct(t *testing.T) {
	r := routerResponse{
		Candidates: []routerCandidate{
			routerCandidateOK("alpha", routerCitation{URL: "https://a.test:8080/x"}),
			routerCandidateOK("beta", routerCitation{URL: "https://a.test/x"}),
		},
	}
	pages := mergedPagesOf(r)
	if len(pages) != 2 {
		t.Fatalf("pages: got %d, want 2 (the port makes these different endpoints): %+v", len(pages), pages)
	}
}

// TestMergedListKeepsEncodedPathDistinct: normalizeMergeKey uses
// EscapedPath(), not the percent-decoded Path — "/a%2Fb" (one path segment
// containing a literal slash) and "/a/b" (two segments) name different
// resources and must not merge just because Path decodes both the same way.
func TestMergedListKeepsEncodedPathDistinct(t *testing.T) {
	r := routerResponse{
		Candidates: []routerCandidate{
			routerCandidateOK("alpha", routerCitation{URL: "https://a.test/a%2Fb"}),
			routerCandidateOK("beta", routerCitation{URL: "https://a.test/a/b"}),
		},
	}
	pages := mergedPagesOf(r)
	if len(pages) != 2 {
		t.Fatalf("pages: got %d, want 2 (%%2F and / name different paths): %+v", len(pages), pages)
	}
}

// TestMergedListDoesNotDoubleCountOneProvidersRepeat: a provider whose own
// citation list cites the same page twice (once plain, once with a
// trailing slash — the same normalized page) must appear in found_by once,
// not twice; foundSet is what mergedPagesOf uses to guard this.
func TestMergedListDoesNotDoubleCountOneProvidersRepeat(t *testing.T) {
	r := routerResponse{
		Candidates: []routerCandidate{
			routerCandidateOK("repeats",
				routerCitation{URL: "https://a.test/x"},
				routerCitation{URL: "https://a.test/x/"},
			),
		},
	}
	pages := mergedPagesOf(r)
	if len(pages) != 1 {
		t.Fatalf("pages: got %d, want 1: %+v", len(pages), pages)
	}
	if got := pages[0].FoundBy; len(got) != 1 || got[0] != "repeats" {
		t.Errorf("found_by: got %v, want exactly one entry for the repeating provider", got)
	}
	// And found_in names the FIRST of those two citations — the same
	// occurrence found_by counted (#126). The second one, rank 1, is the
	// same page under a trailing slash.
	if got := positionsOf(pages[0]); !samePositions(got, [2]int{0, 0}) {
		t.Errorf("found_in: got %v, want [[0 0]], the first of the provider's two citations", got)
	}
}

// TestMergedListOrdering drives all three ordering rules S3 states:
// found_by count descending, then best_rank ascending, then first
// appearance for a genuine tie.
func TestMergedListOrdering(t *testing.T) {
	r := routerResponse{
		Chosen: 0,
		// p1 finds A at rank 0 and B at rank 1. p2 finds C at rank 0 and D
		// at rank 1. p3 finds D again, at rank 0 — lowering D's best_rank
		// to 0 and raising its found_by to 2.
		//
		// Resulting facts: A{found_by:1, best_rank:0, 1st seen}, B{1, 1,
		// 2nd}, C{1, 0, 3rd}, D{2, 0, 4th}. Order: found_by descending
		// puts D first. Among the found_by=1 pages, best_rank ascending
		// puts A and C (both rank 0) ahead of B (rank 1); A and C are a
		// genuine tie (same found_by, same best_rank), broken by first
		// appearance — A was seen before C.
		Candidates: []routerCandidate{
			routerCandidateOK("p1",
				routerCitation{URL: "https://a.test/A"},
				routerCitation{URL: "https://a.test/B"},
			),
			routerCandidateOK("p2",
				routerCitation{URL: "https://a.test/C"},
				routerCitation{URL: "https://a.test/D"},
			),
			routerCandidateOK("p3",
				routerCitation{URL: "https://a.test/D"},
			),
		},
	}
	pages := mergedPagesOf(r)
	// found_in is the positions each page was merged from, which the
	// ordering rules move around but never rewrite: D keeps p2's rank-1
	// citation as well as p3's rank-0 one even though only the smaller
	// became its best_rank (#126).
	want := []struct {
		url      string
		bestRank int
		foundIn  [][2]int
	}{
		{"https://a.test/D", 0, [][2]int{{1, 1}, {2, 0}}}, // found_by=2, beats every found_by=1 page; p3 lowered its rank from 1 to 0
		{"https://a.test/A", 0, [][2]int{{0, 0}}},         // found_by=1, best_rank=0, seen before C
		{"https://a.test/C", 0, [][2]int{{1, 0}}},         // found_by=1, best_rank=0, tie with A broken by appearance
		{"https://a.test/B", 1, [][2]int{{0, 1}}},         // found_by=1, best_rank=1 — loses to both rank-0 pages
	}
	if len(pages) != len(want) {
		t.Fatalf("pages: got %d, want %d: %+v", len(pages), len(want), pages)
	}
	for i, w := range want {
		if pages[i].URL != w.url {
			t.Errorf("position %d: got %q, want %q (full order: %v)", i, pages[i].URL, w.url, urlsOf(pages))
		}
		if pages[i].BestRank != w.bestRank {
			t.Errorf("%s best_rank: got %d, want %d", pages[i].URL, pages[i].BestRank, w.bestRank)
		}
		if got := positionsOf(pages[i]); !samePositions(got, w.foundIn...) {
			t.Errorf("%s found_in: got %v, want %v", pages[i].URL, got, w.foundIn)
		}
	}
}

func urlsOf(pages []machineMergedPage) []string {
	out := make([]string, len(pages))
	for i, p := range pages {
		out[i] = p.URL
	}
	return out
}

// TestMergedListSnippetIsTheLongestFound proves the "snippet is the
// longest of those found" rule independent of found_by or ordering.
func TestMergedListSnippetIsTheLongestFound(t *testing.T) {
	r := routerResponse{
		Candidates: []routerCandidate{
			routerCandidateOK("short", routerCitation{URL: "https://a.test/x", Snippet: "brief"}),
			routerCandidateOK("long", routerCitation{URL: "https://a.test/x", Snippet: "a considerably longer snippet"}),
		},
	}
	pages := mergedPagesOf(r)
	if len(pages) != 1 || pages[0].Snippet != "a considerably longer snippet" {
		t.Fatalf("snippet: got %+v", pages)
	}
}

// TestMergedListOnlyReadsOkCandidates: a timeout, error or pending
// candidate contributes no pages, matching "the pages of all ok
// candidates" — not every candidate that happened to carry citations.
func TestMergedListOnlyReadsOkCandidates(t *testing.T) {
	r := routerResponse{
		Candidates: []routerCandidate{
			{Provider: "broken", Status: "error", Citations: []routerCitation{{URL: "https://a.test/should-not-appear"}}},
			routerCandidateOK("fine", routerCitation{URL: "https://a.test/should-appear"}),
		},
	}
	pages := mergedPagesOf(r)
	if len(pages) != 1 || pages[0].URL != "https://a.test/should-appear" {
		t.Fatalf("pages: %+v", pages)
	}
	// The skipped candidate still occupies position 0 in the result the
	// router stores, so the page found by the second one is candidate 1.
	// A found_in counted over the ok candidates alone would say 0 here,
	// and would name the wrong result to anything joining by position.
	if got := positionsOf(pages[0]); !samePositions(got, [2]int{1, 0}) {
		t.Errorf("found_in: got %v, want [[1 0]] — the index in the router's own candidate list, not among the ok ones", got)
	}
}

// ── view ──────────────────────────────────────────────────────────────

// TestViewMergedOmitsCandidates: view: merged carries no "candidates" key
// and the identical "merged" list "full" carries.
func TestViewMergedOmitsCandidates(t *testing.T) {
	fr, cfg, root := newFakeRouter(t, func(w http.ResponseWriter, _ *http.Request) {
		raw := readSearchTestdata(t, "balanced_response.json")
		_, _ = w.Write(raw)
	})
	h := fixedSearchOps(root)

	_, fullOut, _ := runSearchStdin(t, h, map[string]string{"TOKENDROP_API_KEY": "k"},
		`{"version":1,"query":"q","view":"full"}`, "-config", cfg)
	fullEnv := decodeEnvelope(t, fullOut)
	fullResult, _ := fullEnv["result"].(map[string]any)
	if _, ok := fullResult["candidates"]; !ok {
		t.Fatalf("view:full has no candidates key: %s", fullOut)
	}

	_, mergedOut, _ := runSearchStdin(t, h, map[string]string{"TOKENDROP_API_KEY": "k"},
		`{"version":1,"query":"q","view":"merged"}`, "-config", cfg)
	mergedEnv := decodeEnvelope(t, mergedOut)
	mergedResult, _ := mergedEnv["result"].(map[string]any)
	if _, ok := mergedResult["candidates"]; ok {
		t.Fatalf("view:merged still carries a candidates key: %s", mergedOut)
	}
	if _, ok := mergedResult["merged"]; !ok {
		t.Fatalf("view:merged has no merged key: %s", mergedOut)
	}

	fullMerged, err1 := json.Marshal(fullResult["merged"])
	mergedMerged, err2 := json.Marshal(mergedResult["merged"])
	if err1 != nil || err2 != nil {
		t.Fatalf("marshal: %v %v", err1, err2)
	}
	if string(fullMerged) != string(mergedMerged) {
		t.Errorf("merged differs between the two views:\nfull:   %s\nmerged: %s", fullMerged, mergedMerged)
	}

	t.Logf("envelope size: full=%d bytes, merged=%d bytes", len(fullOut), len(mergedOut))

	fr.mu.Lock()
	defer fr.mu.Unlock()
	if len(fr.reqs) != 2 {
		t.Fatalf("expected exactly 2 router calls, got %d", len(fr.reqs))
	}
}

// TestViewAbsentIsFull: a request that never mentions "view" behaves
// exactly as "full" — the option is additive, not a new required field.
func TestViewAbsentIsFull(t *testing.T) {
	_, cfg, root := newFakeRouter(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(routerBody))
	})
	h := fixedSearchOps(root)
	code, out, _ := runSearchStdin(t, h, map[string]string{"TOKENDROP_API_KEY": "k"},
		`{"version":1,"query":"q"}`, "-config", cfg)
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, out)
	}
	env := decodeEnvelope(t, out)
	result, _ := env["result"].(map[string]any)
	if _, ok := result["candidates"]; !ok {
		t.Errorf("an absent view omitted candidates: %s", out)
	}
	if _, ok := result["merged"]; !ok {
		t.Errorf("an absent view has no merged key: %s", out)
	}
}

// TestViewInvalidValueRefusedBeforeRouterCall: any value other than
// "full" or "merged" — including an explicit "" and the wrong JSON type
// — is fix_input, and never reaches the router.
func TestViewInvalidValueRefusedBeforeRouterCall(t *testing.T) {
	for _, tc := range []struct{ name, stdin string }{
		{"unknown word", `{"version":1,"query":"q","view":"summary"}`},
		{"empty string", `{"version":1,"query":"q","view":""}`},
		{"wrong type", `{"version":1,"query":"q","view":7}`},
		{"null", `{"version":1,"query":"q","view":null}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fr, cfg, root := newFakeRouter(t, func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(routerBody))
			})
			h := fixedSearchOps(root)
			code, out, errOut := runSearchStdin(t, h, map[string]string{"TOKENDROP_API_KEY": "k"}, tc.stdin, "-config", cfg)
			if code != exitUsage {
				t.Fatalf("exit %d, want %d (%s)", code, exitUsage, out)
			}
			env := decodeEnvelope(t, out)
			if got := envField(t, env, "code"); got != codeInvalidView {
				t.Errorf("code %q, want %q", got, codeInvalidView)
			}
			if got := envField(t, env, "action"); got != actionFixInput {
				t.Errorf("action %q, want %q", got, actionFixInput)
			}
			if errOut != "" {
				t.Errorf("an expected protocol error also explained itself on stderr: %q", errOut)
			}
			fr.mu.Lock()
			defer fr.mu.Unlock()
			if len(fr.reqs) != 0 {
				t.Error("a refused request was sent to the router anyway")
			}
		})
	}
}
