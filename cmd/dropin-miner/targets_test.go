package main

// Structural properties of the install-target registry itself, as opposed
// to what any one target's plan contains (agents_characterization_test.go)
// or what commit 2's refactor reproduces byte for byte. These hold
// regardless of how many targets exist or what they do, so a seventh
// target that gets any of them wrong fails here before it ever reaches a
// golden.

import (
	"strings"
	"testing"
)

func TestRegistryIDsAreUniqueNonEmptyAndStable(t *testing.T) {
	seen := map[string]bool{}
	for _, tg := range installTargets {
		id := tg.ID()
		if id == "" {
			t.Errorf("%s: empty id", tg.Label())
			continue
		}
		if id != strings.ToLower(id) {
			t.Errorf("%s: id %q is not lowercase", tg.Label(), id)
		}
		if strings.ContainsAny(id, " \t\n") {
			t.Errorf("%s: id %q contains whitespace", tg.Label(), id)
		}
		if seen[id] {
			t.Errorf("id %q is used by more than one target", id)
		}
		seen[id] = true
	}
}

func TestRegistryLabelsAreNonEmpty(t *testing.T) {
	for _, tg := range installTargets {
		if strings.TrimSpace(tg.Label()) == "" {
			t.Errorf("%s: empty label", tg.ID())
		}
	}
}

func TestRegistryKindsAreValid(t *testing.T) {
	for _, tg := range installTargets {
		switch tg.Kind() {
		case targetHost, targetIntegration:
		default:
			t.Errorf("%s: kind %q is neither host nor integration", tg.ID(), tg.Kind())
		}
	}
}

// TestRegistryOrderedSequenceIsExact is A.2's "order is part of the
// contract": the registry's declared order, checked directly against the
// installTargets slice (not a view), because targetsByKind and the help
// render both derive from this order and would otherwise hide a reorder
// here from every test that only ever looks at a view.
func TestRegistryOrderedSequenceIsExact(t *testing.T) {
	wantIDs := []string{"claude", "codex", "cursor", "opencode", "pi", "hermes"}
	wantLabels := []string{"Claude Code", "Codex", "Cursor", "opencode", "Pi", "Hermes"}
	if len(installTargets) != len(wantIDs) {
		t.Fatalf("installTargets has %d entries, want %d", len(installTargets), len(wantIDs))
	}
	for i, tg := range installTargets {
		if tg.ID() != wantIDs[i] || tg.Label() != wantLabels[i] {
			t.Fatalf("installTargets[%d] = {%q,%q}, want {%q,%q}", i, tg.ID(), tg.Label(), wantIDs[i], wantLabels[i])
		}
	}
}

// TestRegistryHasNoIntegrationsYet documents A.2's claim directly: nothing
// installs through the integration view in this PR, but the view exists
// for PR B's setup -with to select one by name the day one exists.
func TestRegistryHasNoIntegrationsYet(t *testing.T) {
	if got := targetsByKind(targetIntegration); len(got) != 0 {
		t.Errorf("targetsByKind(targetIntegration) = %v, want none yet", got)
	}
	if got := targetsByKind(targetHost); len(got) != len(installTargets) {
		t.Errorf("targetsByKind(targetHost) has %d targets, installTargets has %d; every target today is a host", len(got), len(installTargets))
	}
}

// TestPreferenceCapabilitySetIsExact is the one place the set of hosts
// that carry the search-default preference is pinned by name and order:
// opencode has no skill, so it must not satisfy preferenceTarget, and the
// day a target wrongly claims (or loses) the capability this goes red.
func TestPreferenceCapabilitySetIsExact(t *testing.T) {
	want := []string{"claude", "codex", "cursor", "pi", "hermes"}
	var got []string
	for _, tg := range installTargets {
		if _, ok := tg.(preferenceTarget); ok {
			got = append(got, tg.ID())
		}
	}
	if len(got) != len(want) {
		t.Fatalf("preferenceTarget implementers = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("preferenceTarget implementers = %v, want %v (in that order)", got, want)
		}
	}
}

func TestTargetsByIDsResolvesEveryKnownID(t *testing.T) {
	ids := make([]string, 0, len(installTargets))
	for _, tg := range installTargets {
		ids = append(ids, tg.ID())
	}
	resolved, err := targetsByIDs(ids)
	if err != nil {
		t.Fatalf("targetsByIDs(%v): %v", ids, err)
	}
	if len(resolved) != len(installTargets) {
		t.Fatalf("resolved %d targets, want %d", len(resolved), len(installTargets))
	}
	for i, tg := range resolved {
		if tg.ID() != ids[i] {
			t.Errorf("resolved[%d] = %q, want %q", i, tg.ID(), ids[i])
		}
	}
}

func TestTargetsByIDsRefusesAnUnknownIDNamingEveryKnownOne(t *testing.T) {
	_, err := targetsByIDs([]string{"nonesuch"})
	if err == nil {
		t.Fatal("targetsByIDs(nonesuch): want an error")
	}
	for _, tg := range installTargets {
		if !strings.Contains(err.Error(), tg.ID()) {
			t.Errorf("unknown-id error omits %q:\n%s", tg.ID(), err)
		}
	}
}

// TestHostTargetsByIDsAdmitsOnlyHostKind is A.2's frozen selection
// semantic for the agents command specifically: an integration id (there
// are none yet, so this is exercised against a synthetic scenario) must
// never resolve through the host-only resolver the -client flag uses. No
// integration is registered today, so a synthetic one is added to
// installTargets for the duration of this test and removed after: a
// resolver that searched the whole registry instead of
// targetsByKind(targetHost) would resolve it anyway, and this is the one
// place that distinction is observable before a real integration exists.
type fakeIntegrationTarget struct{}

func (fakeIntegrationTarget) ID() string                                              { return "fake-integration" }
func (fakeIntegrationTarget) Label() string                                           { return "Fake Integration" }
func (fakeIntegrationTarget) Kind() targetKind                                        { return targetIntegration }
func (fakeIntegrationTarget) Detect(agentOps, agentPaths, func(string) string) string { return "" }
func (fakeIntegrationTarget) PlanInstall(agentOps, agentPaths, binEntry, func(string) string, *agentPlan) {
}
func (fakeIntegrationTarget) PlanUninstall(agentOps, agentPaths, binEntry, func(string) string, *agentPlan) {
}
func (fakeIntegrationTarget) Status(agentOps, agentPaths, binEntry) targetStatus {
	return targetStatus{}
}

func TestHostTargetsByIDsAdmitsOnlyHostKind(t *testing.T) {
	hosts := targetsByKind(targetHost)
	ids := make([]string, 0, len(hosts))
	for _, tg := range hosts {
		ids = append(ids, tg.ID())
	}
	resolved, err := hostTargetsByIDs(ids)
	if err != nil {
		t.Fatalf("hostTargetsByIDs(%v): %v", ids, err)
	}
	if len(resolved) != len(hosts) {
		t.Fatalf("resolved %d targets, want %d", len(resolved), len(hosts))
	}
	_, err = hostTargetsByIDs([]string{"nonesuch"})
	if err == nil {
		t.Fatal("hostTargetsByIDs(nonesuch): want an error")
	}
	want := targetIDs(targetHost)
	if !strings.Contains(err.Error(), want) {
		t.Errorf("hostTargetsByIDs unknown-id error %q does not name the host id list %q", err, want)
	}

	saved := installTargets
	installTargets = append(append([]installTarget{}, installTargets...), fakeIntegrationTarget{})
	t.Cleanup(func() { installTargets = saved })
	if resolved, err := hostTargetsByIDs([]string{"fake-integration"}); err == nil {
		t.Fatalf("hostTargetsByIDs admitted an integration-kind id: %v", resolved)
	}
	// targetsByIDs (any kind, PR B's setup -with) is the resolver that
	// should admit it, proving the two are not the same function wearing
	// two names.
	if _, err := targetsByIDs([]string{"fake-integration"}); err != nil {
		t.Fatalf("targetsByIDs should admit any kind, including the synthetic integration: %v", err)
	}
}

// idTokens splits a targetIDs/allTargetIDs-style ", "-joined list into its
// exact entries, so membership can be checked token by token rather than
// by substring — "pi" must not be satisfied by an id that merely contains
// "pi" somewhere inside it.
func idTokens(list string) map[string]bool {
	out := map[string]bool{}
	if list == "" {
		return out
	}
	for _, id := range strings.Split(list, ", ") {
		out[id] = true
	}
	return out
}

// TestTargetIDsListsOnlyTheRequestedKind branches on each target's own
// Kind(), not on a literal id: a host's id must be an exact token of
// targetIDs(targetHost), and a non-host's id must be absent from it.
func TestTargetIDsListsOnlyTheRequestedKind(t *testing.T) {
	hostIDs := idTokens(targetIDs(targetHost))
	for _, tg := range installTargets {
		switch tg.Kind() {
		case targetHost:
			if !hostIDs[tg.ID()] {
				t.Errorf("targetIDs(targetHost) omits host %q: %v", tg.ID(), hostIDs)
			}
		default:
			if hostIDs[tg.ID()] {
				t.Errorf("targetIDs(targetHost) wrongly includes non-host %q: %v", tg.ID(), hostIDs)
			}
		}
	}
	if got := targetIDs(targetIntegration); got != "" {
		t.Errorf("targetIDs(targetIntegration) = %q, want empty (no integration exists yet)", got)
	}
}

func TestAllTargetIDsListsEveryRegisteredID(t *testing.T) {
	all := allTargetIDs()
	for _, tg := range installTargets {
		if !strings.Contains(all, tg.ID()) {
			t.Errorf("allTargetIDs omits %q: %s", tg.ID(), all)
		}
	}
}
