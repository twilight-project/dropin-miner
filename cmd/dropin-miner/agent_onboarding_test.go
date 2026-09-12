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
	"errors"
	"fmt"
	"io/fs"
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
	platformapi "github.com/twilight-project/dropin-miner/pkg/platform"
)

// ── stubs ──────────────────────────────────────────────────────────────

type stubPlatform struct {
	srv *httptest.Server

	mu         sync.Mutex
	status     string // "unclaimed" | "claimed" | "expired"
	scopes     []string
	slots      []string
	consoleURL string // "" means the stub omits console_url, matching an older platform

	registerCalls           int
	statusCalls             int
	enrollCalls             int
	meCalls                 int
	lastRequestedScopes     []string
	lastRequestedScopesSeen bool
	failNextRegisters       int
	dropNextRegisterBodies  int
	statusNotFound          bool
	statusError             bool
	statusByAgent           map[string]string
	scopesByAgent           map[string][]string
	agentByKey              map[string]string // bearer key -> agent id, for /v1/agents/me
	claimCodeByAgent        map[string]string
	meError                 bool // every /v1/agents/me call answers 500
	meOmitClaimFields       bool // /v1/agents/me never sends claim_url/claim_code, modeling B.1's known gap
}

func newStubPlatform(t *testing.T) *stubPlatform {
	t.Helper()
	f := &stubPlatform{
		status:           "unclaimed",
		slots:            []string{"twilight-slot-3"},
		statusByAgent:    make(map[string]string),
		scopesByAgent:    make(map[string][]string),
		agentByKey:       make(map[string]string),
		claimCodeByAgent: make(map[string]string),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/agents/register", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			RequestedScopes []string `json:"requested_scopes"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.registerCalls++
		f.lastRequestedScopes = body.RequestedScopes
		f.lastRequestedScopesSeen = true
		registerNumber := f.registerCalls
		if f.failNextRegisters > 0 {
			f.failNextRegisters--
			f.mu.Unlock()
			writeStubJSON(w, http.StatusInternalServerError, map[string]any{"error": map[string]any{"code": "internal"}})
			return
		}
		agentID := fmt.Sprintf("agent-%d", registerNumber)
		key := "sr-stubkey"
		claimCode := "AB12-CD34"
		if registerNumber > 1 {
			key = fmt.Sprintf("sr-stubkey-%d", registerNumber)
			claimCode = fmt.Sprintf("EF56-GH%02d", registerNumber)
		}
		registrationStatus := f.status
		registrationScopes := append([]string(nil), f.scopes...)
		if registrationStatus == "expired" {
			// An expired identity's replacement starts a fresh claim. Preserve
			// the stub's older pre-claim behavior for installer tests, where
			// claim is deliberately called before the first registration.
			registrationStatus = "unclaimed"
			registrationScopes = nil
		}
		f.statusByAgent[agentID] = registrationStatus
		f.scopesByAgent[agentID] = registrationScopes
		f.agentByKey[key] = agentID
		f.claimCodeByAgent[agentID] = claimCode
		drop := f.dropNextRegisterBodies > 0
		if drop {
			f.dropNextRegisterBodies--
		}
		f.mu.Unlock()
		if drop {
			if hj, ok := w.(http.Hijacker); ok {
				conn, _, err := hj.Hijack()
				if err == nil {
					_ = conn.Close()
				}
			}
			return
		}
		writeStubJSON(w, http.StatusCreated, map[string]any{
			"agent_id": agentID, "key": key,
			"claim_url": f.srv.URL + "/claim/" + claimCode, "claim_code": claimCode,
			"claim_expires_at": "2026-09-16T00:00:00Z",
			"poll":             map[string]any{"interval_s": 1},
			"tier":             "unclaimed",
		})
	})
	mux.HandleFunc("GET /v1/agents/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.statusCalls++
		id := strings.TrimPrefix(r.URL.Path, "/v1/agents/")
		st, scopes, slots, consoleURL := f.status, f.scopes, f.slots, f.consoleURL
		notFound := f.statusNotFound
		statusError := f.statusError
		if agentStatus, ok := f.statusByAgent[id]; ok {
			st = agentStatus
		}
		if agentScopes, ok := f.scopesByAgent[id]; ok {
			scopes = agentScopes
		}
		f.mu.Unlock()
		if notFound {
			writeStubJSON(w, http.StatusNotFound, map[string]any{"error": map[string]any{"code": "not_found"}})
			return
		}
		if statusError {
			writeStubJSON(w, http.StatusInternalServerError, map[string]any{"error": map[string]any{"code": "temporary"}})
			return
		}
		body := map[string]any{
			"status": st, "scopes": scopes,
			"mining": map[string]any{"available": len(slots) > 0, "slots": slots},
		}
		if consoleURL != "" {
			body["console_url"] = consoleURL
		}
		writeStubJSON(w, http.StatusOK, body)
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
	// GET /v1/agents/me (B.2): a by-key self-lookup, keyed on the bearer
	// the register handler above minted for that agent. 404 for a key
	// this stub never issued (or one it does not recognize as current —
	// modeling a revoked key, which the design gives the same answer as
	// an unknown one).
	mux.HandleFunc("GET /v1/agents/me", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.meCalls++
		meError := f.meError
		key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		id, known := f.agentByKey[key]
		var st string
		var scopes, slots []string
		var claimCode string
		if known {
			st = f.statusByAgent[id]
			scopes = f.scopesByAgent[id]
			slots = f.slots
			claimCode = f.claimCodeByAgent[id]
		}
		omitClaim := f.meOmitClaimFields
		f.mu.Unlock()
		if meError {
			writeStubJSON(w, http.StatusInternalServerError, map[string]any{"error": map[string]any{"code": "internal"}})
			return
		}
		if !known {
			writeStubJSON(w, http.StatusNotFound, map[string]any{"error": map[string]any{"code": "not_found"}})
			return
		}
		body := map[string]any{
			"agent_id": id,
			"status":   st, "scopes": scopes,
			"mining": map[string]any{"available": len(slots) > 0, "slots": slots},
		}
		if st == "unclaimed" && !omitClaim {
			body["claim_url"] = f.srv.URL + "/claim/" + claimCode
			body["claim_code"] = claimCode
		}
		writeStubJSON(w, http.StatusOK, body)
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
	for id := range f.statusByAgent {
		f.statusByAgent[id] = "claimed"
		f.scopesByAgent[id] = append([]string(nil), scopes...)
	}
}

// setStatus forces a status the ordinary lifecycle (claim) doesn't reach
// on its own — namely "expired".
func (f *stubPlatform) setStatus(status string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status = status
	for id := range f.statusByAgent {
		f.statusByAgent[id] = status
	}
}

func (f *stubPlatform) setStatusNotFound(notFound bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statusNotFound = notFound
}

func (f *stubPlatform) setStatusError(statusError bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statusError = statusError
}

// setSlots overrides the single-slot default (WP2-review judgment call 1:
// chooseSlot's more-than-one-offered path).
func (f *stubPlatform) setSlots(slots ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.slots = slots
}

// setConsoleURL makes the stub's poll response include console_url, as
// the real platform now does (found live testing §2.2's re-approval UX).
func (f *stubPlatform) setConsoleURL(url string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.consoleURL = url
}

func (f *stubPlatform) counts() (register, status, enroll int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.registerCalls, f.statusCalls, f.enrollCalls
}

func (f *stubPlatform) meCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.meCalls
}

// setMeError makes every /v1/agents/me call answer 500, modeling a
// transient platform failure during a rebuild attempt.
func (f *stubPlatform) setMeError(fail bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.meError = fail
}

// setMeOmitsClaimFields models B.1's known gap: /v1/agents/me answering
// for a still-unclaimed agent without claim_url/claim_code.
func (f *stubPlatform) setMeOmitsClaimFields(omit bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.meOmitClaimFields = omit
}

// requestedScopes returns the requested_scopes the most recent register
// call actually sent (nil if it sent none, or omitted the field
// entirely), and whether a register call has happened at all.
func (f *stubPlatform) requestedScopes() (scopes []string, seen bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastRequestedScopes, f.lastRequestedScopesSeen
}

// failNextRegister makes the next n register calls fail with a 500 —
// used to exercise a register call that fails after the mining question
// has already been answered and persisted.
func (f *stubPlatform) failNextRegister(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failNextRegisters = n
}

func (f *stubPlatform) dropNextRegisterResponse() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dropNextRegisterBodies++
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

	mu                   sync.Mutex
	declared             string
	activeAddress        string // "" means no active binding yet (GET answers 404)
	declareCalls         int
	tokenCalls           int
	assertionRedemptions int // /oauth/token calls that carried a non-empty assertion (the enrollment grant, as opposed to an ordinary refresh-token grant a second authenticated client needs)
	// declareOutcome overrides the PUT response's shape for the hold-shape
	// coverage tests (WP2-adversarial-review: declaration stubs answering
	// every hold shape, not only plain ACTIVE) — nil means the default
	// "always effective" behavior above.
	declareOutcome func(address string) map[string]any
	revokeCalls    int
	revokeFails    bool // mining disable's "the AS could not be reached" coverage
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
	mux.HandleFunc("POST /oauth/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		f.mu.Lock()
		n := f.tokenCalls
		f.tokenCalls++
		if r.Form.Get("assertion") != "" {
			f.assertionRedemptions++
		}
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"access_token":"at-%d","token_type":"DPoP","expires_in":900,"refresh_token":"rt-%d"}`, n, n)
	})
	mux.HandleFunc("PUT /v1/payout/declaration", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Address string `json:"address"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.declared = body.Address
		f.declareCalls++
		outcome := f.declareOutcome
		f.mu.Unlock()
		if outcome != nil {
			writeStubJSON(w, http.StatusOK, outcome(body.Address))
			return
		}
		writeStubJSON(w, http.StatusOK, map[string]any{
			"status": "ACTIVE", "address": body.Address, "canonical_address": body.Address,
			"effective": true, "declared_at": "2026-09-09T00:00:00Z",
		})
	})
	mux.HandleFunc("POST /oauth/revoke", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		f.mu.Lock()
		f.revokeCalls++
		fails := f.revokeFails
		f.mu.Unlock()
		if fails {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
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

// assertionRedemptionCount counts only /oauth/token calls that redeemed
// an enrollment assertion — distinct from the raw token-call count, which
// also includes ordinary refresh-token grants a freshly constructed
// *auth.OAuthClient makes on its own first authenticated request (every
// buildMiningClient call in connect.go — one for enrollment, a separate
// one for the declare step — constructs its own client instance).
func (f *stubOnboardingAS) assertionRedemptionCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.assertionRedemptions
}

func (f *stubOnboardingAS) revokeCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.revokeCalls
}

// setRevokeFails makes POST /oauth/revoke answer 500 — mining disable's
// "the AS could not be reached" path, without simulating a real network
// failure: any non-200 is what OAuthClient.Revoke treats as unconfirmed.
func (f *stubOnboardingAS) setRevokeFails(fails bool) {
	f.mu.Lock()
	f.revokeFails = fails
	f.mu.Unlock()
}

// setDeclareOutcome overrides what PUT /v1/payout/declaration answers, for
// the hold-shape coverage below (WP2-adversarial-review: declaration
// stubs answering every hold shape, not only plain ACTIVE).
func (f *stubOnboardingAS) setDeclareOutcome(fn func(address string) map[string]any) {
	f.mu.Lock()
	f.declareOutcome = fn
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
	fmt.Fprintf(&b, "[platform]\nbase_url = %q\nagents_api_url = %q\n", platformURL, platformURL)
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

func mustLoadConfig(t *testing.T, cfgPath string) *config.Config {
	t.Helper()
	cfg, _, err := loadConfig(cfgPath, noEnv)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
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
	body := fmt.Sprintf("[platform]\nbase_url = %q\nagents_api_url = %q\n\n[mining]\nstate_dir = %q\nas_url = \"https://as.example.invalid\"\nchain_id = \"twilight-1\"\nslot_id = 7\n%s\n",
		platformURL, platformURL, stateDir, extra)
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

	// First persist the claimed state through the detached poll path.
	if code, _, errOut := runConnect(t, cfgPath, nil, "-resume"); code != exitOK {
		t.Fatalf("claimed resume exited %d: %s", code, errOut)
	}
	// A later foreground connect picks up the stored claimed registration
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

	// First run: (non-interactive stdin, [mining] enabled explicit=true
	// with no address) the mining question resolves via config, silently,
	// with no address yet — then registers, hinting the scope it just
	// decided.
	if code, _, _ := runConnect(t, cfgPath, nil); code != exitOK {
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

// Case: claimed with search only, mining added later — a registration hint
// (a "no" at the terminal, here) is not binding on the platform; the
// granted scope alone triggers enrollment on the next poll (design
// decision 3).
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

	if code, _, _ := runConnect(t, cfgPath, nil); code != exitOK {
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

	if code, _, _ := runConnect(t, cfgPath, nil); code != exitOK {
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
	}, []string{"-config", routerCfg, "-no-flush", "hello"}, strings.NewReader(""), &out, &errOut, noEnv)
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

	t.Run("not enrolled, decision on file is off: settled", func(t *testing.T) {
		stateDir := shouldResumeFixture(t, "claimed", []string{"mining"}, false, "")
		// ASBaseURL IS set here — isolates "opted out" from "no AS
		// configured" below, so deleting either check independently is
		// each caught by a different subtest. The decision file, not
		// config, is what miningActive reads now — this models what
		// askMiningQuestion actually persists for a real opt-out
		// (mining_decision.json), not a config-only Enabled value that
		// nothing but the (now-removed) config fallback ever consulted.
		store, err := auth.OpenStore(stateDir)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.SaveMiningEnabled(false); err != nil {
			t.Fatal(err)
		}
		cfg := config.Mining{StateDir: stateDir, ASBaseURL: "https://as.example"}
		if shouldResume(&config.Config{Mining: cfg}) {
			t.Fatal("shouldResume true despite a stored decision of mining off")
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

	// The actionable cases must still spawn. Both need a decision of
	// "on" explicitly on file now — an absent decision reads as stopped
	// (miningActive's own default), matching a real install: connect
	// always asks before either of these states is reachable.
	t.Run("still actionable: not yet enrolled, configured", func(t *testing.T) {
		stateDir := shouldResumeFixture(t, "claimed", []string{"mining"}, false, "")
		store, err := auth.OpenStore(stateDir)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.SaveMiningEnabled(true); err != nil {
			t.Fatal(err)
		}
		cfg := configured
		cfg.StateDir = stateDir
		if !shouldResume(&config.Config{Mining: cfg}) {
			t.Fatal("shouldResume false for an installation that can still enroll")
		}
	})

	t.Run("still actionable: enrolled with an undeclared address", func(t *testing.T) {
		stateDir := shouldResumeFixture(t, "claimed", []string{"mining"}, true, "twilight16hmu7hucv64zeanfsa58cjdhku98k43j4cnrkr")
		store, err := auth.OpenStore(stateDir)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.SaveMiningEnabled(true); err != nil {
			t.Fatal(err)
		}
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

// Design item 1: mining_decision.json is the only thing miningActive
// ever reads, so a fresh install must never be in a "no decision"
// state. askMiningQuestion writes it on every path that reaches it,
// whether a terminal answered or a scripted config did — the exact
// requirement design item 1 states as "the installer writes it for
// both answers of the terminal question; a scripted install writes it
// from [mining] enabled at install time."
func TestAFreshInstallAlwaysHasADecisionOnFile(t *testing.T) {
	platform := newStubPlatform(t)

	for _, tc := range []struct {
		name        string
		interactive bool
		stdin       string
		scripted    string
		wantEnabled bool
	}{
		{"interactive yes", true, "y\ntwilight1k5stzqa2sgvfgx9u04cv93pek3gcmm9h5t9hkn\n", "", true},
		{"interactive no", true, "n\n", "", false},
		{"scripted enabled=true", false, "", "enabled = true\npayout_address = \"twilight1xnxmhqt2l55flef42ks4sn0er6tv6yhyqx7wy8\"\n", true},
		{"scripted enabled=false", false, "", "enabled = false\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var cfgPath, stateDir string
			if tc.interactive {
				cfgPath, stateDir = connectConfig(t, platform.srv.URL, "")
			} else {
				cfgPath, stateDir = scriptedMiningConfig(t, platform.srv.URL, tc.scripted)
			}
			cfg, _, err := loadConfig(cfgPath, noEnv)
			if err != nil {
				t.Fatal(err)
			}
			store, err := auth.OpenStore(stateDir)
			if err != nil {
				t.Fatal(err)
			}
			stdin := bytes.NewBufferString(tc.stdin)
			br := bufio.NewReader(stdin)
			if _, code := askMiningQuestion(stdin, br, &bytes.Buffer{}, &bytes.Buffer{}, noEnv, cfg, store, tc.interactive, false); code != exitOK {
				t.Fatalf("code=%d", code)
			}
			enabled, ok, err := store.LoadMiningEnabled()
			if err != nil || !ok {
				t.Fatalf("no decision on file: enabled=%v ok=%v err=%v", enabled, ok, err)
			}
			if enabled != tc.wantEnabled {
				t.Fatalf("decision on file = %v, want %v", enabled, tc.wantEnabled)
			}
		})
	}
}

// The other half of "never in a no-decision state": a state directory
// nothing has ever decided anything about — no connect, no mining
// enable, no scripted config ever ran askMiningQuestion against it —
// reads as stopped, not active. This used to default active for the
// legacy installers' sake (setup.sh/install.ps1 wrote [mining] enabled
// = true and enrolled without ever running connect); now that both
// installers run connect first, an absent file means only "never
// decided," and that is not the same as "on."
func TestAnInstallationWithNoDecisionFileAtAllIsStopped(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := auth.OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.LoadMiningEnabled(); err != nil || ok {
		t.Fatalf("setup: expected no decision file, got ok=%v err=%v", ok, err)
	}
	if miningActive(store) {
		t.Fatal("miningActive true with no decision file at all; want false")
	}
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

// ── WP2-adversarial-review coverage ─────────────────────────────────────

// Finding 1: N concurrent connect -resume invocations against one
// already-claimed, mining-scoped, undeclared registration must produce
// exactly one platform enroll call, one AS token redemption, and one
// declaration — connect.lock serializes them, and finding 6's re-read
// means every invocation after the first sees the winner's result already
// on disk and does nothing further. Goroutines contend on connect.lock
// mining enable's re-approval message had never been driven at the
// cmdMining level at all (only askMiningQuestion, a level below it) —
// which is how a dead one-time claim_url shipped as that message
// unnoticed until a live run against the real platform 404'd on it
// (search-router's handleAgentClaim refuses any resubmission outright,
// even by the original owner). The fix points at the platform's generic
// claim address instead (reachable console grant, search-router commit
// d20a6ac) — this pins BOTH that the dead per-code URL is gone AND that
// the generic address present.
func TestMiningEnableReApprovalPointsAtTheGenericClaimAddressNotTheDeadOneTimeURL(t *testing.T) {
	platform := newStubPlatform(t)
	cfgPath, stateDir := connectConfig(t, platform.srv.URL, "https://as.example.invalid")
	cfg, _, err := loadConfig(cfgPath, noEnv)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(credentialsPath(cfg.Miner)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeCredentials(credentialsPath(cfg.Miner), credentials{APIKey: "sr-key"}); err != nil {
		t.Fatal(err)
	}
	store, err := auth.OpenStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	deadClaimURL := platform.srv.URL + "/claim/AB12-CD34"
	if err := store.SaveAgentRegistration(auth.AgentRegistration{
		AgentID: "agent-1", Status: "claimed", Scopes: []string{"credits"}, ClaimURL: deadClaimURL,
	}); err != nil {
		t.Fatal(err)
	}
	platform.claim("credits") // claimed, credits only — mining not granted

	var stdout bytes.Buffer
	code := cmdMining([]string{"enable", "-config", cfgPath}, &bytes.Buffer{}, &stdout, &bytes.Buffer{}, noEnv)
	if code != exitOK {
		t.Fatalf("cmdMining enable: code=%d stdout=%s", code, stdout.String())
	}
	out := stdout.String()
	wantAddr := cfg.Platform.BaseURL + "/claim"
	if !strings.Contains(out, wantAddr) {
		t.Errorf("stdout does not name the generic claim address %q:\n%s", wantAddr, out)
	}
	if strings.Contains(out, deadClaimURL) {
		t.Errorf("stdout still names the dead, already-consumed one-time claim URL:\n%s", out)
	}
}

// When the platform's poll response carries console_url (confirmed live:
// search-router added this after the generic-claim-address fix above,
// specifically to remove the "submit a doomed code first" step), mining
// enable's re-approval message must prefer it over the generic address —
// the whole point of shipping it.
func TestMiningEnableReApprovalPrefersTheRealConsoleURLWhenThePlatformSendsOne(t *testing.T) {
	platform := newStubPlatform(t)
	cfgPath, stateDir := connectConfig(t, platform.srv.URL, "https://as.example.invalid")
	cfg, _, err := loadConfig(cfgPath, noEnv)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(credentialsPath(cfg.Miner)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeCredentials(credentialsPath(cfg.Miner), credentials{APIKey: "sr-key"}); err != nil {
		t.Fatal(err)
	}
	store, err := auth.OpenStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveAgentRegistration(auth.AgentRegistration{
		AgentID: "agent-1", Status: "claimed", Scopes: []string{"credits"},
		ClaimURL: platform.srv.URL + "/claim/AB12-CD34",
	}); err != nil {
		t.Fatal(err)
	}
	platform.claim("credits") // claimed, credits only — mining not granted
	realConsoleURL := platform.srv.URL + "/projects/8fe850f9-eb9a-4a80-b9c8-7341bd346a48"
	platform.setConsoleURL(realConsoleURL)

	var stdout bytes.Buffer
	code := cmdMining([]string{"enable", "-config", cfgPath}, &bytes.Buffer{}, &stdout, &bytes.Buffer{}, noEnv)
	if code != exitOK {
		t.Fatalf("cmdMining enable: code=%d stdout=%s", code, stdout.String())
	}
	out := stdout.String()
	if !strings.Contains(out, realConsoleURL) {
		t.Errorf("stdout does not name the real console_url %q:\n%s", realConsoleURL, out)
	}
	genericAddr := cfg.Platform.BaseURL + "/claim"
	if strings.Contains(out, genericAddr) {
		t.Errorf("stdout fell back to the generic claim address %q even though a real console_url was available:\n%s", genericAddr, out)
	}
}

// An agent that was never claimed at all has a DIFFERENT, still-valid
// claim link — reg.ClaimURL hasn't been consumed by anything yet, unlike
// the already-claimed re-approval case above. Printing the console/generic
// fallback here would be worse, not just different: it lands the human on
// a bare claim form with no code to type, when they already have a working
// link. Found tracing the code after the console_url fix, not live —
// worth pinning before it becomes a live surprise the way the dead
// re-approval URL was.
func TestMiningEnableOnAnUnclaimedAgentPrintsTheOriginalStillValidClaimLink(t *testing.T) {
	platform := newStubPlatform(t) // default status: "unclaimed"
	platform.setConsoleURL(platform.srv.URL + "/projects/should-not-be-used")
	cfgPath, stateDir := connectConfig(t, platform.srv.URL, "https://as.example.invalid")
	cfg, _, err := loadConfig(cfgPath, noEnv)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(credentialsPath(cfg.Miner)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeCredentials(credentialsPath(cfg.Miner), credentials{APIKey: "sr-key"}); err != nil {
		t.Fatal(err)
	}
	store, err := auth.OpenStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	originalClaimURL := platform.srv.URL + "/claim/AB12-CD34"
	if err := store.SaveAgentRegistration(auth.AgentRegistration{
		AgentID: "agent-1", Status: "unclaimed", ClaimURL: originalClaimURL,
	}); err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	code := cmdMining([]string{"enable", "-config", cfgPath}, &bytes.Buffer{}, &stdout, &bytes.Buffer{}, noEnv)
	if code != exitOK {
		t.Fatalf("cmdMining enable: code=%d stdout=%s", code, stdout.String())
	}
	out := stdout.String()
	if !strings.Contains(out, originalClaimURL) {
		t.Errorf("stdout does not name the original, still-valid claim URL %q:\n%s", originalClaimURL, out)
	}
	if strings.Contains(out, "should-not-be-used") {
		t.Errorf("stdout used the console_url fallback for an unclaimed agent, which has no project yet:\n%s", out)
	}
	// The bare generic address (cfg.Platform.BaseURL+"/claim") is a
	// substring of originalClaimURL too, sharing the same origin — so the
	// wording, not a URL substring check, is what actually distinguishes
	// "unclaimed" from the already-claimed re-approval branch.
	if strings.Contains(out, "Sign in and grant it") {
		t.Errorf("stdout used the already-claimed re-approval wording for a never-claimed agent:\n%s", out)
	}
}

// An expired registration has no valid path forward at all — not the
// original link (its window closed), not a console grant (nothing was
// ever claimed to grant a scope on). Matches pollOnce's identical wording
// for the same state elsewhere.
func TestMiningEnableOnAnExpiredRegistrationSaysToRegisterAgain(t *testing.T) {
	platform := newStubPlatform(t)
	platform.setStatus("expired")
	cfgPath, stateDir := connectConfig(t, platform.srv.URL, "https://as.example.invalid")
	cfg, _, err := loadConfig(cfgPath, noEnv)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(credentialsPath(cfg.Miner)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeCredentials(credentialsPath(cfg.Miner), credentials{APIKey: "sr-key"}); err != nil {
		t.Fatal(err)
	}
	store, err := auth.OpenStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	deadClaimURL := platform.srv.URL + "/claim/AB12-CD34"
	if err := store.SaveAgentRegistration(auth.AgentRegistration{
		AgentID: "agent-1", Status: "expired", ClaimURL: deadClaimURL,
	}); err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	code := cmdMining([]string{"enable", "-config", cfgPath}, &bytes.Buffer{}, &stdout, &bytes.Buffer{}, noEnv)
	if code != exitOK {
		t.Fatalf("cmdMining enable: code=%d stdout=%s", code, stdout.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "run `dropin-miner connect` again for a new one") {
		t.Errorf("stdout does not say to register again:\n%s", out)
	}
	if strings.Contains(out, deadClaimURL) {
		t.Errorf("stdout still names the expired claim URL:\n%s", out)
	}
}

// exactly as separate processes would (minerlock_unix.go: a flock belongs
// to the open file description, not the process), so this is a faithful
// in-process model of what search's detached spawns actually do.
func TestConcurrentResumesProduceExactlyOneEnrollmentAndDeclaration(t *testing.T) {
	withShortConnectTimings(t)
	platform := newStubPlatform(t)
	as := newStubAS(t)
	cfgPath, stateDir := connectConfig(t, platform.srv.URL, as.srv.URL)
	cfg, _, err := loadConfig(cfgPath, noEnv)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(credentialsPath(cfg.Miner)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeCredentials(credentialsPath(cfg.Miner), credentials{APIKey: "sr-key"}); err != nil {
		t.Fatal(err)
	}

	store, err := auth.OpenStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveAgentRegistration(auth.AgentRegistration{
		AgentID: "agent-1", Status: "claimed", Scopes: []string{"search", "mining"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveMiningEnabled(true); err != nil {
		t.Fatal(err)
	}
	const addr = "twilight1wx0rwcuexfwc36h0r2cg3fvfsds66f0qadt8cs"
	if err := store.SavePayoutAddress(addr); err != nil {
		t.Fatal(err)
	}
	platform.claim("mining")

	const n = 10
	var wg sync.WaitGroup
	codes := make([]int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i] = cmdConnect([]string{"-config", cfgPath, "-resume"}, &bytes.Buffer{}, &bytes.Buffer{}, &bytes.Buffer{}, noEnv)
		}(i)
	}
	wg.Wait()

	_, _, enrollCalls := platform.counts()
	if enrollCalls != 1 {
		t.Fatalf("platform /v1/agents/enroll calls = %d, want exactly 1", enrollCalls)
	}
	if got := as.assertionRedemptionCount(); got != 1 {
		t.Fatalf("AS enrollment-assertion redemptions = %d, want exactly 1", got)
	}
	if got := as.declarationAttempts(); got != 1 {
		t.Fatalf("AS declaration attempts = %d, want exactly 1", got)
	}
	if got := as.declaredAddress(); got != addr {
		t.Fatalf("declared address = %q, want %q", got, addr)
	}
	reg, ok, err := store.LoadAgentRegistration()
	if err != nil || !ok || reg.LastEnrollmentSlot == "" {
		t.Fatalf("registration not enrolled after the storm: %+v ok=%v err=%v", reg, ok, err)
	}
	for i, c := range codes {
		if c != exitOK {
			t.Errorf("resume %d exited %d, want %d", i, c, exitOK)
		}
	}
}

// Finding 7: declarePayoutIfSafe must honor the DECLARE call's own
// returned document, not just the pre-read — a hold can surface only on
// the declare response itself (ADDRESS_IN_USE: a different PARTICIPANT
// already holds this exact address, which read-before-declare's own
// PayoutStanding check has no way to see) or from a race between the
// pre-read and the declare call (REPLACES_ACTIVE, mid-flight). Both must
// be recorded as a hold with the AS's real reason, never as "declared."
func TestDeclareHonorsEveryHoldShapeTheASReturns(t *testing.T) {
	cases := []struct {
		name   string
		reason string
	}{
		{"address in use by another participant", auth.HeldAddressInUse},
		{"replaces active, raced with the pre-read", auth.HeldReplacesActive},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			as := newStubAS(t)
			as.setDeclareOutcome(func(address string) map[string]any {
				return map[string]any{
					"status": "PENDING", "address": address, "canonical_address": address,
					"effective": false, "declared_at": "2026-09-09T00:00:00Z", "held_for": tc.reason,
				}
			})
			mining, stateDir := as.miningClient(t)
			store, err := auth.OpenStore(stateDir)
			if err != nil {
				t.Fatal(err)
			}
			const addr = "twilight1uwew6p63453wm0znz723lrneuls4xy29swp89n"
			var stdout, stderr bytes.Buffer
			declarePayoutIfSafe(context.Background(), mining, store, addr, &stdout, &stderr)

			if _, ok, _ := store.LoadPayoutDeclared(); ok {
				t.Fatal("a held declaration was recorded as settled")
			}
			held, ok, err := store.LoadPayoutBindingHeld()
			if err != nil || !ok {
				t.Fatalf("hold not recorded: ok=%v err=%v", ok, err)
			}
			if held.HeldFor != tc.reason {
				t.Fatalf("held.HeldFor = %q, want %q", held.HeldFor, tc.reason)
			}
			if !addressSettled(store, addr) {
				// finding 8: a hold is NOT terminal — but that's covered
				// elsewhere; this test only needs the hold itself recorded
				// with the right reason, not the settlement question.
				t.Log("addressSettled correctly still false for a held address (finding 8)")
			}
		})
	}
}

// Finding 4, end to end: a "y" answered at an interactive terminal must
// actually drive an enrollment, not just produce the right in-memory
// outcome struct. isInteractive cannot be faked with a plain
// *bytes.Buffer (it requires a real *os.File character device — the same
// constraint askMiningQuestion's own doc comment and every other test in
// this file already work around by forcing the boolean), so this
// continues past where the unit-level installer-question tests stop:
// askMiningQuestion's outcome feeds directly into pollOnce, and the stub
// AS is checked for an actual token redemption and declaration, not just
// the returned struct.
func TestTerminalYesIsFollowedThroughToAnActualEnrollment(t *testing.T) {
	platform := newStubPlatform(t)
	as := newStubAS(t)
	cfgPath, stateDir := connectConfig(t, platform.srv.URL, as.srv.URL)
	cfg, _, err := loadConfig(cfgPath, noEnv)
	if err != nil {
		t.Fatal(err)
	}
	store, err := auth.OpenStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(credentialsPath(cfg.Miner)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeCredentials(credentialsPath(cfg.Miner), credentials{APIKey: "sr-key"}); err != nil {
		t.Fatal(err)
	}
	platform.claim("mining")

	// connectConfig writes `enabled = true` explicitly (setup.sh's own
	// shape) — finding 4's exact regression: with that already in the
	// file, the "enable mining?" question must NOT run again (the file
	// already answered it), but the ADDRESS question must still reach the
	// terminal. One line of stdin, not two: an "n"/"y" here would be
	// consumed as the address instead, since there is no enable question
	// left to answer.
	const addr = "twilight1k5stzqa2sgvfgx9u04cv93pek3gcmm9h5t9hkn"
	stdin := bytes.NewBufferString(addr + "\n")
	br := bufio.NewReader(stdin)
	outcome, code := askMiningQuestion(stdin, br, &bytes.Buffer{}, &bytes.Buffer{}, noEnv, cfg, store, true, false)
	if code != exitOK || !outcome.enabled || outcome.payoutAddress != addr {
		t.Fatalf("askMiningQuestion: got %+v code=%d", outcome, code)
	}

	if err := store.SaveAgentRegistration(auth.AgentRegistration{
		AgentID: "agent-1", Status: "claimed", Scopes: []string{"search", "mining"},
	}); err != nil {
		t.Fatal(err)
	}
	if code, out, errOut := runConnect(t, cfgPath, nil, "-resume"); code != exitOK {
		t.Fatalf("resume exited %d: stdout=%s stderr=%s", code, out, errOut)
	}

	if got := as.assertionRedemptionCount(); got != 1 {
		t.Fatalf("the terminal-driven address answer never reached an actual AS enrollment redemption: redemptions = %d", got)
	}
	if got := as.declaredAddress(); got != addr {
		t.Fatalf("the terminal-typed address was never declared: declared = %q, want %q", got, addr)
	}
}

// WP2-adversarial-review finding 14: an existing credentials.json that
// already looks like a platform key must not be silently replaced by a
// fresh registration's key.
func TestConnectRefusesToOverwriteAnExistingCredentialsFileUnlessForced(t *testing.T) {
	withShortConnectTimings(t)
	platform := newStubPlatform(t)
	cfgPath, stateDir := connectConfig(t, platform.srv.URL, "")
	cfg, _, err := loadConfig(cfgPath, noEnv)
	if err != nil {
		t.Fatal(err)
	}
	credPath := credentialsPath(cfg.Miner)
	if err := os.MkdirAll(filepath.Dir(credPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeCredentials(credPath, credentials{APIKey: "sr-existing-real-key"}); err != nil {
		t.Fatal(err)
	}

	if code, _, errOut := runConnect(t, cfgPath, nil); code == exitOK {
		t.Fatalf("connect overwrote an existing credentials.json without -force; stderr=%q", errOut)
	}
	if raw, err := os.ReadFile(credPath); err != nil || !strings.Contains(string(raw), "sr-existing-real-key") { // #nosec G304 -- the test's own credentialsPath output, not an external path
		t.Fatalf("credentials.json was modified despite the refusal: %v %q", err, raw)
	}
	if _, ok := loadAgent(t, stateDir); ok {
		t.Fatal("a registration was persisted despite the refusal")
	}
	if registerCalls, _, _ := platform.counts(); registerCalls != 0 {
		t.Fatalf("Register calls = %d, want 0", registerCalls)
	}

	if code, _, errOut := runConnect(t, cfgPath, nil, "-force"); code != exitOK {
		t.Fatalf("connect -force still refused: %d %s", code, errOut)
	}
	if raw, err := os.ReadFile(credPath); err != nil || !strings.Contains(string(raw), "sr-stubkey") { // #nosec G304 -- the test's own credentialsPath output, not an external path
		t.Fatalf("-force did not overwrite: %v %q", err, raw)
	}
}

// WP2-adversarial-review finding 16: status is not a failure for the two
// most ordinary agent-onboarding states.
func TestStatusExitsZeroForUnclaimedAndSearchOnly(t *testing.T) {
	withShortConnectTimings(t)
	platform := newStubPlatform(t)
	cfgPath, _ := connectConfig(t, platform.srv.URL, "") // no [mining] block at all
	if code, _, errOut := runConnect(t, cfgPath, nil); code != exitOK {
		t.Fatalf("connect failed: %s", errOut)
	}
	needsMining := printAgentIdentityStatus([]string{"-config", cfgPath}, &bytes.Buffer{}, &bytes.Buffer{}, noEnv)
	if needsMining {
		t.Fatal("an unclaimed registration with no [mining] block should not need the AS-facing report")
	}

	platform.claim("credits") // claimed, no mining scope: search-only
	if code, _, errOut := runConnect(t, cfgPath, nil, "-resume"); code != exitOK {
		t.Fatalf("resume failed: %s", errOut)
	}
	needsMining = printAgentIdentityStatus([]string{"-config", cfgPath}, &bytes.Buffer{}, &bytes.Buffer{}, noEnv)
	if needsMining {
		t.Fatal("a search-only claim should not need the AS-facing report either")
	}
}

// A corrupt registration with no platform credential can proceed, but the
// unreadable evidence must be preserved rather than silently overwritten.
func TestConnectPreservesCorruptRegistrationWithoutPlatformCredential(t *testing.T) {
	withShortConnectTimings(t)
	platform := newStubPlatform(t)
	cfgPath, stateDir := connectConfig(t, platform.srv.URL, "")
	corrupt := []byte("{not json")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "agent.json"), corrupt, 0o600); err != nil {
		t.Fatal(err)
	}

	code, _, errOut := runConnect(t, cfgPath, nil)
	if code != exitOK {
		t.Fatalf("connect did not recover with no platform credential: exit %d, stderr=%s", code, errOut)
	}
	if !strings.Contains(errOut, "could not be decoded") {
		t.Fatalf("no acknowledgment of the corrupt record on stderr: %q", errOut)
	}
	reg, ok := loadAgent(t, stateDir)
	if !ok || reg.AgentID != "agent-1" || reg.Status != "unclaimed" {
		t.Fatalf("did not persist the fresh registration: %+v ok=%v", reg, ok)
	}
	if got, err := os.ReadFile(filepath.Join(stateDir, "agent.json.corrupt")); err != nil || string(got) != string(corrupt) { // #nosec G304 -- test controls its temporary state directory
		t.Fatalf("corrupt registration evidence was not preserved: err=%v contents=%q", err, got)
	}
	if registerCalls, _, _ := platform.counts(); registerCalls != 1 {
		t.Fatalf("Register calls = %d, want 1", registerCalls)
	}
}

// A corrupt registration beside a platform credential the platform does not
// recognize (revoked, or never actually registered — /v1/agents/me answers
// 404 either way, §5.2's deliberate no-oracle answer) is refused exactly as
// a plain fresh-registration attempt would refuse it, naming -force. B.3
// added the /me rebuild attempt in between, but a 404 leaves this test's
// outcome unchanged: still zero Register calls, still the credential
// conflict message, still the corrupt bytes and the existing credential
// both untouched until -force authorizes a replacement.
func TestConnectRefusesCorruptRegistrationWithExistingPlatformCredential(t *testing.T) {
	withShortConnectTimings(t)
	platform := newStubPlatform(t)
	cfgPath, stateDir := connectConfig(t, platform.srv.URL, "")
	cfg, _, err := loadConfig(cfgPath, noEnv)
	if err != nil {
		t.Fatal(err)
	}
	corrupt := []byte("{not json")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "agent.json"), corrupt, 0o600); err != nil {
		t.Fatal(err)
	}
	credPath := credentialsPath(cfg.Miner)
	if err := os.MkdirAll(filepath.Dir(credPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeCredentials(credPath, credentials{APIKey: "sr-existing-key"}); err != nil { // #nosec G101 -- canned test credential
		t.Fatal(err)
	}
	store, err := auth.OpenStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveMiningEnabled(false); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkHealth(auth.HealthCapture, auth.HealthIntakeUnwritable, "preserve capture health"); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkHealth(auth.HealthFlush, auth.HealthSubmissionFailed, "preserve flush health"); err != nil {
		t.Fatal(err)
	}

	code, _, errOut := runConnect(t, cfgPath, nil)
	if code == exitOK {
		t.Fatalf("connect succeeded despite corrupt registration and existing credential")
	}
	if registerCalls, _, _ := platform.counts(); registerCalls != 0 {
		t.Fatalf("Register calls = %d, want 0", registerCalls)
	}
	if platform.meCallCount() != 1 {
		t.Fatalf("/v1/agents/me calls = %d, want exactly 1 (the rebuild attempt, which came back 404)", platform.meCallCount())
	}
	if !strings.Contains(errOut, "credentials.json already holds a platform key") {
		t.Fatalf("missing credential-conflict diagnostic: %q", errOut)
	}
	if !strings.Contains(errOut, "-force") {
		t.Fatalf("missing -force guidance: %q", errOut)
	}
	if got, err := os.ReadFile(filepath.Join(stateDir, "agent.json")); err != nil || string(got) != string(corrupt) { // #nosec G304 -- test controls its temporary state directory
		t.Fatalf("corrupt registration was modified: err=%v contents=%q", err, got)
	}
	if got, err := os.ReadFile(credPath); err != nil || !strings.Contains(string(got), "sr-existing-key") { // #nosec G304 -- test controls its temporary credential path
		t.Fatalf("existing credential was modified: err=%v contents=%q", err, got)
	}

	if code, _, errOut := runConnect(t, cfgPath, nil, "-force"); code != exitOK {
		t.Fatalf("connect -force did not authorize replacement: code=%d stderr=%s", code, errOut)
	}
	if registerCalls, _, _ := platform.counts(); registerCalls != 1 {
		t.Fatalf("Register calls after -force = %d, want 1", registerCalls)
	}
	if platform.meCallCount() != 1 {
		t.Fatalf("/v1/agents/me calls after -force = %d, want still exactly 1 (-force bypasses the rebuild entirely)", platform.meCallCount())
	}
	reg, ok := loadAgent(t, stateDir)
	if !ok || reg.AgentID != "agent-1" {
		t.Fatalf("fresh registration was not persisted after -force: %+v ok=%v", reg, ok)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "agent.json.corrupt")); err != nil {
		t.Fatalf("corrupt registration evidence was not preserved after -force: %v", err)
	}
	if decision := store.ReadMiningDecision(); decision.State != auth.MiningDisabled {
		t.Fatalf("-force changed the persisted mining decision: %+v", decision)
	}
	if _, ok, err := store.LoadHealth(auth.HealthCapture); err != nil || !ok {
		t.Fatalf("-force cleared capture health: ok=%v err=%v", ok, err)
	}
	if _, ok, err := store.LoadHealth(auth.HealthFlush); err != nil || !ok {
		t.Fatalf("-force cleared flush health: ok=%v err=%v", ok, err)
	}
}

// ── B.3: rebuilding a lost or unreadable registration from the platform ──

// registerAgent drives a real Register call against the stub, independent
// of connectRun, so a rebuild test can start from a credential the
// platform genuinely recognizes (agentByKey/statusByAgent both populated)
// rather than one hand-inserted into the stub's maps.
func registerAgent(t *testing.T, platform *stubPlatform) (agentID, key string) {
	t.Helper()
	c := platformapi.New(platform.srv.URL, platform.srv.URL)
	reg, err := c.Register(context.Background(), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	return reg.AgentID, reg.Key
}

// setupLostRegistration writes key to credentials.json and, when corrupt
// is true, an undecodable agent.json beside it (returning those bytes for
// the caller to assert against later); when corrupt is false, agent.json
// is simply absent. Either way this models "a platform credential is
// stored, but the local registration is gone."
func setupLostRegistration(t *testing.T, cfg *config.Config, key string, corrupt bool) []byte {
	t.Helper()
	stateDir := cfg.Mining.StateDir
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	credPath := credentialsPath(cfg.Miner)
	if err := os.MkdirAll(filepath.Dir(credPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeCredentials(credPath, credentials{APIKey: key}); err != nil {
		t.Fatal(err)
	}
	if !corrupt {
		return nil
	}
	corruptBytes := []byte("{not json")
	if err := os.WriteFile(filepath.Join(stateDir, "agent.json"), corruptBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	return corruptBytes
}

// A claimed-with-mining identity, rebuilt from /v1/agents/me, continues
// straight into the ordinary enrollment flow — no Register, and (for the
// corrupt case) the undecodable bytes are preserved only after /me
// actually answered, never before.
func TestConnectRebuildsClaimedRegistrationFromThePlatform(t *testing.T) {
	for _, corrupt := range []bool{true, false} {
		corrupt := corrupt
		name := "absent"
		if corrupt {
			name = "corrupt"
		}
		t.Run(name, func(t *testing.T) {
			withShortConnectTimings(t)
			platform := newStubPlatform(t)
			as := newStubAS(t)
			cfgPath, stateDir := connectConfig(t, platform.srv.URL, as.srv.URL)
			cfg := mustLoadConfig(t, cfgPath)

			agentID, key := registerAgent(t, platform)
			platform.claim("mining")
			setupLostRegistration(t, cfg, key, corrupt)
			// This installation already decided to mine on a prior run
			// (the same run that originally registered, before agent.json
			// was lost) — the rebuild resumes an existing decision, it
			// does not re-ask it.
			store, err := auth.OpenStore(cfg.Mining.StateDir)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.SaveMiningEnabled(true); err != nil {
				t.Fatal(err)
			}
			corruptBytes, _ := os.ReadFile(filepath.Join(cfg.Mining.StateDir, "agent.json")) // #nosec G304 -- test controls its temporary state directory
			registerCallsBefore, _, _ := platform.counts()

			code, out, errOut := runConnect(t, cfgPath, nil)
			if code != exitOK {
				t.Fatalf("connect did not rebuild: code=%d stderr=%s", code, errOut)
			}
			if !strings.Contains(out, "enrolled for mining on") {
				t.Fatalf("did not continue into enrollment: stdout=%q", out)
			}
			reg, ok := loadAgent(t, stateDir)
			if !ok || reg.AgentID != agentID || reg.Status != "claimed" || reg.LastEnrollmentSlot == "" {
				t.Fatalf("rebuilt registration not as expected: %+v ok=%v", reg, ok)
			}
			if registerCalls, _, enrollCalls := platform.counts(); registerCalls != registerCallsBefore || enrollCalls != 1 {
				t.Fatalf("register calls = %d (want unchanged from %d), enroll calls = %d (want 1)", registerCalls, registerCallsBefore, enrollCalls)
			}
			if platform.meCallCount() == 0 {
				t.Fatal("expected a /v1/agents/me call")
			}
			if corrupt {
				got, err := os.ReadFile(filepath.Join(stateDir, "agent.json.corrupt")) // #nosec G304 -- test controls its temporary state directory
				if err != nil || string(got) != string(corruptBytes) {
					t.Fatalf("corrupt registration evidence was not preserved: err=%v contents=%q", err, got)
				}
			}
		})
	}
}

// An unclaimed identity whose claim bootstrap the platform still returns
// rebuilds and continues the ordinary unclaimed flow: it prints the claim
// URL/code exactly as a fresh Register's response would.
func TestConnectRebuildsUnclaimedRegistrationWithClaimLink(t *testing.T) {
	withShortConnectTimings(t)
	platform := newStubPlatform(t)
	cfgPath, stateDir := connectConfig(t, platform.srv.URL, "")
	cfg := mustLoadConfig(t, cfgPath)

	agentID, key := registerAgent(t, platform)
	setupLostRegistration(t, cfg, key, true)
	registerCallsBefore, _, _ := platform.counts()

	code, out, errOut := runConnect(t, cfgPath, nil)
	if code != exitOK {
		t.Fatalf("connect did not rebuild: code=%d stderr=%s", code, errOut)
	}
	if !strings.Contains(out, "claim this agent:") || !strings.Contains(out, "AB12-CD34") {
		t.Fatalf("did not print the recovered claim link: stdout=%q", out)
	}
	reg, ok := loadAgent(t, stateDir)
	if !ok || reg.AgentID != agentID || reg.Status != "unclaimed" || reg.ClaimURL == "" || reg.ClaimCode != "AB12-CD34" {
		t.Fatalf("rebuilt registration not as expected: %+v ok=%v", reg, ok)
	}
	if registerCalls, _, _ := platform.counts(); registerCalls != registerCallsBefore {
		t.Fatalf("Register calls = %d, want unchanged from %d", registerCalls, registerCallsBefore)
	}
}

// The one documented gap (B.1): /v1/agents/me answering for a still-
// unclaimed agent without the claim bootstrap fields. Rebuild persists
// the identity anyway, prints the fallback (never a bare empty URL),
// exits 0 without polling, and — on any later run, including `status` —
// the durable state keeps printing the same fallback rather than ever
// falling back into the ordinary print-and-wait narration.
func TestConnectRebuildsUnclaimedRegistrationWithoutClaimLink(t *testing.T) {
	withShortConnectTimings(t)
	platform := newStubPlatform(t)
	platform.setMeOmitsClaimFields(true)
	cfgPath, stateDir := connectConfig(t, platform.srv.URL, "")
	cfg := mustLoadConfig(t, cfgPath)

	agentID, key := registerAgent(t, platform)
	setupLostRegistration(t, cfg, key, true)
	registerCallsBefore, statusCallsBefore, _ := platform.counts()

	code, out, errOut := runConnect(t, cfgPath, nil)
	if code != exitOK {
		t.Fatalf("connect exited %d, stderr=%s", code, errOut)
	}
	if !strings.Contains(out, "run `dropin-miner connect -force`") {
		t.Fatalf("missing the recovered-without-link fallback: stdout=%q", out)
	}
	if strings.Contains(out, "claim this agent:") {
		t.Fatalf("printed the ordinary print-and-wait narration despite no claim link: stdout=%q", out)
	}
	if strings.Contains(out, "  \n") {
		t.Fatalf("printed a bare empty claim link: stdout=%q", out)
	}
	reg, ok := loadAgent(t, stateDir)
	if !ok || reg.AgentID != agentID || reg.Status != "unclaimed" || reg.ClaimURL != "" {
		t.Fatalf("rebuilt registration not as expected: %+v ok=%v", reg, ok)
	}
	if registerCalls, statusCalls, _ := platform.counts(); registerCalls != registerCallsBefore || statusCalls != statusCallsBefore {
		t.Fatalf("register calls = %d (want unchanged from %d), status(poll) calls = %d (want unchanged from %d — no poll on the rebuild run)",
			registerCalls, registerCallsBefore, statusCalls, statusCallsBefore)
	}

	// Second run: an ordinary load (not a rebuild — agent.json now decodes
	// fine) must still never enter the print-and-wait narration, including
	// the poll loop's own timeout message — a poll may still run (to
	// discover claimed/expired), but nothing it prints may assume a claim
	// URL exists.
	code, out, errOut = runConnect(t, cfgPath, nil)
	if code != exitOK {
		t.Fatalf("second connect exited %d, stderr=%s", code, errOut)
	}
	if !strings.Contains(out, "run `dropin-miner connect -force`") {
		t.Fatalf("second run dropped the recovered-without-link fallback: stdout=%q", out)
	}
	if strings.Contains(out, "claim this agent:") {
		t.Fatalf("second run printed the ordinary print-and-wait narration despite no claim link: stdout=%q", out)
	}
	if strings.Contains(out, "  \n") {
		t.Fatalf("second run printed a bare empty claim link: stdout=%q", out)
	}
	if strings.Contains(out, "URL above") {
		t.Fatalf("second run's poll-timeout narration assumed a claim URL was printed above it: stdout=%q", out)
	}

	// `status` reports the same durable state the same way — never a bare
	// "claim at " followed by nothing.
	statusOut := captureStdout(t, func() {
		printAgentIdentityStatus([]string{"-config", cfgPath}, os.Stdout, os.Stderr, noEnv)
	})
	if !strings.Contains(statusOut, "recovered from the platform") {
		t.Fatalf("status did not report the recovered-without-link state: %q", statusOut)
	}
	if strings.Contains(statusOut, "claim at \n") || strings.Contains(statusOut, "claim at\n") {
		t.Fatalf("status printed a bare empty claim link: %q", statusOut)
	}
}

// An identity that comes back expired from the rebuild flows into the
// existing expired-replacement path unchanged — the same client.Status
// re-verification and Register-a-replacement flow a normally-loaded
// expired registration already takes.
func TestConnectRebuildExpiredRegistrationFlowsIntoReplacementPath(t *testing.T) {
	withShortConnectTimings(t)
	platform := newStubPlatform(t)
	cfgPath, stateDir := connectConfig(t, platform.srv.URL, "")
	cfg := mustLoadConfig(t, cfgPath)

	_, key := registerAgent(t, platform)
	platform.setStatus("expired")
	setupLostRegistration(t, cfg, key, true)
	registerCallsBefore, _, _ := platform.counts()

	code, _, errOut := runConnect(t, cfgPath, nil)
	if code != exitOK {
		t.Fatalf("connect did not replace the expired rebuild: code=%d stderr=%s", code, errOut)
	}
	reg, ok := loadAgent(t, stateDir)
	if !ok || reg.AgentID != "agent-2" || reg.Status != "unclaimed" {
		t.Fatalf("did not register a replacement after the expired rebuild: %+v ok=%v", reg, ok)
	}
	if registerCalls, _, _ := platform.counts(); registerCalls != registerCallsBefore+1 {
		t.Fatalf("Register calls = %d, want %d (exactly one replacement Register)", registerCalls, registerCallsBefore+1)
	}
}

// A transient /v1/agents/me failure (or a 404 for a genuinely unknown
// key — TestConnectRefusesCorruptRegistrationWithExistingPlatformCredential
// covers that case directly) refuses the same way a plain fresh-Register
// attempt would, and never renames the corrupt evidence: preserving it is
// conditioned on /me actually answering, not merely being attempted.
func TestConnectMeErrorRefusesWithoutRenamingCorruptRecord(t *testing.T) {
	withShortConnectTimings(t)
	platform := newStubPlatform(t)
	cfgPath, stateDir := connectConfig(t, platform.srv.URL, "")
	cfg := mustLoadConfig(t, cfgPath)

	_, key := registerAgent(t, platform)
	platform.setMeError(true)
	corruptBytes := setupLostRegistration(t, cfg, key, true)
	registerCallsBefore, _, _ := platform.counts()

	code, _, errOut := runConnect(t, cfgPath, nil)
	if code == exitOK {
		t.Fatal("connect succeeded despite a failing /v1/agents/me")
	}
	if !strings.Contains(errOut, "-force") {
		t.Fatalf("missing -force guidance: %q", errOut)
	}
	if registerCalls, _, _ := platform.counts(); registerCalls != registerCallsBefore {
		t.Fatalf("Register calls = %d, want unchanged from %d", registerCalls, registerCallsBefore)
	}
	got, err := os.ReadFile(filepath.Join(stateDir, "agent.json")) // #nosec G304 -- test controls its temporary state directory
	if err != nil || string(got) != string(corruptBytes) {
		t.Fatalf("corrupt registration was renamed or modified despite the refusal: err=%v contents=%q", err, got)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "agent.json.corrupt")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("agent.json.corrupt should not exist after a refusal: err=%v", err)
	}
}

// `-resume` never attempts the rebuild (or a fresh Register) on a lost or
// unreadable registration: it is the unattended background poll, not the
// place to originate a new registration or decide a corrupt one is dead.
func TestConnectResumeNeverRebuildsOrRegistersOnLostRegistration(t *testing.T) {
	for _, corrupt := range []bool{true, false} {
		corrupt := corrupt
		name := "absent"
		if corrupt {
			name = "corrupt"
		}
		t.Run(name, func(t *testing.T) {
			withShortConnectTimings(t)
			platform := newStubPlatform(t)
			cfgPath, stateDir := connectConfig(t, platform.srv.URL, "")
			cfg := mustLoadConfig(t, cfgPath)

			_, key := registerAgent(t, platform)
			setupLostRegistration(t, cfg, key, corrupt)
			registerCallsBefore, _, _ := platform.counts()

			code, _, errOut := runConnect(t, cfgPath, nil, "-resume")
			if code != exitOK {
				t.Fatalf("-resume on a lost registration exited %d, stderr=%s", code, errOut)
			}
			if platform.meCallCount() != 0 {
				t.Fatalf("/v1/agents/me calls = %d, want 0 (-resume must never rebuild)", platform.meCallCount())
			}
			if registerCalls, _, _ := platform.counts(); registerCalls != registerCallsBefore {
				t.Fatalf("Register calls = %d, want unchanged from %d (-resume must never register)", registerCalls, registerCallsBefore)
			}
			if _, err := os.Stat(filepath.Join(stateDir, "agent.json.corrupt")); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("-resume must write nothing: agent.json.corrupt err=%v", err)
			}
			// loadAgent itself fails the test on ErrAgentRegistrationCorrupt,
			// so check directly: the point here is "unchanged", not "decodes."
			store, err := auth.OpenStore(stateDir)
			if err != nil {
				t.Fatal(err)
			}
			if _, ok, rerr := store.LoadAgentRegistration(); corrupt {
				if !errors.Is(rerr, auth.ErrAgentRegistrationCorrupt) {
					t.Fatalf("-resume must write nothing: agent.json is no longer corrupt: ok=%v err=%v", ok, rerr)
				}
			} else if ok {
				t.Fatal("-resume must write nothing: a registration now exists")
			}
		})
	}
}

func TestForegroundConnectReplacesExpiredRegistrationAndPropagatesNewIdentity(t *testing.T) {
	withShortConnectTimings(t)
	platform := newStubPlatform(t)
	cfgPath, stateDir := connectConfig(t, platform.srv.URL, "")

	if code, _, errOut := runConnect(t, cfgPath, nil); code != exitOK {
		t.Fatalf("initial connect failed: %s", errOut)
	}
	oldURL := platform.srv.URL + "/claim/AB12-CD34"
	store, err := auth.OpenStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveMiningEnabled(true); err != nil {
		t.Fatal(err)
	}
	const payout = "twilight1uwew6p63453wm0znz723lrneuls4xy29swp89n"
	if err := store.SavePayoutAddress(payout); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkHealth(auth.HealthCapture, auth.HealthIntakeUnwritable, "preserve capture health"); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkHealth(auth.HealthFlush, auth.HealthSubmissionFailed, "preserve flush health"); err != nil {
		t.Fatal(err)
	}
	platform.setStatus("expired")

	code, out, errOut := runConnect(t, cfgPath, nil)
	if code != exitOK {
		t.Fatalf("expired replacement failed: code=%d stdout=%s stderr=%s", code, out, errOut)
	}
	newURL := platform.srv.URL + "/claim/EF56-GH02"
	if !strings.Contains(out, newURL) || !strings.Contains(out, "EF56-GH02") {
		t.Fatalf("replacement output did not show the new claim target/code: %q", out)
	}
	if strings.Contains(out, oldURL) || strings.Contains(out, "AB12-CD34") {
		t.Fatalf("replacement output retained the expired claim target/code: %q", out)
	}
	if registerCalls, _, _ := platform.counts(); registerCalls != 2 {
		t.Fatalf("Register calls = %d, want exactly 2", registerCalls)
	}
	reg, ok := loadAgent(t, stateDir)
	if !ok || reg.AgentID != "agent-2" || reg.ClaimURL != newURL || reg.ClaimCode != "EF56-GH02" || reg.Status != "unclaimed" {
		t.Fatalf("durable replacement registration = %+v ok=%v", reg, ok)
	}
	if got, err := os.ReadFile(credentialsPath(mustLoadConfig(t, cfgPath).Miner)); err != nil || !strings.Contains(string(got), "sr-stubkey-2") {
		t.Fatalf("new platform key was not published: err=%v contents=%q", err, got)
	}
	if got, ok, err := store.LoadPayoutAddress(); err != nil || !ok || got != payout {
		t.Fatalf("unrelated payout state changed: value=%q ok=%v err=%v", got, ok, err)
	}
	if decision := store.ReadMiningDecision(); decision.State != auth.MiningEnabled {
		t.Fatalf("expired replacement changed the persisted mining decision: %+v", decision)
	}
	if _, ok, err := store.LoadHealth(auth.HealthCapture); err != nil || !ok {
		t.Fatalf("expired replacement cleared capture health: ok=%v err=%v", ok, err)
	}
	if _, ok, err := store.LoadHealth(auth.HealthFlush); err != nil || !ok {
		t.Fatalf("expired replacement cleared flush health: ok=%v err=%v", ok, err)
	}
	if _, ok, err := store.LoadPendingRegistration(); err != nil || ok {
		t.Fatalf("pending registration journal remains after replacement: ok=%v err=%v", ok, err)
	}

	var statusOut bytes.Buffer
	if needsMining := printAgentIdentityStatus([]string{"-config", cfgPath}, &statusOut, &bytes.Buffer{}, noEnv); needsMining {
		t.Fatal("search-only replacement unexpectedly requested the AS mining report")
	}
	if !strings.Contains(statusOut.String(), newURL) || strings.Contains(statusOut.String(), oldURL) {
		t.Fatalf("status did not report only the replacement identity: %q", statusOut.String())
	}

	if code, _, errOut := runConnect(t, cfgPath, nil); code != exitOK {
		t.Fatalf("second foreground connect failed: code=%d stderr=%s", code, errOut)
	}
	if registerCalls, _, _ := platform.counts(); registerCalls != 2 {
		t.Fatalf("second foreground connect minted another agent: Register calls=%d", registerCalls)
	}
	reg, ok = loadAgent(t, stateDir)
	if !ok || reg.AgentID != "agent-2" {
		t.Fatalf("replacement identity did not remain durable: %+v ok=%v", reg, ok)
	}
}

func TestDetachedResumePersistsExpiredWithoutReplacing(t *testing.T) {
	withShortConnectTimings(t)
	platform := newStubPlatform(t)
	cfgPath, stateDir := connectConfig(t, platform.srv.URL, "")
	if code, _, errOut := runConnect(t, cfgPath, nil); code != exitOK {
		t.Fatalf("initial connect failed: %s", errOut)
	}
	platform.setStatus("expired")

	if code, _, errOut := runConnect(t, cfgPath, nil, "-resume"); code != exitOK {
		t.Fatalf("detached resume failed: %d %s", code, errOut)
	}
	if code, _, errOut := runConnect(t, cfgPath, nil, "-resume"); code != exitOK {
		t.Fatalf("detached resume of persisted expiry failed: %d %s", code, errOut)
	}
	if registerCalls, _, _ := platform.counts(); registerCalls != 1 {
		t.Fatalf("detached resume replaced the expired identity: Register calls=%d", registerCalls)
	}
	reg, ok := loadAgent(t, stateDir)
	if !ok || reg.AgentID != "agent-1" || reg.Status != "expired" {
		t.Fatalf("detached resume did not persist the expired identity: %+v ok=%v", reg, ok)
	}
	if shouldResume(mustLoadConfig(t, cfgPath)) {
		t.Fatal("shouldResume approved a replacement for an expired identity")
	}
}

func TestExpiredRegistrationNotFoundDoesNotTriggerReplacement(t *testing.T) {
	withShortConnectTimings(t)
	platform := newStubPlatform(t)
	cfgPath, stateDir := connectConfig(t, platform.srv.URL, "")
	if code, _, errOut := runConnect(t, cfgPath, nil); code != exitOK {
		t.Fatalf("initial connect failed: %s", errOut)
	}
	platform.setStatus("expired")
	if code, _, errOut := runConnect(t, cfgPath, nil, "-resume"); code != exitOK {
		t.Fatalf("detached expiry observation failed: %d %s", code, errOut)
	}
	platform.setStatusNotFound(true)

	code, _, errOut := runConnect(t, cfgPath, nil)
	if code == exitOK {
		t.Fatal("not-found expired identity was replaced as if expiry had been confirmed")
	}
	if !strings.Contains(errOut, "no longer known to the platform") {
		t.Fatalf("not-found result was not kept distinct from expiry: %q", errOut)
	}
	if registerCalls, _, _ := platform.counts(); registerCalls != 1 {
		t.Fatalf("not-found handling minted a replacement: Register calls=%d", registerCalls)
	}
	reg, ok := loadAgent(t, stateDir)
	if !ok || reg.AgentID != "agent-1" || reg.Status != "expired" {
		t.Fatalf("not-found handling changed the expired registration: %+v ok=%v", reg, ok)
	}
}

func TestExpiredRegistrationWithMissingCredentialRefusesForceReplacement(t *testing.T) {
	withShortConnectTimings(t)
	platform := newStubPlatform(t)
	cfgPath, stateDir := connectConfig(t, platform.srv.URL, "")
	store, err := auth.OpenStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveAgentRegistration(auth.AgentRegistration{
		AgentID: "agent-expired", Status: "expired", ClaimURL: platform.srv.URL + "/claim/OLD", ClaimCode: "OLD",
	}); err != nil {
		t.Fatal(err)
	}
	cfg := mustLoadConfig(t, cfgPath)
	credPath := credentialsPath(cfg.Miner)
	if err := os.Remove(credPath); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}

	code, _, errOut := runConnect(t, cfgPath, nil, "-force")
	if code == exitOK {
		t.Fatal("connect -force replaced an expired registration without a verifiable credential")
	}
	if registerCalls, _, _ := platform.counts(); registerCalls != 0 {
		t.Fatalf("Register calls = %d, want 0", registerCalls)
	}
	if !strings.Contains(errOut, "cannot safely verify") || !strings.Contains(errOut, "platform credential") {
		t.Fatalf("missing actionable missing-credential diagnostic: %q", errOut)
	}
	reg, ok := loadAgent(t, stateDir)
	if !ok || reg.AgentID != "agent-expired" || reg.Status != "expired" {
		t.Fatalf("expired registration changed: %+v ok=%v", reg, ok)
	}
	if _, err := os.Stat(credPath); !os.IsNotExist(err) {
		t.Fatalf("missing credential was unexpectedly written: %v", err)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "registration_pending.json")); !os.IsNotExist(err) {
		t.Fatalf("replacement journal was unexpectedly created: %v", err)
	}
}

func TestExpiredRegistrationStatusFailureRefusesForceReplacement(t *testing.T) {
	withShortConnectTimings(t)
	platform := newStubPlatform(t)
	cfgPath, stateDir := connectConfig(t, platform.srv.URL, "")
	store, err := auth.OpenStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveAgentRegistration(auth.AgentRegistration{
		AgentID: "agent-expired", Status: "expired", ClaimURL: platform.srv.URL + "/claim/OLD", ClaimCode: "OLD",
	}); err != nil {
		t.Fatal(err)
	}
	cfg := mustLoadConfig(t, cfgPath)
	credPath := credentialsPath(cfg.Miner)
	if err := writeCredentials(credPath, credentials{APIKey: "sr-old-key"}); err != nil { // #nosec G101 -- canned test credential
		t.Fatal(err)
	}
	platform.setStatusError(true)

	code, _, errOut := runConnect(t, cfgPath, nil, "-force")
	if code == exitOK {
		t.Fatal("connect -force replaced an expired registration after status verification failed")
	}
	if !strings.Contains(errOut, "could not safely verify") || !strings.Contains(errOut, "-force") {
		t.Fatalf("missing actionable status-failure diagnostic: %q", errOut)
	}
	if registerCalls, _, _ := platform.counts(); registerCalls != 0 {
		t.Fatalf("Register calls = %d, want 0", registerCalls)
	}
	reg, ok := loadAgent(t, stateDir)
	if !ok || reg.AgentID != "agent-expired" || reg.Status != "expired" {
		t.Fatalf("expired registration changed: %+v ok=%v", reg, ok)
	}
	if raw, err := os.ReadFile(credPath); err != nil || !strings.Contains(string(raw), "sr-old-key") { // #nosec G304 -- test controls its credential path
		t.Fatalf("existing credential changed: err=%v contents=%q", err, raw)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "registration_pending.json")); !os.IsNotExist(err) {
		t.Fatalf("replacement journal was unexpectedly created: %v", err)
	}
}

func TestLostRegisterResponseLeavesNoLocalCommitAndNextConnectMayRetry(t *testing.T) {
	withShortConnectTimings(t)
	platform := newStubPlatform(t)
	platform.dropNextRegisterResponse()
	cfgPath, stateDir := connectConfig(t, platform.srv.URL, "")

	code, _, errOut := runConnect(t, cfgPath, nil)
	if code == exitOK {
		t.Fatal("connect claimed success after the Register response was lost")
	}
	if !strings.Contains(errOut, "register") {
		t.Fatalf("lost response was not reported as a Register transport failure: %q", errOut)
	}
	if registerCalls, _, _ := platform.counts(); registerCalls != 1 {
		t.Fatalf("Register calls after lost response=%d, want 1", registerCalls)
	}
	if _, ok := loadAgent(t, stateDir); ok {
		t.Fatal("an invented agent registration was persisted")
	}
	if _, err := os.Stat(filepath.Join(stateDir, "registration_pending.json")); !os.IsNotExist(err) {
		t.Fatalf("a complete-response journal was created after a lost response: %v", err)
	}

	if code, _, errOut := runConnect(t, cfgPath, nil); code != exitOK {
		t.Fatalf("ordinary connect could not retry after ambiguous Register failure: %d %s", code, errOut)
	}
	if registerCalls, _, _ := platform.counts(); registerCalls != 2 {
		t.Fatalf("ordinary retry did not register normally: Register calls=%d", registerCalls)
	}
}

func TestPendingRegistrationRecoveryPublishesWithoutRegister(t *testing.T) {
	withShortConnectTimings(t)
	platform := newStubPlatform(t)
	cfgPath, stateDir := connectConfig(t, platform.srv.URL, "")
	cfg := mustLoadConfig(t, cfgPath)
	store, err := auth.OpenStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	client := platformapi.New(cfg.Platform.AgentsAPIURL, cfg.Platform.BaseURL)
	fresh, err := client.Register(context.Background(), "journal-crash", nil)
	if err != nil {
		t.Fatalf("completed Register response: %v", err)
	}
	pending := pendingRegistrationFromPlatform(fresh)
	if err := store.SavePendingRegistration(pending); err != nil {
		t.Fatal(err)
	}

	if code, _, errOut := runConnect(t, cfgPath, nil); code != exitOK {
		t.Fatalf("pending registration recovery failed: %d %s", code, errOut)
	}
	if registerCalls, _, _ := platform.counts(); registerCalls != 1 {
		t.Fatalf("pending recovery changed the completed Register count: %d", registerCalls)
	}
	reg, ok := loadAgent(t, stateDir)
	if !ok || reg.AgentID != pending.AgentID || reg.ClaimURL != pending.ClaimURL {
		t.Fatalf("journaled registration was not published: %+v ok=%v", reg, ok)
	}
	if got, err := os.ReadFile(credentialsPath(cfg.Miner)); err != nil || !strings.Contains(string(got), pending.Key) {
		t.Fatalf("journaled platform key was not published: err=%v contents=%q", err, got)
	}
	if _, ok, err := store.LoadPendingRegistration(); err != nil || ok {
		t.Fatalf("journal was not cleared after recovery: ok=%v err=%v", ok, err)
	}

	if code, _, errOut := runConnect(t, cfgPath, nil); code != exitOK {
		t.Fatalf("repeated recovery/resume failed: %d %s", code, errOut)
	}
	if registerCalls, _, _ := platform.counts(); registerCalls != 1 {
		t.Fatalf("repeated recovery minted another registration: %d", registerCalls)
	}
}

func TestPendingExpiredReplacementRecoveryCompletesAcrossRestart(t *testing.T) {
	withShortConnectTimings(t)
	platform := newStubPlatform(t)
	cfgPath, stateDir := connectConfig(t, platform.srv.URL, "")
	cfg := mustLoadConfig(t, cfgPath)
	store, err := auth.OpenStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	oldURL := platform.srv.URL + "/claim/OLD"
	if err := store.SaveAgentRegistration(auth.AgentRegistration{
		AgentID: "agent-old", ClaimURL: oldURL, ClaimCode: "OLD", Status: "expired",
	}); err != nil {
		t.Fatal(err)
	}
	if err := writeCredentials(credentialsPath(cfg.Miner), credentials{APIKey: "sr-old-key"}); err != nil { // #nosec G101 -- canned test credential
		t.Fatal(err)
	}
	if err := store.SavePendingRegistration(auth.PendingRegistration{
		AgentID:         "agent-new",
		Key:             "sr-new-key",
		ClaimURL:        platform.srv.URL + "/claim/NEW",
		ClaimCode:       "NEW",
		ClaimExpiresAt:  "2026-09-16T00:00:00Z",
		Status:          "unclaimed",
		ReplaceExpired:  true,
		PreviousAgentID: "agent-old",
		PreviousKey:     "sr-old-key",
	}); err != nil {
		t.Fatal(err)
	}

	if code, _, errOut := runConnect(t, cfgPath, nil); code != exitOK {
		t.Fatalf("replacement journal recovery failed: %d %s", code, errOut)
	}
	if registerCalls, _, _ := platform.counts(); registerCalls != 0 {
		t.Fatalf("restart recovery minted another registration: %d", registerCalls)
	}
	reg, ok := loadAgent(t, stateDir)
	if !ok || reg.AgentID != "agent-new" || reg.ClaimURL != platform.srv.URL+"/claim/NEW" || reg.Status != "unclaimed" {
		t.Fatalf("replacement journal did not complete publication: %+v ok=%v", reg, ok)
	}
	if got, err := os.ReadFile(credentialsPath(cfg.Miner)); err != nil || !strings.Contains(string(got), "sr-new-key") {
		t.Fatalf("replacement key was not published after restart: err=%v contents=%q", err, got)
	}
	if _, ok, err := store.LoadPendingRegistration(); err != nil || ok {
		t.Fatalf("replacement journal survived successful recovery: ok=%v err=%v", ok, err)
	}
}

func TestPendingRegistrationConflictDoesNotOverwriteOrRegister(t *testing.T) {
	platform := newStubPlatform(t)
	cfgPath, stateDir := connectConfig(t, platform.srv.URL, "")
	cfg := mustLoadConfig(t, cfgPath)
	store, err := auth.OpenStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	pending := auth.PendingRegistration{
		AgentID:        "agent-journal",
		Key:            "sr-journal-key",
		ClaimURL:       platform.srv.URL + "/claim/JOURNAL-1",
		ClaimCode:      "JOURNAL-1",
		ClaimExpiresAt: "2026-09-16T00:00:00Z",
		Status:         "unclaimed",
	}
	if err := store.SavePendingRegistration(pending); err != nil {
		t.Fatal(err)
	}
	if err := writeCredentials(credentialsPath(cfg.Miner), credentials{APIKey: "sr-other-key"}); err != nil { // #nosec G101 -- canned test credential
		t.Fatal(err)
	}

	if code, _, errOut := runConnect(t, cfgPath, nil); code == exitOK {
		t.Fatal("connect overwrote a conflicting credential during journal recovery")
	} else if !strings.Contains(errOut, "different platform key") {
		t.Fatalf("missing journal conflict diagnostic: %q", errOut)
	}
	if registerCalls, _, _ := platform.counts(); registerCalls != 0 {
		t.Fatalf("journal conflict issued a new Register: %d", registerCalls)
	}
	if _, ok, err := store.LoadPendingRegistration(); err != nil || !ok {
		t.Fatalf("journal was lost after a publication conflict: ok=%v err=%v", ok, err)
	}
}

func TestPendingRegistrationAgentConflictDoesNotOverwriteOrRegister(t *testing.T) {
	platform := newStubPlatform(t)
	cfgPath, stateDir := connectConfig(t, platform.srv.URL, "")
	store, err := auth.OpenStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveAgentRegistration(auth.AgentRegistration{
		AgentID: "agent-other", ClaimURL: platform.srv.URL + "/claim/OTHER", ClaimCode: "OTHER", Status: "unclaimed",
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.SavePendingRegistration(auth.PendingRegistration{
		AgentID:        "agent-journal",
		Key:            "sr-journal-key",
		ClaimURL:       platform.srv.URL + "/claim/JOURNAL-1",
		ClaimCode:      "JOURNAL-1",
		ClaimExpiresAt: "2026-09-16T00:00:00Z",
		Status:         "unclaimed",
	}); err != nil {
		t.Fatal(err)
	}

	if code, _, errOut := runConnect(t, cfgPath, nil); code == exitOK {
		t.Fatal("connect overwrote a conflicting agent registration during journal recovery")
	} else if !strings.Contains(errOut, "different identity") {
		t.Fatalf("missing agent conflict diagnostic: %q", errOut)
	}
	if registerCalls, _, _ := platform.counts(); registerCalls != 0 {
		t.Fatalf("agent conflict issued a new Register: %d", registerCalls)
	}
	reg, ok := loadAgent(t, stateDir)
	if !ok || reg.AgentID != "agent-other" {
		t.Fatalf("conflicting agent registration was overwritten: %+v ok=%v", reg, ok)
	}
	if _, ok, err := store.LoadPendingRegistration(); err != nil || !ok {
		t.Fatalf("journal was lost after an agent publication conflict: ok=%v err=%v", ok, err)
	}
}

// WP2-adversarial-review finding 11 (second half): the foreground loop's
// own sleep must be bounded by its remaining budget, not just by the
// platform's (already ceiling-clamped) advertised interval. A platform
// that never claims and advertises a long interval must still see the
// loop give up close to connectPollBudget, not wait out the interval.
func TestForegroundLoopSleepIsBoundedByRemainingBudget(t *testing.T) {
	origBudget, origInterval := connectPollBudget, resumePollInterval
	connectPollBudget = 100 * time.Millisecond
	t.Cleanup(func() { connectPollBudget, resumePollInterval = origBudget, origInterval })

	// newStubPlatform's default register response advertises interval_s: 1,
	// which clampPollInterval floors to 2s (well past the 100ms budget) —
	// exactly the gap that matters: if the loop's sleep used that raw
	// interval instead of min(interval, remaining budget), this test would
	// time out waiting for connect to return at all. The registration
	// stays "unclaimed" throughout (nothing calls platform.claim).
	platform := newStubPlatform(t)
	cfgPath, _ := connectConfig(t, platform.srv.URL, "")

	start := time.Now()
	code, out, _ := runConnect(t, cfgPath, nil)
	elapsed := time.Since(start)
	if code != exitOK {
		t.Fatalf("connect exited %d, want %d", code, exitOK)
	}
	if !strings.Contains(out, "not claimed yet") {
		t.Fatalf("expected the timeout message, got: %q", out)
	}
	// Generous slack (10x the budget) for scheduling noise, but nowhere
	// near the multi-second interval a stub's default advertises — this
	// only needs to prove the sleep did NOT wait out the raw interval.
	if elapsed > 10*connectPollBudget {
		t.Fatalf("connect took %v, want close to connectPollBudget (%v) — the sleep was not bounded by the remaining budget", elapsed, connectPollBudget)
	}
}
