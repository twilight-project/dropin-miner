package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeAS is a minimal AS: metadata + a token endpoint scripted per test.
type fakeAS struct {
	srv     *httptest.Server
	token   func(w http.ResponseWriter, r *http.Request)
	revokes atomic.Int64
	// revokeStatus is what /oauth/revoke answers. Zero means 200, so every
	// test written before revocation could fail keeps its old behavior.
	revokeStatus atomic.Int64
}

func newFakeAS(t *testing.T) *fakeAS {
	t.Helper()
	f := &fakeAS{}
	mux := http.NewServeMux()
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
	mux.HandleFunc("POST /oauth/token", func(w http.ResponseWriter, r *http.Request) { f.token(w, r) })
	mux.HandleFunc("POST /oauth/revoke", func(w http.ResponseWriter, _ *http.Request) {
		f.revokes.Add(1)
		status := int(f.revokeStatus.Load())
		if status == 0 {
			status = http.StatusOK
		}
		w.WriteHeader(status)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func newClient(t *testing.T, f *fakeAS) *OAuthClient {
	t.Helper()
	store, err := OpenStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	d, err := NewDiscoverer(DiscoveryConfig{BaseURL: f.srv.URL, ChainID: "twilight-1", SlotID: 7})
	if err != nil {
		t.Fatal(err)
	}
	c, err := NewOAuthClient(context.Background(), d, store)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func tokenJSON(access, refresh string) string {
	return tokenJSONExpiring(access, refresh, 900)
}

// tokenJSONExpiring lets a fake AS choose the access-token lifetime it
// advertises. expiresIn <= 0 omits the field entirely, which is how an AS
// declines to say — and what the client must treat as uncacheable.
func tokenJSONExpiring(access, refresh string, expiresIn int) string {
	if expiresIn <= 0 {
		return fmt.Sprintf(`{"access_token":%q,"token_type":"DPoP","refresh_token":%q}`, access, refresh)
	}
	return fmt.Sprintf(`{"access_token":%q,"token_type":"DPoP","expires_in":%d,"refresh_token":%q}`, access, expiresIn, refresh)
}

// The transport must answer a DPoP-Nonce challenge by retrying once
// with the nonce inside the proof (contract §12 / RFC 9449 §8).
func TestTokenRequestRetriesOnNonceChallenge(t *testing.T) {
	f := newFakeAS(t)
	var calls atomic.Int64
	f.token = func(w http.ResponseWriter, r *http.Request) {
		proof := r.Header.Get("DPoP")
		if proof == "" {
			t.Error("token request without DPoP proof")
		}
		if calls.Add(1) == 1 {
			w.Header().Set("DPoP-Nonce", "nonce-42")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"use_dpop_nonce"}`))
			return
		}
		// Second attempt must carry the nonce claim (payload is the
		// middle JWS segment; nonce appears as a JSON claim).
		payload := strings.Split(proof, ".")[1]
		if !strings.Contains(decodeSegment(t, payload), `"nonce":"nonce-42"`) {
			t.Errorf("retry proof missing nonce claim: %s", decodeSegment(t, payload))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(tokenJSON("at-1", "rt-1")))
	}
	c := newClient(t, f)
	if err := c.store.SaveRefreshToken("rt-0"); err != nil {
		t.Fatal(err)
	}
	tok, err := c.Refresh(context.Background())
	if err != nil {
		t.Fatalf("refresh through nonce challenge: %v", err)
	}
	if tok.AccessToken != "at-1" || calls.Load() != 2 {
		t.Fatalf("tok=%+v calls=%d", tok, calls.Load())
	}
}

// Rotation persistence: the successor refresh token must be durably
// stored before Refresh returns.
func TestRefreshPersistsRotatedToken(t *testing.T) {
	f := newFakeAS(t)
	f.token = func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if r.PostFormValue("grant_type") != "refresh_token" || r.PostFormValue("refresh_token") != "rt-old" {
			t.Errorf("unexpected token request: %v", r.PostForm)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(tokenJSON("at-2", "rt-new")))
	}
	c := newClient(t, f)
	if err := c.store.SaveRefreshToken("rt-old"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, ok, err := c.store.LoadRefreshToken()
	if err != nil || !ok || got != "rt-new" {
		t.Fatalf("rotated token not persisted: %q ok=%v err=%v", got, ok, err)
	}
}

// Off-origin OAuth metadata is a different AS identity — refused.
func TestOAuthMetadataRefusesOffOriginEndpoints(t *testing.T) {
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/.well-known/oauth-authorization-server" {
			_ = json.NewEncoder(w).Encode(map[string]string{ // #nosec G101 -- URLs, not credentials
				"issuer":                        "https://evil.example.com",
				"authorization_endpoint":        "https://evil.example.com/a",
				"token_endpoint":                "https://evil.example.com/t",
				"device_authorization_endpoint": "https://evil.example.com/d",
				"revocation_endpoint":           "https://evil.example.com/r",
				"jwks_uri":                      "https://evil.example.com/j",
			})
		}
	}))
	t.Cleanup(evil.Close)
	d, err := NewDiscoverer(DiscoveryConfig{BaseURL: evil.URL, ChainID: "twilight-1", SlotID: 7})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.OAuthMetadata(context.Background()); err == nil || !strings.Contains(err.Error(), "off-origin") {
		t.Fatalf("off-origin metadata not refused: %v", err)
	}
}

// The authorization URL must carry PKCE S256 + state + the jkt binding.
func TestStartAuthorizationURLShape(t *testing.T) {
	f := newFakeAS(t)
	c := newClient(t, f)
	pending, err := c.StartAuthorization(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer pending.Close()
	for _, needle := range []string{
		"code_challenge_method=S256",
		"code_challenge=",
		"state=",
		"dpop_jkt=" + c.JKT(),
		"client_id=" + ClientID,
		"redirect_uri=http%3A%2F%2F127.0.0.1%3A",
	} {
		if !strings.Contains(pending.URL, needle) {
			t.Errorf("authorization URL missing %q:\n%s", needle, pending.URL)
		}
	}
}

func TestRevokeDeletesLocalToken(t *testing.T) {
	f := newFakeAS(t)
	c := newClient(t, f)
	if err := c.store.SaveRefreshToken("rt-x"); err != nil {
		t.Fatal(err)
	}
	if err := c.Revoke(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.revokes.Load() != 1 {
		t.Fatalf("revocation endpoint calls = %d", f.revokes.Load())
	}
	if _, ok, _ := c.store.LoadRefreshToken(); ok {
		t.Fatal("refresh token survived revocation")
	}
}

func decodeSegment(t *testing.T, seg string) string {
	t.Helper()
	pad := strings.Repeat("=", (4-len(seg)%4)%4)
	raw, err := base64URLDecode(seg + pad)
	if err != nil {
		t.Fatalf("decode JWS segment: %v", err)
	}
	return string(raw)
}

func base64URLDecode(s string) ([]byte, error) {
	return base64.URLEncoding.DecodeString(s)
}

// RFC 8252: stray requests to the loopback callback (prefetch, local
// port probe, wrong state) must not abort the pending authorization.
func TestStrayCallbackDoesNotAbortFlow(t *testing.T) {
	f := newFakeAS(t)
	f.token = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(tokenJSON("at-ok", "rt-ok")))
	}
	c := newClient(t, f)
	pending, err := c.StartAuthorization(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer pending.Close()

	// Probe the listener with garbage first.
	for _, path := range []string{"/callback", "/callback?state=wrong&code=x", "/favicon.ico"} {
		resp, err := http.Get(pending.RedirectURI[:strings.LastIndex(pending.RedirectURI, "/callback")] + path)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}

	// The genuine callback still completes the flow.
	u, _ := url.Parse(pending.URL)
	state := u.Query().Get("state")
	go func() {
		resp, err := http.Get(pending.RedirectURI + "?state=" + url.QueryEscape(state) + "&code=genuine-code")
		if err == nil {
			_ = resp.Body.Close()
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tok, err := pending.Wait(ctx)
	if err != nil {
		t.Fatalf("flow aborted despite genuine callback: %v", err)
	}
	if tok.AccessToken != "at-ok" {
		t.Fatalf("unexpected token: %+v", tok)
	}
}

// Revoke deletes the local token ON SUCCESS, and on failure leaves it exactly
// where it is.
//
// The comment on Revoke claimed the opposite for two phases — "deletes it
// locally regardless of transport outcome" — while the body has always
// returned before the delete on every failure path. Nothing caught it because
// nothing tested a failing revocation. This does, so the comment is now a
// claim the suite can hold it to.
//
// The behavior is also the correct one and the test says so: revoking
// requires presenting the token, so discarding it after a failed attempt
// destroys the only means of retrying and strands a live credential at the AS.
func TestRevokeKeepsTheTokenWhenRevocationFails(t *testing.T) {
	t.Run("the AS refuses", func(t *testing.T) {
		f := newFakeAS(t)
		f.revokeStatus.Store(http.StatusServiceUnavailable)
		c := newClient(t, f)
		if err := c.store.SaveRefreshToken("rt-x"); err != nil {
			t.Fatal(err)
		}
		if err := c.Revoke(context.Background()); err == nil {
			t.Fatal("a refused revocation was reported as success")
		}
		stored, ok, _ := c.store.LoadRefreshToken()
		if !ok || stored != "rt-x" {
			t.Fatal("a failed revocation discarded the token, destroying the only means of retrying it")
		}
	})

	t.Run("the AS is unreachable", func(t *testing.T) {
		f := newFakeAS(t)
		c := newClient(t, f)
		if err := c.store.SaveRefreshToken("rt-x"); err != nil {
			t.Fatal(err)
		}
		f.srv.Close() // after the client is built, so discovery already resolved
		if err := c.Revoke(context.Background()); err == nil {
			t.Fatal("revocation against a closed AS was reported as success")
		}
		if _, ok, _ := c.store.LoadRefreshToken(); !ok {
			t.Fatal("a transport failure discarded the token")
		}
	})

	t.Run("nothing to revoke", func(t *testing.T) {
		f := newFakeAS(t)
		c := newClient(t, f)
		if err := c.Revoke(context.Background()); err != nil {
			t.Fatalf("revoking with no stored authorization should be a no-op: %v", err)
		}
		if f.revokes.Load() != 0 {
			t.Fatalf("the revocation endpoint was called %d times with nothing to revoke", f.revokes.Load())
		}
	})
}
