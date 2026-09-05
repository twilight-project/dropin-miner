package auth

// P-I6 delivery: the collector's Submitter, implemented here because it
// needs the capability and DPoP machinery. It obtains the capability
// for the record's own (slot, target_epoch) — never reuses another
// target's — and maps the AS's answer onto the spool's removal rule.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/twilight-project/dropin-miner/pkg/mining/spool"
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
}

// Submit delivers one record. It returns satisfied=true only for
// ACCEPTED / ALREADY_ACCEPTED — the only answers that may remove the
// durable copy (§58).
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

	resp, err := (&http.Client{Transport: s.mining.oauth.transport, Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		// Network failure: retryable, nothing is lost.
		return false, false, 0, fmt.Errorf("auth: submission transport failed")
	}
	defer drainAndClose(resp.Body)
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return false, false, 0, fmt.Errorf("auth: read submission response: %w", err)
	}

	switch resp.StatusCode {
	case http.StatusCreated, http.StatusOK:
		var ack submissionAck
		if err := json.Unmarshal(raw, &ack); err != nil {
			return false, false, 0, fmt.Errorf("auth: parse acknowledgement: %w", err)
		}
		if ack.SubmissionStatus != "ACCEPTED" && ack.SubmissionStatus != "ALREADY_ACCEPTED" {
			return false, false, 0, fmt.Errorf("auth: unexpected submission status %q", ack.SubmissionStatus)
		}
		return true, false, 0, nil

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
	v := resp.Header.Get("Retry-After")
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	if when, err := http.ParseTime(v); err == nil {
		if d := time.Until(when); d > 0 {
			return d
		}
	}
	return 0
}
