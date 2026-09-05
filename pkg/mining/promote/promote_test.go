package promote

// §54's promotion rule is profile-independent by design: a non-empty
// generation id and a COMPLETE outcome, whatever response shape produced
// them. These cases pin that for the search-router profile, whose
// observations differ from a completion's in every field the rule does NOT
// look at — no usage, no finish reason, a provider name where a model slug
// would be.

import (
	"testing"
	"time"

	"github.com/twilight-project/dropin-miner/pkg/observe"
	"github.com/twilight-project/dropin-miner/pkg/wire"
)

// searchObservation mirrors what internal/observe's search profile emits for
// a healthy POST /v1/search: identity from request_id, the chosen
// candidate's provider as the resolved model, and no token usage at all.
func searchObservation() *observe.Observation {
	started := time.Date(2026, 8, 22, 10, 0, 0, 0, time.UTC)
	return &observe.Observation{
		Provider:      "openrouter",
		GenerationID:  "f47ac10b-58cc-4372-a567-0e02b2c3d479",
		ResolvedModel: "synthetic-search-b",
		StatusCode:    200,
		StartedAt:     started,
		FinishedAt:    started.Add(610 * time.Millisecond),
		Streaming:     false,
		Outcome:       observe.Complete(observe.TerminationDone),
		SawDone:       true,
	}
}

func TestSearchObservationIsPromotable(t *testing.T) {
	obs := searchObservation()
	if !Eligible(obs) {
		t.Fatal("a complete, identified search observation must be eligible")
	}

	rec, err := Build(obs, "crid-search-1")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if rec.ProviderEventID != obs.GenerationID || rec.ResolvedModel != "synthetic-search-b" {
		t.Errorf("record identity: %+v", rec)
	}
	if rec.Outcome.Type != wire.OutcomeComplete || rec.Outcome.Termination != "done" {
		t.Errorf("outcome: %+v", rec.Outcome)
	}
	if rec.Streaming {
		t.Error("a JSON search response is not streaming")
	}
	// No token counters were reported, so the record carries no usage
	// object at all — never a zero-filled one, which would read as
	// "the provider reported zero" (§52).
	if rec.Usage != nil {
		t.Errorf("usage must be omitted, not zero-filled: %+v", rec.Usage)
	}
	if err := rec.Validate(); err != nil {
		t.Errorf("record does not satisfy the frozen envelope: %v", err)
	}
}

// The two ways a search observation fails the rule, kept explicit because
// the search profile can produce both: an abandoned body, and a response
// with no usable request id.
func TestSearchObservationNotPromotable(t *testing.T) {
	abandoned := searchObservation()
	abandoned.Outcome = observe.Abandoned(observe.AbandonMalformedBody)
	if Eligible(abandoned) {
		t.Error("an abandoned observation must never be promoted")
	}
	if _, err := Build(abandoned, "crid-search-2"); err != ErrNotPromotable {
		t.Errorf("Build error: %v", err)
	}

	unidentified := searchObservation()
	unidentified.GenerationID = ""
	if Eligible(unidentified) {
		t.Error("an observation with no generation id must never be promoted")
	}
}

// A search that chose no candidate is still a real, promotable observation:
// resolved_model is simply omitted. Nothing invents a provider.
func TestSearchObservationWithoutChosenCandidate(t *testing.T) {
	obs := searchObservation()
	obs.ResolvedModel = ""
	rec, err := Build(obs, "crid-search-3")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if rec.ResolvedModel != "" {
		t.Errorf("resolved_model: %q", rec.ResolvedModel)
	}
	if err := rec.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}
