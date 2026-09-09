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
	c := New(stub.srv.URL)
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

	// -mining sends requested_scopes as a hint.
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
	c := New(stub.srv.URL)
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
	c := New(stub.srv.URL)
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
	c := New(stub.srv.URL)
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
	c := New(stub.srv.URL)
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
	c := New(stub.srv.URL)
	reg, err := c.Register(context.Background(), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if reg.PollInterval > maxPollInterval {
		t.Fatalf("poll interval = %v, want <= ceiling %v", reg.PollInterval, maxPollInterval)
	}
}
