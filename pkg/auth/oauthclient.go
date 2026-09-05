package auth

// The proxy's OAuth client (P-I2): x/oauth2 (ADR-0005) drives the
// protocol flows — authorization code + PKCE S256 over an RFC 8252
// loopback redirect, and the RFC 8628 device grant — through the
// DPoP-injecting transport. Refresh rotation persists through the
// custody store BEFORE tokens are used (a lost rotated token is a lost
// authorization).

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"
)

// ClientID is the public client identity (contract §12; the per-machine
// identity is the DPoP key, not the client record).
const ClientID = "tokendrop-proxy"

// Scopes of the normal proxy authorization (contract §16).
var NormalScopes = []string{"mining:join", "mining:read", "provider-verification:manage"}

// OAuthMetadata is the RFC 8414 subset the proxy consumes.
type OAuthMetadata struct {
	Issuer                      string `json:"issuer"`
	AuthorizationEndpoint       string `json:"authorization_endpoint"`
	TokenEndpoint               string `json:"token_endpoint"`
	DeviceAuthorizationEndpoint string `json:"device_authorization_endpoint"`
	RevocationEndpoint          string `json:"revocation_endpoint"`
	JWKSURI                     string `json:"jwks_uri"`
	// GrantTypesSupported is read so `enroll --assertion` can say "this AS
	// does not offer assertion enrollment" instead of relaying a token
	// endpoint's refusal, which is indistinguishable from a bad assertion.
	// It is not validated below: it names grants, not endpoints, so the
	// same-origin rule has nothing to check.
	GrantTypesSupported []string `json:"grant_types_supported"`
}

// GrantTypeJWTBearer is RFC 7523 §2.1, the grant the AS redeems enrollment
// assertions under (AS `MINIS-VER-013`).
const GrantTypeJWTBearer = "urn:ietf:params:oauth:grant-type:jwt-bearer" // #nosec G101 -- an RFC 7523 grant-type URN, not a credential

// Supports reports whether the AS advertises a grant.
func (m *OAuthMetadata) Supports(grant string) bool {
	for _, g := range m.GrantTypesSupported {
		if g == grant {
			return true
		}
	}
	return false
}

// OAuthMetadata fetches and validates the AS's OAuth metadata. Every
// endpoint must live on the discovered document's own origin — a
// metadata document steering the proxy to a foreign token endpoint is
// the same attack AUTH-022 refuses for redirects.
func (d *Discoverer) OAuthMetadata(ctx context.Context) (*OAuthMetadata, error) {
	endpoint := d.origin.JoinPath("/.well-known/oauth-authorization-server").String()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("auth: build metadata request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := d.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("auth: oauth metadata fetch: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("auth: oauth metadata returned status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxDiscoveryBytes))
	if err != nil {
		return nil, fmt.Errorf("auth: read oauth metadata: %w", err)
	}
	var meta OAuthMetadata
	if err := json.Unmarshal(body, &meta); err != nil {
		return nil, fmt.Errorf("auth: parse oauth metadata: %w", err)
	}
	for name, v := range map[string]string{
		"issuer":                        meta.Issuer,
		"authorization_endpoint":        meta.AuthorizationEndpoint,
		"token_endpoint":                meta.TokenEndpoint,
		"device_authorization_endpoint": meta.DeviceAuthorizationEndpoint,
		"revocation_endpoint":           meta.RevocationEndpoint,
		"jwks_uri":                      meta.JWKSURI,
	} {
		u, err := url.Parse(v)
		if err != nil || !sameOrigin(u, d.origin) {
			return nil, fmt.Errorf("auth: oauth metadata %s is off-origin or invalid (%q); refusing a different AS identity", name, v)
		}
	}
	return &meta, nil
}

// OAuthClient runs the proxy's OAuth flows against one AS.
//
// It holds the discoverer rather than the endpoints, and resolves them on
// first use. That is the whole of the fail-open property at startup: this
// constructor touches no network, so an AS that is slow or down cannot delay
// the process that forwards inference (ESC-028).
type OAuthClient struct {
	disc      *Discoverer
	store     *Store
	proofer   *Proofer
	transport *dpopTransport

	// mu guards cached. It is a process-local cache of the access token,
	// NOT of the refresh token — see Refresh.
	mu     sync.Mutex
	cached *oauth2.Token
}

// accessTokenMargin is how long an access token must still be good for
// before Refresh will hand it back rather than rotate.
//
// The AS issues 15-minute access tokens (contract §17: short-lived), so a
// minute of slack costs one rotation in fifteen and removes every race
// between "valid when we checked" and "expired when the AS saw it".
const accessTokenMargin = time.Minute

// cachedAccessToken returns the process's current access token if it is
// still good, and nil otherwise.
func (c *OAuthClient) cachedAccessToken() *oauth2.Token {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cached == nil || c.cached.AccessToken == "" {
		return nil
	}
	if c.cached.Expiry.IsZero() || time.Now().Add(accessTokenMargin).After(c.cached.Expiry) {
		return nil
	}
	return c.cached
}

// InvalidateAccessToken drops the cached access token so the next Refresh
// rotates for a new one.
//
// Callers use this when the AS REJECTS a token this cache handed them.
// Without it a token the AS has stopped accepting would be re-offered for
// the rest of its nominal lifetime, turning a one-tick failure into a
// fifteen-minute stall — which is the same "looks fine, is not" shape the
// cache is meant to reduce, arriving from the other direction.
func (c *OAuthClient) InvalidateAccessToken() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cached = nil
}

// NewOAuthClient assembles the client from custody alone.
//
// It used to fetch OAuth metadata here, and that one call was the only
// network I/O in the whole startup path. It ran BEFORE the listener opened,
// so a slow AS held the proxy shut for up to the discovery client's timeout —
// and on failure the caller abandoned the entire mining plane for the life of
// the process, which meant the spool and collector were never built and every
// observation was silently discarded until somebody restarted.
//
// Deferring the fetch costs one startup diagnostic and buys both back: the
// listener opens immediately, and a failing AS degrades per-call and heals by
// itself when it returns, because the runtime paths already re-resolve
// through the discoverer's TTL cache on every use.
func NewOAuthClient(_ context.Context, d *Discoverer, store *Store) (*OAuthClient, error) {
	key, err := store.DPoPKey()
	if err != nil {
		return nil, err
	}
	proofer, err := NewProofer(key)
	if err != nil {
		return nil, err
	}
	return &OAuthClient{
		disc:      d,
		store:     store,
		proofer:   proofer,
		transport: newDPoPTransport(proofer),
	}, nil
}

// oauthConfig resolves the endpoints, fetching metadata if the discoverer's
// cache is cold or stale. Every caller already holds a context, so the fetch
// carries the caller's deadline rather than a startup one.
func (c *OAuthClient) oauthConfig(ctx context.Context) (oauth2.Config, error) {
	meta, err := c.disc.OAuthMetadata(ctx)
	if err != nil {
		return oauth2.Config{}, err
	}
	return oauth2.Config{
		ClientID: ClientID,
		Endpoint: oauth2.Endpoint{
			AuthURL:       meta.AuthorizationEndpoint,
			TokenURL:      meta.TokenEndpoint,
			DeviceAuthURL: meta.DeviceAuthorizationEndpoint,
		},
		Scopes: NormalScopes,
	}, nil
}

// revocationURL resolves the revocation endpoint, on the same terms.
func (c *OAuthClient) revocationURL(ctx context.Context) (string, error) {
	meta, err := c.disc.OAuthMetadata(ctx)
	if err != nil {
		return "", err
	}
	return meta.RevocationEndpoint, nil
}

// JKT is the installation key thumbprint.
func (c *OAuthClient) JKT() string { return c.proofer.JKT() }

// httpCtx routes every x/oauth2 request through the DPoP transport.
func (c *OAuthClient) httpCtx(ctx context.Context) context.Context {
	return context.WithValue(ctx, oauth2.HTTPClient, &http.Client{
		Transport: c.transport,
		Timeout:   30 * time.Second,
	})
}

// persist writes the rotated refresh authorization durably BEFORE the
// caller sees the new tokens.
// persistLocked is persist under the cross-process lock, for the paths that
// establish a FIRST refresh authorization rather than rotating one.
//
// Those paths are not the read-modify-write Refresh is, so they cannot cause
// reuse. What they can do is race a daemon tick that is rotating: both write
// atomically, one wins, and if enrollment loses, the participant has just
// been told they are enrolled while the file holds a token from the family
// they were replacing. If that family is dead — which is usually why somebody
// re-enrolls — they are stuck holding a dead token and were shown no error.
//
// NOT for use from Refresh, which already holds the lock. These locks are not
// reentrant and calling this there would deadlock until the bound expires.
func (c *OAuthClient) persistLocked(ctx context.Context, tok *oauth2.Token) error {
	release, err := c.store.lockRefreshToken(ctx)
	if err != nil {
		return err
	}
	defer release()
	return c.persist(tok)
}

func (c *OAuthClient) persist(tok *oauth2.Token) error {
	if tok.RefreshToken == "" {
		return nil
	}
	return c.store.SaveRefreshToken(tok.RefreshToken)
}

// --- authorization code + PKCE over a loopback redirect (RFC 8252) ---

// PendingAuthorization is a started interactive flow: send the
// participant to URL, then Wait for the tokens.
type PendingAuthorization struct {
	// URL is the authorization URL for the system browser.
	URL string
	// RedirectURI is the loopback callback (for diagnostics).
	RedirectURI string
	wait        func(ctx context.Context) (*oauth2.Token, error)
	Close       func()
}

// Wait blocks until the loopback callback fires, then exchanges the
// code (PKCE-verified, DPoP-proofed) and persists the refresh token.
func (p *PendingAuthorization) Wait(ctx context.Context) (*oauth2.Token, error) {
	return p.wait(ctx)
}

// StartAuthorization opens the loopback listener and builds the
// authorization URL: PKCE S256, fresh state, and the dpop_jkt binding
// parameter so the grant is key-bound from the very first step.
func (c *OAuthClient) StartAuthorization(ctx context.Context) (*PendingAuthorization, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("auth: loopback listener: %w", err)
	}
	redirectURI := fmt.Sprintf("http://127.0.0.1:%d/callback", listener.Addr().(*net.TCPAddr).Port)

	verifier := oauth2.GenerateVerifier()
	stateRaw := make([]byte, 24)
	if _, err := rand.Read(stateRaw); err != nil {
		_ = listener.Close()
		return nil, fmt.Errorf("auth: generate state: %w", err)
	}
	state := base64.RawURLEncoding.EncodeToString(stateRaw)

	cfg, err := c.oauthConfig(ctx)
	if err != nil {
		return nil, err
	}
	cfg.RedirectURL = redirectURI
	authURL := cfg.AuthCodeURL(state,
		oauth2.S256ChallengeOption(verifier),
		oauth2.SetAuthURLParam("dpop_jkt", c.proofer.JKT()),
	)

	type callback struct {
		code string
		err  error
	}
	results := make(chan callback, 1)
	server := &http.Server{
		ReadHeaderTimeout: 10 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/callback" {
				http.NotFound(w, r)
				return
			}
			q := r.URL.Query()
			// Stray requests (prefetches, local port probes, wrong
			// state) are IGNORED, not fatal: only a state-matching
			// callback may conclude the flow — anything else would let
			// any local process cancel an authorization (RFC 8252).
			if q.Get("state") != state {
				http.Error(w, "unrecognized request", http.StatusBadRequest)
				return
			}
			// Non-blocking: only the FIRST result concludes the flow;
			// a duplicate state-matching callback (page refresh) must
			// never block a handler goroutine forever.
			switch {
			case q.Get("error") != "":
				http.Error(w, "authorization refused", http.StatusBadRequest)
				select {
				case results <- callback{err: fmt.Errorf("auth: authorization refused: %s", q.Get("error"))}:
				default:
				}
			case q.Get("code") == "":
				http.Error(w, "missing code", http.StatusBadRequest)
			default:
				_, _ = io.WriteString(w, "Authorization complete. You can close this page.")
				select {
				case results <- callback{code: q.Get("code")}:
				default:
				}
			}
		}),
	}
	go func() { _ = server.Serve(listener) }()
	closeServer := func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}

	wait := func(ctx context.Context) (*oauth2.Token, error) {
		defer closeServer()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case cb := <-results:
			if cb.err != nil {
				return nil, cb.err
			}
			tok, err := cfg.Exchange(c.httpCtx(ctx), cb.code, oauth2.VerifierOption(verifier))
			if err != nil {
				return nil, fmt.Errorf("auth: code exchange: %w", err)
			}
			if err := c.persistLocked(ctx, tok); err != nil {
				return nil, err
			}
			return tok, nil
		}
	}
	return &PendingAuthorization{URL: authURL, RedirectURI: redirectURI, wait: wait, Close: closeServer}, nil
}

// --- device grant (RFC 8628, headless bootstrap, contract §14) ---

// StartDeviceAuthorization requests a device/user code pair.
func (c *OAuthClient) StartDeviceAuthorization(ctx context.Context) (*oauth2.DeviceAuthResponse, error) {
	cfg, err := c.oauthConfig(ctx)
	if err != nil {
		return nil, err
	}
	resp, err := cfg.DeviceAuth(c.httpCtx(ctx))
	if err != nil {
		return nil, fmt.Errorf("auth: device authorization: %w", err)
	}
	return resp, nil
}

// WaitForDeviceApproval polls the token endpoint (interval/slow_down
// honored by x/oauth2) and persists the refresh token.
func (c *OAuthClient) WaitForDeviceApproval(ctx context.Context, da *oauth2.DeviceAuthResponse) (*oauth2.Token, error) {
	cfg, err := c.oauthConfig(ctx)
	if err != nil {
		return nil, err
	}
	tok, err := cfg.DeviceAccessToken(c.httpCtx(ctx), da)
	if err != nil {
		return nil, fmt.Errorf("auth: device approval: %w", err)
	}
	if err := c.persistLocked(ctx, tok); err != nil {
		return nil, err
	}
	return tok, nil
}

// --- refresh + revocation ---

// Refresh rotates the stored refresh authorization and durably persists
// the successor before returning the new tokens (§18.1 client side).
//
// The whole cycle runs under the cross-process lock (refreshlock.go),
// because load, spend and persist are a read-modify-write over one file
// that the daemon and every CLI command share. The lock covers endpoint
// resolution too, which is a network fetch on a cold cache: metadata is
// good for DefaultMetadataTTL and the daemon's minute tick keeps it
// warm, so paying for it inside the lock costs nothing in practice and
// leaves this function's error ordering exactly as it was — an
// installation with no refresh authorization still says so before it
// says anything about the AS.
func (c *OAuthClient) Refresh(ctx context.Context) (*oauth2.Token, error) {
	// The access token is reusable until it expires; the refresh token is
	// not reusable at all. Handing back a live access token is what keeps
	// those two facts from being confused.
	if tok := c.cachedAccessToken(); tok != nil {
		return tok, nil
	}

	release, err := c.store.lockRefreshToken(ctx)
	if err != nil {
		return nil, err
	}
	defer release()

	// Checked again under the lock. Without this, two callers that both
	// miss the cache each rotate: the first refreshes, the second waits on
	// the lock and then rotates a perfectly good token it could have had.
	// The waiting is what makes the second check worth making.
	if tok := c.cachedAccessToken(); tok != nil {
		return tok, nil
	}

	stored, ok, err := c.store.LoadRefreshToken()
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errors.New("auth: no refresh authorization; interactive authorization required")
	}
	cfg, err := c.oauthConfig(ctx)
	if err != nil {
		return nil, err
	}
	tok, err := cfg.TokenSource(c.httpCtx(ctx), &oauth2.Token{RefreshToken: stored}).Token()
	if err != nil {
		return nil, fmt.Errorf("auth: refresh: %w", err)
	}
	if err := c.persist(tok); err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.cached = tok
	c.mu.Unlock()
	return tok, nil
}

// Revoke revokes the stored refresh authorization at the AS (RFC 7009) and,
// on success, deletes it locally.
//
// ON SUCCESS — not regardless of outcome, which is what this comment used to
// claim and what the body has never done. Every failure path returns before
// the delete: a load error, an unresolvable revocation endpoint, a transport
// failure, or any status other than 200.
//
// That is the right way round, and worth stating so nobody restores the old
// claim by "fixing" the code to match it. Revoking requires presenting the
// token, so a client that discarded it after a failed attempt would have
// destroyed its only means of retrying and left a live credential at the AS
// with no way to reach it. A secret still on disk after a revocation that
// failed is the smaller harm, and the caller is told it failed. RFC 7009 has
// the endpoint answer 200 for a token that is already invalid, so retrying is
// safe and a 200 means "not valid any more" rather than "something was just
// killed".
//
// TWO THINGS TO SETTLE BEFORE THIS GETS ITS FIRST PRODUCTION CALLER.
//
// It has none. Revocation in this system is an AS-side operator act
// (CONTRACT-AUTH-012, owner AS, proven by AUTH-013a at GATE-1) and no
// requirement asks the proxy to revoke its own authorization; the only
// callers are tests, and the GATE-1 live test uses this to manufacture a
// revoked token whose refusal is the actual assertion. So decide what the
// operator-facing act is before wiring one up — "decommission this machine"
// and "stop mining here" are not the same request and do not want the same
// behavior on failure.
//
// And unlike Refresh, this takes no cross-process lock. A daemon tick
// refreshing concurrently can persist a successor after this deletes the
// file, leaving a token on disk behind a family the AS has already revoked.
// Harmless while nothing calls this during normal operation, and not
// harmless once something does.
func (c *OAuthClient) Revoke(ctx context.Context) error {
	stored, ok, err := c.store.LoadRefreshToken()
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	form := url.Values{
		"token":           {stored},
		"token_type_hint": {"refresh_token"},
		"client_id":       {ClientID},
	}
	revokeURL, err := c.revocationURL(ctx)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, revokeURL,
		strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("auth: build revocation request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := (&http.Client{Transport: c.transport, Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return fmt.Errorf("auth: revocation: %w", err)
	}
	drainAndClose(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("auth: revocation returned status %d", resp.StatusCode)
	}
	return c.store.DeleteRefreshToken()
}

// --- enrollment assertion (RFC 7523) ---

// RedeemEnrollmentAssertion exchanges a provider-signed enrollment assertion
// for a DPoP-bound grant and persists the refresh token, exactly as the
// browser and device flows do (AS `MINIS-VER-013`).
//
// The assertion replaces the user-authentication step and nothing else. There
// is no separate code path for what happens afterwards, deliberately: the
// stored refresh authorization this produces is the same artifact the other
// two flows produce, so everything downstream of enrollment cannot tell which
// door was used and cannot come to depend on one.
//
// The assertion is single-use at the AS. A transport failure after the request
// left the machine may therefore have consumed it, so a retry can legitimately
// fail — which is why the caller is told to obtain a new one rather than
// invited to try again.
func (c *OAuthClient) RedeemEnrollmentAssertion(ctx context.Context, assertion string) (*oauth2.Token, error) {
	if strings.TrimSpace(assertion) == "" {
		return nil, errors.New("auth: enrollment assertion is empty")
	}
	meta, err := c.disc.OAuthMetadata(ctx)
	if err != nil {
		return nil, err
	}
	if !meta.Supports(GrantTypeJWTBearer) {
		return nil, fmt.Errorf("auth: this AS does not offer assertion enrollment (%s is not in grant_types_supported); use the device flow", GrantTypeJWTBearer)
	}

	form := url.Values{
		"grant_type": {GrantTypeJWTBearer},
		"assertion":  {strings.TrimSpace(assertion)},
		"client_id":  {ClientID},
		"scope":      {strings.Join(NormalScopes, " ")},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, meta.TokenEndpoint,
		strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("auth: build assertion request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	// Through the DPoP transport: the proof is what binds the resulting
	// tokens to this installation's key, and it is the same transport the
	// capability exchange uses.
	resp, err := (&http.Client{Transport: c.transport, Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("auth: assertion redemption: %w", err)
	}
	defer drainAndClose(resp.Body)
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("auth: read assertion response: %w", err)
	}
	var out struct {
		AccessToken      string `json:"access_token"`
		RefreshToken     string `json:"refresh_token"`
		TokenType        string `json:"token_type"`
		ExpiresIn        int64  `json:"expires_in"`
		Scope            string `json:"scope"`
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("auth: parse assertion response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		// The AS refuses every bad assertion identically on purpose, so
		// there is nothing here to elaborate and elaborating would invent a
		// distinction the AS declined to make.
		if out.ErrorDescription != "" {
			return nil, fmt.Errorf("auth: enrollment assertion refused (%d %s): %s", resp.StatusCode, out.Error, out.ErrorDescription)
		}
		return nil, fmt.Errorf("auth: enrollment assertion refused (%d %s)", resp.StatusCode, out.Error)
	}
	if !strings.EqualFold(out.TokenType, "DPoP") {
		// An unbound token here would be replayable by anyone who saw it,
		// and accepting one would silently drop the sender constraint the
		// rest of this client assumes.
		return nil, fmt.Errorf("auth: assertion grant returned token_type %q, want DPoP", out.TokenType)
	}
	if out.RefreshToken == "" {
		return nil, errors.New("auth: assertion grant returned no refresh token; this installation could never renew")
	}
	tok := &oauth2.Token{
		AccessToken:  out.AccessToken,
		RefreshToken: out.RefreshToken,
		TokenType:    out.TokenType,
	}
	if out.ExpiresIn > 0 {
		tok.Expiry = time.Now().Add(time.Duration(out.ExpiresIn) * time.Second)
	}
	if err := c.persistLocked(ctx, tok); err != nil {
		return nil, err
	}
	return tok, nil
}
