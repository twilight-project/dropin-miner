// Package wire holds the proxy↔AS data models frozen by the V1
// integration contract: ProviderObservationV1 (§48–§57), the submission
// acknowledgement (§58–§59), the stable error envelope (§26), and the
// Twilight service-metadata document (§19).
//
// The package is deliberately stdlib-only and imports nothing else from
// this repository (contract Appendix A): it is a data model, not a
// client. Fixtures under testdata/fixtures/wire are the shared L3
// conformance corpus (ADR-X-0001) — both repositories parse identical
// bytes.
package wire

import (
	"encoding/json"
	"fmt"
	"time"
)

// ObservationVersion is the frozen envelope version (§48).
const ObservationVersion = "provider-observation-v1"

// The normative V1 source profiles (§35, contract V1_9).
const (
	SourceProfileOpenRouterV1   = "OPENROUTER_V1"
	SourceProfileSearchRouterV1 = "SEARCH_ROUTER_V1"
)

// sourceProfile carries the envelope rules §35 lets a profile define for
// itself. It mirrors the AS's table field for field on purpose: a proxy that
// validated by a different rule than the AS would either submit what the AS
// rejects, or refuse to submit what the AS would have taken.
type sourceProfile struct {
	name string
	// sourceData is the closed §56 allowlist. Empty permits no key, which is
	// a rule rather than a gap — see sourceDataAllowlistSearchRouterV1.
	sourceData map[string]bool
	// eventIDField names the evidence handle in the profile's own
	// vocabulary (§55 generation_id, §35.1 request_id), so a refusal says
	// which identifier was missing in the terms the submitter knows.
	eventIDField string
}

// sourceDataAllowlistSearchRouterV1 is EMPTY on purpose rather than by
// omission. §56 permits only fields the applicable profile explicitly
// allows; §35.1 specifies a verification model and says nothing about
// proxy-side source data, so the permitted set is empty and the object is
// omitted. Letting it inherit OpenRouter's list would submit provider_name
// and native_finish_reason describing a generation this profile never
// produced.
var sourceDataAllowlistSearchRouterV1 = map[string]bool{}

var sourceProfiles = map[string]sourceProfile{
	SourceProfileOpenRouterV1: {
		name:         SourceProfileOpenRouterV1,
		sourceData:   sourceDataAllowlistOpenRouterV1,
		eventIDField: "generation_id",
	},
	SourceProfileSearchRouterV1: {
		name:         SourceProfileSearchRouterV1,
		sourceData:   sourceDataAllowlistSearchRouterV1,
		eventIDField: "request_id",
	},
}

// Outcome types (§53).
const (
	OutcomeComplete  = "COMPLETE"
	OutcomeAbandoned = "ABANDONED"
)

// Bounds frozen by §51 and §56. The AS rejects over-limit objects rather
// than truncating; the proxy therefore never builds one.
const (
	MaxRequestParameterKeys  = 16
	MaxRequestParameterBytes = 512
	MaxSourceDataKeys        = 8
	MaxSourceDataValueBytes  = 128
	MaxSourceDataBytes       = 1024
)

// terminations is the closed COMPLETE set (§53.1); abandonReasons the
// closed ABANDONED set (§53.2). Both inherited from the proxy
// observation model — internal/observe carries the same literals.
var terminations = map[string]bool{
	"done":                 true,
	"eof_without_done":     true,
	"client_disconnect":    true,
	"upstream_error_event": true,
}

var abandonReasons = map[string]bool{
	"observer_overrun":   true,
	"body_too_large":     true,
	"encoded_body":       true,
	"max_depth_exceeded": true,
	"malformed_body":     true,
}

// sourceDataAllowlistOpenRouterV1 is the closed §56 allowlist.
var sourceDataAllowlistOpenRouterV1 = map[string]bool{
	"provider_name":        true,
	"native_finish_reason": true,
}

// UsageV1 carries provider-reported values only (§52): integer counters
// as unsigned decimal strings, cost as a decimal string never computed
// locally. Absent fields are omitted, never zero-filled.
type UsageV1 struct {
	PromptTokens     string `json:"prompt_tokens,omitempty"`
	CompletionTokens string `json:"completion_tokens,omitempty"`
	TotalTokens      string `json:"total_tokens,omitempty"`
	CachedTokens     string `json:"cached_tokens,omitempty"`
	ReasoningTokens  string `json:"reasoning_tokens,omitempty"`
	Cost             string `json:"cost,omitempty"`
}

// OutcomeV1 is the two-shape outcome (§53): COMPLETE carries a
// termination, ABANDONED carries a reason, never both.
type OutcomeV1 struct {
	Type        string `json:"type"`
	Termination string `json:"termination,omitempty"`
	Reason      string `json:"reason,omitempty"`
}

// ProviderObservationV1 is the §48/§50 envelope. The saw_*/degradation
// booleans always serialize (the example emits false explicitly);
// optional extension fields are omitted when unset.
type ProviderObservationV1 struct {
	Version        string `json:"version"`
	ClientRecordID string `json:"client_record_id"`

	SourceProfile   string `json:"source_profile"`
	ProviderEventID string `json:"provider_event_id,omitempty"`

	RequestedModel    string         `json:"requested_model,omitempty"`
	ResolvedModel     string         `json:"resolved_model,omitempty"`
	RequestParameters map[string]any `json:"request_parameters,omitempty"`

	StartedAt  string `json:"started_at,omitempty"`
	FinishedAt string `json:"finished_at,omitempty"`

	Streaming  bool `json:"streaming"`
	StatusCode int  `json:"status_code,omitempty"`

	Usage        *UsageV1 `json:"usage,omitempty"`
	FinishReason string   `json:"finish_reason,omitempty"`

	Outcome OutcomeV1 `json:"outcome"`

	SawDone          bool `json:"saw_done"`
	SawUsage         bool `json:"saw_usage"`
	SawFinishReason  bool `json:"saw_finish_reason"`
	ParseDegraded    bool `json:"parse_degraded"`
	MetadataConflict bool `json:"metadata_conflict"`

	SourceData map[string]any `json:"source_data,omitempty"`
}

// Validate enforces the frozen envelope rules for a record the proxy is
// about to submit. It rejects rather than repairs — an invalid record is
// a build defect, never something to patch on the wire.
func (o *ProviderObservationV1) Validate() error {
	if o.Version != ObservationVersion {
		return fmt.Errorf("wire: version must be %q, got %q", ObservationVersion, o.Version)
	}
	if o.ClientRecordID == "" || len(o.ClientRecordID) > 128 {
		return fmt.Errorf("wire: client_record_id must be 1..128 bytes")
	}
	prof, known := sourceProfiles[o.SourceProfile]
	if !known {
		return fmt.Errorf("wire: unknown source_profile %q (V1 defines %s and %s)",
			o.SourceProfile, SourceProfileOpenRouterV1, SourceProfileSearchRouterV1)
	}
	// §54/§55 and §35.1: only promoted observations are submitted, and
	// promotion requires the profile's own evidence identifier — a
	// generation id under OPENROUTER_V1, a request id under
	// SEARCH_ROUTER_V1. The field is one wire field; which identifier
	// belongs in it is the profile's to say.
	if o.ProviderEventID == "" || len(o.ProviderEventID) > 256 {
		return fmt.Errorf("wire: provider_event_id must be 1..256 bytes for %s (its %s)",
			prof.name, prof.eventIDField)
	}
	if err := o.Outcome.validate(); err != nil {
		return err
	}
	for name, ts := range map[string]string{"started_at": o.StartedAt, "finished_at": o.FinishedAt} {
		if ts == "" {
			continue
		}
		if _, err := time.Parse(time.RFC3339Nano, ts); err != nil {
			return fmt.Errorf("wire: %s is not RFC 3339: %w", name, err)
		}
	}
	if o.StatusCode != 0 && (o.StatusCode < 100 || o.StatusCode > 599) {
		return fmt.Errorf("wire: status_code %d out of range", o.StatusCode)
	}
	if o.Usage != nil {
		if err := o.Usage.validate(); err != nil {
			return err
		}
	}
	// §51: V1 defines no request_parameters allowlist for OPENROUTER_V1,
	// and every key off the allowlist must be omitted — so the object
	// must be absent entirely.
	if len(o.RequestParameters) > 0 {
		return fmt.Errorf("wire: request_parameters must be omitted for %s (no V1 allowlist)", prof.name)
	}
	return o.validateSourceData(prof)
}

func (out OutcomeV1) validate() error {
	switch out.Type {
	case OutcomeComplete:
		if !terminations[out.Termination] {
			return fmt.Errorf("wire: COMPLETE outcome has invalid termination %q", out.Termination)
		}
		if out.Reason != "" {
			return fmt.Errorf("wire: COMPLETE outcome must not carry a reason")
		}
	case OutcomeAbandoned:
		if !abandonReasons[out.Reason] {
			return fmt.Errorf("wire: ABANDONED outcome has invalid reason %q", out.Reason)
		}
		if out.Termination != "" {
			return fmt.Errorf("wire: ABANDONED outcome must not carry a termination")
		}
	default:
		return fmt.Errorf("wire: outcome type %q is not COMPLETE or ABANDONED", out.Type)
	}
	return nil
}

func (u *UsageV1) validate() error {
	for name, v := range map[string]string{
		"prompt_tokens":     u.PromptTokens,
		"completion_tokens": u.CompletionTokens,
		"total_tokens":      u.TotalTokens,
		"cached_tokens":     u.CachedTokens,
		"reasoning_tokens":  u.ReasoningTokens,
	} {
		if v != "" && !isCanonicalUnsignedDecimal(v) {
			return fmt.Errorf("wire: usage.%s %q is not an unsigned decimal string", name, v)
		}
	}
	if u.Cost != "" && !isCanonicalDecimal(u.Cost) {
		return fmt.Errorf("wire: usage.cost %q is not a decimal string", u.Cost)
	}
	return nil
}

func (o *ProviderObservationV1) validateSourceData(prof sourceProfile) error {
	if o.SourceData == nil {
		return nil
	}
	if len(o.SourceData) > MaxSourceDataKeys {
		return fmt.Errorf("wire: source_data has %d keys, max %d", len(o.SourceData), MaxSourceDataKeys)
	}
	for k, v := range o.SourceData {
		if !prof.sourceData[k] {
			return fmt.Errorf("wire: source_data key %q not on the %s allowlist", k, prof.name)
		}
		switch val := v.(type) {
		case string:
			if len(val) > MaxSourceDataValueBytes {
				return fmt.Errorf("wire: source_data.%s exceeds %d UTF-8 bytes", k, MaxSourceDataValueBytes)
			}
		case float64, bool, nil:
			// scalar; permitted (§56)
		default:
			return fmt.Errorf("wire: source_data.%s is not a scalar", k)
		}
	}
	raw, err := json.Marshal(o.SourceData)
	if err != nil {
		return fmt.Errorf("wire: source_data does not serialize: %w", err)
	}
	if len(raw) > MaxSourceDataBytes {
		return fmt.Errorf("wire: source_data serializes to %d bytes, max %d", len(raw), MaxSourceDataBytes)
	}
	return nil
}

// isCanonicalUnsignedDecimal accepts "0" or a digit string without a
// leading zero — the only forms the proxy's own encoder emits.
func isCanonicalUnsignedDecimal(s string) bool {
	if s == "" || (len(s) > 1 && s[0] == '0') {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// isCanonicalDecimal accepts an unsigned decimal with an optional
// fractional part ("0.50443"). Scientific notation is never emitted on
// the wire — the scanner renders provider cost values to plain decimal.
func isCanonicalDecimal(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == '.' {
			return isCanonicalUnsignedDecimal(s[:i]) && allDigits(s[i+1:])
		}
	}
	return isCanonicalUnsignedDecimal(s)
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
