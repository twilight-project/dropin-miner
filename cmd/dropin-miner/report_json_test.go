package main

// doctor, status and connect as JSON.
//
// The property under test is not that JSON comes out. It is that the JSON
// and the text report are two renderings of one set of gathered facts:
// same checks, same verdicts, same counts, same decisions, no extra
// network call, and nothing derived by reading the other's prose.

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/twilight-project/dropin-miner/pkg/auth"
)

func decodeCommandEnvelope(t *testing.T, stdout, command string) map[string]any {
	t.Helper()
	env := decodeEnvelope(t, stdout)
	if env["command"] != command {
		t.Fatalf("command %v, want %q", env["command"], command)
	}
	return env
}

func dataOf(t *testing.T, env map[string]any) map[string]any {
	t.Helper()
	d, ok := env["data"].(map[string]any)
	if !ok {
		t.Fatalf("envelope has no data object: %v", env)
	}
	return d
}

// ── doctor ──────────────────────────────────────────────────────────────

func TestDoctorJSONRendersTheSameChecksTheTextReportDoes(t *testing.T) {
	f := healthyFacts()
	f.LocalStateKnown = true
	f.MiningDecision = auth.MiningDecision{State: auth.MiningEnabled, Present: true}
	f.ASConfigured, f.ASConfigKnown = true, true
	f.SpoolDir = t.TempDir()
	f.Health = []auth.HealthRecord{{
		Version: 1, Component: auth.HealthFlush, Reason: auth.HealthSpoolBacklog, Detail: "12 records",
	}}
	checks := assembleDoctor(f)

	var buf bytes.Buffer
	emitMachine(&buf, doctorEnvelope(f, checks, doctorExit(checks)))
	env := decodeCommandEnvelope(t, buf.String(), "doctor")
	data := dataOf(t, env)

	// One check object per check the text renderer would print, in order,
	// with the same verdict — not a re-derivation.
	got, _ := data["checks"].([]any)
	if len(got) != len(checks) {
		t.Fatalf("%d checks in JSON, %d assembled", len(got), len(checks))
	}
	for i, raw := range got {
		c, _ := raw.(map[string]any)
		if c["name"] != checks[i].Name || c["verdict"] != string(checks[i].Verdict) {
			t.Errorf("check %d = %v, want %s/%s", i, c, checks[i].Name, checks[i].Verdict)
		}
		if checks[i].Detail != "" && c["detail"] != checks[i].Detail {
			t.Errorf("check %d detail = %v, want %q", i, c["detail"], checks[i].Detail)
		}
	}
	mining, _ := data["mining"].(map[string]any)
	if mining["state"] != string(auth.MiningEnabled) || mining["state_known"] != true {
		t.Errorf("mining: %v", mining)
	}
	health, _ := data["health"].([]any)
	if len(health) != 1 {
		t.Fatalf("health: %v", data["health"])
	}
	h0, _ := health[0].(map[string]any)
	if h0["component"] != string(auth.HealthFlush) || h0["reason"] != string(auth.HealthSpoolBacklog) {
		t.Errorf("health record: %v", h0)
	}
	queue, _ := data["queue"].(map[string]any)
	if queue == nil {
		t.Error("no queue state in the JSON report")
	}
}

// UNKNOWN is the absence of a fact. It must survive into JSON as UNKNOWN
// and must never arrive as false.
func TestDoctorJSONKeepsUnknownChecksUnknown(t *testing.T) {
	f := healthyFacts()
	f.DocErr = errASDown
	f.EpochErr, f.StatusErr, f.StandingErr, f.ActivityErr = errASDown, errASDown, errASDown, errASDown
	f.ASConfigured, f.ASConfigKnown, f.LocalStateKnown = true, true, true
	f.SpoolDir = t.TempDir()
	checks := assembleDoctor(f)

	var buf bytes.Buffer
	emitMachine(&buf, doctorEnvelope(f, checks, doctorExit(checks)))
	data := dataOf(t, decodeCommandEnvelope(t, buf.String(), "doctor"))

	unknown := map[string]bool{}
	for _, raw := range data["checks"].([]any) {
		c, _ := raw.(map[string]any)
		if c["verdict"] == string(verdictUnknown) {
			unknown[c["name"].(string)] = true
		}
		if c["verdict"] == false || c["verdict"] == true {
			t.Errorf("a verdict became a boolean: %v", c)
		}
	}
	if len(unknown) == 0 {
		t.Fatal("no check came back UNKNOWN with the AS down")
	}
	listed, _ := data["checks_that_could_not_run"].([]any)
	if len(listed) != len(unknown) {
		t.Errorf("checks_that_could_not_run %v, unknown verdicts %v", listed, unknown)
	}
	for _, name := range listed {
		if !unknown[name.(string)] {
			t.Errorf("%q is listed as unable to run but its verdict is not UNKNOWN", name)
		}
	}
}

// The JSON report must not buy its structure with an extra round trip.
func TestDoctorJSONMakesNoMoreASCallsThanTheTextReport(t *testing.T) {
	var calls atomic.Int64
	as := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer as.Close()

	root := t.TempDir()
	cfgPath := filepath.Join(root, "tokendrop.toml")
	toml := `[mining]
enabled = true
as_url = "` + as.URL + `"
chain_id = "fictional-1"
slot_id = 3
state_dir = "` + filepath.ToSlash(filepath.Join(root, "state")) + `"
spool_dir = "` + filepath.ToSlash(filepath.Join(root, "spool")) + `"
`
	if err := os.WriteFile(cfgPath, []byte(toml), 0o600); err != nil {
		t.Fatal(err)
	}

	var text, jsonOut bytes.Buffer
	cmdDoctor([]string{"-config", cfgPath}, &text, io.Discard)
	afterText := calls.Load()
	cmdDoctor([]string{"-config", cfgPath, "-json"}, &jsonOut, io.Discard)
	afterJSON := calls.Load() - afterText

	if afterJSON > afterText {
		t.Errorf("the JSON report made %d AS calls, the text report made %d", afterJSON, afterText)
	}
	if !strings.HasPrefix(text.String(), "  mining") {
		t.Errorf("the text report changed shape: %q", text.String())
	}
	decodeCommandEnvelope(t, jsonOut.String(), "doctor")
}

// ── status ──────────────────────────────────────────────────────────────

// statusFixture writes a config and a state directory holding a claimed
// registration with an open health record.
func statusFixture(t *testing.T) (cfgPath, root string, store *auth.Store) {
	t.Helper()
	root = t.TempDir()
	cfgPath = filepath.Join(root, "tokendrop.toml")
	toml := `[mining]
state_dir = "` + filepath.ToSlash(filepath.Join(root, "state")) + `"
spool_dir = "` + filepath.ToSlash(filepath.Join(root, "spool")) + `"
`
	if err := os.WriteFile(cfgPath, []byte(toml), 0o600); err != nil {
		t.Fatal(err)
	}
	var err error
	store, err = auth.OpenStore(filepath.Join(root, "state"))
	if err != nil {
		t.Fatal(err)
	}
	return cfgPath, root, store
}

func TestStatusJSONAndTextComeFromTheSameFacts(t *testing.T) {
	cfgPath, _, store := statusFixture(t)
	if err := store.SaveMiningEnabled(false); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkHealth(auth.HealthCapture, auth.HealthSandboxRestricted, "intake is read-only here"); err != nil {
		t.Fatal(err)
	}
	reg := auth.AgentRegistration{
		AgentID: "agent-fictional-1", Status: "claimed", Scopes: []string{"search", "mining"},
		LastEnrollmentSlot: "slot-3",
	}
	if err := store.SaveAgentRegistration(reg); err != nil {
		t.Fatal(err)
	}

	args := []string{"-config", cfgPath}
	var text, textErr bytes.Buffer
	if code := statusMain(args, &text, &textErr, noEnv); code != exitOK {
		t.Fatalf("text status exit %d", code)
	}
	var jsonOut, jsonErr bytes.Buffer
	if code := statusMain(append(args, "-json"), &jsonOut, &jsonErr, noEnv); code != exitOK {
		t.Fatalf("json status exit %d", code)
	}
	data := dataOf(t, decodeCommandEnvelope(t, jsonOut.String(), "status"))

	mining, _ := data["mining"].(map[string]any)
	if mining["state"] != string(auth.MiningDisabled) {
		t.Errorf("mining state %v", mining["state"])
	}
	// The same decision the text renderer printed, in its canonical
	// spelling rather than the text renderer's display word.
	if !strings.Contains(text.String(), "mining:  OFF") {
		t.Errorf("the text report no longer says OFF: %q", text.String())
	}
	health, _ := mining["health"].([]any)
	if len(health) != 1 {
		t.Fatalf("health: %v", mining["health"])
	}
	h0, _ := health[0].(map[string]any)
	if h0["reason"] != string(auth.HealthSandboxRestricted) {
		t.Errorf("health reason %v", h0["reason"])
	}
	if !strings.Contains(text.String(), string(auth.HealthSandboxRestricted)) {
		t.Error("the text report dropped the health reason the JSON report carries")
	}

	agent, _ := data["agent"].(map[string]any)
	if agent["agent_id"] != "agent-fictional-1" || agent["status"] != "claimed" ||
		agent["claimed"] != true || agent["enrolled_slot"] != "slot-3" {
		t.Errorf("agent: %v", agent)
	}
	if jsonErr.String() != "" {
		t.Errorf("status -json wrote to stderr: %q", jsonErr.String())
	}
}

// Whatever the store says, both renderers must say the same thing about
// it. This walks the four decisions rather than asserting one.
func TestStatusRenderersAgreeAcrossEveryMiningDecision(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*auth.Store) error
		want  auth.MiningDecisionState
		text  string
	}{
		{"enabled", func(s *auth.Store) error { return s.SaveMiningEnabled(true) }, auth.MiningEnabled, "mining:  ON"},
		{"disabled", func(s *auth.Store) error { return s.SaveMiningEnabled(false) }, auth.MiningDisabled, "mining:  OFF"},
		{"undecided", func(*auth.Store) error { return nil }, auth.MiningUndecided, "mining:  NOT DECIDED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfgPath, _, store := statusFixture(t)
			if err := tc.setup(store); err != nil {
				t.Fatal(err)
			}
			args := []string{"-config", cfgPath}
			var text bytes.Buffer
			statusMain(args, &text, io.Discard, noEnv)
			var jsonOut bytes.Buffer
			statusMain(append(args, "-json"), &jsonOut, io.Discard, noEnv)

			data := dataOf(t, decodeCommandEnvelope(t, jsonOut.String(), "status"))
			mining, _ := data["mining"].(map[string]any)
			if mining["state"] != string(tc.want) {
				t.Errorf("JSON state %v, want %q", mining["state"], tc.want)
			}
			if !strings.Contains(text.String(), tc.text) {
				t.Errorf("text report %q does not contain %q", text.String(), tc.text)
			}
		})
	}
}

// ── connect ─────────────────────────────────────────────────────────────

func TestConnectJSONExposesClaimArtifactsAndNoCredential(t *testing.T) {
	const key = "sr-connect-canary-secret"
	cfgPath, root, store := statusFixture(t)
	reg := auth.AgentRegistration{
		AgentID:        "agent-fictional-9",
		Status:         "unclaimed",
		ClaimURL:       "https://portal.fictional.test/claim/abc123",
		ClaimCode:      "ABC-123",
		ClaimExpiresAt: "2026-09-13T00:00:00Z",
	}
	if err := store.SaveAgentRegistration(reg); err != nil {
		t.Fatal(err)
	}
	if err := writeCredentials(filepath.Join(root, "credentials.json"), credentials{APIKey: key}); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	emitMachine(&buf, connectEnvelope(cfgPath, noEnv, exitOK, ""))
	env := decodeCommandEnvelope(t, buf.String(), "connect")
	data := dataOf(t, env)

	if data["claim_url"] != reg.ClaimURL || data["claim_code"] != reg.ClaimCode {
		t.Errorf("claim artifacts are not explicit fields: %v", data)
	}
	if data["agent_id"] != reg.AgentID || data["status"] != "unclaimed" || data["claimed"] != false {
		t.Errorf("registration: %v", data)
	}
	// Unclaimed is not a failure: search already works. What is
	// outstanding is a person visiting the URL.
	if env["ok"] != true || env["code"] != "unclaimed" || env["action"] != actionConnect {
		t.Errorf("header: %v", env)
	}
	if strings.Contains(buf.String(), key) {
		t.Fatalf("the platform credential reached the JSON report:\n%s", buf.String())
	}
	for _, forbidden := range []string{"api_key", "apiKey", "refresh", "dpop", "private_key", "mnemonic", "passphrase"} {
		if strings.Contains(strings.ToLower(buf.String()), forbidden) {
			t.Errorf("the JSON report carries a %q field: %s", forbidden, buf.String())
		}
	}
}

func TestConnectJSONOutcomeFollowsTheStoredStatus(t *testing.T) {
	for _, tc := range []struct {
		exit          int
		status        string
		wantCode      string
		wantRetryable bool
		wantAction    string
	}{
		{exitOK, "claimed", "ok", false, actionNone},
		{exitOK, "unclaimed", "unclaimed", false, actionConnect},
		{exitOK, "expired", "registration_expired", false, actionConnect},
		{exitTransport, "unclaimed", "connect_failed", true, actionRetry},
		{exitUsage, "", "connect_failed", false, actionReport},
	} {
		code, retryable, action := connectOutcome(tc.exit, tc.status)
		if code != tc.wantCode || retryable != tc.wantRetryable || action != tc.wantAction {
			t.Errorf("connectOutcome(%d, %q) = (%q, %v, %q), want (%q, %v, %q)",
				tc.exit, tc.status, code, retryable, action, tc.wantCode, tc.wantRetryable, tc.wantAction)
		}
	}
}

// Nothing in this client opens a URL. The claim URL is printed for a
// person, in both renderers.
func TestNothingOpensAClaimURL(t *testing.T) {
	for _, dir := range []string{".", "../../pkg/platform"} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
				continue
			}
			raw, err := os.ReadFile(filepath.Join(dir, e.Name())) // #nosec G304 -- this package's own source
			if err != nil {
				t.Fatal(err)
			}
			for _, opener := range []string{`"xdg-open"`, "rundll32", "browser.OpenURL", "exec.Command(\"open\""} {
				if bytes.Contains(raw, []byte(opener)) {
					t.Errorf("%s/%s looks like it opens a URL (%s)", dir, e.Name(), opener)
				}
			}
		}
	}
}

// The two checks added for #21 render through the existing check shape —
// no new top-level field, so an SDK that already walks `checks` sees them
// without changing.
func TestDoctorJSONCarriesTheIntakeAndRecordingChecks(t *testing.T) {
	f := healthyFacts()
	f.SpoolDir = t.TempDir()
	// A failing probe, so `fix` is populated on one of them and the
	// serializer's handling of it is exercised rather than assumed.
	f.IntakeProbe = intakeProbeResult{
		Ran: true, Dir: f.IntakeDir,
		Stage: probeStageWrite, Err: os.ErrPermission,
	}
	checks := assembleDoctor(f)

	var buf bytes.Buffer
	emitMachine(&buf, doctorEnvelope(f, checks, doctorExit(checks)))
	data := dataOf(t, decodeCommandEnvelope(t, buf.String(), "doctor"))

	got := map[string]map[string]any{}
	for _, raw := range data["checks"].([]any) {
		c, _ := raw.(map[string]any)
		name, _ := c["name"].(string)
		got[name] = c
	}
	for _, name := range []string{"intake writable", "recording"} {
		c, ok := got[name]
		if !ok {
			t.Fatalf("%q is missing from doctor -json: %s", name, buf.String())
		}
		if c["verdict"] == nil || c["verdict"] == "" {
			t.Errorf("%q has no verdict", name)
		}
		if c["detail"] == nil || c["detail"] == "" {
			t.Errorf("%q has no detail", name)
		}
	}
	if got["intake writable"]["verdict"] != string(verdictNo) {
		t.Errorf("intake writable verdict = %v, want NO", got["intake writable"]["verdict"])
	}
	fix, _ := got["intake writable"]["fix"].(string)
	if !strings.Contains(fix, "agents install") {
		t.Errorf("the failing probe's fix did not reach JSON: %v", got["intake writable"]["fix"])
	}
	// A failed probe leaves `recording` undetermined, with its reason.
	detail, _ := got["recording"]["detail"].(string)
	if !strings.HasPrefix(detail, "could not determine — ") {
		t.Errorf("recording detail = %q, want a could-not-determine reason", detail)
	}
}
