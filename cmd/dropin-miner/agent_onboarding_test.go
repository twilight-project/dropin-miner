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

	mu       sync.Mutex
	declared string
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
		f.mu.Unlock()
		writeStubJSON(w, http.StatusOK, map[string]any{
			"status": "ACTIVE", "address": body.Address, "canonical_address": body.Address,
			"effective": true, "declared_at": "2026-09-09T00:00:00Z",
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
	if err := store.SavePayoutAddress("twilight1abc"); err != nil {
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
	if got := as.declaredAddress(); got != "twilight1abc" {
		t.Fatalf("payout declared address = %q, want twilight1abc", got)
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
	if err := store.SavePayoutAddress("twilight1later"); err != nil {
		t.Fatal(err)
	}
	platform.claim("credits", "mining")

	code, out, _ := runConnect(t, cfgPath, nil)
	if code != exitOK || !strings.Contains(out, "enrolled for mining") {
		t.Fatalf("did not enroll on the later grant: code=%d out=%q", code, out)
	}
	if got := as.declaredAddress(); got != "twilight1later" {
		t.Fatalf("declared address = %q, want twilight1later", got)
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
	if shouldResume(filepath.Join(t.TempDir(), "state")) {
		t.Fatal("shouldResume true with nothing on disk")
	}
	if shouldResume("") {
		t.Fatal("shouldResume true with an empty state dir")
	}
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
		outcome, code := askMiningQuestion(stdin, br, &bytes.Buffer{}, &bytes.Buffer{}, noEnv, cfg, store, true)
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
		stdin := bytes.NewBufferString("y\ntwilight1typed\n")
		br := bufio.NewReader(stdin)
		outcome, code := askMiningQuestion(stdin, br, &bytes.Buffer{}, &bytes.Buffer{}, noEnv, cfg, store, true)
		if code != exitOK || !outcome.enabled || outcome.payoutAddress != "twilight1typed" {
			t.Fatalf("got %+v code=%d", outcome, code)
		}
		if addr, ok, _ := store.LoadPayoutAddress(); !ok || addr != "twilight1typed" {
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
		outcome, code := askMiningQuestion(stdin, br, &out, &bytes.Buffer{}, os.Getenv, cfg, store, true)
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
		outcome, code := askMiningQuestion(&bytes.Buffer{}, bufio.NewReader(&bytes.Buffer{}), &bytes.Buffer{}, &bytes.Buffer{}, noEnv, cfg, store, false)
		if code != exitOK || outcome.enabled {
			t.Fatalf("got %+v code=%d", outcome, code)
		}
	})

	t.Run("enabled=true with payout_address", func(t *testing.T) {
		cfgPath, stateDir := scriptedMiningConfig(t, platform.srv.URL, "enabled = true\npayout_address = \"twilight1scripted\"\n")
		cfg, _, err := loadConfig(cfgPath, noEnv)
		if err != nil {
			t.Fatal(err)
		}
		store, err := auth.OpenStore(stateDir)
		if err != nil {
			t.Fatal(err)
		}
		outcome, code := askMiningQuestion(&bytes.Buffer{}, bufio.NewReader(&bytes.Buffer{}), &bytes.Buffer{}, &bytes.Buffer{}, noEnv, cfg, store, false)
		if code != exitOK || !outcome.enabled || outcome.payoutAddress != "twilight1scripted" {
			t.Fatalf("got %+v code=%d", outcome, code)
		}
		if addr, ok, _ := store.LoadPayoutAddress(); !ok || addr != "twilight1scripted" {
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
		outcome, code := askMiningQuestion(&bytes.Buffer{}, bufio.NewReader(&bytes.Buffer{}), &out, &bytes.Buffer{}, noEnv, cfg, store, false)
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
	if _, code := askMiningQuestion(&bytes.Buffer{}, bufio.NewReader(&bytes.Buffer{}), &bytes.Buffer{}, &bytes.Buffer{}, noEnv, cfg, store, false); code != exitOK {
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
	if err := store.SavePayoutAddress("twilight1unattended"); err != nil {
		t.Fatal(err)
	}
	platform.claim("mining")

	// The detached shape specifically: -resume, no stdin, exactly the
	// invocation search.go's spawnConnectResume makes.
	code, out, _ := runConnect(t, cfgPath, nil, "-resume")
	if code != exitOK {
		t.Fatalf("resume exited %d: %s", code, out)
	}
	if got := as.declaredAddress(); got != "twilight1unattended" {
		t.Fatalf("declared address = %q, want twilight1unattended", got)
	}
}
