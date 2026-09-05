package auth

// The current-target client: the AS is the proxy's only epoch source,
// so the two answers it can give — "here is the target" and "there is
// nothing open" — must never collapse into each other, and neither may
// be confused with "the AS could not answer".

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// targetAS is an AS complete enough for one CurrentTarget call: the
// twilight document, OAuth metadata, a token endpoint for the access
// token, and the current-target endpoint itself.
type targetAS struct {
	srv *httptest.Server
	// advertise, when false, omits current_target_endpoint_template
	// from the discovery document — an AS predating the endpoint.
	advertise bool
	answer    func(w http.ResponseWriter, r *http.Request)
	// probes counts every request whose path mentions current-target,
	// including ones this AS does not route: a client that assembled
	// the URL itself would still be seen here.
	probes atomic.Int64
}

func newTargetAS(t *testing.T) *targetAS {
	t.Helper()
	f := &targetAS{advertise: true}
	mux := http.NewServeMux()
	mux.HandleFunc(WellKnownPath, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(documentJSON(t, f.srv.URL, f.advertise))
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
	mux.HandleFunc("GET /v1/mining/slots/7/current-target", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" || r.Header.Get("DPoP") == "" {
			t.Error("current-target request arrived without a DPoP-bound access token")
		}
		f.answer(w, r)
	})
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "current-target") {
			f.probes.Add(1)
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// documentJSON is the shared L3 discovery fixture, repointed at origin,
// with the optional current-target template added or left out.
func documentJSON(t *testing.T, origin string, advertise bool) []byte {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(fixtureDocument(t, origin), &doc); err != nil {
		t.Fatal(err)
	}
	// Set or REMOVE, never merely set. The shared fixture carries the field
	// as of contract V1_9 §19, so relying on its absence to build the
	// "this AS does not advertise it" case stopped working the moment the
	// corpus caught up with the contract — the not-advertised case silently
	// became an advertised one, and the test that guards against the client
	// inventing the URL itself was the thing that noticed.
	if advertise {
		doc["current_target_endpoint_template"] = origin + "/v1/mining/slots/{slot_id}/current-target"
	} else {
		delete(doc, "current_target_endpoint_template")
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func newTargetClient(t *testing.T, base string) *MiningClient {
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

func writeJSON(t *testing.T, w http.ResponseWriter, body string) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if _, err := w.Write([]byte(body)); err != nil {
		t.Error(err)
	}
}

// The ordinary answer: a target, with the identifiers in the decimal
// string form the frozen contract uses everywhere else.
func TestCurrentTargetReturnsTheOpenTarget(t *testing.T) {
	as := newTargetAS(t)
	as.answer = func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, `{"status":"OPEN","target":{"slot_id":"7","target_epoch":"1042",
			"joinable":true,"distribution_mode":"RANDOM_SLOT","join_status":"NOT_JOINED","phase":"ENROLLMENT"}}`)
	}
	got, err := newTargetClient(t, as.srv.URL).CurrentTarget(context.Background())
	if err != nil {
		t.Fatalf("current target: %v", err)
	}
	if got == nil {
		t.Fatal("an offered target decoded as no target")
	}
	if got.SlotID != 7 || got.TargetEpoch != 1042 || !got.Joinable || got.DistributionMode != "RANDOM_SLOT" {
		t.Fatalf("target misdecoded: %+v", got)
	}
}

// A number-encoded identifier is accepted too: the encoding is not
// something the driver's liveness should depend on.
func TestCurrentTargetAcceptsNumericIdentifiers(t *testing.T) {
	as := newTargetAS(t)
	as.answer = func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, `{"status":"OPEN","target":{"slot_id":7,"target_epoch":1042,"joinable":true,"distribution_mode":"RANDOM_SLOT"}}`)
	}
	got, err := newTargetClient(t, as.srv.URL).CurrentTarget(context.Background())
	if err != nil || got == nil || got.TargetEpoch != 1042 {
		t.Fatalf("numeric identifiers rejected: %+v %v", got, err)
	}
}

// NO_OPEN_TARGET is an answer, not a failure: nil target, nil error.
// The AS spends a 200 on it precisely so a client can tell "poll later"
// from "cannot answer", and the client must preserve that.
func TestCurrentTargetNoOpenTargetIsNotAnError(t *testing.T) {
	as := newTargetAS(t)
	as.answer = func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, `{"status":"NO_OPEN_TARGET","target":null}`)
	}
	got, err := newTargetClient(t, as.srv.URL).CurrentTarget(context.Background())
	if err != nil {
		t.Fatalf("NO_OPEN_TARGET reported as a failure: %v", err)
	}
	if got != nil {
		t.Fatalf("NO_OPEN_TARGET produced a target: %+v", got)
	}
}

// An AS too old to advertise the endpoint is a degradation with a name
// — and the URL is never assembled locally to work around it.
func TestCurrentTargetRequiresTheAdvertisedTemplate(t *testing.T) {
	as := newTargetAS(t)
	as.advertise = false
	as.answer = func(w http.ResponseWriter, _ *http.Request) {
		t.Error("the client reached a current-target URL the AS never advertised")
		writeJSON(t, w, `{"status":"NO_OPEN_TARGET","target":null}`)
	}
	_, err := newTargetClient(t, as.srv.URL).CurrentTarget(context.Background())
	if !errors.Is(err, ErrNoCurrentTargetEndpoint) {
		t.Fatalf("missing template must be reported as such, got %v", err)
	}
	if n := as.probes.Load(); n != 0 {
		t.Fatalf("%d guessed current-target request(s) went to the wire", n)
	}
}

// A refusal is a refusal: a status code must never be read as "nothing
// open", or a broken route would look like a quiet slot forever.
func TestCurrentTargetFailureIsNotNoTarget(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusServiceUnavailable, http.StatusUnauthorized} {
		as := newTargetAS(t)
		as.answer = func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":{"code":"NO_SUCH_ROUTE","message":"nope"}}`))
		}
		got, err := newTargetClient(t, as.srv.URL).CurrentTarget(context.Background())
		if err == nil {
			t.Fatalf("status %d accepted as an answer (target %+v)", status, got)
		}
	}
}

// §19 identity discipline, applied to the answer as well as the
// document: a target for another slot is not this installation's.
func TestCurrentTargetRefusesAnotherSlot(t *testing.T) {
	as := newTargetAS(t)
	as.answer = func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, `{"status":"OPEN","target":{"slot_id":"9","target_epoch":"1042","joinable":true,"distribution_mode":"RANDOM_SLOT"}}`)
	}
	_, err := newTargetClient(t, as.srv.URL).CurrentTarget(context.Background())
	if err == nil || !strings.Contains(err.Error(), "slot") {
		t.Fatalf("a target for slot 9 was accepted by a slot-7 proxy: %v", err)
	}
}

// A contradictory answer is read the closed way: NO_OPEN_TARGET wins
// over a target carried alongside it.
func TestCurrentTargetReadsContradictionClosed(t *testing.T) {
	as := newTargetAS(t)
	as.answer = func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, `{"status":"NO_OPEN_TARGET","target":{"slot_id":"7","target_epoch":"1042","joinable":true}}`)
	}
	got, err := newTargetClient(t, as.srv.URL).CurrentTarget(context.Background())
	if err != nil || got != nil {
		t.Fatalf("contradictory answer not read closed: %+v %v", got, err)
	}
}
