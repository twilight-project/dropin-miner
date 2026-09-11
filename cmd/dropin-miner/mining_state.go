package main

import (
	"errors"
	"fmt"
	"io/fs"
	"strings"

	"github.com/twilight-project/dropin-miner/pkg/auth"
	"github.com/twilight-project/dropin-miner/pkg/config"
)

// miningASConfigured is the presence rule for AS work. The scripted mining
// answer in config.Mining.Enabled is a first-decision input only; it is not a
// second runtime authority and it is not the test for whether an AS exists.
func miningASConfigured(m config.Mining) bool { return m.ASBaseURL != "" }

// miningActive is retained as a convenience for callers that truly need only
// ON/OFF. All safety-sensitive paths first read the typed decision and treat
// undecided and degraded as inactive.
func miningActive(store *auth.Store) bool {
	return store.ReadMiningDecision().State == auth.MiningEnabled
}

func miningDecisionText(d auth.MiningDecision) string {
	switch d.State {
	case auth.MiningEnabled:
		return "ON"
	case auth.MiningDisabled:
		return "OFF"
	case auth.MiningUndecided:
		return "NOT DECIDED"
	case auth.MiningDegraded:
		return "DEGRADED"
	default:
		return strings.ToUpper(string(d.State))
	}
}

func miningDecisionDetail(d auth.MiningDecision) string {
	if d.Err == nil {
		return ""
	}
	return fmt.Sprintf(" (%s)", d.Err)
}

func inspectMiningState(dir string) (auth.MiningDecision, *auth.Store) {
	store, err := auth.OpenStoreExisting(dir)
	if err == nil {
		return store.ReadMiningDecision(), store
	}
	if errors.Is(err, fs.ErrNotExist) {
		return auth.MiningDecision{State: auth.MiningUndecided}, nil
	}
	return auth.MiningDecision{State: auth.MiningDegraded, Present: true, Err: err}, nil
}
