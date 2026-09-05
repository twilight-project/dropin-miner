package auth

// Enrollment-assertion redemption (AS MINIS-VER-013, ESC-029).
//
// The assertion replaces the user-authentication step and nothing else, so
// what this suite checks is mostly that nothing else changed: the grant must
// produce the same durable artifact the device flow produces, and must refuse
// anything less rather than enroll an installation into a weaker state that
// only shows up later.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
)

type assertAS struct {
	srv       *httptest.Server
	advertise bool
	token     func(w http.ResponseWriter, r *http.Request)
	sawForm   url.Values
	sawDPoP   bool
}

func newAssertAS(t *testing.T) *assertAS {
	t.Helper()
	f := &assertAS{advertise: true}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/oauth-authorization-server", func(w http.ResponseWriter, _ *http.Request) {
		base := f.srv.URL
		meta := map[string]any{
			"issuer":                        base,
			"authorization_endpoint":        base + "/oauth/authorize",
			"token_endpoint":                base + "/oauth/token",
			"device_authorization_endpoint": base + "/oauth/device_authorization",
			"revocation_endpoint":           base + "/oauth/revoke",
			"jwks_uri":                      base + "/oauth/jwks.json",
		}
		if f.advertise {
			meta["grant_types_supported"] = []string{"authorization_code", "refresh_token", GrantTypeJWTBearer}
		} else {
			meta["grant_types_supported"] = []string{"authorization_code", "refresh_token"}
		}
		_ = json.NewEncoder(w).Encode(meta)
	})
	mux.HandleFunc("POST /oauth/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		f.sawForm = r.PostForm
		f.sawDPoP = r.Header.Get("DPoP") != ""
		f.token(w, r)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *assertAS) client(t *testing.T) (*OAuthClient, *Store) {
	t.Helper()
	store, err := OpenStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
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
	return oc, store
}

func boundTokenJSON(rt string) string {
	return `{"access_token":"at-1","refresh_token":"` + rt + `","token_type":"DPoP","expires_in":600,"scope":"mining:join mining:read"}`
}

// The redemption is a jwt-bearer grant carrying the assertion, sent with a
// DPoP proof, and it leaves behind the same stored refresh authorization the
// device flow leaves.
func TestRedeemEnrollmentAssertionPersistsTheGrant(t *testing.T) {
	as := newAssertAS(t)
	as.token = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(boundTokenJSON("rt-from-assertion")))
	}
	oc, store := as.client(t)

	if _, err := oc.RedeemEnrollmentAssertion(context.Background(), "  the.assertion.jwt  "); err != nil {
		t.Fatalf("redeem: %v", err)
	}
	if got := as.sawForm.Get("grant_type"); got != GrantTypeJWTBearer {
		t.Errorf("grant_type = %q", got)
	}
	// Trimmed: a token pasted from a web page arrives with whitespace, and
	// an AS comparing bytes would reject it for no reason a person could see.
	if got := as.sawForm.Get("assertion"); got != "the.assertion.jwt" {
		t.Errorf("assertion = %q, want it trimmed", got)
	}
	if !as.sawDPoP {
		t.Error("the redemption was sent without a DPoP proof; the resulting tokens would be bearer tokens")
	}
	if got := as.sawForm.Get("scope"); got != strings.Join(NormalScopes, " ") {
		t.Errorf("scope = %q, want the normal proxy scopes and no more", got)
	}

	stored, ok, err := store.LoadRefreshToken()
	if err != nil || !ok {
		t.Fatalf("no refresh authorization stored after enrollment: ok=%v err=%v", ok, err)
	}
	if stored != "rt-from-assertion" {
		t.Errorf("stored refresh token %q", stored)
	}
}

// An AS that does not offer the grant is reported as such, before the
// request. A token endpoint's refusal is indistinguishable from a bad
// assertion, and telling a person to check their token when the server never
// had the door is the worst available answer.
func TestAssertionEnrollmentRefusedBeforeTheRequestWhenUnsupported(t *testing.T) {
	as := newAssertAS(t)
	as.advertise = false
	as.token = func(w http.ResponseWriter, _ *http.Request) {
		t.Error("the token endpoint was called against an AS that does not advertise the grant")
		w.WriteHeader(http.StatusBadRequest)
	}
	oc, _ := as.client(t)

	_, err := oc.RedeemEnrollmentAssertion(context.Background(), "the.assertion.jwt")
	if err == nil {
		t.Fatal("redeemed against an AS that does not offer the grant")
	}
	if !strings.Contains(err.Error(), "does not offer assertion enrollment") {
		t.Fatalf("unhelpful refusal: %v", err)
	}
}

// Two responses that would enroll this installation into a weaker state than
// the device flow does. Both are refused rather than stored.
func TestAssertionGrantRefusesADegradedResponse(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"unbound token", `{"access_token":"at-1","refresh_token":"rt-1","token_type":"Bearer","expires_in":600}`},
		{"no refresh token", `{"access_token":"at-1","token_type":"DPoP","expires_in":600}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			as := newAssertAS(t)
			as.token = func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.body))
			}
			oc, store := as.client(t)

			if _, err := oc.RedeemEnrollmentAssertion(context.Background(), "the.assertion.jwt"); err == nil {
				t.Fatalf("accepted a response with %s", tc.name)
			}
			if _, ok, _ := store.LoadRefreshToken(); ok {
				t.Fatal("a refused enrollment stored a refresh authorization anyway")
			}
		})
	}
}

// An empty assertion never reaches the network.
func TestEmptyAssertionIsRefusedLocally(t *testing.T) {
	as := newAssertAS(t)
	as.token = func(w http.ResponseWriter, _ *http.Request) {
		t.Error("an empty assertion was sent to the AS")
		w.WriteHeader(http.StatusBadRequest)
	}
	oc, _ := as.client(t)
	if _, err := oc.RedeemEnrollmentAssertion(context.Background(), "   "); err == nil {
		t.Fatal("an empty assertion was accepted")
	}
}
