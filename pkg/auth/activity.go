package auth

// The epoch activity endpoint (contract §70).
//
// This is the only AS answer to "am I actually earning", and it is the
// half of that question the AS owns: whether qualifying verified evidence
// exists for this installation in this epoch. It is NOT a payout promise
// and must never be reported as one — §70 says so in as many words, and
// the chain can still distribute nothing.
//
// Under POC-1 trusted eligibility the threshold is one: a participant is
// eligible when verified_observation_count >= 1 (payout/allocation spec
// §23, and twilight-minis' eligibility engine asks it exactly that way).
// More verified observations do not earn more — the epoch's budget is an
// equal split among eligible participants — so nothing here may present
// the count as a quantity of earnings.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/twilight-project/dropin-miner/pkg/wire"
)

// MinVerifiedObservations is the POC-1 trusted-eligibility threshold: one
// qualifying VERIFIED observation in the target epoch. It is a threshold,
// never a weight.
const MinVerifiedObservations = 1

// EpochActivity is the §70 document.
type EpochActivity struct {
	SlotID      string
	TargetEpoch string
	// VerifiedActivity is the AS's own verdict. It is read in preference
	// to recomputing one from the count, because the AS owns the policy
	// and a client that derived its own would drift from it silently.
	VerifiedActivity         bool
	VerifiedObservationCount uint64
	PendingObservationCount  uint64
	RejectedObservationCount uint64
}

// Eligible reports whether this installation clears the POC-1 activity
// threshold for the epoch. Both halves must agree: an AS that says
// verified_activity=false while reporting a verified observation, or the
// reverse, is reported as not eligible rather than guessed at — telling a
// participant they are earning when the AS's own verdict says otherwise is
// the failure worth being conservative about.
func (a *EpochActivity) Eligible() bool {
	return a.VerifiedActivity && a.VerifiedObservationCount >= MinVerifiedObservations
}

// ErrNoActivityEndpoint reports that this AS advertises no
// activity_status_endpoint_template.
//
// §19 makes the field mandatory today, so a document without one does not
// validate and never reaches here — this is the guard for the day that
// changes, which is not hypothetical: ESC-030 made
// current_target_endpoint_template OPTIONAL in the same section, and the
// client that had assembled that URL itself would have sent a DPoP access
// token at a path the AS never published.
var ErrNoActivityEndpoint = errors.New("auth: this AS advertises no activity_status_endpoint_template")

// EpochActivity fetches §70 for one target epoch.
//
// Authorization is the ordinary DPoP access token, like Status: asking
// what has been credited cannot require a Participation Capability.
func (m *MiningClient) EpochActivity(ctx context.Context, targetEpoch uint64) (*EpochActivity, error) {
	doc, err := m.discoverer.Document(ctx)
	if err != nil {
		return nil, err
	}
	if doc.ActivityStatusEndpointTemplate == "" {
		return nil, ErrNoActivityEndpoint
	}
	endpoint := expandTemplate(doc.ActivityStatusEndpointTemplate, m.discoverer.cfg.SlotID, targetEpoch)
	resp, err := m.authedRequest(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("auth: activity request: %w", err)
	}
	defer drainAndClose(resp.Body)
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("auth: read activity response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, joinRefusal(resp.StatusCode, raw)
	}
	// The counts are decimal STRINGS on the wire (§70), for the same
	// reason every other 64-bit identifier in this contract is: no JSON
	// consumer may round one through a float. decimalU64 tolerates a bare
	// number too, so an AS that emits one does not strand the caller.
	var body struct {
		SlotID           string     `json:"slot_id"`
		TargetEpoch      string     `json:"target_epoch"`
		VerifiedActivity bool       `json:"verified_activity"`
		Verified         decimalU64 `json:"verified_observation_count"`
		Pending          decimalU64 `json:"pending_observation_count"`
		Rejected         decimalU64 `json:"rejected_observation_count"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, fmt.Errorf("auth: parse activity: %w", err)
	}
	return &EpochActivity{
		SlotID:                   body.SlotID,
		TargetEpoch:              body.TargetEpoch,
		VerifiedActivity:         body.VerifiedActivity,
		VerifiedObservationCount: uint64(body.Verified),
		PendingObservationCount:  uint64(body.Pending),
		RejectedObservationCount: uint64(body.Rejected),
	}, nil
}

// ServiceDocument exposes the validated §19 document.
//
// It exists so a caller can ask "is the AS reachable and does it still
// name my slot" without an access token and without building a second
// Discoverer against the same base URL — which would double the fetches
// and, worse, could answer differently from the one the rest of the
// commands use.
func (m *MiningClient) ServiceDocument(ctx context.Context) (*wire.DiscoveryDocument, error) {
	return m.discoverer.Document(ctx)
}
