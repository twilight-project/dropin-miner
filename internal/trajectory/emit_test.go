package trajectory

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const emitFixtureDir = "testdata/emit"

const (
	sessLabels    = "00000000-0000-4000-8000-000000000021"
	sessScrub     = "00000000-0000-4000-8000-000000000022"
	sessAbandoned = "00000000-0000-4000-8000-000000000023"

	consentedWorkspace = "/synthetic/workspace"
)

// Everything planted in the scrubber fixture, and the pieces of it that would
// survive a cut: the words on either side of a secret, and the account names.
var planted = []string{
	"zebra.person@example.test",           // an email address
	"sr-zebra0000zebra0000zebra00",        // a credential-shaped string
	"zebra-secret-value-12345",            // an environment secret's value
	"zebra-host.local",                    // the machine's name
	"zebrauser", "otherzebra", "WinZebra", // account names in home paths
	"WkVCUkEtQlJJREdFLVBST1NF", // the trace bridge on a search's command line
}

var cutResidue = []string{"before the key", "after the key", "before the secret", "after the secret", "answering"}

func testScrubber() *Scrubber {
	return NewScrubber(
		[]string{"ZEBRA_DEPLOY_TOKEN=zebra-secret-value-12345", "PATH=/usr/bin:/bin", "API_KEY=short"},
		"zebra-host.local", "/Users/zebrauser")
}

func settingsJSON(t *testing.T, s Settings) []byte {
	t.Helper()
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// testPolicy writes a config and a consent record for consentedWorkspace,
// each opening the gates it is given, under a temporary directory. nil means
// the file is not written at all.
func testPolicy(t *testing.T, config, consent []Gate) *Policy {
	t.Helper()
	dir := t.TempDir()
	configPath, consentDir := filepath.Join(dir, "config.json"), filepath.Join(dir, "consent")
	if err := os.Mkdir(consentDir, 0o700); err != nil {
		t.Fatal(err)
	}
	asMap := func(gates []Gate) map[Gate]bool {
		m := map[Gate]bool{}
		for _, g := range gates {
			m[g] = true
		}
		return m
	}
	if config != nil {
		data := settingsJSON(t, Settings{V: 1, PolicyVersion: PolicyVersion, Gates: asMap(config)})
		if err := os.WriteFile(configPath, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if consent != nil {
		data := settingsJSON(t, Settings{V: 1, PolicyVersion: PolicyVersion, WorkspaceHash: WorkspaceHash(consentedWorkspace), Gates: asMap(consent)})
		if err := os.WriteFile(filepath.Join(consentDir, WorkspaceHash(consentedWorkspace)+".json"), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	p, err := LoadPolicy(configPath, consentDir)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func buildable() []Gate {
	var gates []Gate
	for _, g := range AllGates {
		if compiledCeiling[g] {
			gates = append(gates, g)
		}
	}
	return gates
}

func emitFixtures(t *testing.T, p *Policy) (string, map[string]Record, *EmitStats) {
	t.Helper()
	var out bytes.Buffer
	stats, err := Emit(emitFixtureDir, p, testScrubber(), &out)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	records := map[string]Record{}
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var r Record
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("emit wrote a line that is not a record: %v\n%s", err, line)
		}
		records[r.SessionID] = r
	}
	if len(records) != 3 {
		t.Fatalf("records = %d, want one per fixture turn that holds a search (3)", len(records))
	}
	return out.String(), records, stats
}

func TestDefaultEmitIsLevelOneIdsOnly(t *testing.T) {
	out, records, stats := emitFixtures(t, testPolicy(t, nil, nil))
	if stats.RecordsByLevel[1] != 3 || stats.Records != 3 {
		t.Fatalf("RecordsByLevel = %v: with no config and no consent every record must be level 1", stats.RecordsByLevel)
	}
	if strings.Contains(out, contentMarker) {
		t.Fatalf("a level 1 record carries authored text:\n%s", out)
	}
	for _, s := range append(append([]string{}, planted...), "example.test", "synthetic-model", "2.1.276", "/synthetic") {
		if strings.Contains(out, s) {
			t.Errorf("a level 1 record carries %q", s)
		}
	}
	r := records[TraceHash(sessLabels)]
	if r.Level != 1 || len(r.Gates) != 0 || r.Labels != nil || r.Host != "" || r.Model != "" || r.Workspace != "" || r.End != "" {
		t.Errorf("default record = %+v", r)
	}
	if r.TurnID != TraceHash(sessLabels+"|prompt-l1") {
		t.Errorf("TurnID = %s, want the id the router already holds", r.TurnID)
	}
	var kinds []Kind
	var ids []string
	for _, ev := range r.Events {
		kinds = append(kinds, ev.Kind)
		ids = append(ids, ev.RequestIDs...)
		if ev.Text != "" || ev.Query != "" || ev.Input != nil || ev.Tool != "" || ev.Bytes != 0 {
			t.Errorf("level 1 event carries more than ids: %+v", ev)
		}
	}
	wantKinds := []Kind{KindSearchCall, KindSearchResult, KindSearchCall, KindSearchResult, KindSearchCall, KindSearchResult}
	if !reflect.DeepEqual(kinds, wantKinds) {
		t.Errorf("level 1 events = %v, want only the searches, in order", kinds)
	}
	if !reflect.DeepEqual(ids, []string{"req_synthetic_0021", "req_synthetic_0022"}) {
		t.Errorf("request ids = %v", ids)
	}
	if r.Events[0].CallID != TraceHash(sessLabels+"|toolu_search_l1") {
		t.Errorf("CallID = %s, want the hashed call id, never the host's raw one", r.Events[0].CallID)
	}
	// Every closed gate that would have changed the output is named, with why.
	for _, g := range buildable() {
		if stats.Withheld[g] == 0 {
			t.Errorf("gate %s withheld material and was not named", g)
		}
		if stats.Blockers[g][BlockedByConfig] == 0 || stats.Blockers[g][BlockedByConsent] == 0 {
			t.Errorf("gate %s blockers = %v, want both config and consent", g, stats.Blockers[g])
		}
	}
	var report strings.Builder
	if _, err := stats.WriteTo(&report); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(report.String(), "every record is level 1") || !strings.Contains(report.String(), "prompts") {
		t.Errorf("the report does not say the run was level 1, or does not name a closed gate:\n%s", report.String())
	}
}

func TestPlantedSecretsAreAbsentAtEveryLevelAndNeverCutOut(t *testing.T) {
	levels := map[string][]Gate{
		"level 1": {},
		"level 2": {GateLevel2},
		"level 3": buildable(),
	}
	for name, gates := range levels {
		t.Run(name, func(t *testing.T) {
			out, records, _ := emitFixtures(t, testPolicy(t, gates, gates))
			for _, s := range planted {
				if strings.Contains(out, s) {
					t.Errorf("the output carries %q", s)
				}
			}
			// An event in which something was detected is omitted whole. If
			// the secret had been cut out and the rest kept, these survive.
			for _, s := range cutResidue {
				if strings.Contains(out, s) {
					t.Errorf("the output carries %q: a detected item was cut out of its event instead of the event being omitted", s)
				}
			}
			if name != "level 3" {
				return
			}
			r := records[TraceHash(sessScrub)]
			byKind := map[Kind][]RecordEvent{}
			for _, ev := range r.Events {
				byKind[ev.Kind] = append(byKind[ev.Kind], ev)
			}
			wantOmitted := func(ev RecordEvent, class ScrubClass) {
				t.Helper()
				if ev.Omitted != class || ev.Text != "" || ev.Input != nil || ev.Query != "" {
					t.Errorf("%s event = %+v, want content omitted as %q", ev.Kind, ev, class)
				}
				if ev.Origin == "" || ev.Bytes == 0 {
					t.Errorf("%s event = %+v: an omitted event keeps its kind, origin and size", ev.Kind, ev)
				}
			}
			wantOmitted(byKind[KindPrompt][0], ScrubEmail)
			wantOmitted(byKind[KindAssistantText][0], ScrubCredential)
			wantOmitted(byKind[KindToolResult][0], ScrubEnvSecret)
			wantOmitted(byKind[KindToolResult][1], ScrubHostname)

			// What is known exactly is replaced whole, and the event kept.
			if got, want := byKind[KindSearchCall][0].Query, contentMarker+"QUERY what is kept in ~/notes"; got != want {
				t.Errorf("query = %q, want %q", got, want)
			}
			if got, want := string(byKind[KindToolCall][0].Input), `{"file_path":"~/project/ZEBRA-TOOL-INPUT.env"}`; got != want {
				t.Errorf("Read parameters = %s, want %s", got, want)
			}
			listing := byKind[KindToolResult][2].Text
			for _, want := range []string{"/home/<user>/shared", `C:\Users\<user>\Desktop`, "~/project/out", "/Users/<user>/x"} {
				if !strings.Contains(listing, want) {
					t.Errorf("rewritten listing = %q, lacks %q", listing, want)
				}
			}
			if clean := byKind[KindAssistantText][1]; clean.Text != contentMarker+"ANSWER nothing sensitive in this one" || clean.Omitted != "" {
				t.Errorf("a clean event was disturbed: %+v", clean)
			}
		})
	}
}

func TestOpeningThePromptsGateDoesNotOpenTheResponsesGate(t *testing.T) {
	gates := []Gate{GateLevel2, GateLevel3, GatePrompts}
	_, records, _ := emitFixtures(t, testPolicy(t, gates, gates))
	r := records[TraceHash(sessLabels)]
	if r.Level != 3 || !reflect.DeepEqual(r.Gates, gates) {
		t.Fatalf("Level = %d, Gates = %v; want 3 and exactly %v", r.Level, r.Gates, gates)
	}
	prompts, withheld := 0, 0
	for _, ev := range r.Events {
		switch ev.Kind {
		case KindPrompt:
			prompts++
			if !strings.HasPrefix(ev.Text, contentMarker+"PROMPT") {
				t.Errorf("the prompts gate is open and the prompt is missing: %+v", ev)
			}
		default:
			withheld++
			if ev.Text != "" || ev.Input != nil || ev.Query != "" {
				t.Errorf("only prompts is open, and a %s event carries content: %+v", ev.Kind, ev)
			}
		}
	}
	if prompts != 1 || withheld < 8 {
		t.Fatalf("saw %d prompt and %d other events; the test is not looking at the turn", prompts, withheld)
	}
	if r.Model != "" || r.Workspace != "" {
		t.Errorf("host_and_model and workspace are closed: %+v", r)
	}
}

// TestEveryGateIsDecidedAlone is the independence case: for each gate, open
// it and nothing else, and it must be the only gate open.
func TestEveryGateIsDecidedAlone(t *testing.T) {
	for _, gate := range buildable() {
		only := []Gate{gate}
		set := testPolicy(t, only, only).For(consentedWorkspace)
		if got := set.OpenGates(); !reflect.DeepEqual(got, only) {
			t.Errorf("opened %s alone; open gates = %v", gate, got)
		}
	}
	// And the other way: everything but one leaves exactly that one closed.
	for _, gate := range buildable() {
		var rest []Gate
		for _, g := range buildable() {
			if g != gate {
				rest = append(rest, g)
			}
		}
		set := testPolicy(t, rest, rest).For(consentedWorkspace)
		if set.Open(gate) || !reflect.DeepEqual(set.OpenGates(), rest) {
			t.Errorf("opened all but %s; open gates = %v", gate, set.OpenGates())
		}
	}
}

func TestAGateOpensOnlyWhenCeilingConfigAndConsentAllAgree(t *testing.T) {
	all := buildable()
	blockers := func(p *Policy, cwd string) []string { return p.For(cwd)[GatePrompts] }

	if got := blockers(testPolicy(t, all, nil), consentedWorkspace); !reflect.DeepEqual(got, []string{BlockedByConsent}) {
		t.Errorf("config alone: prompts blockers = %v, want [consent]", got)
	}
	if got := blockers(testPolicy(t, nil, all), consentedWorkspace); !reflect.DeepEqual(got, []string{BlockedByConfig}) {
		t.Errorf("consent alone: prompts blockers = %v, want [config]", got)
	}
	if got := blockers(testPolicy(t, all, all), "/synthetic/other"); !reflect.DeepEqual(got, []string{BlockedByConsent}) {
		t.Errorf("another workspace: prompts blockers = %v, want [consent]: consent is per workspace", got)
	}

	// The ceiling: asked for by both files, and still closed.
	asked := append(append([]Gate{}, all...), GateUpload, GateRawBodies)
	p := testPolicy(t, asked, asked)
	for _, g := range []Gate{GateUpload, GateRawBodies} {
		if got := p.For(consentedWorkspace)[g]; !reflect.DeepEqual(got, []string{BlockedByCeiling}) {
			t.Errorf("%s blockers = %v, want [ceiling]", g, got)
		}
	}
	_, _, stats := emitFixtures(t, p)
	if !stats.Refused[GateUpload] || !stats.Refused[GateRawBodies] {
		t.Errorf("Refused = %v: a config that asks for upload must be told no, by name", stats.Refused)
	}

	// A consent record written against other terms, and one copied from
	// another workspace, agree to nothing.
	p = testPolicy(t, all, nil)
	consentPath := p.ConsentPath(consentedWorkspace)
	for name, s := range map[string]Settings{
		BlockedByPolicyVersion: {V: 1, PolicyVersion: "some-later-terms", WorkspaceHash: WorkspaceHash(consentedWorkspace), Gates: map[Gate]bool{GatePrompts: true}},
		BlockedByConsent:       {V: 1, PolicyVersion: PolicyVersion, WorkspaceHash: WorkspaceHash("/somewhere/else"), Gates: map[Gate]bool{GatePrompts: true}},
	} {
		if err := os.WriteFile(consentPath, settingsJSON(t, s), 0o600); err != nil {
			t.Fatal(err)
		}
		p.cache = map[string]GateSet{}
		if got := blockers(p, consentedWorkspace); !reflect.DeepEqual(got, []string{name}) {
			t.Errorf("prompts blockers = %v, want [%s]", got, name)
		}
	}
}

// TestConsentInsideTheWorkspaceIsRefused: a cloned repository must not be
// able to carry its own consent, or its own config.
func TestConsentInsideTheWorkspaceIsRefused(t *testing.T) {
	workspace := t.TempDir()
	consentDir := filepath.Join(workspace, ".trajectory-poc", "consent")
	if err := os.MkdirAll(consentDir, 0o700); err != nil {
		t.Fatal(err)
	}
	open := map[Gate]bool{GatePrompts: true}
	configPath := filepath.Join(workspace, "config.json")
	if err := os.WriteFile(configPath, settingsJSON(t, Settings{V: 1, PolicyVersion: PolicyVersion, Gates: open}), 0o600); err != nil {
		t.Fatal(err)
	}
	consent := Settings{V: 1, PolicyVersion: PolicyVersion, WorkspaceHash: WorkspaceHash(workspace), Gates: open}
	if err := os.WriteFile(filepath.Join(consentDir, WorkspaceHash(workspace)+".json"), settingsJSON(t, consent), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := LoadPolicy(configPath, consentDir)
	if err != nil {
		t.Fatal(err)
	}
	got := p.For(workspace)[GatePrompts]
	if want := []string{BlockedByConfigLocation, BlockedByConsentLocation}; !reflect.DeepEqual(got, want) {
		t.Fatalf("prompts blockers = %v, want %v: both files agree, and both are in the workspace they speak for", got, want)
	}
}

func TestAnUnknownGateNameIsAnErrorNotANoOp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"v":1,"policy_version":"poc-phase1","gates":{"promts":true}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPolicy(path, t.TempDir()); err == nil || !strings.Contains(err.Error(), "promts") {
		t.Fatalf("LoadPolicy = %v, want an error naming the misspelled gate", err)
	}
}

func TestEveryEventCarriesAnOriginAndAllSixClassesAppear(t *testing.T) {
	valid := map[Origin]bool{
		OriginParticipant: true, OriginHostModel: true, OriginToolResult: true,
		OriginClient: true, OriginRouter: true, OriginHost: true,
	}
	for name, gates := range map[string][]Gate{"level 1": {}, "level 3": buildable()} {
		_, records, _ := emitFixtures(t, testPolicy(t, gates, gates))
		seen := map[Origin]bool{}
		events := 0
		for _, r := range records {
			for _, ev := range r.Events {
				events++
				if !valid[ev.Origin] {
					t.Errorf("%s: a %s event has origin %q", name, ev.Kind, ev.Origin)
				}
				seen[ev.Origin] = true
			}
		}
		if events < 10 {
			t.Fatalf("%s: only %d events seen", name, events)
		}
		if name == "level 3" && len(seen) != len(valid) {
			t.Errorf("origins seen at level 3 = %v, want all six", seen)
		}
	}
}

func TestOutcomeLabelsAreDecidedFromWhatTheTurnDid(t *testing.T) {
	gates := []Gate{GateLevel2}
	out, records, _ := emitFixtures(t, testPolicy(t, gates, gates))
	zero, one := 0, 1
	want := []Label{
		{Type: LabelFetched, Search: 0, RequestID: "req_synthetic_0021", Candidate: &zero, Citation: &one, Confidence: ConfidenceHigh},
		{Type: LabelCited, Search: 1, RequestID: "req_synthetic_0022", Candidate: &zero, Citation: &zero, Confidence: ConfidenceHigh},
		// The third search repeats the second's query: not a reformulation.
		{Type: LabelReformulated, Search: 1, RequestID: "req_synthetic_0022", Confidence: ConfidenceMedium},
	}
	r := records[TraceHash(sessLabels)]
	if r.Level != 2 || !reflect.DeepEqual(r.Labels, want) {
		got, _ := json.Marshal(r.Labels)
		t.Fatalf("level %d labels = %s", r.Level, got)
	}
	// Level 2 is positions and ids. No page address, no prose.
	if strings.Contains(out, "example.test") || strings.Contains(out, contentMarker) {
		t.Fatalf("a level 2 record carries a URL or text:\n%s", out)
	}
	// The consented workspace is not the abandoned turn's, so its label was
	// derived and withheld; derive it directly.
	sessions := map[string]*Session{}
	if err := Walk(emitFixtureDir, Options{KeepContent: true}, func(s *Session) { sessions[s.SessionID] = s }); err != nil {
		t.Fatal(err)
	}
	abandoned := labelsOf(sessions[sessAbandoned].Turns[0])
	if len(abandoned) != 1 || abandoned[0].Type != LabelAbandoned || abandoned[0].Confidence != ConfidenceHigh {
		t.Fatalf("labels of the interrupted turn = %+v, want one session.abandoned, high", abandoned)
	}
	if got := records[TraceHash(sessAbandoned)]; got.Level != 1 || got.Labels != nil {
		t.Errorf("the unconsented workspace's record = level %d, labels %v; want 1 and none", got.Level, got.Labels)
	}
}

func TestLevelThreeNeedsLevelTwoAndDoesNotOpenIt(t *testing.T) {
	gates := []Gate{GateLevel3, GatePrompts, GateResponses}
	_, records, stats := emitFixtures(t, testPolicy(t, gates, gates))
	r := records[TraceHash(sessLabels)]
	if r.Level != 1 || r.Labels != nil {
		t.Fatalf("level_3 without level_2: Level = %d, Labels = %v; want 1 and none", r.Level, r.Labels)
	}
	for _, ev := range r.Events {
		if ev.Text != "" {
			t.Fatalf("level_3 without level_2 wrote text: %+v", ev)
		}
	}
	if stats.Withheld[GateLevel2] == 0 {
		t.Error("level_2 is what withheld the rest, and it was not named")
	}
}
