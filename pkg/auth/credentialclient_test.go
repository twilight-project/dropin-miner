package auth

// The structural guarantee, and the reproductions underneath it.
//
// The acceptance bar itself now lives in cmd/tokendrop/boundary_test.go as
// TestEveryHTTPClientInTheModuleSetsARedirectPolicy, alongside the other
// module-wide structural invariants. It was package-scoped here first, and
// that scope was itself the defect: it could not see the two clients in
// cmd/, and would have waved a third past.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// --- behavioral reproductions of the exfil the structural test forbids ---

// redirectTrap is a host that should never be reached. It records anything it
// receives, so a test can assert not merely that the call failed but that the
// credential did not arrive.
type redirectTrap struct {
	srv *httptest.Server

	mu      sync.Mutex
	hits    int
	bodies  []string
	headers []string
}

func newRedirectTrap(t *testing.T) *redirectTrap {
	t.Helper()
	trap := &redirectTrap{}
	trap.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, 4096)
		n, _ := r.Body.Read(body)
		trap.mu.Lock()
		trap.hits++
		trap.bodies = append(trap.bodies, string(body[:n]))
		trap.headers = append(trap.headers, r.Header.Get("DPoP"))
		trap.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(trap.srv.Close)
	return trap
}

func (x *redirectTrap) report() (int, []string, []string) {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.hits, append([]string(nil), x.bodies...), append([]string(nil), x.headers...)
}

// A 307 preserves method and body. This is the exfil in its simplest form:
// an endpoint that answers 307 pointing at another host, and a client that
// would carry the request there.
func TestACredentialClientRefusesAnOffOriginRedirect(t *testing.T) {
	const secret = "rt-super-secret-value-do-not-leak"

	trap := newRedirectTrap(t)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, trap.srv.URL+"/collect", http.StatusTemporaryRedirect)
	}))
	t.Cleanup(origin.Close)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, origin.URL+"/oauth/token",
		strings.NewReader(url.Values{"token": {secret}}.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("DPoP", "proof-that-net-http-will-not-strip")

	resp, err := newCredentialClient(nil).Do(req) //nolint:bodyclose // the error path returns no body
	if err == nil {
		drainAndClose(resp.Body)
	}

	// The trap is checked BEFORE the error, so a regression reports what
	// actually leaked rather than stopping at "the redirect was followed".
	hits, bodies, headers := trap.report()
	if hits != 0 {
		t.Errorf("THE CREDENTIAL LEFT THE PROCESS: the redirect target was reached %d time(s)\n"+
			"  body sent: %q\n  DPoP header sent: %q", hits, bodies, headers)
	}
	if err == nil {
		t.Fatal("the off-origin redirect was followed")
	}
	if !strings.Contains(err.Error(), "off-origin") {
		t.Errorf("the error does not name what was refused: %v", err)
	}
	// The secret must not be in the error either — errors get logged.
	if strings.Contains(err.Error(), secret) {
		t.Errorf("the credential appears in the returned error: %v", err)
	}
}

// The policy is same-origin, NOT no-redirect. An AS reorganizing its own paths
// must not break enrollment — a rule that did would be reverted by the first
// person it inconvenienced, which is how security controls die.
func TestACredentialClientFollowsASameOriginRedirect(t *testing.T) {
	var landed bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/token":
			http.Redirect(w, r, "/oauth/v2/token", http.StatusTemporaryRedirect)
		case "/oauth/v2/token":
			landed = true
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL+"/oauth/token",
		strings.NewReader("grant_type=refresh_token"))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := newCredentialClient(nil).Do(req)
	if err != nil {
		t.Fatalf("a same-origin redirect was refused; enrollment would break on an AS that "+
			"reorganized its own paths: %v", err)
	}
	drainAndClose(resp.Body)
	if !landed {
		t.Error("the redirect was not followed to its same-origin target")
	}
}

// Scheme is part of an origin: https → http on the same host is a downgrade
// that puts the credential on the wire in the clear.
func TestACredentialClientRefusesASchemeDowngrade(t *testing.T) {
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(plain.Close)

	u, err := url.Parse(plain.URL)
	if err != nil {
		t.Fatal(err)
	}
	// Same host and port, http instead of https.
	if !sameOrigin(u, originOf(u)) {
		t.Fatal("premise wrong: originOf changed the origin")
	}
	downgraded := &url.URL{Scheme: "http", Host: u.Host, Path: "/collect"}
	https := &url.URL{Scheme: "https", Host: u.Host}
	if sameOrigin(downgraded, https) {
		t.Error("sameOrigin treats http and https on one host as the same origin; a redirect " +
			"from https to http would re-send the credential in the clear")
	}
}

// A chain is bounded, so a loop on one origin cannot spin forever holding a
// credential in flight.
func TestACredentialClientBoundsTheRedirectChain(t *testing.T) {
	// The hop counter drives the chain rather than the request path: echoing
	// a request-derived value into a Location is a taint gosec rightly flags,
	// and a fixture has no reason to do it.
	var hop atomic.Int64
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next := fmt.Sprintf("%s/hop/%d", srv.URL, hop.Add(1))
		http.Redirect(w, r, next, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(srv.Close)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL+"/a", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := newCredentialClient(nil).Do(req) //nolint:bodyclose // the error path returns no body
	if err == nil {
		drainAndClose(resp.Body)
		t.Fatal("an unbounded same-origin redirect chain was followed")
	}
	if !strings.Contains(err.Error(), "too many redirects") {
		t.Errorf("the error does not name the bound: %v", err)
	}
}
