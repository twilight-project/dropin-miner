package main

// Integration tests for connect and mining enable (agent onboarding
// design, search-platform-agent-onboarding-design.md §5.5, §8 WP2
// acceptance): the four §2.2 cases, idempotency, search working before
// the claim, status reporting, the detached resume doing exactly one
// poll, the installer's mining question in all three answers (and its
// scripted-config equivalent), the payout declaration running
// unattended after enrollment, and no wallet on a non-terminal path.
//
// Two stubs stand in for the real services: stubPlatform (register,
// status, enroll — the §5.1-§5.3 contract) and stubOnboardingAS (just enough of
// the AS's OAuth surface and payout route for RedeemEnrollmentAssertion
// and DeclarePayoutAddress to work — not a reimplementation of the AS,
// which pkg/auth's own suite already covers; mirrors
// pkg/auth/payout_test.go's newPayoutAS: one canned token response for
// every grant, no grant-type-specific logic).

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/twilight-project/dropin-miner/pkg/auth"
	"github.com/twilight-project/dropin-miner/pkg/config"
)

// ── stubs ──────────────────────────────────────────────────────────────

type stubPlatform struct {
	srv *httptest.Server

	mu     sync.Mutex
	status string // "unclaimed" | "claimed" | "expired"
	scopes []string
	slots  []string

	registerCalls int
	statusCalls   int
	enrollCalls   int
}

func newStubPlatform(t *testing.T) *stubPlatform {
	t.Helper()
	f := &stubPlatform{status: "unclaimed", slots: []string{"twilight-slot-3"}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/agents/register", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		f.registerCalls++
		f.mu.Unlock()
		writeStubJSON(w, http.StatusCreated, map[string]any{
			"agent_id": "agent-1", "key": "sr-stubkey",
			"claim_url": f.srv.URL + "/claim/AB12-CD34", "claim_code": "AB12-CD34",
			"claim_expires_at": "2026-09-16T00:00:00Z",
			"poll":             map[string]any{"interval_s": 1},
			"tier":             "unclaimed",
		})
	})
	mux.HandleFunc("GET /v1/agents/{id}", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		f.statusCalls++
		st, scopes, slots := f.status, f.scopes, f.slots
		f.mu.Unlock()
		writeStubJSON(w, http.StatusOK, map[string]any{
			"status": st, "scopes": scopes,
			"mining": map[string]any{"available": len(slots) > 0, "slots": slots},
		})
	})
	mux.HandleFunc("POST /v1/agents/enroll", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		f.enrollCalls++
		claimed := f.status == "claimed"
		mining := hasScope(f.scopes, "mining")
		f.mu.Unlock()
		switch {
		case !claimed:
			writeStubJSON(w, http.StatusForbidden, map[string]any{"error": map[string]any{"code": "not_claimed"}})
		case !mining:
			writeStubJSON(w, http.StatusForbidden, map[string]any{"error": map[string]any{"code": "mining_not_granted"}})
		default:
			writeStubJSON(w, http.StatusOK, map[string]any{"token": "enroll-jwt-1"}) // #nosec G101 -- a canned stub response, not a credential
		}
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *stubPlatform) claim(scopes ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status = "claimed"
	f.scopes = scopes
}

// setSlots overrides the single-slot default (WP2-review judgment call 1:
// chooseSlot's more-than-one-offered path).
func (f *stubPlatform) setSlots(slots ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.slots = slots
}

func (f *stubPlatform) counts() (register, status, enroll int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.registerCalls, f.statusCalls, f.enrollCalls
}

func writeStubJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// stubOnboardingAS is an AS complete enough for RedeemEnrollmentAssertion and
// DeclarePayoutAddress.
type stubOnboardingAS struct {
	srv *httptest.Server

	mu            sync.Mutex
	declared      string
	activeAddress string // "" means no active binding yet (GET answers 404)
	declareCalls  int
}

func newStubAS(t *testing.T) *stubOnboardingAS {
	t.Helper()
	f := &stubOnboardingAS{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/oauth-authorization-server", func(w http.ResponseWriter, _ *http.Request) {
		base := f.srv.URL
		writeStubJSON(w, http.StatusOK, map[string]any{
			"issuer":                        base,
			"authorization_endpoint":        base + "/oauth/authorize",
			"token_endpoint":                base + "/oauth/token",
			"device_authorization_endpoint": base + "/oauth/device_authorization",
			"revocation_endpoint":           base + "/oauth/revoke",
			"jwks_uri":                      base + "/oauth/jwks.json",
			"grant_types_supported":         []string{"authorization_code", "refresh_token", auth.GrantTypeJWTBearer},
		})
	})
	mux.HandleFunc("POST /oauth/token", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"at-1","token_type":"DPoP","expires_in":900,"refresh_token":"rt-1"}`))
	})
	mux.HandleFunc("PUT /v1/payout/declaration", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Address string `json:"address"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.declared = body.Address
		f.declareCalls++
		f.mu.Unlock()
		writeStubJSON(w, http.StatusOK, map[string]any{
			"status": "ACTIVE", "address": body.Address, "canonical_address": body.Address,
			"effective": true, "declared_at": "2026-09-09T00:00:00Z",
		})
	})
	mux.HandleFunc("GET /v1/payout/declaration", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		active := f.activeAddress
		f.mu.Unlock()
		// "nothing declared yet" is 200 with active: null, not a 404 — the
		// route always answers for a claimed participant (payout.go's
		// PayoutStanding: "Active is the address in force, or nil").
		if active == "" {
			writeStubJSON(w, http.StatusOK, map[string]any{"active": nil, "pending": nil})
			return
		}
		writeStubJSON(w, http.StatusOK, map[string]any{
			"active": map[string]any{
				"status": "ACTIVE", "address": active, "canonical_address": active,
				"effective": true, "declared_at": "2026-09-09T00:00:00Z",
			},
		})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *stubOnboardingAS) declaredAddress() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.declared
}

func (f *stubOnboardingAS) declarationAttempts() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.declareCalls
}

func (f *stubOnboardingAS) setActiveAddress(addr string) {
	f.mu.Lock()
	f.activeAddress = addr
	f.mu.Unlock()
}

// miningClient builds a *auth.MiningClient against this stub, the same
// shape as pkg/auth/payout_test.go's payoutAS.client — enough for
// PayoutStanding/DeclarePayoutAddress, which is all declarePayoutIfSafe's
// tests below need.
func (f *stubOnboardingAS) miningClient(t *testing.T) (*auth.MiningClient, string) {
	t.Helper()
	stateDir := filepath.Join(t.TempDir(), "state")
	store, err := auth.OpenStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveRefreshToken("rt-0"); err != nil {
		t.Fatal(err)
	}
	d, err := auth.NewDiscoverer(auth.DiscoveryConfig{BaseURL: f.srv.URL, ChainID: "twilight-1", SlotID: 7})
	if err != nil {
		t.Fatal(err)
	}
	oc, err := auth.NewOAuthClient(context.Background(), d, store)
	if err != nil {
		t.Fatal(err)
	}
	return auth.NewMiningClient(d, oc, store), stateDir
}

// ── config helper ─────────────────────────────────────────────────────

// withShortConnectTimings shrinks the foreground poll's budget and
// interval for the duration of a test, so a case that legitimately never
// gets claimed (or claims after a couple of iterations) does not wait
// out the real 3-minute bound. Restored via t.Cleanup regardless of how
// the test ends.
func withShortConnectTimings(t *testing.T) {
	t.Helper()
	origBudget, origInterval := connectPollBudget, resumePollInterval
	connectPollBudget = 300 * time.Millisecond
	resumePollInterval = 20 * time.Millisecond
	t.Cleanup(func() {
		connectPollBudget = origBudget
		resumePollInterval = origInterval
	})
}

func writeTOML(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tokendrop.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil { // #nosec G703 -- fixed filename in the test's own t.TempDir()
		t.Fatal(err)
	}
	return path
}

// connectConfig builds a config with [platform] pointed at platformURL
// and, when asURL != "", a valid [mining] block pointed at it
// (enabled = true, as/chain/slot filled in) — enough for connect to
// both register/poll AND, once claimed with the mining scope, enroll.
// asURL == "" builds a search-only config: [platform] only, no [mining]
// at all, matching a participant who never intends to mine.
func connectConfig(t *testing.T, platformURL, asURL string) (cfgPath, stateDir string) {
	t.Helper()
	dir := t.TempDir()
	stateDir = filepath.Join(dir, "state")
	var b strings.Builder
	fmt.Fprintf(&b, "[platform]\nbase_url = %q\n", platformURL)
	if asURL != "" {
		fmt.Fprintf(&b, "\n[mining]\nenabled = true\nas_url = %q\nchain_id = \"twilight-1\"\nslot_id = 7\nstate_dir = %q\n",
			asURL, stateDir)
	} else {
		// No [mining] at all: state_dir still needs somewhere to land
		// (StateDir defaults regardless of whether [mining] is present),
		// but the default (os.UserConfigDir()/tokendrop/state) is not
		// test-isolated, so this exercises the same field via the one
		// TOML key that does not require enabled = true.
		fmt.Fprintf(&b, "\n[mining]\nstate_dir = %q\n", stateDir)
	}
	return writeTOML(t, b.String()), stateDir
}

// scriptedMiningConfig builds a config for testing askMiningQuestion's
// non-interactive branch directly: a syntactically valid [mining] block
// (as_url/chain_id/slot_id — never dialed by askMiningQuestion itself,
// which only decides and persists an address) plus whatever
// enabled/payout_address line(s) the case under test needs appended.
// Separate from connectConfig because connectConfig's own enabled=true
// shape can't be safely re-toggled by appending a second `enabled =`
// line — TOML rejects the duplicate key.
func scriptedMiningConfig(t *testing.T, platformURL, extra string) (cfgPath, stateDir string) {
	t.Helper()
	dir := t.TempDir()
	stateDir = filepath.Join(dir, "state")
	body := fmt.Sprintf("[platform]\nbase_url = %q\n\n[mining]\nstate_dir = %q\nas_url = \"https://as.example.invalid\"\nchain_id = \"twilight-1\"\nslot_id = 7\n%s\n",
		platformURL, stateDir, extra)
	return writeTOML(t, body), stateDir
}

func runConnect(t *testing.T, cfgPath string, stdin *bytes.Buffer, extra ...string) (code int, stdout, stderr string) {
	t.Helper()
	if stdin == nil {
		stdin = &bytes.Buffer{}
	}
	var out, errOut bytes.Buffer
	args := append([]string{"-config", cfgPath}, extra...)
	code = cmdConnect(args, stdin, &out, &errOut, noEnv)
	return code, out.String(), errOut.String()
}

func loadAgent(t *testing.T, stateDir string) (auth.AgentRegistration, bool) {
	t.Helper()
	store, err := auth.OpenStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	reg, ok, err := store.LoadAgentRegistration()
	if err != nil {
		t.Fatal(err)
	}
	return reg, ok
}

// ── §2.2 cases ────────────────────────────────────────────────────────

// Case: no account, agent registers first. search works unclaimed at
// once (proven by a separate test); the one visit later claims it.
func TestConnectCaseNoAccountRegistersFirst(t *testing.T) {
	withShortConnectTimings(t)
	platform := newStubPlatform(t)
	cfgPath, stateDir := connectConfig(t, platform.srv.URL, "")

	code, out, _ := runConnect(t, cfgPath, nil)
	if code != exitOK {
		t.Fatalf("connect exited %d", code)
	}
	if !strings.Contains(out, "AB12-CD34") || !strings.Contains(out, platform.srv.URL) {
		t.Fatalf("claim URL/code not printed: %q", out)
	}
	reg, ok := loadAgent(t, stateDir)
	if !ok || reg.AgentID != "agent-1" || reg.Status != "unclaimed" {
		t.Fatalf("got %+v ok=%v", reg, ok)
	}
	if creds, err := os.ReadFile(filepath.Join(filepath.Dir(stateDir), "credentials.json")); err != nil || !strings.Contains(string(creds), "sr-stubkey") {
		t.Fatalf("credentials.json not written with the platform key: %v %q", err, creds)
	}
	if registerCalls, _, _ := platform.counts(); registerCalls != 1 {
		t.Fatalf("register called %d times, want 1", registerCalls)
	}
}

// Case: account exists, search only — claim without granting mining.
func TestConnectCaseAccountExistsSearchOnly(t *testing.T) {
	withShortConnectTimings(t)
	platform := newStubPlatform(t)
	cfgPath, stateDir := connectConfig(t, platform.srv.URL, "")
	if code, _, _ := runConnect(t, cfgPath, nil); code != exitOK {
		t.Fatal("first connect failed")
	}
	platform.claim("credits")

	// Resumed connect (a second run) picks up the stored registration
	// and polls rather than re-registering.
	code, out, _ := runConnect(t, cfgPath, nil)
	if code != exitOK {
		t.Fatalf("resumed connect exited %d: %s", code, out)
	}
	reg, ok := loadAgent(t, stateDir)
	if !ok || reg.Status != "claimed" || hasScope(reg.Scopes, "mining") {
		t.Fatalf("got %+v ok=%v", reg, ok)
	}
	if registerCalls, _, _ := platform.counts(); registerCalls != 1 {
		t.Fatalf("register called %d times across two runs, want 1 (resumable)", registerCalls)
	}
}

// Case: account exists, search + mining — same visit, mining ticked.
// Enrollment and payout run unattended once claimed.
func TestConnectCaseAccountExistsSearchAndMining(t *testing.T) {
	withShortConnectTimings(t)
	platform := newStubPlatform(t)
	as := newStubAS(t)
	cfgPath, stateDir := connectConfig(t, platform.srv.URL, as.srv.URL)

	// First run: register, and (non-interactive stdin, [mining] enabled
	// explicit=true with no address) the mining question resolves via
	// config, silently, with no address yet.
	if code, _, _ := runConnect(t, cfgPath, nil, "-mining"); code != exitOK {
		t.Fatal("first connect failed")
	}
	// Give it a payout address the way a scripted install would: set it
	// directly on the store, as mining enable would have.
	store, err := auth.OpenStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SavePayoutAddress("twilight1uwew6p63453wm0znz723lrneuls4xy29swp89n"); err != nil {
		t.Fatal(err)
	}
	platform.claim("credits", "mining")

	code, out, _ := runConnect(t, cfgPath, nil)
	if code != exitOK {
		t.Fatalf("second connect exited %d: %s", code, out)
	}
	if !strings.Contains(out, "enrolled for mining") {
		t.Fatalf("did not report enrollment: %q", out)
	}
	reg, ok := loadAgent(t, stateDir)
	if !ok || reg.LastEnrollmentSlot != "twilight-slot-3" {
		t.Fatalf("got %+v ok=%v", reg, ok)
	}
	if got := as.declaredAddress(); got != "twilight1uwew6p63453wm0znz723lrneuls4xy29swp89n" {
		t.Fatalf("payout declared address = %q, want twilight1uwew6p63453wm0znz723lrneuls4xy29swp89n", got)
	}
}

// Case: claimed with search only, mining added later — connect's
// -mining hint at registration is not required; the granted scope
// alone triggers enrollment on the next poll (design decision 3).
func TestConnectCaseMiningGrantedLater(t *testing.T) {
	withShortConnectTimings(t)
	platform := newStubPlatform(t)
	as := newStubAS(t)
	cfgPath, stateDir := connectConfig(t, platform.srv.URL, as.srv.URL)

	if code, _, _ := runConnect(t, cfgPath, nil); code != exitOK {
		t.Fatal("first connect failed")
	}
	platform.claim("credits") // claimed, search only, first
	if code, _, _ := runConnect(t, cfgPath, nil); code != exitOK {
		t.Fatal("second connect failed")
	}
	if reg, _ := loadAgent(t, stateDir); reg.LastEnrollmentSlot != "" {
		t.Fatal("enrolled before mining was granted")
	}

	// Mining granted later, with a payout address already on file
	// (as if `mining enable` had run in between).
	store, err := auth.OpenStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SavePayoutAddress("twilight1gz3fu9w3jp08jp4qcjj6hsckzktsexd09vz039"); err != nil {
		t.Fatal(err)
	}
	platform.claim("credits", "mining")

	code, out, _ := runConnect(t, cfgPath, nil)
	if code != exitOK || !strings.Contains(out, "enrolled for mining") {
		t.Fatalf("did not enroll on the later grant: code=%d out=%q", code, out)
	}
	if got := as.declaredAddress(); got != "twilight1gz3fu9w3jp08jp4qcjj6hsckzktsexd09vz039" {
		t.Fatalf("declared address = %q, want twilight1gz3fu9w3jp08jp4qcjj6hsckzktsexd09vz039", got)
	}
}

// WP2-review ruling on judgment call 1: one slot offered, take it; more
// than one, refuse and require mining.platform_slot rather than guessing.
func TestChooseSlot(t *testing.T) {
	cases := []struct {
		name         string
		slots        []string
		platformSlot string
		wantSlot     string
		wantErr      bool
	}{
		{"none offered", nil, "", "", false},
		{"exactly one: taken regardless of platform_slot", []string{"twilight-slot-3"}, "", "twilight-slot-3", false},
		{"more than one, no platform_slot: refused", []string{"a", "b"}, "", "", true},
		{"more than one, platform_slot matches: taken", []string{"a", "b"}, "b", "b", false},
		{"more than one, platform_slot matches none: refused", []string{"a", "b"}, "c", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			slot, errText := chooseSlot(c.slots, c.platformSlot)
			if slot != c.wantSlot {
				t.Errorf("slot = %q, want %q", slot, c.wantSlot)
			}
			if c.wantErr && errText == "" {
				t.Error("wanted a non-empty refusal message, got none")
			}
			if !c.wantErr && errText != "" {
				t.Errorf("unexpected refusal message: %q", errText)
			}
		})
	}
}

// End to end: a platform offering more than one slot refuses to enroll
// until mining.platform_slot names one of them.
func TestConnectRefusesToGuessAmongMultipleSlots(t *testing.T) {
	withShortConnectTimings(t)
	platform := newStubPlatform(t)
	platform.setSlots("twilight-slot-3", "twilight-slot-7")
	as := newStubAS(t)
	cfgPath, stateDir := connectConfig(t, platform.srv.URL, as.srv.URL)

	if code, _, _ := runConnect(t, cfgPath, nil); code != exitOK {
		t.Fatal("first connect failed")
	}
	platform.claim("credits", "mining")

	code, _, errOut := runConnect(t, cfgPath, nil)
	if code == exitOK {
		t.Fatal("connect exited OK despite an ambiguous slot choice")
	}
	if !strings.Contains(errOut, "platform_slot") {
		t.Fatalf("refusal did not mention mining.platform_slot: %q", errOut)
	}
	if reg, _ := loadAgent(t, stateDir); reg.LastEnrollmentSlot != "" {
		t.Fatal("enrolled despite an unresolved slot ambiguity")
	}

	// Naming one of the offered slots resolves it.
	cfg, err := os.ReadFile(cfgPath) // #nosec G304 -- the test's own writeTOML output, not an external path
	if err != nil {
		t.Fatal(err)
	}
	cfgPath2 := writeTOML(t, string(cfg)+"platform_slot = \"twilight-slot-7\"\n")
	code, out, _ := runConnect(t, cfgPath2, nil)
	if code != exitOK || !strings.Contains(out, "enrolled for mining") {
		t.Fatalf("did not enroll once platform_slot named one of the offered slots: code=%d out=%q", code, out)
	}
	if reg, _ := loadAgent(t, stateDir); reg.LastEnrollmentSlot != "twilight-slot-7" {
		t.Fatalf("enrolled on %q, want twilight-slot-7", reg.LastEnrollmentSlot)
	}
}

// WP2-review defect 2: "enrolled" and "declared" are separate facts. This
// installation enrolls with NO address on file (a scripted install: mining
// enabled, no payout_address configured yet), so the enrolling poll has
// nothing to declare. Only afterward does a payout address arrive (`mining
// enable`, run later). The old code's `reg.LastEnrollmentSlot != ""` check
// short-circuited pollOnce entirely on every poll from then on, so the
// address was never declared. This proves the next run picks it up.
func TestAddressSetAfterEnrollmentIsDeclaredOnTheNextRun(t *testing.T) {
	withShortConnectTimings(t)
	platform := newStubPlatform(t)
	as := newStubAS(t)
	cfgPath, stateDir := connectConfig(t, platform.srv.URL, as.srv.URL)

	if code, _, _ := runConnect(t, cfgPath, nil, "-mining"); code != exitOK {
		t.Fatal("first connect failed")
	}
	platform.claim("credits", "mining")

	// Enroll with no address on file yet.
	code, out, _ := runConnect(t, cfgPath, nil)
	if code != exitOK || !strings.Contains(out, "enrolled for mining") {
		t.Fatalf("did not enroll: code=%d out=%q", code, out)
	}
	if got := as.declarationAttempts(); got != 0 {
		t.Fatalf("declaration attempts = %d, want 0 (no address was on file to declare)", got)
	}
	reg, ok := loadAgent(t, stateDir)
	if !ok || reg.LastEnrollmentSlot == "" {
		t.Fatalf("expected enrollment to be recorded: %+v ok=%v", reg, ok)
	}

	// The address arrives afterward, as `mining enable` would leave it.
	store, err := auth.OpenStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SavePayoutAddress("twilight19ltafk0vdyzwtnzjcfwgvlu3ldmvgemdqxsn79"); err != nil {
		t.Fatal(err)
	}

	// A subsequent, ordinary poll — already enrolled, nothing about
	// enrollment changed — must still declare the now-present address.
	code, out, _ = runConnect(t, cfgPath, nil)
	if code != exitOK {
		t.Fatalf("third connect exited %d: %s", code, out)
	}
	if got := as.declaredAddress(); got != "twilight19ltafk0vdyzwtnzjcfwgvlu3ldmvgemdqxsn79" {
		t.Fatalf("declared address = %q, want twilight19ltafk0vdyzwtnzjcfwgvlu3ldmvgemdqxsn79 (the old code swallowed this: "+
			"already-enrolled short-circuited before the declare check)", got)
	}
}

// Once an address is confirmed active, a later poll must not keep hitting
// the AS to re-declare it — WP2-review defect 2's "avoid redundant declare
// calls" concern. addressSettled (SavePayoutDeclared) is what stops it.
func TestDeclareIsNotRepeatedOnceSettled(t *testing.T) {
	withShortConnectTimings(t)
	platform := newStubPlatform(t)
	as := newStubAS(t)
	cfgPath, stateDir := connectConfig(t, platform.srv.URL, as.srv.URL)
	store, err := auth.OpenStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}

	if code, _, _ := runConnect(t, cfgPath, nil, "-mining"); code != exitOK {
		t.Fatal("first connect failed")
	}
	if err := store.SavePayoutAddress("twilight1225q9dwktuz2vjj0q220m2jjy3x8cajwcfueq0"); err != nil {
		t.Fatal(err)
	}
	platform.claim("credits", "mining")

	// Enrolls and declares in this run.
	if code, _, _ := runConnect(t, cfgPath, nil); code != exitOK {
		t.Fatal("second connect failed")
	}
	if got := as.declarationAttempts(); got != 1 {
		t.Fatalf("declaration attempts after first success = %d, want 1", got)
	}

	// A resume-style poll with nothing new must not call the AS again.
	if code, _, _ := runConnect(t, cfgPath, nil, "-resume"); code != exitOK {
		t.Fatal("resume poll failed")
	}
	if got := as.declarationAttempts(); got != 1 {
		t.Fatalf("declaration attempts after a settled resume = %d, want still 1 (no redundant AS round trip)", got)
	}
}

// ── idempotency, search-before-claim, status ────────────────────────

func TestConnectIsIdempotentOnSecondRun(t *testing.T) {
	withShortConnectTimings(t)
	platform := newStubPlatform(t)
	cfgPath, _ := connectConfig(t, platform.srv.URL, "")
	for i := 0; i < 3; i++ {
		if code, _, _ := runConnect(t, cfgPath, nil); code != exitOK {
			t.Fatalf("run %d failed", i)
		}
	}
	if registerCalls, statusCalls, _ := platform.counts(); registerCalls != 1 || statusCalls < 3 {
		t.Fatalf("register=%d status=%d, want register=1 status>=3", registerCalls, statusCalls)
	}
}

// search works from the moment connect stores the key, before any
// claim — the property that makes the unclaimed tier useful at all.
func TestConnectStoresKeyAndSearchWorksBeforeClaim(t *testing.T) {
	withShortConnectTimings(t)
	platform := newStubPlatform(t)
	cfgPath, _ := connectConfig(t, platform.srv.URL, "")
	if code, _, _ := runConnect(t, cfgPath, nil); code != exitOK {
		t.Fatal("connect failed")
	}

	router := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer sr-stubkey" {
			t.Errorf("search sent Authorization %q, want the key connect stored", got)
		}
		writeStubJSON(w, http.StatusOK, map[string]any{"request_id": "req-1"})
	}))
	defer router.Close()

	existing, err := os.ReadFile(cfgPath) // #nosec G304 -- the test's own writeTOML output, not an external path
	if err != nil {
		t.Fatal(err)
	}
	routerCfg := writeTOML(t, string(existing)+fmt.Sprintf("\n[miner]\nrouter_url = %q\n", router.URL))
	// search.go's own state_dir/intake_dir resolution needs [mining] to
	// have a state_dir even in the search-only case; connectConfig
	// already wrote one, reused here via the same directory tree.
	hook := realHookOps()
	hook.getenv = noEnv
	var out, errOut bytes.Buffer
	code := searchMain(searchOps{
		getppid: func() int { return 1 }, hostname: func() (string, error) { return "h", nil },
		getwd: func() (string, error) { return t.TempDir(), nil }, now: time.Now, hook: hook,
	}, []string{"-config", routerCfg, "-no-flush", "hello"}, &out, &errOut, noEnv)
	if code != exitOK {
		t.Fatalf("search exited %d: %s", code, errOut.String())
	}
}

func TestStatusReportsAgentIdentity(t *testing.T) {
	withShortConnectTimings(t)
	platform := newStubPlatform(t)
	cfgPath, _ := connectConfig(t, platform.srv.URL, "")
	if code, _, _ := runConnect(t, cfgPath, nil); code != exitOK {
		t.Fatal("connect failed")
	}
	out := captureStdout(t, func() {
		printAgentIdentityStatus([]string{"-config", cfgPath}, os.Stdout, os.Stderr, noEnv)
	})
	if !strings.Contains(out, "unclaimed") {
		t.Fatalf("status did not report unclaimed: %q", out)
	}

	platform.claim("credits", "mining")
	if code, _, _ := runConnect(t, cfgPath, nil); code != exitOK {
		t.Fatal("resumed connect failed")
	}
	out = captureStdout(t, func() {
		printAgentIdentityStatus([]string{"-config", cfgPath}, os.Stdout, os.Stderr, noEnv)
	})
	if !strings.Contains(out, "claimed") || !strings.Contains(out, "mining") {
		t.Fatalf("status did not report claimed+mining: %q", out)
	}
}

// ── detached resume ──────────────────────────────────────────────────

func TestDetachedResumePollsOnceAndExits(t *testing.T) {
	withShortConnectTimings(t)
	platform := newStubPlatform(t)
	cfgPath, stateDir := connectConfig(t, platform.srv.URL, "")
	if code, _, _ := runConnect(t, cfgPath, nil); code != exitOK {
		t.Fatal("first connect failed")
	}
	_, before, _ := platform.counts()

	code, _, _ := runConnect(t, cfgPath, nil, "-resume")
	if code != exitOK {
		t.Fatalf("resume exited %d", code)
	}
	_, after, _ := platform.counts()
	if after-before != 1 {
		t.Fatalf("resume made %d status calls, want exactly 1", after-before)
	}
	if _, ok := loadAgent(t, stateDir); !ok {
		t.Fatal("resume did not read the stored registration")
	}
}

func TestShouldResumeIsALocalCheckWithNoStoredRegistration(t *testing.T) {
	if shouldResume(&config.Config{Mining: config.Mining{StateDir: filepath.Join(t.TempDir(), "state")}}) {
		t.Fatal("shouldResume true with nothing on disk")
	}
	if shouldResume(&config.Config{}) {
		t.Fatal("shouldResume true with an empty state dir")
	}
}

// WP2-review defect 3 / coverage gap A: the spawn must stop once there is
// nothing a resume can do, and — the reviewer's specific finding — every
// condition that decides that needs a test that fails if THAT condition
// alone is deleted, not just a return value it happens to share with a
// neighbor. shouldResumeFixture builds a registration and lets each
// subtest vary exactly one thing.
func shouldResumeFixture(t *testing.T, status string, scopes []string, enrolled bool, address string) (stateDir string) {
	t.Helper()
	stateDir = filepath.Join(t.TempDir(), "state")
	store, err := auth.OpenStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	reg := auth.AgentRegistration{AgentID: "agent-1", Status: status, Scopes: scopes}
	if enrolled {
		reg.LastEnrollmentSlot = "twilight-slot-3"
	}
	if err := store.SaveAgentRegistration(reg); err != nil {
		t.Fatal(err)
	}
	if address != "" {
		if err := store.SavePayoutAddress(address); err != nil {
			t.Fatal(err)
		}
	}
	return stateDir
}

func TestNoResumeSpawnForEnrolledOptedOutOrUnconfigured(t *testing.T) {
	configured := config.Mining{Enabled: true, ASBaseURL: "https://as.example"}

	t.Run("unclaimed: still actionable", func(t *testing.T) {
		stateDir := shouldResumeFixture(t, "unclaimed", nil, false, "")
		if !shouldResume(&config.Config{Mining: config.Mining{StateDir: stateDir}}) {
			t.Fatal("shouldResume false for an unclaimed registration still worth polling")
		}
	})

	t.Run("claimed, no mining scope: settled (search-only)", func(t *testing.T) {
		stateDir := shouldResumeFixture(t, "claimed", []string{"credits"}, false, "")
		cfg := configured
		cfg.StateDir = stateDir
		if shouldResume(&config.Config{Mining: cfg}) {
			t.Fatal("shouldResume true for a search-only claim")
		}
	})

	t.Run("expired: settled", func(t *testing.T) {
		stateDir := shouldResumeFixture(t, "expired", nil, false, "")
		if shouldResume(&config.Config{Mining: config.Mining{StateDir: stateDir}}) {
			t.Fatal("shouldResume true for an expired registration")
		}
	})

	t.Run("not enrolled, mining.enabled = false: settled", func(t *testing.T) {
		stateDir := shouldResumeFixture(t, "claimed", []string{"mining"}, false, "")
		// ASBaseURL IS set here — isolates "not enabled" from "no AS
		// configured" below, so deleting either check independently is
		// each caught by a different subtest. MiningEnabledExplicit true
		// models what loadConfig would actually produce from a real
		// `[mining] enabled = false` file — miningOptedOut (finding 10)
		// requires the explicit flag, matching askMiningQuestion's own
		// opt-out check, not just Enabled's raw zero value.
		cfg := config.Mining{StateDir: stateDir, Enabled: false, ASBaseURL: "https://as.example"}
		if shouldResume(&config.Config{Mining: cfg, MiningEnabledExplicit: true}) {
			t.Fatal("shouldResume true despite mining.enabled = false")
		}
	})

	t.Run("not enrolled, mining.enabled = true but no as_url: settled", func(t *testing.T) {
		stateDir := shouldResumeFixture(t, "claimed", []string{"mining"}, false, "")
		cfg := config.Mining{StateDir: stateDir, Enabled: true, ASBaseURL: ""}
		if shouldResume(&config.Config{Mining: cfg}) {
			t.Fatal("shouldResume true with no mining.as_url configured")
		}
	})

	t.Run("enrolled, no address to declare: settled", func(t *testing.T) {
		stateDir := shouldResumeFixture(t, "claimed", []string{"mining"}, true, "")
		cfg := configured
		cfg.StateDir = stateDir
		if shouldResume(&config.Config{Mining: cfg}) {
			t.Fatal("shouldResume true for a fully settled installation")
		}
	})

	t.Run("enrolled, address already declared: settled", func(t *testing.T) {
		stateDir := shouldResumeFixture(t, "claimed", []string{"mining"}, true, "twilight1225q9dwktuz2vjj0q220m2jjy3x8cajwcfueq0")
		store, err := auth.OpenStore(stateDir)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.SavePayoutDeclared("twilight1225q9dwktuz2vjj0q220m2jjy3x8cajwcfueq0"); err != nil {
			t.Fatal(err)
		}
		cfg := configured
		cfg.StateDir = stateDir
		if shouldResume(&config.Config{Mining: cfg}) {
			t.Fatal("shouldResume true for an address already confirmed declared")
		}
	})

	// The actionable cases must still spawn.
	t.Run("still actionable: not yet enrolled, configured", func(t *testing.T) {
		stateDir := shouldResumeFixture(t, "claimed", []string{"mining"}, false, "")
		cfg := configured
		cfg.StateDir = stateDir
		if !shouldResume(&config.Config{Mining: cfg}) {
			t.Fatal("shouldResume false for an installation that can still enroll")
		}
	})

	t.Run("still actionable: enrolled with an undeclared address", func(t *testing.T) {
		stateDir := shouldResumeFixture(t, "claimed", []string{"mining"}, true, "twilight16hmu7hucv64zeanfsa58cjdhku98k43j4cnrkr")
		cfg := configured
		cfg.StateDir = stateDir
		if !shouldResume(&config.Config{Mining: cfg}) {
			t.Fatal("shouldResume false for an enrolled installation with an undeclared address")
		}
	})
}

// ── the installer's mining question ─────────────────────────────────

func TestInstallerMiningQuestionAllThreeAnswers(t *testing.T) {
	platform := newStubPlatform(t)

	t.Run("no", func(t *testing.T) {
		cfgPath, stateDir := connectConfig(t, platform.srv.URL, "")
		cfg, _, err := loadConfig(cfgPath, noEnv)
		if err != nil {
			t.Fatal(err)
		}
		store, err := auth.OpenStore(stateDir)
		if err != nil {
			t.Fatal(err)
		}
		stdin := bytes.NewBufferString("n\n")
		br := bufio.NewReader(stdin)
		outcome, code := askMiningQuestion(stdin, br, &bytes.Buffer{}, &bytes.Buffer{}, noEnv, cfg, store, true, false)
		if code != exitOK || outcome.enabled {
			t.Fatalf("got %+v code=%d, want disabled", outcome, code)
		}
		if _, ok, _ := store.LoadPayoutAddress(); ok {
			t.Fatal("an address was persisted after answering no")
		}
	})

	t.Run("yes with typed address", func(t *testing.T) {
		cfgPath, stateDir := connectConfig(t, platform.srv.URL, "")
		cfg, _, err := loadConfig(cfgPath, noEnv)
		if err != nil {
			t.Fatal(err)
		}
		store, err := auth.OpenStore(stateDir)
		if err != nil {
			t.Fatal(err)
		}
		stdin := bytes.NewBufferString("y\ntwilight1k5stzqa2sgvfgx9u04cv93pek3gcmm9h5t9hkn\n")
		br := bufio.NewReader(stdin)
		outcome, code := askMiningQuestion(stdin, br, &bytes.Buffer{}, &bytes.Buffer{}, noEnv, cfg, store, true, false)
		if code != exitOK || !outcome.enabled || outcome.payoutAddress != "twilight1k5stzqa2sgvfgx9u04cv93pek3gcmm9h5t9hkn" {
			t.Fatalf("got %+v code=%d", outcome, code)
		}
		if addr, ok, _ := store.LoadPayoutAddress(); !ok || addr != "twilight1k5stzqa2sgvfgx9u04cv93pek3gcmm9h5t9hkn" {
			t.Fatalf("address not persisted: %q ok=%v", addr, ok)
		}
	})

	t.Run("yes with empty address creates a wallet", func(t *testing.T) {
		cfgPath, stateDir := connectConfig(t, platform.srv.URL, "")
		cfg, _, err := loadConfig(cfgPath, noEnv)
		if err != nil {
			t.Fatal(err)
		}
		store, err := auth.OpenStore(stateDir)
		if err != nil {
			t.Fatal(err)
		}
		t.Setenv(walletPassphraseEnv, "correct horse battery staple")
		t.Setenv("TOKENDROP_WALLET_DIR", filepath.Join(t.TempDir(), "wallet"))
		stdin := bytes.NewBufferString("y\n\n")
		br := bufio.NewReader(stdin)
		var out bytes.Buffer
		outcome, code := askMiningQuestion(stdin, br, &out, &bytes.Buffer{}, os.Getenv, cfg, store, true, false)
		if code != exitOK || !outcome.enabled || outcome.payoutAddress == "" {
			t.Fatalf("got %+v code=%d", outcome, code)
		}
		if !strings.Contains(out.String(), "recovery phrase") {
			t.Fatalf("mnemonic was not printed: %q", out.String())
		}
		if addr, ok, _ := store.LoadPayoutAddress(); !ok || addr != outcome.payoutAddress {
			t.Fatalf("address not persisted: %q ok=%v want %q", addr, ok, outcome.payoutAddress)
		}
	})
}

// WP4b (design f0ddb69 §5.5): the question was already visually defaulted
// to "no" (a bare Enter answers N) — participantHasOtherAgent=true adds
// only an explanatory line, never a mechanical block. A human who
// understands the tradeoff can still type "y" and enable it anyway.
func TestMiningQuestionDefaultsToNoWhenParticipantHasAnotherAgent(t *testing.T) {
	platform := newStubPlatform(t)
	cfgPath, stateDir := connectConfig(t, platform.srv.URL, "")
	cfg, _, err := loadConfig(cfgPath, noEnv)
	if err != nil {
		t.Fatal(err)
	}
	store, err := auth.OpenStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("bare Enter: disabled, with the explanatory note", func(t *testing.T) {
		stdin := bytes.NewBufferString("\n")
		br := bufio.NewReader(stdin)
		var stderr bytes.Buffer
		outcome, code := askMiningQuestion(stdin, br, &bytes.Buffer{}, &stderr, noEnv, cfg, store, true, true)
		if code != exitOK || outcome.enabled {
			t.Fatalf("got %+v code=%d, want disabled", outcome, code)
		}
		if !strings.Contains(stderr.String(), "already have mining enabled on another agent") {
			t.Fatalf("no explanatory note printed:\n%s", stderr.String())
		}
	})

	t.Run("explicit y: still overridable", func(t *testing.T) {
		stdin := bytes.NewBufferString("y\ntwilight1wx0rwcuexfwc36h0r2cg3fvfsds66f0qadt8cs\n")
		br := bufio.NewReader(stdin)
		var stderr bytes.Buffer
		outcome, code := askMiningQuestion(stdin, br, &bytes.Buffer{}, &stderr, noEnv, cfg, store, true, true)
		if code != exitOK || !outcome.enabled || outcome.payoutAddress != "twilight1wx0rwcuexfwc36h0r2cg3fvfsds66f0qadt8cs" {
			t.Fatalf("got %+v code=%d, want enabled — the note is a default, not a refusal", outcome, code)
		}
		if !strings.Contains(stderr.String(), "already have mining enabled on another agent") {
			t.Fatalf("no explanatory note printed even though it was overridden:\n%s", stderr.String())
		}
	})
}

// The scripted-install equivalent of the three answers above, driven by
// [mining] enabled/payout_address instead of a terminal.
func TestScriptedInstallMiningConfig(t *testing.T) {
	platform := newStubPlatform(t)

	t.Run("enabled=false", func(t *testing.T) {
		cfgPath, stateDir := scriptedMiningConfig(t, platform.srv.URL, "enabled = false\n")
		cfg, _, err := loadConfig(cfgPath, noEnv)
		if err != nil {
			t.Fatal(err)
		}
		store, err := auth.OpenStore(stateDir)
		if err != nil {
			t.Fatal(err)
		}
		outcome, code := askMiningQuestion(&bytes.Buffer{}, bufio.NewReader(&bytes.Buffer{}), &bytes.Buffer{}, &bytes.Buffer{}, noEnv, cfg, store, false, false)
		if code != exitOK || outcome.enabled {
			t.Fatalf("got %+v code=%d", outcome, code)
		}
	})

	t.Run("enabled=true with payout_address", func(t *testing.T) {
		cfgPath, stateDir := scriptedMiningConfig(t, platform.srv.URL, "enabled = true\npayout_address = \"twilight1xnxmhqt2l55flef42ks4sn0er6tv6yhyqx7wy8\"\n")
		cfg, _, err := loadConfig(cfgPath, noEnv)
		if err != nil {
			t.Fatal(err)
		}
		store, err := auth.OpenStore(stateDir)
		if err != nil {
			t.Fatal(err)
		}
		outcome, code := askMiningQuestion(&bytes.Buffer{}, bufio.NewReader(&bytes.Buffer{}), &bytes.Buffer{}, &bytes.Buffer{}, noEnv, cfg, store, false, false)
		if code != exitOK || !outcome.enabled || outcome.payoutAddress != "twilight1xnxmhqt2l55flef42ks4sn0er6tv6yhyqx7wy8" {
			t.Fatalf("got %+v code=%d", outcome, code)
		}
		if addr, ok, _ := store.LoadPayoutAddress(); !ok || addr != "twilight1xnxmhqt2l55flef42ks4sn0er6tv6yhyqx7wy8" {
			t.Fatalf("address not persisted: %q ok=%v", addr, ok)
		}
	})

	t.Run("enabled=true, no address, no terminal: no wallet, no error", func(t *testing.T) {
		cfgPath, stateDir := scriptedMiningConfig(t, platform.srv.URL, "enabled = true\n")
		cfg, _, err := loadConfig(cfgPath, noEnv)
		if err != nil {
			t.Fatal(err)
		}
		store, err := auth.OpenStore(stateDir)
		if err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		outcome, code := askMiningQuestion(&bytes.Buffer{}, bufio.NewReader(&bytes.Buffer{}), &out, &bytes.Buffer{}, noEnv, cfg, store, false, false)
		if code != exitOK {
			t.Fatalf("this is documented as NOT an error; got code=%d", code)
		}
		if !outcome.enabled || outcome.payoutAddress != "" {
			t.Fatalf("got %+v, want enabled with no address", outcome)
		}
		if !strings.Contains(out.String(), "no wallet was created") {
			t.Fatalf("no explanatory status line: %q", out.String())
		}
		if _, ok, _ := store.LoadPayoutAddress(); ok {
			t.Fatal("an address was persisted with none configured")
		}
	})
}

// TestNoWalletOnNonTerminalPath is the structural half of the scripted
// tests above: askMiningQuestion never reaches createWallet's passphrase
// prompt/mnemonic print at all when interactive is false, regardless of
// what [mining] says — proven by the "enabled=true, no address" case
// completing without ever blocking on stdin (the buffers above are
// empty; a read from them would return EOF, not hang, but a wallet
// creation attempt would still show up as "recovery phrase" in stdout,
// which the case above already asserts is absent).
func TestNoWalletOnNonTerminalPath(t *testing.T) {
	platform := newStubPlatform(t)
	cfgPath, stateDir := scriptedMiningConfig(t, platform.srv.URL, "enabled = true\n")
	cfg, _, err := loadConfig(cfgPath, noEnv)
	if err != nil {
		t.Fatal(err)
	}
	store, err := auth.OpenStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	walletDir := filepath.Join(stateDir, "..", "wallet")
	t.Setenv("TOKENDROP_WALLET_DIR", walletDir)
	if _, code := askMiningQuestion(&bytes.Buffer{}, bufio.NewReader(&bytes.Buffer{}), &bytes.Buffer{}, &bytes.Buffer{}, noEnv, cfg, store, false, false); code != exitOK {
		t.Fatalf("code=%d", code)
	}
	if _, err := os.Stat(filepath.Join(walletDir, walletKeyFile)); err == nil {
		t.Fatal("a wallet keyfile was created on the non-interactive path")
	}
}

// ── unattended declaration ──────────────────────────────────────────

// The declaration itself running unattended once enrollment succeeds is
// exercised end-to-end by TestConnectCaseAccountExistsSearchAndMining
// and TestConnectCaseMiningGrantedLater above (both assert
// as.declaredAddress()); this isolates the property that a payout
// address stored BEFORE enrollment is what a detached resume declares
// AFTER it, with no human present at either boundary.
func TestDeclarationRunsUnattendedAfterEnrollment(t *testing.T) {
	withShortConnectTimings(t)
	platform := newStubPlatform(t)
	as := newStubAS(t)
	cfgPath, stateDir := connectConfig(t, platform.srv.URL, as.srv.URL)

	if code, _, _ := runConnect(t, cfgPath, nil); code != exitOK {
		t.Fatal("connect failed")
	}
	store, err := auth.OpenStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SavePayoutAddress("twilight1lpdtlehaqn95mkcfgae8rut89s4pq9ayxdp4yc"); err != nil {
		t.Fatal(err)
	}
	platform.claim("mining")

	// The detached shape specifically: -resume, no stdin, exactly the
	// invocation search.go's spawnConnectResume makes.
	code, out, _ := runConnect(t, cfgPath, nil, "-resume")
	if code != exitOK {
		t.Fatalf("resume exited %d: %s", code, out)
	}
	if got := as.declaredAddress(); got != "twilight1lpdtlehaqn95mkcfgae8rut89s4pq9ayxdp4yc" {
		t.Fatalf("declared address = %q, want twilight1lpdtlehaqn95mkcfgae8rut89s4pq9ayxdp4yc", got)
	}
}

// WP4b (design f0ddb69 §5.5): "read before declaring". Three direct tests
// of declarePayoutIfSafe against stubOnboardingAS, plus one integration
// test (TestStatusReportsBothAddressesOnBindingConflict below) proving
// status surfaces the held case end to end.

func TestDeclareProceedsWhenNoActiveBinding(t *testing.T) {
	as := newStubAS(t) // activeAddress unset: GET answers 404, ErrNoPayoutDeclaration
	mining, stateDir := as.miningClient(t)
	store, err := auth.OpenStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	declarePayoutIfSafe(context.Background(), mining, store, "twilight1new", &stdout, &stderr)

	if got := as.declaredAddress(); got != "twilight1new" {
		t.Fatalf("declared address = %q, want twilight1new (no active binding, must proceed)", got)
	}
	if stderr.String() != "" {
		t.Fatalf("stderr: %s", stderr.String())
	}
}

// WP4b's "a declaration of the same address is not a change" makes a
// redundant re-declare SAFE; it does not make it necessary. WP2-review
// defect 2's efficiency question resolved to skipping the AS round trip
// entirely when the active binding already matches — declarePayoutIfSafe
// records the same outcome (SavePayoutDeclared, no held-binding note)
// without spending a PUT on it.
func TestDeclareProceedsWhenActiveMatchesLocal(t *testing.T) {
	as := newStubAS(t)
	as.setActiveAddress("twilight1same")
	mining, stateDir := as.miningClient(t)
	store, err := auth.OpenStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	declarePayoutIfSafe(context.Background(), mining, store, "twilight1same", &stdout, &stderr)

	if got := as.declarationAttempts(); got != 0 {
		t.Fatalf("declaration attempts = %d, want 0 (already active and matching; no redundant AS round trip needed)", got)
	}
	if declared, ok, derr := store.LoadPayoutDeclared(); derr != nil || !ok || declared != "twilight1same" {
		t.Fatalf("LoadPayoutDeclared() = %q ok=%v err=%v, want twilight1same recorded as settled", declared, ok, derr)
	}
	if _, ok, _ := store.LoadPayoutBindingHeld(); ok {
		t.Fatal("a matching declaration must not leave a held-binding note")
	}
}

func TestDeclareReadsStandingFirstAndSkipsWhenActiveDiffers(t *testing.T) {
	as := newStubAS(t)
	as.setActiveAddress("twilight1active")
	mining, stateDir := as.miningClient(t)
	store, err := auth.OpenStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	declarePayoutIfSafe(context.Background(), mining, store, "twilight1local", &stdout, &stderr)

	if got := as.declarationAttempts(); got != 0 {
		t.Fatalf("declaration attempts = %d, want 0 — a different active address must never be declared over blind", got)
	}
	held, ok, err := store.LoadPayoutBindingHeld()
	if err != nil || !ok || held.Local != "twilight1local" || held.Active != "twilight1active" {
		t.Fatalf("LoadPayoutBindingHeld() = %+v, ok=%v, err=%v", held, ok, err)
	}
	if !strings.Contains(stdout.String(), "twilight1active") || !strings.Contains(stdout.String(), "twilight1local") {
		t.Fatalf("stdout does not name both addresses: %s", stdout.String())
	}
}

// End to end: connect enrolls, declares against a stub AS that already
// has a DIFFERENT address active, and `status` shows both — without a
// second network round trip (printAgentIdentityStatus reads the store).
func TestStatusReportsBothAddressesOnBindingConflict(t *testing.T) {
	withShortConnectTimings(t)
	platform := newStubPlatform(t)
	as := newStubAS(t)
	as.setActiveAddress("twilight1operator")
	cfgPath, stateDir := connectConfig(t, platform.srv.URL, as.srv.URL)

	if code, _, _ := runConnect(t, cfgPath, nil); code != exitOK {
		t.Fatal("connect failed")
	}
	store, err := auth.OpenStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SavePayoutAddress("twilight1nv60etsw245g8z00h4h322m4vr764z3840swpa"); err != nil {
		t.Fatal(err)
	}
	platform.claim("mining")
	if code, out, _ := runConnect(t, cfgPath, nil, "-resume"); code != exitOK {
		t.Fatalf("resume exited %d: %s", code, out)
	}
	if got := as.declarationAttempts(); got != 0 {
		t.Fatalf("declaration attempts = %d, want 0 (active address differs)", got)
	}

	var stdout, stderr bytes.Buffer
	printAgentIdentityStatus([]string{"-config", cfgPath}, &stdout, &stderr, os.Getenv)
	if !strings.Contains(stdout.String(), "twilight1operator") || !strings.Contains(stdout.String(), "twilight1nv60etsw245g8z00h4h322m4vr764z3840swpa") {
		t.Fatalf("status does not name both addresses:\n%s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "HELD") {
		t.Fatalf("status does not say the binding is held:\n%s", stdout.String())
	}
}
