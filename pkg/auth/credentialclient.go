package auth

// One constructor for every client in this package that carries a credential.
//
// THE DEFECT THIS EXISTS TO CLOSE. Every credential-bearing call here used to
// build its own `&http.Client{Transport: …, Timeout: …}` — seven of them —
// and not one set CheckRedirect, so all seven inherited net/http's default of
// following up to ten redirects. 307 and 308 preserve the method AND the body,
// and Go strips only Authorization/Cookie on a cross-host redirect: a custom
// DPoP header is not stripped, and the body is re-sent regardless. A token
// endpoint answering 307 therefore re-sent, to whatever host it named:
//
//	Revoke                        the refresh token, in the token form field
//	RedeemEnrollmentAssertion     the provider-signed assertion
//	Redeem (provider auth)        the authorization code and its verifier
//	the x/oauth2 client           refresh, device and authorization-code traffic
//	join / capability / submit    the DPoP proof and the observation body
//
// The discovery client had the right policy the whole time (discovery.go,
// AUTH-022) — for a request that fetches PUBLIC METADATA. That the same
// discipline was missing from every request carrying a secret is the
// inconsistency that gave this away, and it is the argument for the fix.
//
// WHY A CONSTRUCTOR RATHER THAN SEVEN FIXES. Seven call sites each restating a
// policy is why the policy drifted, and an eighth was added by the
// provider-authorization work without anyone noticing it was missing — copied
// from the shape next to it, which was the defective shape. A constructor makes
// the next call site correct without its author having to know this comment
// exists. TestNoBareHTTPClientInThisPackage enforces it structurally, so the
// guarantee does not depend on remembering to state it.

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// credentialClientTimeout is the ceiling every AS-facing call already used.
const credentialClientTimeout = 30 * time.Second

// maxCredentialRedirects bounds a redirect chain, matching the discovery
// client. Three is not a tuning parameter: it is "a couple of hops within one
// origin is normal, a chain is not".
const maxCredentialRedirects = 3

// newCredentialClient builds the only kind of HTTP client this package may use
// for a request that carries a credential.
//
// The redirect policy is discovery.go's, unchanged in substance: bounded, and
// same-origin only. It is deliberately NOT "refuse every redirect" — an AS
// legitimately redirecting within its own origin must not break enrollment,
// and a policy that broke it would be reverted by the first person it
// inconvenienced.
//
// The origin compared against is the ORIGINAL request's (via[0]), not a
// configured one. Every endpoint these clients call was already required to
// live on the discovered document's own origin, so the first request's origin
// is the identity the caller intended; a redirect leaving it is the AS
// handing the credential to someone else, whatever the reason.
func newCredentialClient(rt http.RoundTripper) *http.Client {
	return &http.Client{
		Transport:     rt,
		Timeout:       credentialClientTimeout,
		CheckRedirect: SameOriginRedirects,
	}
}

// SameOriginRedirects is the redirect policy every http.Client in this module
// carries. TestEveryHTTPClientInTheModuleSetsARedirectPolicy enforces that
// module-wide; this is the thing to install.
//
// Exported because the clients that need it are not all in this package. The
// composition root talks to the local listener and to a chain node, and both
// have their own timeout and transport reasoning that a shared CONSTRUCTOR
// would flatten — cmd/tokendrop/client.go deliberately has no overall timeout
// because a finite one silently caps generation length, and the chain client
// sets Proxy: nil for a node endpoint the operator named. So what travels is
// the policy, not the client.
//
// (If internal/forward or internal/observe ever need a client, they may not
// import this package — the boundary tests forbid it — and the policy will
// need a stdlib-only home. Nothing there constructs one today.)
func SameOriginRedirects(req *http.Request, via []*http.Request) error {
	if len(via) >= maxCredentialRedirects {
		return errors.New("auth: too many redirects on a credential-bearing request")
	}
	// via is never empty when CheckRedirect is called, but a nil deref here
	// would turn a security control into a panic on the enrollment path, so
	// it is checked rather than assumed.
	if len(via) == 0 {
		return errors.New("auth: redirect with no originating request; refusing")
	}
	if !sameOrigin(req.URL, originOf(via[0].URL)) {
		return fmt.Errorf("auth: request redirected off-origin to %s://%s — "+
			"refusing to re-send a credential to a different host",
			req.URL.Scheme, req.URL.Host)
	}
	return nil
}

// originOf reduces a URL to the scheme+host pair sameOrigin compares, so the
// comparison cannot be accidentally widened by a path or query.
func originOf(u *url.URL) *url.URL {
	return &url.URL{Scheme: u.Scheme, Host: u.Host}
}
