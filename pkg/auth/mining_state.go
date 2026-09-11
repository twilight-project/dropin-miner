package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
)

const miningDecisionVersion = 1

// MiningDecisionState is the complete runtime decision state. In particular,
// undecided and degraded are not aliases for disabled: the former is a normal
// first-run state, while the latter says the local authority could not be
// trusted and must be repaired explicitly.
type MiningDecisionState string

const (
	MiningEnabled   MiningDecisionState = "enabled"
	MiningDisabled  MiningDecisionState = "disabled"
	MiningUndecided MiningDecisionState = "undecided"
	MiningDegraded  MiningDecisionState = "degraded"
)

// MiningDecision is the canonical read of mining_decision.json.
//
// Present distinguishes a real persisted decision from an absent file. Err
// is populated only for degraded state and is intentionally retained for the
// caller's diagnostic; Enabled is never true for degraded or undecided.
type MiningDecision struct {
	State   MiningDecisionState
	Enabled bool
	Present bool
	Err     error
}

func (d MiningDecision) IsEnabled() bool { return d.State == MiningEnabled }

// ReadMiningDecision reads the persisted runtime authority without applying
// a configuration fallback. A missing state file is undecided; malformed,
// unsupported, unsafe, or unreadable state is degraded.
func (s *Store) ReadMiningDecision() MiningDecision {
	raw, err := s.readSecret("mining_decision.json")
	if errors.Is(err, fs.ErrNotExist) {
		return MiningDecision{State: MiningUndecided}
	}
	if err != nil {
		return MiningDecision{State: MiningDegraded, Present: true, Err: err}
	}

	var rec struct {
		Version *int  `json:"version"`
		Enabled *bool `json:"enabled"`
	}
	if err := json.Unmarshal(raw, &rec); err != nil {
		return MiningDecision{State: MiningDegraded, Present: true,
			Err: fmt.Errorf("auth: decode mining decision: %w", err)}
	}
	version := miningDecisionVersion
	if rec.Version != nil {
		version = *rec.Version
	}
	if version != miningDecisionVersion {
		return MiningDecision{State: MiningDegraded, Present: true,
			Err: fmt.Errorf("auth: unsupported mining decision version %d", version)}
	}
	if rec.Enabled == nil {
		return MiningDecision{State: MiningDegraded, Present: true,
			Err: errors.New("auth: mining decision has no enabled value")}
	}
	if *rec.Enabled {
		return MiningDecision{State: MiningEnabled, Enabled: true, Present: true}
	}
	return MiningDecision{State: MiningDisabled, Present: true}
}

// ReadMiningDecisionAt inspects a state location without creating it. A
// missing directory is an untouched installation and therefore undecided;
// every other failure to inspect the location is degraded.
func ReadMiningDecisionAt(dir string) MiningDecision {
	store, err := OpenStoreExisting(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return MiningDecision{State: MiningUndecided}
		}
		return MiningDecision{State: MiningDegraded, Present: true, Err: err}
	}
	return store.ReadMiningDecision()
}
