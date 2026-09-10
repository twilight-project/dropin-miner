package platform

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// stubPlatform is the control-plane stub, same shape as pkg/auth's
// enrollassert_test.go newAssertAS: a mux plus assignable handler funcs
// per test.
type stubPlatform struct {
	srv      *httptest.Server
	register func(w http.ResponseWriter, r *http.Request)
	status   func(w http.ResponseWriter, r *http.Request)
	enroll   func(w http.ResponseWriter, r *http.Request)
}

func newStubPlatform(t *testing.T) *stubPlatform {
	t.Helper()
	f := &stubPlatform{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/agents/register", func(w http.ResponseWriter, r *http.Request) {
		if f.register != nil {
			f.register(w, r)
			return
		}
		http.Error(w, "not configured", http.StatusInternalServerError)
	})
	mux.HandleFunc("GET /v1/agents/{id}", func(w http.ResponseWriter, r *http.Request) {
		if f.status != nil {
			f.status(w, r)
			return
		}
		http.Error(w, "not configured", http.StatusInternalServerError)
	})
	mux.HandleFunc("POST /v1/agents/enroll", func(w http.ResponseWriter, r *http.Request) {
		if f.enroll != nil {
			f.enroll(w, r)
			return
		}
		http.Error(w, "not configured", http.StatusInternalServerError)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func TestRegisterReturnsAgentIdentity(t *testing.T) {
	stub := newStubPlatform(t)
	var sawBody map[string]any
	stub.register = func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&sawBody)
		writeJSON(w, http.StatusCreated, map[string]any{
			"agent_id":         "agent-1",
			"key":              "sr-abc123",
			"claim_url":        stub.srv.URL + "/claim/AB12-CD34",
			"claim_code":       "AB12-CD34",
			"claim_expires_at": "2026-09-16T00:00:00Z",
			"poll":             map[string]any{"url": stub.srv.URL + "/v1/agents/agent-1", "interval_s": 5},
			"tier":             "unclaimed",
		})
	}
	c := New(stub.srv.URL, stub.srv.URL)
	reg, err := c.Register(context.Background(), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if reg.AgentID != "agent-1" || reg.Key != "sr-abc123" || reg.ClaimURL == "" || reg.ClaimCode != "AB12-CD34" {
		t.Fatalf("got %+v", reg)
	}
	if reg.PollInterval != 5*time.Second {
		t.Fatalf("poll interval = %v, want 5s", reg.PollInterval)
	}
	// §5.1: a client with nothing to say omits the fields.
	if _, ok := sawBody["name"]; ok {
		t.Error("empty name was sent as a field instead of omitted")
	}
	if _, ok := sawBody["requested_scopes"]; ok {
		t.Error("empty requested_scopes was sent as a field instead of omitted")
	}

	// A non-empty requested_scopes is sent as a hint.
	stub.register = func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&sawBody)
		writeJSON(w, http.StatusCreated, map[string]any{
			"agent_id": "agent-2", "key": "sr-def456", "claim_url": stub.srv.URL + "/claim/X",
		})
	}
	if _, err := c.Register(context.Background(), "my-agent", []string{"mining"}); err != nil {
		t.Fatal(err)
	}
	if sawBody["name"] != "my-agent" {
		t.Errorf("name not sent: %+v", sawBody)
	}
	scopes, _ := sawBody["requested_scopes"].([]any)
	if len(scopes) != 1 || scopes[0] != "mining" {
		t.Errorf("requested_scopes not sent as hint: %+v", sawBody)
	}
}

func TestPollReportsUnclaimedThenClaimed(t *testing.T) {
	stub := newStubPlatform(t)
	claimed := false
	stub.status = func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sr-abc123" {
			t.Errorf("status request carried wrong/no bearer key: %q", r.Header.Get("Authorization"))
		}
		if !claimed {
			writeJSON(w, http.StatusOK, map[string]any{"status": "unclaimed", "scopes": []string{}})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"status": "claimed", "scopes": []string{"credits", "mining"},
			"mining": map[string]any{"available": true, "slots": []string{"twilight-slot-3"}},
		})
	}
	c := New(stub.srv.URL, stub.srv.URL)
	st, err := c.Status(context.Background(), "agent-1", "sr-abc123")
	if err != nil || st.Status != "unclaimed" {
		t.Fatalf("got %+v err=%v", st, err)
	}
	claimed = true
	st, err = c.Status(context.Background(), "agent-1", "sr-abc123")
	if err != nil || st.Status != "claimed" || !st.HasScope("mining") {
		t.Fatalf("got %+v err=%v", st, err)
	}

	// Unknown agent/key: one answer, no oracle (§5.2).
	stub.status = func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}
	if _, err := c.Status(context.Background(), "agent-x", "sr-nope"); err != ErrAgentNotFound {
		t.Fatalf("got %v, want ErrAgentNotFound", err)
	}
}

// WP4b (design f0ddb69 §5.5): the wire field cmd/dropin-miner's
// askMiningQuestion reads to default the mining question to "no" on a
// participant's later agents — an assumption about §5.2's shape pending
// WP1 confirmation (client.go's own doc comment on the field). This
// proves the client decodes it when present and defaults it false when
// absent, which is all a caller can rely on either way.
func TestStatusDecodesParticipantHasOtherMiningAgent(t *testing.T) {
	stub := newStubPlatform(t)
	stub.status = func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"status": "claimed", "scopes": []string{"mining"},
			"mining": map[string]any{"available": true, "slots": []string{"twilight-slot-3"}, "participant_has_other_agent": true},
		})
	}
	c := New(stub.srv.URL, stub.srv.URL)
	st, err := c.Status(context.Background(), "agent-1", "sr-abc123")
	if err != nil || !st.ParticipantHasOtherMiningAgent {
		t.Fatalf("got %+v err=%v, want ParticipantHasOtherMiningAgent=true", st, err)
	}

	stub.status = func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "claimed", "scopes": []string{"mining"}})
	}
	st, err = c.Status(context.Background(), "agent-1", "sr-abc123")
	if err != nil || st.ParticipantHasOtherMiningAgent {
		t.Fatalf("got %+v err=%v, want ParticipantHasOtherMiningAgent=false when the field is absent", st, err)
	}
}

// console_url is search-router's fix for §2.2's re-approval UX (a human
// otherwise has to submit an already-consumed claim code and fail before
// discovering the console link) — confirmed live against the real
// deployment. Proves it decodes and passes the same origin-lock
// validation claim_url already gets.
func TestStatusDecodesConsoleURLOnThePortalOrigin(t *testing.T) {
	stub := newStubPlatform(t)
	consoleURL := stub.srv.URL + "/projects/8fe850f9-eb9a-4a80-b9c8-7341bd346a48"
	stub.status = func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"status": "claimed", "scopes": []string{"credits"}, "console_url": consoleURL,
			"mining": map[string]any{"available": false},
		})
	}
	c := New(stub.srv.URL, stub.srv.URL)
	st, err := c.Status(context.Background(), "agent-1", "sr-abc123")
	if err != nil {
		t.Fatal(err)
	}
	if st.ConsoleURL != consoleURL {
		t.Fatalf("ConsoleURL = %q, want %q", st.ConsoleURL, consoleURL)
	}
}

// Absent console_url (an older platform, or an unclaimed agent) leaves it
// empty rather than erroring — callers fall back to the generic address.
func TestStatusConsoleURLDefaultsEmptyWhenAbsent(t *testing.T) {
	stub := newStubPlatform(t)
	stub.status = func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "unclaimed", "scopes": []string{}})
	}
	c := New(stub.srv.URL, stub.srv.URL)
	st, err := c.Status(context.Background(), "agent-1", "sr-abc123")
	if err != nil || st.ConsoleURL != "" {
		t.Fatalf("got ConsoleURL=%q err=%v, want empty and no error", st.ConsoleURL, err)
	}
}

// An off-origin (or control-character-bearing) console_url is dropped,
// not failed — unlike claim_url, which IS the point of a register call,
// console_url is supplementary to an otherwise-good status poll: the
// scopes and enrollment progress this same response carries are still
// worth having even when console_url itself is hostile or malformed.
func TestStatusDropsAnInvalidConsoleURLWithoutFailingTheWholePoll(t *testing.T) {
	stub := newStubPlatform(t)
	stub.status = func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"status": "claimed", "scopes": []string{"credits", "mining"},
			"console_url": "https://not-the-platform.example/projects/evil",
			"mining":      map[string]any{"available": true, "slots": []string{"twilight-slot-3"}},
		})
	}
	c := New(stub.srv.URL, stub.srv.URL)
	st, err := c.Status(context.Background(), "agent-1", "sr-abc123")
	if err != nil {
		t.Fatalf("an invalid console_url failed the whole status poll: %v", err)
	}
	if st.ConsoleURL != "" {
		t.Fatalf("ConsoleURL = %q, want dropped (empty) for an off-origin value", st.ConsoleURL)
	}
	if !st.HasScope("mining") {
		t.Fatalf("the rest of the response (scopes) should still be usable: %+v", st)
	}
}

func TestEnrollRequiresClaimedMiningScope(t *testing.T) {
	stub := newStubPlatform(t)
	stub.enroll = func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sr-noscope" {
			t.Errorf("enroll request carried wrong/no bearer key")
		}
		writeJSON(w, http.StatusForbidden, map[string]any{
			"error": map[string]any{"code": CodeMiningNotGranted, "message": "the mining scope was not granted at the claim"},
		})
	}
	c := New(stub.srv.URL, stub.srv.URL)
	_, err := c.Enroll(context.Background(), "agent-1", "sr-noscope", "twilight-slot-3")
	if err == nil {
		t.Fatal("enroll with no mining scope was not refused")
	}
	var refusal *RefusalError
	if !errors.As(err, &refusal) || refusal.Code != CodeMiningNotGranted {
		t.Fatalf("got %v, want a RefusalError with code %q", err, CodeMiningNotGranted)
	}

	stub.enroll = func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"token": "enroll-jwt-1"}) // #nosec G101 -- a canned stub response, not a credential
	}
	token, err := c.Enroll(context.Background(), "agent-1", "sr-hasscope", "twilight-slot-3")
	if err != nil || token != "enroll-jwt-1" { // #nosec G101 -- comparing against the stub's own canned value above
		t.Fatalf("got %q err=%v", token, err)
	}
}

// A control plane advertising interval_s: 0 (or omitting it) must not
// turn a caller's poll loop tight against a real service.
func TestPollIntervalIsFloorClamped(t *testing.T) {
	stub := newStubPlatform(t)
	stub.register = func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusCreated, map[string]any{
			"agent_id": "agent-1", "key": "sr-abc123", "claim_url": stub.srv.URL + "/claim/X",
			"poll": map[string]any{"interval_s": 0},
		})
	}
	c := New(stub.srv.URL, stub.srv.URL)
	reg, err := c.Register(context.Background(), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if reg.PollInterval < minPollInterval {
		t.Fatalf("poll interval = %v, want >= floor %v", reg.PollInterval, minPollInterval)
	}
}

// WP2-adversarial-review finding 11: a control plane advertising an
// enormous interval_s (100000 — over a day) must not park a caller's poll
// for anywhere near that long either. The ceiling is the other half of
// the same clamp the floor test above exercises.
func TestPollIntervalIsCeilingClamped(t *testing.T) {
	stub := newStubPlatform(t)
	stub.register = func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusCreated, map[string]any{
			"agent_id": "agent-1", "key": "sr-abc123", "claim_url": stub.srv.URL + "/claim/X",
			"poll": map[string]any{"interval_s": 100000},
		})
	}
	c := New(stub.srv.URL, stub.srv.URL)
	reg, err := c.Register(context.Background(), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if reg.PollInterval > maxPollInterval {
		t.Fatalf("poll interval = %v, want <= ceiling %v", reg.PollInterval, maxPollInterval)
	}
}

// WP2-adversarial-review finding 19: the structural check
// (TestNoBareHTTPClientInThisPackage) confirms every http.Client literal
// SETS CheckRedirect, but nothing previously exercised the POLICY itself
// — a mutation that quietly disabled auth.SameOriginRedirects (e.g.
// passing nil instead) left the whole suite green. This drives a real
// redirect through a real client and checks the credential's fate, not
// just the presence of a field.
func TestClientRefusesACrossOriginRedirect(t *testing.T) {
	evilHits := 0
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		evilHits++
		w.WriteHeader(http.StatusForbidden)
	}))
	defer evil.Close()

	var good *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/agents/register", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, evil.URL+r.URL.Path, http.StatusFound) // #nosec G710 -- deliberately proving the client refuses this, not a real redirect
	})
	good = httptest.NewServer(mux)
	defer good.Close()

	c := New(good.URL, good.URL)
	if _, err := c.Register(context.Background(), "", nil); err == nil {
		t.Fatal("Register followed a cross-origin redirect instead of refusing it")
	}
	if evilHits != 0 {
		t.Fatalf("the off-origin server was contacted %d time(s); it must never be reached", evilHits)
	}
}

// The same-origin half: a redirect that stays on the platform's own
// origin is ordinary and must still work.
func TestClientFollowsASameOriginRedirect(t *testing.T) {
	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/agents/register", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, srv.URL+"/elsewhere", http.StatusTemporaryRedirect)
	})
	mux.HandleFunc("/elsewhere", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusCreated, map[string]any{"agent_id": "a", "key": "sr-1", "claim_url": srv.URL + "/claim/X"})
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()
	if _, err := New(srv.URL, srv.URL).Register(context.Background(), "", nil); err != nil {
		t.Fatalf("same-origin redirect refused: %v", err)
	}
}

// WP2-adversarial-review finding 12: an off-origin claim_url must be
// refused outright, not stored or printed.
func TestClaimURLOffOriginIsRefused(t *testing.T) {
	stub := newStubPlatform(t)
	stub.register = func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusCreated, map[string]any{
			"agent_id": "a", "key": "sr-1", "claim_url": "https://not-the-platform.example/claim/X",
		})
	}
	if _, err := New(stub.srv.URL, stub.srv.URL).Register(context.Background(), "", nil); err == nil {
		t.Fatal("an off-origin claim_url was accepted")
	}
}

// The real deployment splits this across two hosts (confirmed live
// against agents-v1.nyks.dev/platform.nyks.dev, via search-router's own
// skill file): register/status/enroll go to the API host, but the
// claim_url they return legitimately points at a DIFFERENT portal host.
// New's two-argument split exists for exactly this — a claim_url must
// validate against the configured portal origin, not the API origin the
// request itself was sent to.
func TestClaimURLValidatesAgainstThePortalOriginNotTheAPIOrigin(t *testing.T) {
	api := newStubPlatform(t)
	portal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer portal.Close()
	api.register = func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusCreated, map[string]any{
			"agent_id": "a", "key": "sr-1", "claim_url": portal.URL + "/claim/X",
		})
	}
	reg, err := New(api.srv.URL, portal.URL).Register(context.Background(), "", nil)
	if err != nil {
		t.Fatalf("a claim_url on the configured portal origin (different from the API origin) was refused: %v", err)
	}
	if reg.ClaimURL != portal.URL+"/claim/X" {
		t.Fatalf("got %q", reg.ClaimURL)
	}
}

// The property that actually matters, named rather than left implied by
// the origin-mismatch check above: a claim_url on the AGENTS API's own
// origin — not some unrelated third origin, specifically the origin this
// request was just sent to — must still be rejected. A compromised or
// malicious API host must not be able to hand back a claim_url pointing
// the human at itself; only the configured portal origin is trusted to
// be where a real claim page lives.
func TestClaimURLOnTheAgentsAPIOriginIsRejectedNotJustAnyMismatch(t *testing.T) {
	api := newStubPlatform(t)
	portal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer portal.Close()
	api.register = func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusCreated, map[string]any{
			// The API host handing back a claim_url pointing at ITSELF,
			// not the configured portal — the phishing shape this guards
			// against, not an arbitrary off-origin string.
			"agent_id": "a", "key": "sr-1", "claim_url": api.srv.URL + "/claim/X",
		})
	}
	if _, err := New(api.srv.URL, portal.URL).Register(context.Background(), "", nil); err == nil {
		t.Fatal("a claim_url on the agents API's own origin was accepted; " +
			"a compromised API host could point a human at a page it controls")
	}
}

// A control character (here, a newline) in claim_url could forge the
// client's own terminal output when printed back.
func TestClaimURLControlCharacterIsRefused(t *testing.T) {
	stub := newStubPlatform(t)
	stub.register = func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusCreated, map[string]any{
			"agent_id": "a", "key": "sr-1", "claim_url": stub.srv.URL + "/claim/X\nfake line",
		})
	}
	if _, err := New(stub.srv.URL, stub.srv.URL).Register(context.Background(), "", nil); err == nil {
		t.Fatal("a claim_url with a control character was accepted")
	}
}

func TestClaimCodeControlCharacterIsRefused(t *testing.T) {
	stub := newStubPlatform(t)
	stub.register = func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusCreated, map[string]any{
			"agent_id": "a", "key": "sr-1", "claim_url": stub.srv.URL + "/claim/X",
			"claim_code": "AB12\r\nCD34",
		})
	}
	if _, err := New(stub.srv.URL, stub.srv.URL).Register(context.Background(), "", nil); err == nil {
		t.Fatal("a claim_code with a control character was accepted")
	}
}

// WP2-adversarial-review finding 13: a status outside {unclaimed,
// claimed, expired} must never reach a caller as anything more specific
// than "unclaimed" (keep polling).
func TestStatusRejectsValueOutsideTheEnum(t *testing.T) {
	stub := newStubPlatform(t)
	stub.status = func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"status": "PENDING_REVIEW", "scopes": []string{"mining"},
		})
	}
	st, err := New(stub.srv.URL, stub.srv.URL).Status(context.Background(), "agent-1", "sr-key")
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != "unclaimed" {
		t.Fatalf("status = %q, want it normalized to %q", st.Status, "unclaimed")
	}
	if st.HasScope("mining") {
		t.Fatal("an unrecognized status must not be trusted to carry real scopes either")
	}
}
