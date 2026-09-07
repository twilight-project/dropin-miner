package auth

// Provider authorization (§37.2, TWILIGHT_MINING_PROXY_AS_V1_12).
//
// Two properties carry this door, and neither is about the happy path.
//
// The proxy MUST NOT exchange the code with the provider. If it did, and
// then told the AS who the participant was, that would be a
// participant-supplied identifier and MINIS-VER-003 forbids it at MUST
// level. So the code has exactly one destination, and that is asserted by
// enumerating the destinations rather than by reading the code.
//
// The advertised template is DISPLAYED and never fetched, which is the whole
// reason it is exempt from the same-origin rule. "Never fetched" asserted
// only by reading the code is the weakest kind of claim on a surface where a
// helpful refactor could add a HEAD request to validate the URL, so it gets
// a server that counts.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/twilight-project/dropin-miner/pkg/wire"
)

// corpusTemplate reads enrollment_authorization_template from the shared L3
// fixture, which is where it belongs now that the corpus carries it: the
// happy path of this suite exercises the template the AS design repository
// publishes, not one this file invented that happens to agree with it.
//
// The value is the shape measured against OpenRouter on 2026-09-03 — three
// authorizations across two accounts, callback_url absent, the code rendered
// on screen, no redirect, which is what makes the door headless.
func corpusTemplate(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("../../testdata/fixtures/wire/discovery_document.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc wire.DiscoveryDocument
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.EnrollmentAuthorizationTemplate == "" {
		t.Fatal("the shared fixture carries no enrollment_authorization_template; the mirror " +
			"is stale and this suite would be testing a value of its own invention")
	}
	return doc.EnrollmentAuthorizationTemplate
}

type seenRequest struct {
	path string
	body string
}

type provAS struct {
	srv *httptest.Server

	advertiseGrant bool
	// template is the enrollment_authorization_template served; "" omits
	// the key entirely, which is how an AS says it does not offer the door.
	template string
	token    func(w http.ResponseWriter, r *http.Request)

	mu       sync.Mutex
	seen     []seenRequest
	sawForm  url.Values
	sawDPoP  bool
	tokenHit int
}

func newProvAS(t *testing.T) *provAS {
	t.Helper()
	f := &provAS{advertiseGrant: true, template: corpusTemplate(t)}

	// Every request is recorded whole, then handed on with its body intact,
	// so TestTheCodeReachesOnlyTheASTokenEndpoint can enumerate destinations
	// without the recording itself changing what the handlers see.
	record := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			var body []byte
			if r.Body != nil {
				body, _ = io.ReadAll(io.LimitReader(r.Body, 1<<16))
				r.Body = io.NopCloser(bytes.NewReader(body))
			}
			f.mu.Lock()
			f.seen = append(f.seen, seenRequest{path: r.URL.Path, body: string(body)})
			f.mu.Unlock()
			next(w, r)
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET "+WellKnownPath, record(func(w http.ResponseWriter, _ *http.Request) {
		var doc wire.DiscoveryDocument
		if err := json.Unmarshal(fixtureDocument(t, f.srv.URL), &doc); err != nil {
			t.Error(err)
			return
		}
		// The fixture already carries the corpus template; this assignment
		// is what lets a test serve a DIFFERENT one — an off-allowlist host,
		// a loopback server, or none at all.
		doc.EnrollmentAuthorizationTemplate = f.template
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(&doc)
	}))
	mux.HandleFunc("GET /.well-known/oauth-authorization-server", record(func(w http.ResponseWriter, _ *http.Request) {
		base := f.srv.URL
		grants := []string{"authorization_code", "refresh_token"}
		if f.advertiseGrant {
			grants = append(grants, GrantTypeProviderAuthorization)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                        base,
			"authorization_endpoint":        base + "/oauth/authorize",
			"token_endpoint":                base + "/oauth/token",
			"device_authorization_endpoint": base + "/oauth/device_authorization",
			"revocation_endpoint":           base + "/oauth/revoke",
			"jwks_uri":                      base + "/oauth/jwks.json",
			"grant_types_supported":         grants,
		})
	}))
	mux.HandleFunc("POST /oauth/token", record(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		f.mu.Lock()
		f.sawForm = r.PostForm
		f.sawDPoP = r.Header.Get("DPoP") != ""
		f.tokenHit++
		f.mu.Unlock()
		f.token(w, r)
	}))
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *provAS) client(t *testing.T) (*OAuthClient, *Store) {
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

func (f *provAS) requests() []seenRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]seenRequest(nil), f.seen...)
}

func okToken(rt string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"at-1","refresh_token":"` + rt +
			`","token_type":"DPoP","expires_in":600,"scope":"mining:join mining:read"}`))
	}
}

// The grant is the extension grant carrying the code and the verifier, sent
// with a DPoP proof, and it leaves behind the same stored refresh
// authorization the device flow leaves — the artifact is identical on
// purpose, so nothing downstream can tell which door was used.
func TestProviderAuthorizationPersistsTheGrant(t *testing.T) {
	as := newProvAS(t)
	as.token = okToken("rt-from-provider-auth")
	oc, store := as.client(t)

	pending, err := oc.StartProviderAuthorization(context.Background(), wire.SourceProfileOpenRouterV1)
	if err != nil {
		t.Fatal(err)
	}
	if pending.Host != "openrouter.ai" {
		t.Errorf("Host = %q, want openrouter.ai", pending.Host)
	}
	if _, err := pending.Redeem(context.Background(), "  code-from-the-screen \n"); err != nil {
		t.Fatal(err)
	}

	if !as.sawDPoP {
		t.Error("the redemption carried no DPoP proof; the resulting tokens would not be bound to this installation")
	}
	for field, want := range map[string]string{
		"grant_type": GrantTypeProviderAuthorization,
		"code":       "code-from-the-screen",
		"client_id":  ClientID,
		"scope":      strings.Join(NormalScopes, " "),
	} {
		if got := as.sawForm.Get(field); got != want {
			t.Errorf("form %s = %q, want %q", field, got, want)
		}
	}
	if as.sawForm.Get("code_verifier") == "" {
		t.Error("no code_verifier was sent; the AS cannot complete the PKCE exchange without it")
	}

	stored, ok, err := store.LoadRefreshToken()
	if err != nil || !ok {
		t.Fatalf("refresh token not persisted (ok=%t): %v", ok, err)
	}
	if stored != "rt-from-provider-auth" {
		t.Errorf("stored refresh token = %q", stored)
	}
}

// §37.2's downgrade rule from the wire side: the AS published the method in
// the template, so it already knows, and a method the client supplies is a
// downgrade vector because plain is legal PKCE.
func TestTheRedemptionDoesNotSendCodeChallengeMethod(t *testing.T) {
	as := newProvAS(t)
	as.token = okToken("rt-1")
	oc, _ := as.client(t)

	pending, err := oc.StartProviderAuthorization(context.Background(), wire.SourceProfileOpenRouterV1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pending.Redeem(context.Background(), "code-1"); err != nil {
		t.Fatal(err)
	}
	if _, present := as.sawForm["code_challenge_method"]; present {
		t.Errorf("the redemption sent code_challenge_method=%q; §37.2 forbids the AS to accept "+
			"a client-supplied method, and sending one invites an AS that does",
			as.sawForm.Get("code_challenge_method"))
	}
}

// The URL the participant authorizes against and the verifier the AS
// receives must be two halves of one PKCE pair. Computed independently here
// rather than by calling the same helper the implementation uses, which
// would agree with itself no matter what it did.
func TestTheChallengeShownMatchesTheVerifierSent(t *testing.T) {
	as := newProvAS(t)
	as.token = okToken("rt-1")
	oc, _ := as.client(t)

	pending, err := oc.StartProviderAuthorization(context.Background(), wire.SourceProfileOpenRouterV1)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(pending.URL)
	if err != nil {
		t.Fatal(err)
	}
	shown := u.Query().Get("code_challenge")
	if shown == "" {
		t.Fatal("the authorization URL carries no code_challenge")
	}

	if _, err := pending.Redeem(context.Background(), "code-1"); err != nil {
		t.Fatal(err)
	}
	sent := as.sawForm.Get("code_verifier")

	sum := sha256.Sum256([]byte(sent))
	want := base64.RawURLEncoding.EncodeToString(sum[:])
	if shown != want {
		t.Errorf("the challenge shown is not S256(verifier sent):\n  shown %q\n  want  %q", shown, want)
	}
}

// THE CODE HAS ONE DESTINATION. Enumerate every request that left this
// process and prove the code appears in exactly one of them.
func TestTheCodeReachesOnlyTheASTokenEndpoint(t *testing.T) {
	const code = "unmistakable-code-value-9f3a"

	as := newProvAS(t)
	as.token = okToken("rt-1")
	oc, _ := as.client(t)

	pending, err := oc.StartProviderAuthorization(context.Background(), wire.SourceProfileOpenRouterV1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pending.Redeem(context.Background(), code); err != nil {
		t.Fatal(err)
	}

	var carrying []string
	for _, r := range as.requests() {
		if strings.Contains(r.body, code) {
			carrying = append(carrying, r.path)
		}
	}
	if len(carrying) != 1 || carrying[0] != "/oauth/token" {
		t.Errorf("the code appeared in requests to %v; it may reach the AS token endpoint and "+
			"nothing else (MINIS-VER-003 forbids the proxy exchanging it itself)", carrying)
	}
	// And it must not be in a URL anywhere: a query string is logged by
	// every intermediary that touches it.
	for _, r := range as.requests() {
		if strings.Contains(r.path, code) {
			t.Errorf("the code appeared in a request path: %s", r.path)
		}
	}
}

// THE ADVERTISED TEMPLATE IS NEVER FETCHED.
//
// Reachable only because checkProviderAuthorizationTemplate permits a port
// on loopback and this test supplies its own allowlist;
// TestNoLoopbackInTheProductionAllowlist proves no release can take the same
// path.
// CONNECTIONS are counted, not requests, and that distinction was found by
// injecting the defect rather than by reasoning about it. A probe written
// with an ordinary http.Client fails TLS verification against this server's
// self-signed certificate, so it never reaches the handler — and a test
// counting handler invocations reports zero for code that did reach out,
// resolved the host, and opened a socket. Reaching out IS the fetch.
//
// THE WRONG OBSERVABLE IS THE GENERAL HAZARD, NOT A DETAIL OF THIS TEST.
// Any "we never call X" test that counts handler invocations against a TLS
// server asserts something weaker than it appears to: it proves the callee
// never *answered*, not that the caller never *tried*, and every failure
// mode short of a completed HTTP exchange — TLS refusal, a 500, a dropped
// connection, a wrong path — looks like success. When a test's subject is
// that no request was made, the boundary to measure is the connection.
// ConnState at StateNew is where that is observable.
func TestTheAuthorizationTemplateIsNeverFetched(t *testing.T) {
	var mu sync.Mutex
	conns, reqs := 0, 0
	provider := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		reqs++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	provider.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			mu.Lock()
			conns++
			mu.Unlock()
		}
	}
	provider.StartTLS()
	t.Cleanup(provider.Close)

	as := newProvAS(t)
	as.template = provider.URL + "/auth?code_challenge={code_challenge}&code_challenge_method=S256"
	as.token = okToken("rt-1")
	oc, _ := as.client(t)

	local := map[string]string{wire.SourceProfileOpenRouterV1: "127.0.0.1"}
	pending, err := oc.startProviderAuthorization(context.Background(), wire.SourceProfileOpenRouterV1, local)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pending.Redeem(context.Background(), "code-1"); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	if conns != 0 || reqs != 0 {
		t.Errorf("the advertised authorization template was reached out to (%d connection(s), "+
			"%d request(s)); it is exempt from the same-origin rule precisely because it is "+
			"only ever displayed", conns, reqs)
	}
}

// §37.2 requires the AS to name the remedy and to say that an
// over-privileged key should be deleted. That text is the only part the
// participant can act on, so it survives structured rather than flattened
// into a sentence beginning "enrollment failed".
func TestARefusalKeepsTheRemedyIntact(t *testing.T) {
	const remedy = "The authorized key has a non-zero spending limit. " +
		"Delete it in your OpenRouter account and authorize again, setting the limit to 0."

	as := newProvAS(t)
	as.token = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":` +
			strconv.Quote(remedy) + `}`))
	}
	oc, _ := as.client(t)

	pending, err := oc.StartProviderAuthorization(context.Background(), wire.SourceProfileOpenRouterV1)
	if err != nil {
		t.Fatal(err)
	}
	_, err = pending.Redeem(context.Background(), "code-1")
	if err == nil {
		t.Fatal("a 400 was accepted")
	}
	var refused *AuthorizationRefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("refusal is not an *AuthorizationRefusedError (%T); the caller cannot relay "+
			"the remedy verbatim from a flattened string", err)
	}
	if refused.Description != remedy {
		t.Errorf("the remedy did not survive:\n  got  %q\n  want %q", refused.Description, remedy)
	}
	if refused.Status != http.StatusBadRequest || refused.Code != "invalid_grant" {
		t.Errorf("status/code lost: %d %q", refused.Status, refused.Code)
	}
}

// Anything less than a bound, renewable authorization is refused rather than
// enrolled into a weaker state that only shows up later.
func TestTheGrantRefusesAnythingLessThanTheOtherDoorsProduce(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"an unbound token, replayable by anyone who saw it",
			`{"access_token":"at","refresh_token":"rt","token_type":"Bearer","expires_in":600}`},
		{"no refresh token, so this installation could never renew",
			`{"access_token":"at","token_type":"DPoP","expires_in":600}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			as := newProvAS(t)
			as.token = func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.body))
			}
			oc, store := as.client(t)

			pending, err := oc.StartProviderAuthorization(context.Background(), wire.SourceProfileOpenRouterV1)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := pending.Redeem(context.Background(), "code-1"); err == nil {
				t.Fatal("accepted " + tc.name)
			}
			if _, ok, _ := store.LoadRefreshToken(); ok {
				t.Error("something was persisted from a refused redemption")
			}
		})
	}
}

// The two ways an AS can decline the door, kept apart because they are
// different faults with different remedies — no feature at all, versus half
// of one — and because a participant should learn either BEFORE authorizing.
func TestTheTwoWaysAnASDeclinesTheDoor(t *testing.T) {
	t.Run("no template advertised", func(t *testing.T) {
		as := newProvAS(t)
		as.template = ""
		oc, _ := as.client(t)

		_, err := oc.StartProviderAuthorization(context.Background(), wire.SourceProfileOpenRouterV1)
		if !errors.Is(err, ErrNoProviderAuthorization) {
			t.Fatalf("err = %v, want ErrNoProviderAuthorization", err)
		}
	})
	t.Run("template advertised, grant not offered", func(t *testing.T) {
		as := newProvAS(t)
		as.advertiseGrant = false
		oc, _ := as.client(t)

		_, err := oc.StartProviderAuthorization(context.Background(), wire.SourceProfileOpenRouterV1)
		if !errors.Is(err, ErrProviderAuthorizationGrantUnsupported) {
			t.Fatalf("err = %v, want ErrProviderAuthorizationGrantUnsupported", err)
		}
	})
}

// An AS steering participants somewhere this build does not permit is
// refused before anyone is shown a URL.
func TestAnOffAllowlistTemplateNeverProducesAURL(t *testing.T) {
	as := newProvAS(t)
	as.template = "https://evil.example/auth?code_challenge={code_challenge}&code_challenge_method=S256"
	oc, _ := as.client(t)

	pending, err := oc.StartProviderAuthorization(context.Background(), wire.SourceProfileOpenRouterV1)
	if err == nil {
		t.Fatalf("accepted an off-allowlist template and produced %q", pending.URL)
	}
	if !strings.Contains(err.Error(), "openrouter.ai") {
		t.Errorf("the refusal does not name the permitted host: %v", err)
	}
}

// PKCE binds one challenge to one code. A retry against a held verifier
// could only fail, so the verifier is consumed and starting again is the
// structural outcome rather than an instruction in a comment.
func TestAPendingAuthorizationRedeemsAtMostOnce(t *testing.T) {
	as := newProvAS(t)
	as.token = okToken("rt-1")
	oc, _ := as.client(t)

	pending, err := oc.StartProviderAuthorization(context.Background(), wire.SourceProfileOpenRouterV1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pending.Redeem(context.Background(), "code-1"); err != nil {
		t.Fatal(err)
	}
	before := as.tokenHit

	if _, err := pending.Redeem(context.Background(), "code-2"); err == nil {
		t.Fatal("a second redemption against the same verifier was allowed")
	}
	if as.tokenHit != before {
		t.Error("the second redemption reached the token endpoint; it must be refused locally, " +
			"before a spent challenge is presented again")
	}
}

// Also after a refusal: the pending authorization is spent either way, so a
// caller cannot re-prompt against it.
func TestAPendingAuthorizationIsSpentByARefusalToo(t *testing.T) {
	as := newProvAS(t)
	as.token = func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
	}
	oc, _ := as.client(t)

	pending, err := oc.StartProviderAuthorization(context.Background(), wire.SourceProfileOpenRouterV1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pending.Redeem(context.Background(), "code-1"); err == nil {
		t.Fatal("the refusal was not reported")
	}
	before := as.tokenHit

	if _, err := pending.Redeem(context.Background(), "code-1"); err == nil {
		t.Fatal("a retry against the refused authorization was allowed; PKCE needs a fresh " +
			"challenge, so the participant must start again")
	}
	// Erroring is not enough: a second attempt that reaches the AS and is
	// refused again looks identical from the caller's side, and would leave
	// a CLI free to loop on "paste it again" against a spent challenge. The
	// refusal has to happen here.
	if as.tokenHit != before {
		t.Error("the retry reached the token endpoint; a spent challenge must be refused " +
			"locally rather than presented again")
	}
}

func TestAnEmptyCodeNeverLeavesTheProcess(t *testing.T) {
	as := newProvAS(t)
	as.token = okToken("rt-1")
	oc, _ := as.client(t)

	pending, err := oc.StartProviderAuthorization(context.Background(), wire.SourceProfileOpenRouterV1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pending.Redeem(context.Background(), "   \n"); err == nil {
		t.Fatal("an empty code was redeemed")
	}
	if as.tokenHit != 0 {
		t.Error("an empty code reached the token endpoint")
	}
}
