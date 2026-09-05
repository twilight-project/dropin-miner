package wire

// L3 fixture conformance (ADR-X-0001): the corpus under
// testdata/fixtures/wire is byte-identical in both repositories; each
// side must parse, validate, and round-trip every fixture. SHA256SUMS
// pins the corpus — the test recomputes every hash and demands full
// coverage in both directions.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const fixtureDir = "../../testdata/fixtures/wire"

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(fixtureDir, name)) // #nosec G304 -- names are test constants under testdata
	if err != nil {
		t.Fatalf("fixture %s: %v", name, err)
	}
	return data
}

// decodeStrict unmarshals with unknown fields forbidden: a fixture field
// our model silently drops would be exactly the divergence L3 exists to
// catch.
func decodeStrict(t *testing.T, data []byte, v any) {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		t.Fatalf("strict decode: %v", err)
	}
}

// roundTrip asserts our struct's serialization is semantically identical
// to the fixture bytes (same fields, same values — formatting aside).
func roundTrip(t *testing.T, fixture []byte, v any) {
	t.Helper()
	ours, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	// Compare via canonical re-marshal (encoding/json sorts map keys),
	// since reflect is banned repo-wide.
	if !bytes.Equal(canonicalJSON(t, fixture), canonicalJSON(t, ours)) {
		t.Fatalf("round-trip diverged from fixture:\n fixture: %s\n ours:    %s", fixture, ours)
	}
}

func canonicalJSON(t *testing.T, data []byte) []byte {
	t.Helper()
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestObservationFixturesConform(t *testing.T) {
	for _, name := range []string{
		"provider_observation_openrouter_complete.json",
		"provider_observation_openrouter_minimal.json",
	} {
		t.Run(name, func(t *testing.T) {
			data := readFixture(t, name)
			var obs ProviderObservationV1
			decodeStrict(t, data, &obs)
			if err := obs.Validate(); err != nil {
				t.Fatalf("fixture does not validate: %v", err)
			}
			roundTrip(t, data, &obs)
		})
	}
}

func TestAckAndErrorFixturesConform(t *testing.T) {
	for _, name := range []string{"submission_ack_accepted.json", "submission_ack_already_accepted.json"} {
		data := readFixture(t, name)
		var ack SubmissionAck
		decodeStrict(t, data, &ack)
		if !ack.Satisfied() {
			t.Fatalf("%s: durable ack must satisfy the delivery obligation", name)
		}
		roundTrip(t, data, &ack)
	}
	data := readFixture(t, "error_envelope_enrollment_closed.json")
	var env ErrorEnvelope
	decodeStrict(t, data, &env)
	if env.Error.Code != "ENROLLMENT_CLOSED" || env.Error.Message == "" {
		t.Fatalf("error envelope malformed: %+v", env)
	}
	roundTrip(t, data, &env)
}

func TestDiscoveryFixtureConforms(t *testing.T) {
	data := readFixture(t, "discovery_document.json")
	var doc DiscoveryDocument
	decodeStrict(t, data, &doc)
	if err := doc.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := doc.CheckIdentity("twilight-1", 7); err != nil {
		t.Fatal(err)
	}
	if err := doc.CheckIdentity("twilight-2", 7); err == nil {
		t.Fatal("chain mismatch accepted; §19 requires fail-closed")
	}
	if err := doc.CheckIdentity("twilight-1", 8); err == nil {
		t.Fatal("slot mismatch accepted; §19 requires fail-closed")
	}
	roundTrip(t, data, &doc)
}

// SHA256SUMS must cover exactly the fixture set, and every hash must
// match (ADR-X-0001: drift between repos is the bug class L3 catches).
func TestFixtureCorpusChecksums(t *testing.T) {
	sums, err := os.ReadFile(filepath.Join(fixtureDir, "SHA256SUMS"))
	if err != nil {
		t.Fatalf("SHA256SUMS missing: %v", err)
	}
	pinned := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(sums)), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			t.Fatalf("malformed SHA256SUMS line: %q", line)
		}
		pinned[strings.TrimPrefix(fields[1], "*")] = fields[0]
	}
	entries, err := os.ReadDir(fixtureDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() == "SHA256SUMS" {
			continue
		}
		want, ok := pinned[e.Name()]
		if !ok {
			t.Errorf("fixture %s not pinned in SHA256SUMS", e.Name())
			continue
		}
		sum := sha256.Sum256(readFixture(t, e.Name()))
		if got := hex.EncodeToString(sum[:]); got != want {
			t.Errorf("fixture %s checksum mismatch:\n  got  %s\n  want %s", e.Name(), got, want)
		}
		delete(pinned, e.Name())
	}
	for name := range pinned {
		t.Errorf("SHA256SUMS pins %s but the file is absent", name)
	}
}

func validObservation() *ProviderObservationV1 {
	return &ProviderObservationV1{
		Version:         ObservationVersion,
		ClientRecordID:  "019c7a8e-1b2d-7c3e-8f40-5a6b7c8d9e0f",
		SourceProfile:   SourceProfileOpenRouterV1,
		ProviderEventID: "gen-x",
		Streaming:       true,
		Outcome:         OutcomeV1{Type: OutcomeComplete, Termination: "done"},
	}
}

func TestObservationValidationRejects(t *testing.T) {
	cases := map[string]func(*ProviderObservationV1){
		"wrong version":            func(o *ProviderObservationV1) { o.Version = "provider-observation-v2" },
		"empty client_record_id":   func(o *ProviderObservationV1) { o.ClientRecordID = "" },
		"unknown source_profile":   func(o *ProviderObservationV1) { o.SourceProfile = "OPENAI_V1" },
		"missing generation id":    func(o *ProviderObservationV1) { o.ProviderEventID = "" },
		"bad termination":          func(o *ProviderObservationV1) { o.Outcome.Termination = "aborted" },
		"outcome cross-fields":     func(o *ProviderObservationV1) { o.Outcome.Reason = "observer_overrun" },
		"abandoned bad reason":     func(o *ProviderObservationV1) { o.Outcome = OutcomeV1{Type: OutcomeAbandoned, Reason: "bored"} },
		"bad outcome type":         func(o *ProviderObservationV1) { o.Outcome.Type = "PARTIAL" },
		"non-decimal counter":      func(o *ProviderObservationV1) { o.Usage = &UsageV1{PromptTokens: "12a"} },
		"negative-ish counter":     func(o *ProviderObservationV1) { o.Usage = &UsageV1{TotalTokens: "-5"} },
		"leading-zero counter":     func(o *ProviderObservationV1) { o.Usage = &UsageV1{TotalTokens: "007"} },
		"scientific cost":          func(o *ProviderObservationV1) { o.Usage = &UsageV1{Cost: "1.2e-05"} },
		"bare-dot cost":            func(o *ProviderObservationV1) { o.Usage = &UsageV1{Cost: "1."} },
		"bad timestamp":            func(o *ProviderObservationV1) { o.StartedAt = "yesterday" },
		"status out of range":      func(o *ProviderObservationV1) { o.StatusCode = 42 },
		"request_parameters":       func(o *ProviderObservationV1) { o.RequestParameters = map[string]any{"temperature": 0.7} },
		"source_data off-list key": func(o *ProviderObservationV1) { o.SourceData = map[string]any{"prompt": "hi"} },
		"source_data non-scalar":   func(o *ProviderObservationV1) { o.SourceData = map[string]any{"provider_name": []any{"x"}} },
		"source_data oversize value": func(o *ProviderObservationV1) {
			o.SourceData = map[string]any{"provider_name": strings.Repeat("a", MaxSourceDataValueBytes+1)}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			obs := validObservation()
			mutate(obs)
			if err := obs.Validate(); err == nil {
				t.Fatalf("mutation %q passed validation", name)
			}
		})
	}
	if err := validObservation().Validate(); err != nil {
		t.Fatalf("baseline observation invalid: %v", err)
	}
}

func TestDecimalForms(t *testing.T) {
	for s, want := range map[string]bool{
		"0": true, "7": true, "252020": true, "0.50443": true, "10.0": true,
		"": false, "01": false, ".5": false, "5.": false, "1.2e3": false,
		"+1": false, "-1": false, "0x10": false, "1_000": false,
	} {
		if got := isCanonicalDecimal(s); got != want {
			t.Errorf("isCanonicalDecimal(%q) = %v, want %v", s, got, want)
		}
	}
}

func ExampleSubmissionAck_Satisfied() {
	ack := SubmissionAck{SubmissionStatus: SubmissionAlreadyAccepted}
	fmt.Println(ack.Satisfied())
	// Output: true
}
