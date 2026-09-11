package auth

// P-I6 delivery: the collector's Submitter, implemented here because it
// needs the capability and DPoP machinery. It obtains the capability
// for the record's own (slot, target_epoch) — never reuses another
// target's — and maps the AS's answer onto the spool's removal rule.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/twilight-project/dropin-miner/pkg/mining/spool"
)

const maxSubmissionAckBytes = 1 << 20

var (
	ErrAckBodyTooLarge           = errors.New("auth: acknowledgement body too large")
	ErrAckMalformedJSON          = errors.New("auth: malformed acknowledgement JSON")
	ErrAckTrailingData           = errors.New("auth: acknowledgement has trailing data")
	ErrAckClientRecordIDMismatch = errors.New("auth: acknowledgement client_record_id mismatch")
	ErrAckMissingClientRecordID  = errors.New("auth: acknowledgement missing client_record_id")
	ErrAckMissingObservationID   = errors.New("auth: acknowledgement missing observation_id")
	ErrAckUnexpectedStatus       = errors.New("auth: unexpected acknowledgement status")
)

// Submitter delivers spooled records to the AS.
type Submitter struct {
	mining *MiningClient
	caps   *CapabilityClient
}

func NewSubmitter(m *MiningClient, caps *CapabilityClient) *Submitter {
	return &Submitter{mining: m, caps: caps}
}

// submissionAck is the §58 acknowledgement.
type submissionAck struct {
	SubmissionStatus     string `json:"submission_status"`
	ObservationID        string `json:"observation_id"`
	ClientRecordID       string `json:"client_record_id"`
	ReconciliationStatus string `json:"reconciliation_status"`
	Reason               string `json:"reason"`
}

// classifySubmissionAck validates the identity binding before classifying an
// acknowledgement. DUPLICATE/EVIDENCE_ALREADY_CONSUMED is intentionally a
// future-compatible local classifier case: today's submission endpoint
// normally returns ACCEPTED/PENDING, and the client does not poll status.
func classifySubmissionAck(ack submissionAck, expectedID string) (bool, error) {
	if ack.ClientRecordID == "" {
		return false, ErrAckMissingClientRecordID
	}
	if ack.ClientRecordID != expectedID {
		return false, fmt.Errorf("%w: got %q, want %q", ErrAckClientRecordIDMismatch, ack.ClientRecordID, expectedID)
	}
	if ack.ObservationID == "" {
		return false, ErrAckMissingObservationID
	}
	if (ack.ReconciliationStatus == "DUPLICATE" && ack.Reason == "EVIDENCE_ALREADY_CONSUMED") ||
		ack.SubmissionStatus == "ACCEPTED" || ack.SubmissionStatus == "ALREADY_ACCEPTED" {
		return true, nil
	}
	return false, fmt.Errorf("%w: %q", ErrAckUnexpectedStatus, ack.SubmissionStatus)
}

func readSubmissionBody(body io.Reader) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(body, maxSubmissionAckBytes+1))
	if err != nil {
		return nil, fmt.Errorf("auth: read submission response: %w", err)
	}
	if len(raw) > maxSubmissionAckBytes {
		return nil, ErrAckBodyTooLarge
	}
	return raw, nil
}

func parseSubmissionAck(raw []byte) (submissionAck, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	var ack submissionAck
	if err := dec.Decode(&ack); err != nil {
		return submissionAck{}, fmt.Errorf("%w: %v", ErrAckMalformedJSON, err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return submissionAck{}, ErrAckTrailingData
		}
		return submissionAck{}, fmt.Errorf("%w: %v", ErrAckTrailingData, err)
	}
	return ack, nil
}

func readSubmissionAck(body io.Reader) (submissionAck, error) {
	raw, err := readSubmissionBody(body)
	if err != nil {
		return submissionAck{}, err
	}
	return parseSubmissionAck(raw)
}

// Submit delivers one record. It returns satisfied=true only for a validated
// ACCEPTED/ALREADY_ACCEPTED acknowledgement, or the synthetic future-
// compatible DUPLICATE/EVIDENCE_ALREADY_CONSUMED classifier case.
func (s *Submitter) Submit(ctx context.Context, rec *spool.Record) (bool, bool, time.Duration, error) {
	doc, err := s.mining.discoverer.Document(ctx)
	if err != nil {
		return false, false, 0, err
	}
	// Records are grouped by target, and each group is delivered under
	// the capability for ITS target (§63).
	cap, err := s.caps.Ensure(ctx, rec.TargetEpoch)
	if err != nil {
		return false, false, 0, err
	}
	if cap.SlotID != rec.SlotID {
		return false, true, 0, fmt.Errorf("auth: capability slot %d does not match record slot %d", cap.SlotID, rec.SlotID)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, doc.ObservationsEndpoint,
		bytes.NewReader(rec.Observation))
	if err != nil {
		return false, false, 0, fmt.Errorf("auth: build submission: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "DPoP "+cap.Capability)

	resp, err := newCredentialClient(s.mining.oauth.transport).Do(req)
	if err != nil {
		// Network failure: retryable, nothing is lost.
		return false, false, 0, fmt.Errorf("auth: submission transport failed")
	}
	defer resp.Body.Close()
	raw, err := readSubmissionBody(resp.Body)
	if err != nil {
		return false, false, 0, err
	}

	switch resp.StatusCode {
	case http.StatusCreated, http.StatusOK:
		ack, err := parseSubmissionAck(raw)
		if err != nil {
			return false, false, 0, err
		}
		satisfied, err := classifySubmissionAck(ack, rec.ClientRecordID)
		return satisfied, false, 0, err

	case http.StatusConflict:
		// §59: same transport identity, different evidence identity.
		// Retrying can never fix this — quarantine for inspection.
		return false, true, 0, fmt.Errorf("auth: evidence conflict: %s", refusalCode(raw))

	case http.StatusGone, http.StatusRequestEntityTooLarge,
		http.StatusBadRequest, http.StatusUnprocessableEntity:
		// Window closed, oversized, malformed, unsupported profile:
		// permanent for this record.
		return false, true, 0, fmt.Errorf("auth: permanent refusal: %s", refusalCode(raw))

	default:
		// 401/403/503 and anything else: retryable. 401 usually means
		// the capability expired mid-flight; the next attempt
		// re-exchanges.
		if resp.StatusCode == http.StatusUnauthorized {
			s.caps.Clear()
		}
		return false, false, retryAfter(resp), fmt.Errorf("auth: submission refused with status %d", resp.StatusCode)
	}
}

// refusalCode pulls the stable §26 machine code out of an error body.
func refusalCode(raw []byte) string {
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &env); err == nil && env.Error.Code != "" {
		return env.Error.Code
	}
	return "unknown"
}

// retryAfter honors a server-supplied Retry-After (§64).
func retryAfter(resp *http.Response) time.Duration {
	return retryAfterAt(resp.Header.Get("Retry-After"), time.Now())
}

func retryAfterAt(v string, now time.Time) time.Duration {
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	if when, err := http.ParseTime(v); err == nil {
		if d := when.Sub(now); d > 0 {
			return d
		}
	}
	return 0
}
