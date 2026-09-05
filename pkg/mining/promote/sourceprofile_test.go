package promote

// Which source profile a promoted observation carries.
//
// The AS resolves provider_event_id through the lookup registered for the
// profile named beside it. Naming the wrong one sends a search router's
// request id to be looked up as an OpenRouter generation id: it resolves to
// nothing, and "no such generation" is CONCLUSIVE on that path — the
// observation is REJECTED and never revisited. So a mislabel does not
// degrade, it destroys the participant's work silently.

import (
	"testing"
	"time"

	"github.com/twilight-project/dropin-miner/pkg/observe"
	"github.com/twilight-project/dropin-miner/pkg/wire"
)

func completed(profile observe.Profile, id string) *observe.Observation {
	return &observe.Observation{
		Provider:     "openrouter",
		Profile:      profile,
		GenerationID: id,
		StatusCode:   200,
		StartedAt:    time.Now().Add(-time.Second),
		FinishedAt:   time.Now(),
		Outcome:      observe.Complete(observe.TerminationDone),
	}
}

func TestPromotedRecordCarriesTheObservedProfile(t *testing.T) {
	for _, tc := range []struct {
		name    string
		profile observe.Profile
		want    string
	}{
		{"a chat completion", observe.ProfileChatCompletion, wire.SourceProfileOpenRouterV1},
		{"a search router response", observe.ProfileSearchRouter, wire.SourceProfileSearchRouterV1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec, err := Build(completed(tc.profile, "id-1"), "crid-1")
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			if rec.SourceProfile != tc.want {
				t.Fatalf("source_profile = %q, want %q — the AS would resolve this identifier "+
					"through the wrong provider's lookup, and a miss there is conclusive",
					rec.SourceProfile, tc.want)
			}
			if err := rec.Validate(); err != nil {
				t.Fatalf("the record the proxy would submit does not validate: %v", err)
			}
		})
	}
}

// The zero value is the shape the observer shipped with. An observation
// built by a caller that never set a profile must not silently become the
// other one.
func TestUnsetProfileIsOpenRouter(t *testing.T) {
	rec, err := Build(completed(0, "id-1"), "crid-1")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if rec.SourceProfile != wire.SourceProfileOpenRouterV1 {
		t.Fatalf("an unset profile promoted as %q", rec.SourceProfile)
	}
}
