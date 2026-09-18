package trajectory

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
)

const measureFixtureDir = "testdata/measure"

func mustMeasure(t *testing.T) *Measurement {
	t.Helper()
	m, err := Measure(measureFixtureDir, nil, testScrubber())
	if err != nil {
		t.Fatalf("Measure: %v", err)
	}
	if m.Records != 2 || m.Workspaces != 2 {
		t.Fatalf("records = %d, workspaces = %d; the fixture holds two of each and this test is not looking at it",
			m.Records, m.Workspaces)
	}
	return m
}

// TestMeasureCountsEachCategoryOfUnconsentedMaterial holds every category to
// an exact count, so that a rule which stops firing is a failure rather than
// a smaller number nobody reads. The fixture plants each one deliberately:
//
//	search_result_provider_content      both searches' results
//	tool_result_file_outside_workspace  the Read of a file beside the
//	                                    workspace, and the `ls` of /srv/shared
//	                                    — a shell command's path is a path
//	other_account_home_directory        a listing naming /home/<somebody else>
//	email_address                       an author line in a git log
//	path_under_another_workspace        a path under the OTHER workspace this
//	                                    corpus ran a turn in
//	path_beside_the_workspace           the same Read, counted for where the
//	                                    file sits rather than for who read it
//
// The second workspace's own turn names a path under itself, and is the
// negative control: its own workspace is not another one, and a path under it
// is not beside it.
func TestMeasureCountsEachCategoryOfUnconsentedMaterial(t *testing.T) {
	m := mustMeasure(t)
	wantEvents := map[ConsentCategory]int{
		ConsentProviderContent:      2,
		ConsentFileOutsideWorkspace: 2,
		ConsentOtherAccountHome:     1,
		ConsentEmail:                1,
		ConsentAnotherWorkspace:     1,
		ConsentWorkspaceSibling:     1,
	}
	wantTurns := map[ConsentCategory]int{
		ConsentProviderContent:      2,
		ConsentFileOutsideWorkspace: 1,
		ConsentOtherAccountHome:     1,
		ConsentEmail:                1,
		ConsentAnotherWorkspace:     1,
		ConsentWorkspaceSibling:     1,
	}
	for _, cat := range AllConsentCategories {
		if got := m.Consent.Events[cat]; got != wantEvents[cat] {
			t.Errorf("%s: %d events, want %d", cat, got, wantEvents[cat])
		}
		if got := m.Consent.Turns[cat]; got != wantTurns[cat] {
			t.Errorf("%s: %d turns, want %d", cat, got, wantTurns[cat])
		}
		if m.Consent.Events[cat] > 0 && m.Consent.Bytes[cat] == 0 {
			t.Errorf("%s: counted events and no bytes; a category is weighed as well as counted", cat)
		}
	}
	if m.Consent.TurnsAny != 2 {
		t.Errorf("TurnsAny = %d, want 2", m.Consent.TurnsAny)
	}
	// Every category is in the printed order, or the findings would quote a
	// number the report never shows.
	if len(AllConsentCategories) != len(wantEvents) {
		t.Errorf("AllConsentCategories has %d entries and the test names %d", len(AllConsentCategories), len(wantEvents))
	}
}

// TestASecondWorkspaceIsNotForeignToItself is the negative control on its
// own: the rule is about a path under ANOTHER workspace, and the corpus's
// second turn names a path under its own.
func TestASecondWorkspaceIsNotForeignToItself(t *testing.T) {
	roots := map[string]bool{"/synthetic/workspace": true, "/synthetic/other": true}
	own := "ZEBRA-ANSWER in /synthetic/other/ZEBRA-OWN.go"
	if namesAnotherRoot(own, "/synthetic/other", roots) {
		t.Error("a path under the turn's own workspace was counted as another workspace's")
	}
	if namesWorkspaceSibling(own, "/synthetic/other", roots) {
		t.Error("a path under the turn's own workspace was counted as beside it")
	}
	// And from the other side: the same path, read in the other workspace.
	if !namesAnotherRoot(own, "/synthetic/workspace", roots) {
		t.Error("a path under a workspace this corpus has seen was not counted")
	}
	// A directory that shares the parent and is no workspace we have seen.
	beside := "ZEBRA-TEXT /synthetic/elsewhere/ZEBRA-NOTES.md"
	if namesAnotherRoot(beside, "/synthetic/workspace", roots) {
		t.Error("a directory that is not a workspace was counted as one")
	}
	if !namesWorkspaceSibling(beside, "/synthetic/workspace", roots) {
		t.Error("a directory beside the workspace was not counted")
	}
	// A longer name that merely begins with the parent is not beside it.
	if namesWorkspaceSibling("ZEBRA-TEXT /syntheticother/x", "/synthetic/workspace", roots) {
		t.Error("/syntheticother is not inside /synthetic")
	}
}

// TestMeasureReportsTheNumbersEmitWouldProduce is the point of sharing
// measureTurn: a finding about what a record costs is a statement about the
// records emit writes. If the two ever diverge, the findings describe a
// program nobody runs.
func TestMeasureReportsTheNumbersEmitWouldProduce(t *testing.T) {
	m := mustMeasure(t)
	var out bytes.Buffer
	stats, err := Emit(measureFixtureDir, testPolicy(t, nil, nil), testScrubber(), &out)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if m.Records != stats.Records {
		t.Errorf("measure counted %d records, emit wrote %d", m.Records, stats.Records)
	}
	if !reflect.DeepEqual(m.Emit.MeasuredBytes, stats.MeasuredBytes) {
		t.Errorf("per-level sizes differ:\n  measure %v\n  emit    %v", m.Emit.MeasuredBytes, stats.MeasuredBytes)
	}
	if !reflect.DeepEqual(m.Emit.ScrubOmitted, stats.ScrubOmitted) || !reflect.DeepEqual(m.Emit.ScrubRewritten, stats.ScrubRewritten) {
		t.Errorf("scrubber tallies differ:\n  measure %v %v\n  emit    %v %v",
			m.Emit.ScrubOmitted, m.Emit.ScrubRewritten, stats.ScrubOmitted, stats.ScrubRewritten)
	}
	if m.Emit.ScrubbedItems != stats.ScrubbedItems || m.Emit.SearchesNoURLs != stats.SearchesNoURLs {
		t.Errorf("items %d/%d, searches without a citation %d/%d",
			m.Emit.ScrubbedItems, stats.ScrubbedItems, m.Emit.SearchesNoURLs, stats.SearchesNoURLs)
	}
	if !reflect.DeepEqual(m.Emit.Labels, stats.Labels) {
		t.Errorf("labels differ:\n  measure %v\n  emit    %v", m.Emit.Labels, stats.Labels)
	}
	// The scrubber ran: the fixture plants one address and one home path.
	if m.Emit.ScrubOmitted[ScrubEmail] != 1 || m.Emit.ScrubRewritten[ScrubAccountName] != 1 {
		t.Errorf("scrubber tallies = omitted %v, rewritten %v", m.Emit.ScrubOmitted, m.Emit.ScrubRewritten)
	}
}

// TestMeasureReportPrintsCountsAndNoContent. The report is printed whole into
// a findings document, so anything of a transcript that reaches it is
// published.
func TestMeasureReportPrintsCountsAndNoContent(t *testing.T) {
	m := mustMeasure(t)
	var report bytes.Buffer
	if _, err := m.WriteTo(&report); err != nil {
		t.Fatal(err)
	}
	out := report.String()
	for _, leak := range []string{
		"ZEBRA-", "req_synthetic", "toolu_", "00000000-0000-4000", "example.test",
		"/synthetic", "/home/", "/srv/", "zebracolleague", "zebra.author",
	} {
		if strings.Contains(out, leak) {
			t.Errorf("the report carries %q:\n%s", leak, out)
		}
	}
	for _, want := range []string{
		"search_result_provider_content", "tool_result_file_outside_workspace",
		"workspaces this corpus ran turns in       2", "nothing was written for any of them",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the report lacks %q:\n%s", want, out)
		}
	}
}
