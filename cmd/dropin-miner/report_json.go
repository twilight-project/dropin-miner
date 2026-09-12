package main

// JSON for the three report commands: doctor, status and connect.
//
// Presentation only. None of these changes a state machine, adds a network
// call, or decides anything the text report did not already decide — each
// is a second renderer over the same gathered facts. That is the rule that
// matters here: an SDK must not have to scrape prose, and a renderer must
// not have to parse its own prose back into fields, because then every
// wording change is a silent data change.

import (
	"strings"

	"github.com/twilight-project/dropin-miner/pkg/auth"
	"github.com/twilight-project/dropin-miner/pkg/redact"
)

// ── doctor ──────────────────────────────────────────────────────────────

type doctorReport struct {
	Mining           doctorMiningJSON  `json:"mining"`
	ASConfigured     *bool             `json:"as_configured,omitempty"`
	ASBaseURL        string            `json:"as_base_url,omitempty"`
	Health           []machineHealth   `json:"health,omitempty"`
	HealthError      string            `json:"health_error,omitempty"`
	Checks           []doctorCheckJSON `json:"checks"`
	ChecksCannotRun  []string          `json:"checks_that_could_not_run"`
	Queue            queueState        `json:"queue"`
	RegistrationSlot string            `json:"last_enrollment_slot,omitempty"`
	RegistrationAt   string            `json:"last_enrollment_at,omitempty"`
}

type doctorMiningJSON struct {
	// State is the persisted runtime decision in its canonical spelling,
	// not a rendering of it: enabled, disabled, undecided, degraded.
	State  string `json:"state"`
	Detail string `json:"detail,omitempty"`
	// StateKnown is false when the state directory could not be read at
	// all. An unknown decision is not a decision of "off".
	StateKnown bool `json:"state_known"`
}

type doctorCheckJSON struct {
	Name string `json:"name"`
	// Verdict is OK, NO or UNKNOWN. UNKNOWN is the absence of a fact and
	// is deliberately not a polite spelling of NO — a check that could not
	// run must never read as a check that answered no.
	Verdict string `json:"verdict"`
	Detail  string `json:"detail,omitempty"`
	Fix     string `json:"fix,omitempty"`
}

func doctorEnvelope(f doctorFacts, checks []doctorCheck, exitCode int) commandEnvelope {
	report := doctorReport{
		Mining: doctorMiningJSON{
			State:      string(f.MiningDecision.State),
			StateKnown: f.LocalStateKnown,
		},
		ASBaseURL: f.ASBaseURL,
		Health:    healthOf(f.Health),
		Queue:     spoolQueueState(f.SpoolDir),
		Checks:    make([]doctorCheckJSON, 0, len(checks)),
		// Never nil: an empty list is "every check ran", which is a
		// different statement from "this field was omitted".
		ChecksCannotRun:  []string{},
		RegistrationSlot: f.RegistrationSlot,
		RegistrationAt:   f.RegistrationAt,
	}
	if f.MiningDecision.Err != nil {
		report.Mining.Detail = boundMessage(f.MiningDecision.Err.Error())
	}
	if f.ASConfigKnown {
		configured := f.ASConfigured
		report.ASConfigured = &configured
	}
	if f.HealthErr != nil {
		report.HealthError = boundMessage(redact.Error(f.HealthErr).Error())
	}
	for _, c := range checks {
		report.Checks = append(report.Checks, doctorCheckJSON{
			Name:    c.Name,
			Verdict: string(c.Verdict),
			Detail:  c.Detail,
			Fix:     c.Fix,
		})
		if c.Verdict == verdictUnknown {
			report.ChecksCannotRun = append(report.ChecksCannotRun, c.Name)
		}
	}

	code, retryable, action := "ok", false, actionNone
	if exitCode != exitOK {
		// The exit status reports whether the DIAGNOSIS succeeded, not
		// whether the news is good — NO is a successful run. Only a report
		// that could not be produced at all is a failure.
		code, retryable, action = "diagnosis_unavailable", true, actionRetry
	}
	return commandEnvelope{
		machineHeader: newMachineHeader("doctor", exitCode, code, retryable, action),
		Data:          report,
	}
}

// ── status ──────────────────────────────────────────────────────────────

type statusReport struct {
	Mining     statusMiningJSON       `json:"mining"`
	Agent      *statusAgentJSON       `json:"agent,omitempty"`
	AgentErr   string                 `json:"agent_error,omitempty"`
	PayoutHold *statusPayoutHoldJSON  `json:"payout_hold,omitempty"`
	Conflicts  []auth.ConflictedEpoch `json:"epoch_conflicts,omitempty"`
	AS         *statusASJSON          `json:"authorization_server,omitempty"`
}

type statusMiningJSON struct {
	State                 string          `json:"state"`
	Detail                string          `json:"detail,omitempty"`
	ASConfigured          bool            `json:"as_configured"`
	StateDirectoryPresent bool            `json:"state_directory_present"`
	Health                []machineHealth `json:"health,omitempty"`
	HealthError           string          `json:"health_error,omitempty"`
}

// statusAgentJSON is the stored registration. Every field in it is already
// printed by the text report or shown to the participant at claim time;
// the agent API key, the refresh authorization, the DPoP key and any
// wallet material are held elsewhere in the store and none of them is read
// here.
type statusAgentJSON struct {
	AgentID        string   `json:"agent_id,omitempty"`
	Status         string   `json:"status"`
	Claimed        bool     `json:"claimed"`
	Scopes         []string `json:"scopes,omitempty"`
	ClaimURL       string   `json:"claim_url,omitempty"`
	ClaimCode      string   `json:"claim_code,omitempty"`
	ClaimExpiresAt string   `json:"claim_expires_at,omitempty"`
	EnrolledSlot   string   `json:"enrolled_slot,omitempty"`
	EnrolledAt     string   `json:"enrolled_at,omitempty"`
	PayoutAddress  string   `json:"payout_address,omitempty"`
	SlotRefusal    string   `json:"slot_refusal,omitempty"`
}

type statusPayoutHoldJSON struct {
	Reason string `json:"reason"`
	Active string `json:"active"`
	Local  string `json:"local"`
}

type statusASJSON struct {
	SetupError  string             `json:"setup_error,omitempty"`
	BaseURL     string             `json:"base_url,omitempty"`
	ChainID     string             `json:"chain_id,omitempty"`
	SlotID      uint64             `json:"slot_id,omitempty"`
	Epoch       *uint64            `json:"epoch,omitempty"`
	EpochPinned bool               `json:"epoch_pinned"`
	EpochError  string             `json:"epoch_error,omitempty"`
	Status      *statusEpochJSON   `json:"epoch_status,omitempty"`
	StatusError string             `json:"epoch_status_error,omitempty"`
	Provider    *statusBindingJSON `json:"provider,omitempty"`
	ProviderErr string             `json:"provider_error,omitempty"`
	Queue       *queueState        `json:"queue,omitempty"`
}

type statusEpochJSON struct {
	Phase               string `json:"phase"`
	DistributionMode    string `json:"distribution_mode"`
	Joinable            bool   `json:"joinable"`
	JoinStatus          string `json:"join_status"`
	ParticipationStatus string `json:"participation_status"`
	CapabilityAvailable bool   `json:"capability_available"`
}

type statusBindingJSON struct {
	Provider       string `json:"provider"`
	Status         string `json:"status"`
	SourceProfile  string `json:"source_profile"`
	KeyFingerprint string `json:"key_fingerprint"`
	LastVerifiedAt string `json:"last_verified_at,omitempty"`
}

func statusEnvelope(f agentIdentityFacts, as *statusASFacts) commandEnvelope {
	report := statusReport{
		Mining: statusMiningJSON{
			State:                 string(f.Decision.State),
			ASConfigured:          miningASConfigured(f.Mining),
			StateDirectoryPresent: !f.StoreMissing,
			Health:                healthOf(f.Health),
		},
		Conflicts: f.Conflicts,
	}
	if f.Decision.Err != nil {
		report.Mining.Detail = boundMessage(f.Decision.Err.Error())
	}
	if f.HealthErr != nil {
		report.Mining.HealthError = boundMessage(redact.Error(f.HealthErr).Error())
	}
	if f.RegistrationErr != nil {
		report.AgentErr = boundMessage(f.RegistrationErr.Error())
	} else if f.HasRegistration {
		reg := f.Registration
		agent := &statusAgentJSON{
			AgentID:        reg.AgentID,
			Status:         reg.Status,
			Claimed:        reg.Status == "claimed",
			Scopes:         reg.Scopes,
			ClaimURL:       reg.ClaimURL,
			ClaimCode:      reg.ClaimCode,
			ClaimExpiresAt: reg.ClaimExpiresAt,
			EnrolledSlot:   reg.LastEnrollmentSlot,
			EnrolledAt:     reg.LastEnrollmentAt,
			SlotRefusal:    reg.SlotRefusal,
		}
		if f.PayoutAddressErr == nil && f.HasPayoutAddress {
			agent.PayoutAddress = f.PayoutAddress
		}
		report.Agent = agent
	}
	if f.HasHeld {
		reason := f.Held.HeldFor
		if reason == "" {
			reason = "REPLACES_ACTIVE" // this client's own pre-check, not an AS-returned reason
		}
		report.PayoutHold = &statusPayoutHoldJSON{Reason: reason, Active: f.Held.Active, Local: f.Held.Local}
	}
	if as != nil {
		report.AS = statusASJSONOf(*as)
	}
	return commandEnvelope{
		machineHeader: newMachineHeader("status", exitOK, "ok", false, actionNone),
		Data:          report,
	}
}

func statusASJSONOf(f statusASFacts) *statusASJSON {
	out := &statusASJSON{EpochPinned: f.EpochPinned}
	if f.ClientErr != nil {
		out.SetupError = boundMessage(redact.Error(f.ClientErr).Error())
		return out
	}
	out.BaseURL, out.ChainID, out.SlotID = f.Mining.ASBaseURL, f.Mining.ChainID, f.Mining.SlotID
	if !f.EpochKnown {
		if f.EpochErr != nil {
			out.EpochError = boundMessage(redact.Error(f.EpochErr).Error())
		}
		return out
	}
	epoch := f.Epoch
	out.Epoch = &epoch
	if f.StatusErr != nil {
		out.StatusError = boundMessage(redact.Error(f.StatusErr).Error())
		return out
	}
	if st := f.Status; st != nil {
		out.Status = &statusEpochJSON{
			Phase:               st.Phase,
			DistributionMode:    st.DistributionMode,
			Joinable:            st.Joinable,
			JoinStatus:          st.JoinStatus,
			ParticipationStatus: st.ParticipationStatus,
			CapabilityAvailable: st.CapabilityAvailable,
		}
	}
	if f.BindingErr != nil {
		out.ProviderErr = boundMessage(redact.Error(f.BindingErr).Error())
	} else if b := f.Binding; b != nil {
		out.Provider = &statusBindingJSON{
			Provider:       b.Provider,
			Status:         b.Status,
			SourceProfile:  b.SourceProfile,
			KeyFingerprint: b.KeyFingerprint,
			LastVerifiedAt: b.LastSuccessfulVerificationAt,
		}
	}
	queue := f.Queue
	out.Queue = &queue
	return out
}

// ── connect ─────────────────────────────────────────────────────────────

type connectReport struct {
	AgentID        string   `json:"agent_id,omitempty"`
	Status         string   `json:"status,omitempty"`
	Claimed        bool     `json:"claimed"`
	Scopes         []string `json:"scopes,omitempty"`
	ClaimURL       string   `json:"claim_url,omitempty"`
	ClaimCode      string   `json:"claim_code,omitempty"`
	ClaimExpiresAt string   `json:"claim_expires_at,omitempty"`
	EnrolledSlot   string   `json:"enrolled_slot,omitempty"`
	SlotRefusal    string   `json:"slot_refusal,omitempty"`
	// MiningDecision is the persisted runtime decision this connect left
	// behind, so a caller can tell "registered, mining on" from
	// "registered, mining never decided here" without a second command.
	MiningDecision string `json:"mining_decision,omitempty"`
}

// connectEnvelope reads back what the run persisted rather than narrating
// what it did. The store is the authority on the registration — that is
// the whole reason connect writes it before printing anything — so an
// envelope built from it says what is true now, including after a run that
// failed partway.
//
// Nothing here reads or exposes the agent API key, the refresh
// authorization, the DPoP key or any wallet material, and nothing opens
// the claim URL: it is printed for a person, exactly as the text path
// prints it.
func connectEnvelope(cfgPath string, getenv func(string) string, exitCode int, narration string) commandEnvelope {
	report := connectReport{}
	cfg, _, err := loadConfig(cfgPath, getenv)
	if err == nil {
		if store, serr := auth.OpenStoreExisting(cfg.Mining.StateDir); serr == nil {
			report.MiningDecision = string(store.ReadMiningDecision().State)
			if reg, ok, rerr := store.LoadAgentRegistration(); rerr == nil && ok {
				report.AgentID = reg.AgentID
				report.Status = reg.Status
				report.Claimed = reg.Status == "claimed"
				report.Scopes = reg.Scopes
				report.ClaimURL = reg.ClaimURL
				report.ClaimCode = reg.ClaimCode
				report.ClaimExpiresAt = reg.ClaimExpiresAt
				report.EnrolledSlot = reg.LastEnrollmentSlot
				report.SlotRefusal = reg.SlotRefusal
			}
		}
	}

	code, retryable, action := connectOutcome(exitCode, report.Status)
	env := commandEnvelope{
		machineHeader: newMachineHeader("connect", exitCode, code, retryable, action),
		Data:          report,
	}
	if exitCode != exitOK {
		// The run's own diagnostics, bounded and redacted. Diagnostic
		// only: the header above is what a caller branches on.
		if msg := strings.TrimSpace(narration); msg != "" {
			env.Error = &machineError{Message: boundMessage(msg), Source: "client"}
		}
	}
	return env
}

func connectOutcome(exitCode int, status string) (code string, retryable bool, action string) {
	switch {
	case exitCode == exitTransport:
		return "connect_failed", true, actionRetry
	case exitCode != exitOK:
		return "connect_failed", false, actionReport
	case status == "claimed":
		return "ok", false, actionNone
	case status == "expired":
		// A new registration is needed, and only a person can start one.
		return "registration_expired", false, actionConnect
	case status == "unclaimed":
		// The registration exists and search already works; what is
		// outstanding is a human visiting the claim URL. Still action
		// connect: that action names the registration/claim workflow, not
		// the absence of a registration.
		return "unclaimed", false, actionConnect
	default:
		return "ok", false, actionNone
	}
}
