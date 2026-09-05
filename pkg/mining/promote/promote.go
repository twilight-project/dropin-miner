// Package promote turns a terminal proxy observation into a
// ProviderObservationV1 record (contract §54 promotion rule, §48–§57
// envelope). It is pure data mapping: no I/O, no network, no clock
// beyond what the observation already carries.
//
// The privacy line is absolute here — an Observation contains only
// response-side metadata, and this package copies a fixed set of those
// fields. There is no path by which prompt or completion content could
// enter a record.
package promote

import (
	"errors"
	"strconv"

	"github.com/twilight-project/dropin-miner/pkg/observe"
	"github.com/twilight-project/dropin-miner/pkg/wire"
)

// ErrNotPromotable means the observation is not structurally eligible
// (§54). It is an ordinary outcome, not a failure: most observations of
// a healthy proxy are promotable, and the rest are simply dropped.
var ErrNotPromotable = errors.New("promote: observation is not structurally eligible")

// Eligible reports the §54 rule for OPENROUTER_V1:
//
//	Outcome == COMPLETE
//	AND provider_event_id (generation id) is non-empty
//
// Deliberately NOT required: saw_done, saw_usage, saw_finish_reason,
// streaming, or termination == done. An eof_without_done or
// client_disconnect observation with a usable generation id is still
// submitted — whether it qualifies economically is the AS's call.
func Eligible(obs *observe.Observation) bool {
	if obs == nil || obs.GenerationID == "" {
		return false
	}
	_, complete := obs.Outcome.Termination()
	return complete
}

// Build maps an eligible observation into the wire record. clientRecordID
// must already be minted and stable across retries (§49).
func Build(obs *observe.Observation, clientRecordID string) (*wire.ProviderObservationV1, error) {
	if !Eligible(obs) {
		return nil, ErrNotPromotable
	}
	if clientRecordID == "" {
		return nil, errors.New("promote: client_record_id is required")
	}
	termination, _ := obs.Outcome.Termination()

	rec := &wire.ProviderObservationV1{
		Version:         wire.ObservationVersion,
		ClientRecordID:  clientRecordID,
		SourceProfile:   sourceProfileFor(obs.Profile),
		ProviderEventID: obs.GenerationID,
		ResolvedModel:   obs.ResolvedModel,
		Streaming:       obs.Streaming,
		StatusCode:      obs.StatusCode,
		FinishReason:    obs.FinishReason,
		Outcome: wire.OutcomeV1{
			Type:        wire.OutcomeComplete,
			Termination: string(termination),
		},
		SawDone:          obs.SawDone,
		SawUsage:         obs.SawUsage,
		SawFinishReason:  obs.SawFinishReason,
		ParseDegraded:    obs.ParseDegraded,
		MetadataConflict: obs.MetadataConflict,
	}
	if !obs.StartedAt.IsZero() {
		rec.StartedAt = obs.StartedAt.UTC().Format("2006-01-02T15:04:05.000Z")
	}
	if !obs.FinishedAt.IsZero() {
		rec.FinishedAt = obs.FinishedAt.UTC().Format("2006-01-02T15:04:05.000Z")
	}
	// §52: provider-reported values only, as decimal strings. Absence of
	// a usage field is not an error, and nothing is estimated locally.
	if obs.Usage != nil {
		rec.Usage = &wire.UsageV1{
			PromptTokens:     strconv.FormatUint(obs.Usage.PromptTokens, 10),
			CompletionTokens: strconv.FormatUint(obs.Usage.CompletionTokens, 10),
			TotalTokens:      strconv.FormatUint(obs.Usage.TotalTokens, 10),
		}
	}
	// §56: each profile's own closed allowlist, response-side only.
	//
	// provider_name is on OPENROUTER_V1's list and on no other. Under
	// SEARCH_ROUTER_V1 the permitted set is EMPTY — §35.1 specifies a
	// verification model and says nothing about proxy-side source data —
	// so the object is omitted entirely. Sending it anyway is not a
	// tolerated extra: the AS rejects the observation, so a field nobody
	// asked for would have cost the participant the whole record.
	if rec.SourceProfile == wire.SourceProfileOpenRouterV1 && obs.Provider != "" {
		rec.SourceData = map[string]any{"provider_name": obs.Provider}
	}
	if err := rec.Validate(); err != nil {
		return nil, err
	}
	return rec, nil
}

// sourceProfileFor names the wire source profile for an observed response
// shape.
//
// The mapping is by ROUTE, decided upstream in internal/forward from the
// provider route table — /v1/search is a search-router exchange by
// definition. It is deliberately not inferred from the response body: a
// payload shape is evidence about a payload, and source_profile is a claim
// about which provider's vocabulary produced the identifier the AS will
// resolve. Guessing that wrong sends a request id to be looked up as a
// generation id, which resolves to nothing and rejects honest work.
//
// The zero value is ProfileChatCompletion, so an observation built without a
// profile set reads as OPENROUTER_V1 — the shape the observer shipped with,
// and the one every existing caller means.
func sourceProfileFor(p observe.Profile) string {
	if p == observe.ProfileSearchRouter {
		return wire.SourceProfileSearchRouterV1
	}
	return wire.SourceProfileOpenRouterV1
}
