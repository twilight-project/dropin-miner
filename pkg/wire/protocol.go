package wire

import (
	"fmt"
	"strconv"
)

// Submission statuses (§58–§59).
const (
	SubmissionAccepted        = "ACCEPTED"
	SubmissionAlreadyAccepted = "ALREADY_ACCEPTED"
)

// Reconciliation statuses (§58, Part XI).
const (
	ReconciliationPending   = "PENDING"
	ReconciliationVerified  = "VERIFIED"
	ReconciliationDuplicate = "DUPLICATE"
	ReconciliationRejected  = "REJECTED"
)

// SubmissionAck is the durable acknowledgement (§58). ACCEPTED and
// ALREADY_ACCEPTED are the only statuses that release a spooled record
// (PX-15); everything else means retain-and-retry.
type SubmissionAck struct {
	SubmissionStatus     string `json:"submission_status"`
	ObservationID        string `json:"observation_id"`
	ClientRecordID       string `json:"client_record_id"`
	ReconciliationStatus string `json:"reconciliation_status"`
}

// Satisfied reports whether this acknowledgement discharges the delivery
// obligation for the record (§58 spool rule).
func (a *SubmissionAck) Satisfied() bool {
	return a.SubmissionStatus == SubmissionAccepted || a.SubmissionStatus == SubmissionAlreadyAccepted
}

// ErrorEnvelope is the stable error shape every AS error uses (§26):
//
//	{"error": {"code": "ENROLLMENT_CLOSED", "message": "..."}}
type ErrorEnvelope struct {
	Error ErrorBody `json:"error"`
}

type ErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// DiscoveryVersion is the frozen service-metadata version (§19).
const DiscoveryVersion = "twilight-mining-as-v1"

// DiscoveryDocument is /.well-known/twilight-mining (§19). OAuth
// endpoints are deliberately absent — they come from OAuth Authorization
// Server Metadata, never duplicated here.
type DiscoveryDocument struct {
	Version string `json:"version"`
	ChainID string `json:"chain_id"`
	SlotID  string `json:"slot_id"`

	AuthorizationServer string `json:"authorization_server"`

	JoinEpochEndpointTemplate     string `json:"join_epoch_endpoint_template"`
	EpochStatusEndpointTemplate   string `json:"epoch_status_endpoint_template"`
	CandidateListEndpointTemplate string `json:"candidate_list_endpoint_template"`

	// CurrentTargetEndpointTemplate is the "which target is open now"
	// endpoint. OPTIONAL, and therefore not in Validate: an AS that
	// predates it simply omits the key, and a client must read that
	// absence as "this AS cannot tell me" rather than assembling the
	// URL itself — an endpoint the AS did not advertise is not an
	// endpoint. omitempty keeps a document that lacked it round-tripping
	// byte-identically (the L3 fixture corpus is shared with the AS).
	CurrentTargetEndpointTemplate string `json:"current_target_endpoint_template,omitempty"`

	ParticipationResource string `json:"participation_resource"`

	ObservationsEndpoint              string `json:"observations_endpoint"`
	ObservationsBatchEndpoint         string `json:"observations_batch_endpoint"`
	ObservationStatusEndpointTemplate string `json:"observation_status_endpoint_template"`
	ActivityStatusEndpointTemplate    string `json:"activity_status_endpoint_template"`

	ProviderVerificationEndpointTemplate string `json:"provider_verification_endpoint_template"`

	SourceProfiles []string `json:"source_profiles"`

	MaxObservationBytes      int64 `json:"max_observation_bytes"`
	MaxObservationBatchSize  int64 `json:"max_observation_batch_size"`
	MaxObservationBatchBytes int64 `json:"max_observation_batch_bytes"`
}

// Validate checks structural well-formedness of the document itself.
// Identity verification against the proxy's configured chain/slot is
// CheckIdentity — a separate, mandatory step (§19).
func (d *DiscoveryDocument) Validate() error {
	if d.Version != DiscoveryVersion {
		return fmt.Errorf("wire: discovery version must be %q, got %q", DiscoveryVersion, d.Version)
	}
	if d.ChainID == "" {
		return fmt.Errorf("wire: discovery chain_id empty")
	}
	if _, err := strconv.ParseUint(d.SlotID, 10, 64); err != nil {
		return fmt.Errorf("wire: discovery slot_id %q is not an unsigned decimal", d.SlotID)
	}
	for name, v := range map[string]string{
		"authorization_server":                    d.AuthorizationServer,
		"join_epoch_endpoint_template":            d.JoinEpochEndpointTemplate,
		"epoch_status_endpoint_template":          d.EpochStatusEndpointTemplate,
		"candidate_list_endpoint_template":        d.CandidateListEndpointTemplate,
		"participation_resource":                  d.ParticipationResource,
		"observations_endpoint":                   d.ObservationsEndpoint,
		"observations_batch_endpoint":             d.ObservationsBatchEndpoint,
		"observation_status_endpoint_template":    d.ObservationStatusEndpointTemplate,
		"activity_status_endpoint_template":       d.ActivityStatusEndpointTemplate,
		"provider_verification_endpoint_template": d.ProviderVerificationEndpointTemplate,
	} {
		if v == "" {
			return fmt.Errorf("wire: discovery field %s empty", name)
		}
	}
	if len(d.SourceProfiles) == 0 {
		return fmt.Errorf("wire: discovery advertises no source_profiles")
	}
	if d.MaxObservationBytes <= 0 || d.MaxObservationBatchSize <= 0 || d.MaxObservationBatchBytes <= 0 {
		return fmt.Errorf("wire: discovery limits must be positive advertised values")
	}
	return nil
}

// CheckIdentity enforces the §19 pre-use rule: the discovered document
// must match the proxy's configured chain and the Core Slot it mines
// through. A mismatch fails the mining plane closed — and must never
// interrupt provider inference (the caller owns that separation).
func (d *DiscoveryDocument) CheckIdentity(chainID string, slotID uint64) error {
	if d.ChainID != chainID {
		return fmt.Errorf("wire: discovered chain_id %q does not match configured %q", d.ChainID, chainID)
	}
	got, err := strconv.ParseUint(d.SlotID, 10, 64)
	if err != nil {
		return fmt.Errorf("wire: discovery slot_id %q is not an unsigned decimal", d.SlotID)
	}
	if got != slotID {
		return fmt.Errorf("wire: discovered slot_id %d does not match configured %d", got, slotID)
	}
	return nil
}
