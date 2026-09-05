package auth

// The payout declaration client (AS MINIS-VER-014, ESC-029).
//
// Everything here is about one property: a proposal must never be reported as
// a completed change. The AS is inert by construction; this client's job is
// not to undo that by phrasing.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// payoutAS is an AS complete enough for one declaration call.
type payoutAS struct {
	srv *httptest.Server
	// declare answers PUT /v1/payout/declaration.
	declare func(w http.ResponseWriter, r *http.Request)
	// show answers GET.
	show func(w http.ResponseWriter, r *http.Request)
	// sawBody is the last declaration body the AS received.
	sawBody map[string]string
	// sawAuth records whether the request was authenticated at all.
	sawAuth bool
	// sourceProfiles overrides what the discovery document advertises.
	sourceProfiles []string
	// token overrides POST /oauth/token, so a test can count rotations.
	token func(w http.ResponseWriter, r *http.Request)
}

func newPayoutAS(t *testing.T) *payoutAS {
	t.Helper()
	f := &payoutAS{}
	mux := http.NewServeMux()
	mux.HandleFunc(WellKnownPath, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		raw := documentJSON(t, f.srv.URL, true)
		if len(f.sourceProfiles) > 0 {
			var doc map[string]any
			if err := json.Unmarshal(raw, &doc); err != nil {
				t.Fatal(err)
			}
			doc["source_profiles"] = f.sourceProfiles
			edited, err := json.Marshal(doc)
			if err != nil {
				t.Fatal(err)
			}
			raw = edited
		}
		_, _ = w.Write(raw)
	})
	mux.HandleFunc("GET /.well-known/oauth-authorization-server", func(w http.ResponseWriter, _ *http.Request) {
		base := f.srv.URL
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                        base,
			"authorization_endpoint":        base + "/oauth/authorize",
			"token_endpoint":                base + "/oauth/token",
			"device_authorization_endpoint": base + "/oauth/device_authorization",
			"revocation_endpoint":           base + "/oauth/revoke",
			"jwks_uri":                      base + "/oauth/jwks.json",
		})
	})
	mux.HandleFunc("POST /oauth/token", func(w http.ResponseWriter, r *http.Request) {
		if f.token != nil {
			f.token(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(tokenJSON("at-1", "rt-1")))
	})
	mux.HandleFunc("PUT /v1/payout/declaration", func(w http.ResponseWriter, r *http.Request) {
		f.sawAuth = r.Header.Get("Authorization") != "" && r.Header.Get("DPoP") != ""
		f.sawBody = map[string]string{}
		_ = json.NewDecoder(r.Body).Decode(&f.sawBody)
		f.declare(w, r)
	})
	mux.HandleFunc("GET /v1/payout/declaration", func(w http.ResponseWriter, r *http.Request) {
		f.sawAuth = r.Header.Get("Authorization") != "" && r.Header.Get("DPoP") != ""
		f.show(w, r)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *payoutAS) client(t *testing.T) *MiningClient {
	t.Helper()
	store, err := OpenStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveRefreshToken("rt-0"); err != nil {
		t.Fatal(err)
	}
	d, err := NewDiscoverer(DiscoveryConfig{BaseURL: f.srv.URL, ChainID: "twilight-1", SlotID: 7})
	if err != nil {
		t.Fatal(err)
	}
	oc, err := NewOAuthClient(context.Background(), d, store)
	if err != nil {
		t.Fatal(err)
	}
	return NewMiningClient(d, oc, store)
}

const pendingJSON = `{"status":"PENDING","address":"twilight1abc","canonical_address":"twilight1abc","effective":false,"declared_at":"2026-08-26T00:00:00.000Z"}`

// A declaration is sent authenticated, carries only the address, and a 202
// with a PENDING body is read as held rather than done.
func TestDeclarePayoutAddressSendsAnAuthenticatedProposal(t *testing.T) {
	as := newPayoutAS(t)
	as.declare = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(pendingJSON))
	}

	doc, err := as.client(t).DeclarePayoutAddress(context.Background(), "twilight1abc")
	if err != nil {
		t.Fatalf("declare: %v", err)
	}
	if !as.sawAuth {
		t.Error("the declaration arrived without a DPoP-bound access token")
	}
	if as.sawBody["address"] != "twilight1abc" {
		t.Errorf("address sent as %q", as.sawBody["address"])
	}
	// No participant identifier travels in the body: the AS takes it from
	// the session, and a client that offered one would invite an AS that
	// read it.
	if _, present := as.sawBody["participant_id"]; present {
		t.Error("the declaration body named a participant; identity comes from the token")
	}
	if doc.Effective {
		t.Error("a fresh proposal reported as effective")
	}
	if doc.Status != "PENDING" {
		t.Errorf("status %q", doc.Status)
	}
}

// ESC-031: a first declaration IS in force, and this client must relay it.
//
// The guard that used to sit here refused ANY declaration reported effective,
// on the reasoning that only an operator could make one so. That reasoning
// went with the operator step: keeping the guard would have made every new
// participant's `payout set` fail against a current AS, and the failure would
// have read as an AS defect rather than a stale client.
func TestDeclarationReportedInForceIsRelayed(t *testing.T) {
	as := newPayoutAS(t)
	as.declare = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ACTIVE","address":"twilight1abc","canonical_address":"twilight1abc","effective":true}`))
	}

	doc, err := as.client(t).DeclarePayoutAddress(context.Background(), "twilight1abc")
	if err != nil {
		t.Fatalf("a first declaration reported in force was refused: %v", err)
	}
	if !doc.Effective || doc.Status != "ACTIVE" {
		t.Fatalf("relayed as effective=%v status=%q", doc.Effective, doc.Status)
	}
	if doc.HeldFor != "" {
		t.Errorf("a binding in force reports held_for=%q", doc.HeldFor)
	}
}

// What is still refused is a document that contradicts itself. A client
// deciding which half to believe is how a participant gets told the wrong
// thing about where their money goes — and the two halves disagreeing is a
// far better signal of a broken AS than either half alone.
func TestSelfContradictoryDeclarationIsRefused(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"in force and held at once", `{"status":"ACTIVE","address":"twilight1abc","effective":true,"held_for":"REPLACES_ACTIVE"}`},
		{"pending but effective", `{"status":"PENDING","address":"twilight1abc","effective":true}`},
		{"active but not effective", `{"status":"ACTIVE","address":"twilight1abc","effective":false}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			as := newPayoutAS(t)
			as.declare = func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusAccepted)
				_, _ = w.Write([]byte(tc.body))
			}
			if _, err := as.client(t).DeclarePayoutAddress(context.Background(), "twilight1abc"); err == nil {
				t.Fatal("a self-contradictory declaration was relayed to the participant")
			}
		})
	}
}

// The hold reason reaches the caller, because the two are not interchangeable
// advice: one resolves by waiting for an operator, the other never does.
func TestDeclarationCarriesTheHoldReason(t *testing.T) {
	for _, want := range []string{HeldReplacesActive, HeldAddressInUse} {
		t.Run(want, func(t *testing.T) {
			as := newPayoutAS(t)
			as.declare = func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusAccepted)
				_, _ = w.Write([]byte(`{"status":"PENDING","address":"twilight1abc","effective":false,"held_for":"` + want + `"}`))
			}
			doc, err := as.client(t).DeclarePayoutAddress(context.Background(), "twilight1abc")
			if err != nil {
				t.Fatalf("declare: %v", err)
			}
			if doc.HeldFor != want {
				t.Fatalf("held_for = %q, want %q", doc.HeldFor, want)
			}
		})
	}
}

// Absence is an ordinary state now, not a 404. The three states a participant
// can be in must stay distinguishable, because collapsing them is what made
// an actively earning participant read as a failed one.
func TestStandingReportsAllThreeStates(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		body                    string
		wantActive, wantPending bool
	}{
		{"never declared", `{}`, false, false},
		{"proposed only", `{"pending":{"status":"PENDING","address":"twilight1p","effective":false}}`, false, true},
		{"active only", `{"active":{"status":"ACTIVE","address":"twilight1a","effective":true}}`, true, false},
		{"active with a correction", `{"active":{"status":"ACTIVE","address":"twilight1a","effective":true},` +
			`"pending":{"status":"PENDING","address":"twilight1p","effective":false}}`, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			as := newPayoutAS(t)
			as.show = func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.body))
			}
			got, err := as.client(t).PayoutStanding(context.Background())
			if err != nil {
				t.Fatalf("standing: %v", err)
			}
			if (got.Active != nil) != tc.wantActive || (got.Pending != nil) != tc.wantPending {
				t.Fatalf("active=%v pending=%v, want %v/%v", got.Active != nil, got.Pending != nil, tc.wantActive, tc.wantPending)
			}
		})
	}
}

// The refusal that must survive the split: a PENDING proposal claiming to be
// in force. Telling a participant an unapproved address is live is the failure
// the whole declare/activate separation exists to prevent.
func TestPendingClaimingEffectiveIsStillRefused(t *testing.T) {
	as := newPayoutAS(t)
	as.show = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"pending":{"status":"PENDING","address":"twilight1p","effective":true}}`))
	}
	if _, err := as.client(t).PayoutStanding(context.Background()); err == nil {
		t.Fatal("a PENDING proposal claiming to be effective was relayed to the participant")
	}
}

// A transport failure is still a failure, not an empty standing.
func TestStandingFailureIsNotAnEmptyState(t *testing.T) {
	as := newPayoutAS(t)
	as.show = func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) }
	if _, err := as.client(t).PayoutStanding(context.Background()); err == nil {
		t.Fatal("a 502 was reported as 'nothing declared'")
	}
}

// The client has no way to activate anything. Enumerated rather than
// asserted in prose, so adding one has to pass through this test.
func TestPayoutClientCannotActivate(t *testing.T) {
	as := newPayoutAS(t)
	client := as.client(t)
	// The declaration endpoint is the only payout URL this client builds.
	if got := client.payoutEndpoint(); got != as.srv.URL+"/v1/payout/declaration" {
		t.Fatalf("payout endpoint is %q", got)
	}
	// And the only methods it sends there are PUT and GET — proved by the
	// AS refusing everything else, which is what would happen if a future
	// change reached for POST .../activate.
	as.declare = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(pendingJSON))
	}
	as.show = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"pending":` + pendingJSON + `}`))
	}
	if _, err := client.DeclarePayoutAddress(context.Background(), "twilight1abc"); err != nil {
		t.Fatalf("declare: %v", err)
	}
	if _, err := client.PayoutStanding(context.Background()); err != nil {
		t.Fatalf("show: %v", err)
	}
}

// A refusal arrives in the contract's error envelope and its code and message
// reach the caller. The AS emitted a flat {code, message} at first, which
// parses to nothing here — an operator would have seen "AS refused with status
// 503" and lost the sentence saying the chain could not be asked and no
// binding was created. Payout §11.3 turns on that distinction reaching a
// person.
func TestPayoutRefusalSurfacesTheASReason(t *testing.T) {
	as := newPayoutAS(t)
	as.declare = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"code":"PAYOUT_ADDRESS_VALIDATION_UNAVAILABLE","message":"the chain could not be asked; no binding was created"}}`))
	}

	_, err := as.client(t).DeclarePayoutAddress(context.Background(), "twilight1abc")
	if err == nil {
		t.Fatal("a 503 was reported as success")
	}
	for _, want := range []string{"PAYOUT_ADDRESS_VALIDATION_UNAVAILABLE", "no binding was created"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal lost %q: %v", want, err)
		}
	}
}

// Two responses that would print a blank or misleading line to a participant.
// The whole value of echoing the declaration back is that they check the
// canonical rendering against what they meant.
func TestDeclarationWithNoAddressIsRefused(t *testing.T) {
	as := newPayoutAS(t)
	as.declare = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"status":"PENDING","effective":false}`))
	}
	if _, err := as.client(t).DeclarePayoutAddress(context.Background(), "twilight1abc"); err == nil {
		t.Fatal("a declaration naming no address was shown to the participant")
	}
}

// A whitespace-only address is refused before it becomes a request.
func TestWhitespaceAddressNeverReachesTheAS(t *testing.T) {
	as := newPayoutAS(t)
	as.declare = func(w http.ResponseWriter, _ *http.Request) {
		t.Error("a whitespace-only address was sent to the AS")
		w.WriteHeader(http.StatusBadRequest)
	}
	if _, err := as.client(t).DeclarePayoutAddress(context.Background(), "   "); err == nil {
		t.Fatal("a whitespace-only address was accepted")
	}
}

// `provider` is meaningful only where the AS accepts OPENROUTER_V1.
//
// Under SEARCH_ROUTER_V1 the participant holds no provider credential at all
// (§35.1, MINIS-VER-006), so there is no binding for a registration to
// establish. Asking discovery lets the proxy say that plainly instead of
// relaying a refusal the participant cannot interpret.
func TestAcceptsOpenRouterProfileFollowsDiscovery(t *testing.T) {
	for _, tc := range []struct {
		name     string
		profiles []string
		want     bool
	}{
		{"OpenRouter only", []string{"OPENROUTER_V1"}, true},
		{"both profiles", []string{"OPENROUTER_V1", "SEARCH_ROUTER_V1"}, true},
		{"search router only", []string{"SEARCH_ROUTER_V1"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			as := newPayoutAS(t)
			as.sourceProfiles = tc.profiles
			got, err := as.client(t).AcceptsOpenRouterProfile(context.Background())
			if err != nil {
				t.Fatalf("read discovery: %v", err)
			}
			if got != tc.want {
				t.Fatalf("accepts=%v, want %v for %v", got, tc.want, tc.profiles)
			}
		})
	}
}

// A 401 on the wire drops the cached access token, so the retry carries a
// different one.
//
// The unit test beside this one calls InvalidateAccessToken directly, which
// proves the cache can be dropped and NOT that anything drops it — removing
// the 401 branch from authedRequest left that test green. This one drives a
// real refusal through the real request path, which is where the branch
// either exists or does not.
//
// What it protects is the behavior the Slot 3 run depended on without
// knowing: before the cache every call rotated, so their 401 "missing scope
// mining:join" cleared on an immediate retry. A cache without this branch
// would re-offer the rejected token for its whole nominal lifetime.
func TestA401OnTheWireDropsTheCachedAccessToken(t *testing.T) {
	as := &rotatingAS{spent: map[string]bool{}, expiresIn: 900}
	f := newPayoutAS(t)
	f.token = as.handle
	calls := 0
	f.show = func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"code":"INVALID_TOKEN","message":"access token invalid or missing scope mining:join"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"active":{"status":"ACTIVE","address":"twilight1a","canonical_address":"twilight1a","effective":true}}`))
	}

	c := f.client(t)
	if _, err := c.PayoutStanding(context.Background()); err == nil {
		t.Fatal("a 401 was reported as success")
	}
	if _, err := c.PayoutStanding(context.Background()); err != nil {
		t.Fatalf("the retry after a 401 failed: %v", err)
	}
	if redeemed, reuse, _, _ := as.counts(); redeemed != 2 || reuse != 0 {
		t.Fatalf("%d rotations (%d reuse) across a 401 and its retry, want 2 and 0 — the rejected token was offered again", redeemed, reuse)
	}
}
