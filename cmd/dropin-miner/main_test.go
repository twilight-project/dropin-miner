package main

import (
	"strings"
	"testing"
)

// section extracts the text between two markers in usageText, in order,
// so a reshuffle of the help text cannot pass a check by matching a
// stray occurrence of the marker string somewhere else in the constant.
func section(t *testing.T, from, to string) string {
	t.Helper()
	start := strings.Index(usageText, from)
	if start == -1 {
		t.Fatalf("usageText: start marker %q not found", from)
	}
	end := strings.Index(usageText[start:], to)
	if end == -1 {
		t.Fatalf("usageText: end marker %q not found after %q", to, from)
	}
	return usageText[start : start+end]
}

func TestUsageTextSearchSection(t *testing.T) {
	s := section(t, "\n  search ", "\n  agents ")

	for _, want := range []string{"--stdin", "-tier", "-format", "valid search response", "invalid server response"} {
		if !strings.Contains(s, want) {
			t.Errorf("search section: missing %q\n%s", want, s)
		}
	}

	if !strings.Contains(s, "-timeout") {
		t.Errorf("search section: missing -timeout\n%s", s)
	}
	if !strings.Contains(s, defaultSearchTimeout.String()) {
		t.Errorf("search section: does not advertise the real default timeout %q\n%s",
			defaultSearchTimeout.String(), s)
	}
}

func TestUsageTextAgentsSection(t *testing.T) {
	s := section(t, "\n  agents ", "\n  hook ")

	for _, host := range []string{"claude", "codex", "cursor", "opencode", "pi", "hermes"} {
		if !strings.Contains(s, host) {
			t.Errorf("agents section: missing host %q\n%s", host, s)
		}
	}
}

func TestUsageTextConnectSection(t *testing.T) {
	s := section(t, "\n  connect ", "\n  mining enable ")

	if !strings.Contains(s, "-json") {
		t.Errorf("connect section: missing -json\n%s", s)
	}
	if !strings.Contains(s, "rather than prompting") {
		t.Errorf("connect section: missing rather than prompting\n%s", s)
	}
}

func TestUsageTextStatusSection(t *testing.T) {
	s := section(t, "\n  status ", "\nmanual enrollment")

	if !strings.Contains(s, "-json") {
		t.Errorf("status section: missing -json\n%s", s)
	}
}

// TestUsageTextManualEnrollmentSectionNamesProvider: `provider` is a real
// dispatched command, and `join` ends by naming it as the next step on a
// Slot that accepts OPENROUTER_V1 (enroll.go's cmdJoin). A command the
// binary tells you to run, and that the binary's own help does not mention,
// is the 0.2.6 help-surface finding in miniature — so the manual-enrollment
// section, where it belongs, is asserted to name it.
func TestUsageTextManualEnrollmentSectionNamesProvider(t *testing.T) {
	s := section(t, "\nmanual enrollment", "\nis it working")

	if !strings.Contains(s, "\n  provider ") {
		t.Errorf("manual enrollment section: no `provider` entry; cmdProvider is dispatched and cmdJoin points at it\n%s", s)
	}
	// Scoped, not unconditional: under the default SEARCH_ROUTER_V1 profile
	// the participant holds no provider credential at all, and help that
	// implied otherwise would send that participant into a refusal.
	if !strings.Contains(s, "OPENROUTER_V1") {
		t.Errorf("manual enrollment section: `provider` is not scoped to OPENROUTER_V1\n%s", s)
	}
}

func TestUsageTextDoctorSection(t *testing.T) {
	s := section(t, "\n  doctor ", "\n  earnings ")

	if !strings.Contains(s, "-json") {
		t.Errorf("doctor section: missing -json\n%s", s)
	}

	// Matched against the section with its whitespace collapsed: the help is
	// hand-wrapped, so a phrase can straddle a line break, and a reflow is
	// not the drift this guards against. The wording is the assertion.
	flat := strings.Join(strings.Fields(s), " ")

	// The probe's actual properties, pinned so the help cannot drift back to
	// the unconditional "writes and removes one probe file" it used to make.
	// That wording was wrong twice over: the probe is skipped entirely
	// unless [miner] is enabled AND the persisted decision says mining is on
	// (gatherDoctorFactsFor), and removal is attempted, not guaranteed — a
	// cleanup failure is itself a probe failure, reported by pathname
	// (probeIntakeWritable's Leftover). Help that promises a successful
	// removal describes a contract doctor does not offer.
	for _, want := range []struct{ phrase, why string }{
		{"when that check is active", "the probe is conditional, not unconditional"},
		{"bounded probe operation", "it is a bounded operation, not a single write"},
		{"at most one", "at most one probe file, never a guaranteed count"},
		{"non-.json", "the probe name is what keeps it out of the mining pipeline"},
		{"cleanup is attempted", "removal is attempted, never promised"},
		{"leftover is reported", "a cleanup failure is reported, not swallowed"},
	} {
		if !strings.Contains(flat, want.phrase) {
			t.Errorf("doctor section: missing %q — %s\n%s", want.phrase, want.why, s)
		}
	}

	// The superseded claim, asserted absent rather than merely unasserted:
	// a guard that only checks for the new words stays green if the old
	// sentence is restored alongside them.
	if strings.Contains(flat, "writes and removes") {
		t.Errorf("doctor section: still claims the probe \"writes and removes\" a file; "+
			"removal is attempted, and the probe does not run at all unless the check is active\n%s", s)
	}
}
