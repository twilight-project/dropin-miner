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

	for _, want := range []string{"--stdin", "-tier", "-format"} {
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
}

func TestUsageTextStatusSection(t *testing.T) {
	s := section(t, "\n  status ", "\nmanual enrollment")

	if !strings.Contains(s, "-json") {
		t.Errorf("status section: missing -json\n%s", s)
	}
}

func TestUsageTextDoctorSection(t *testing.T) {
	s := section(t, "\n  doctor ", "\n  earnings ")

	if !strings.Contains(s, "-json") {
		t.Errorf("doctor section: missing -json\n%s", s)
	}
}
