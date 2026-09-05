package auth

// The §70 activity client. Two things matter and neither is the happy path:
// that the decimal-STRING counts on the wire survive the trip, and that
// "eligible" is never reported from half an answer — an AS whose verdict and
// whose count disagree gets read the conservative way, because telling a
// participant they are earning when the AS's own verdict says otherwise is
// the mistake that costs them an epoch of attention.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/twilight-project/dropin-miner/pkg/wire"
)

// activityAS is an AS complete enough for one EpochActivity call.
type activityAS struct {
	srv *httptest.Server
	// advertise=false removes activity_status_endpoint_template from the
	// document, which is how the "this AS does not publish the route" case
	// is built rather than assumed from the corpus.
	advertise bool
	answer    func(w http.ResponseWriter, r *http.Request)
	// offRoute counts requests to an activity-shaped path this AS does not
	// serve, so a client that built the URL itself is still seen.
	offRoute int
}

func newActivityAS(t *testing.T) *activityAS {
	t.Helper()
	f := &activityAS{advertise: true}
	mux := http.NewServeMux()
	mux.HandleFunc(WellKnownPath, func(w http.ResponseWriter, _ *http.Request) {
		var doc map[string]any
		if err := json.Unmarshal(fixtureDocument(t, f.srv.URL), &doc); err != nil {
			t.Fatal(err)
		}
		if !f.advertise {
			delete(doc, "activity_status_endpoint_template")
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(doc)
	})
	mux.HandleFunc("GET /.well-known/oauth-authorization-server", func(w http.ResponseWriter, _ *http.Request) {
		base := f.srv.URL
		_ = json.NewEncoder(w).Encode(map[string]string{
			"issuer":                        base,
			"authorization_endpoint":        base + "/oauth/authorize",
			"token_endpoint":                base + "/oauth/token",
			"device_authorization_endpoint": base + "/oauth/device_authorization",
			"revocation_endpoint":           base + "/oauth/revoke",
			"jwks_uri":                      base + "/oauth/jwks.json",
		})
	})
	mux.HandleFunc("POST /oauth/token", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(tokenJSON("at-1", "rt-1")))
	})
	mux.HandleFunc("GET /v1/mining/slots/7/epochs/1042/activity", func(w http.ResponseWriter, r *http.Request) {
		// Ordinary DPoP access token, never a Participation Capability:
		// asking what has been credited cannot require having earned it.
		if r.Header.Get("Authorization") == "" || r.Header.Get("DPoP") == "" {
			t.Error("activity request arrived without a DPoP-bound access token")
		}
		f.answer(w, r)
	})
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/activity") && r.URL.Path != "/v1/mining/slots/7/epochs/1042/activity" {
			f.offRoute++
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func newActivityClient(t *testing.T, base string) *MiningClient {
	t.Helper()
	store, err := OpenStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveRefreshToken("rt-0"); err != nil {
		t.Fatal(err)
	}
	d, err := NewDiscoverer(DiscoveryConfig{BaseURL: base, ChainID: "twilight-1", SlotID: 7})
	if err != nil {
		t.Fatal(err)
	}
	oc, err := NewOAuthClient(context.Background(), d, store)
	if err != nil {
		t.Fatal(err)
	}
	return NewMiningClient(d, oc, store)
}

// The counts are decimal STRINGS on the wire (§70). A client that decoded
// them as JSON numbers would work against a fixture written with numbers and
// fail against the contract.
func TestEpochActivityDecodesTheStringCounts(t *testing.T) {
	as := newActivityAS(t)
	as.answer = func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, `{"slot_id":"7","target_epoch":"1042","verified_activity":true,
			"verified_observation_count":"4","pending_observation_count":"2",
			"rejected_observation_count":"1"}`)
	}
	got, err := newActivityClient(t, as.srv.URL).EpochActivity(context.Background(), 1042)
	if err != nil {
		t.Fatalf("activity: %v", err)
	}
	if got.VerifiedObservationCount != 4 || got.PendingObservationCount != 2 || got.RejectedObservationCount != 1 {
		t.Fatalf("counts misdecoded: %+v", got)
	}
	if got.SlotID != "7" || got.TargetEpoch != "1042" || !got.VerifiedActivity {
		t.Fatalf("document misdecoded: %+v", got)
	}
	if !got.Eligible() {
		t.Error("four verified observations with verified_activity=true is not eligible")
	}
}

// A number-encoded count is accepted too: the encoding is not something a
// participant's report should hinge on.
func TestEpochActivityAcceptsNumericCounts(t *testing.T) {
	as := newActivityAS(t)
	as.answer = func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, `{"slot_id":"7","target_epoch":"1042","verified_activity":true,
			"verified_observation_count":3,"pending_observation_count":0,"rejected_observation_count":0}`)
	}
	got, err := newActivityClient(t, as.srv.URL).EpochActivity(context.Background(), 1042)
	if err != nil {
		t.Fatalf("activity: %v", err)
	}
	if got.VerifiedObservationCount != 3 {
		t.Fatalf("numeric count misdecoded: %+v", got)
	}
}

// Eligibility over the whole input domain, not over the case that works.
//
// The threshold is one qualifying VERIFIED observation (payout/allocation
// §23). What must be unreachable is "eligible" from an answer that does not
// say so: the AS's own verdict and the count both have to hold.
func TestEligibleIsUnreachableWithoutBothHalvesOfTheAnswer(t *testing.T) {
	for _, tc := range []struct {
		name     string
		verdict  bool
		verified uint64
		want     bool
	}{
		{"the ordinary yes", true, 1, true},
		{"many verified", true, 97, true},
		{"nothing verified", false, 0, false},
		{"verdict yes, count zero", true, 0, false},
		{"count positive, verdict no", false, 5, false},
		{"both no", false, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &EpochActivity{VerifiedActivity: tc.verdict, VerifiedObservationCount: tc.verified}
			if got := a.Eligible(); got != tc.want {
				t.Fatalf("Eligible()=%v, want %v (verdict=%v verified=%d)", got, tc.want, tc.verdict, tc.verified)
			}
		})
	}
}

// The threshold is a threshold. If this constant ever becomes a weight the
// whole participant-facing vocabulary is wrong, so it is pinned here.
func TestTheActivityThresholdIsOne(t *testing.T) {
	if MinVerifiedObservations != 1 {
		t.Fatalf("MinVerifiedObservations is %d; POC-1 trusted eligibility is one qualifying VERIFIED observation",
			MinVerifiedObservations)
	}
}

// A refusal is a refusal, and carries the AS's own code so a participant can
// be told which one it was.
func TestEpochActivityReportsARefusal(t *testing.T) {
	as := newActivityAS(t)
	as.answer = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"code":"PARTICIPATION_NOT_AUTHORIZED","message":"not joined"}}`))
	}
	_, err := newActivityClient(t, as.srv.URL).EpochActivity(context.Background(), 1042)
	if err == nil {
		t.Fatal("a 403 decoded as an activity document")
	}
	if !strings.Contains(err.Error(), "PARTICIPATION_NOT_AUTHORIZED") {
		t.Fatalf("the refusal should name the AS's code: %v", err)
	}
}

// An AS that publishes no activity route is degraded to, never guessed at.
//
// Two halves, because §19 currently makes the field mandatory and so the
// interesting failure is reachable from two different directions:
//
//   - a document that omits it does not validate at all, so discovery
//     refuses it and no activity-shaped request is ever made;
//   - and if §19 ever relaxes the field to OPTIONAL — which is exactly what
//     happened to current_target_endpoint_template — the client must still
//     refuse rather than assemble the URL itself, so the guard is exercised
//     directly against a document that reached the cache without one.
func TestEpochActivityWithoutATemplateDoesNotInventOne(t *testing.T) {
	as := newActivityAS(t)
	as.advertise = false
	as.answer = func(w http.ResponseWriter, _ *http.Request) {
		t.Error("the client reached an activity route the AS did not advertise")
	}
	client := newActivityClient(t, as.srv.URL)

	if _, err := client.EpochActivity(context.Background(), 1042); err == nil {
		t.Fatal("a document missing the mandatory template produced an activity answer")
	}
	if as.offRoute != 0 {
		t.Fatalf("%d request(s) reached an activity-shaped path anyway", as.offRoute)
	}

	// The guard itself, with a validated-shaped document that simply has no
	// template — the shape §19 would produce if the field became optional.
	client.discoverer.mu.Lock()
	client.discoverer.doc = &wire.DiscoveryDocument{
		Version: wire.DiscoveryVersion, ChainID: "twilight-1", SlotID: "7",
	}
	client.discoverer.fetchedAt = time.Now()
	client.discoverer.mu.Unlock()

	_, err := client.EpochActivity(context.Background(), 1042)
	if !errors.Is(err, ErrNoActivityEndpoint) {
		t.Fatalf("err = %v, want ErrNoActivityEndpoint", err)
	}
	if as.offRoute != 0 {
		t.Fatalf("%d request(s) reached an activity-shaped path anyway", as.offRoute)
	}
}

// The service document is exposed for the health check, and it is the SAME
// document the rest of the client uses — a second Discoverer against the same
// base URL could answer differently and would double the fetches.
func TestServiceDocumentReturnsTheValidatedDocument(t *testing.T) {
	as := newActivityAS(t)
	doc, err := newActivityClient(t, as.srv.URL).ServiceDocument(context.Background())
	if err != nil {
		t.Fatalf("service document: %v", err)
	}
	if doc.ChainID != "twilight-1" || doc.SlotID != "7" {
		t.Fatalf("document is not the configured identity: %+v", doc)
	}
}
