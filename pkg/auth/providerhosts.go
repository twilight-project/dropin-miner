package auth

// The compiled-in trust anchor for provider authorization (§19, §37.2,
// TWILIGHT_MINING_PROXY_AS_V1_12).
//
// enrollment_authorization_template is the one value in the discovery
// document that does not live on the AS's own origin. The same-origin rule
// every other endpoint is held to is not punctured for it: that rule exists
// for values the proxy FETCHES, and this one is only ever displayed to a
// person. What replaces it is this file — an allowlist of which host each
// source profile's authorization may live on, held by the client rather
// than read from the document being checked, because a metadata document
// that could nominate its own trust anchor would be checking itself.
//
// The division of ownership is deliberate and worth stating, because the
// obvious simplification is to hold the whole URL here and it would be
// wrong. The HOST is a trust decision: changing where participants are sent
// to sign in deserves a release, and compiled-in is what a release's
// SHA256SUMS covers. The PATH, the query, and the challenge parameter are
// mechanism: they move with provider API versions, and they belong to the
// AS. Split that way, a provider renaming code_challenge to challenge is
// one AS config change and no proxy release.

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/twilight-project/dropin-miner/pkg/wire"
)

// providerAuthorizationHosts is the allowlist itself: source profile to the
// single host that profile's provider authorization may live on.
//
// Keyed by profile, not global, so an AS serving one profile cannot send
// participants to another profile's provider — and so a refusal can name
// both the host advertised and the host permitted, which is the difference
// between a diagnosis and a complaint.
//
// COMPILED IN, NOT CONFIGURED. A trust anchor a participant can edit is one
// a phishing instruction can talk them into editing, and "paste this line
// into your config and re-run enrollment" is a materially easier attack than
// anything this check is defending against. Compiled in, it is covered by
// the release's SHA256SUMS.
//
// No entry here may be a loopback host: expandChallenge's port allowance is
// reachable only for loopback, so a loopback entry would be the one way to
// turn a test affordance into a production one. TestNoLoopbackInTheProduction
// Allowlist asserts that rather than leaving it to this comment.
var providerAuthorizationHosts = map[string]string{
	wire.SourceProfileOpenRouterV1: "openrouter.ai",
}

// placeholder matches a {…} substitution slot in an advertised template.
var placeholder = regexp.MustCompile(`\{[^{}]*\}`)

// codeChallengePlaceholder is the only substitution a template may carry.
const codeChallengePlaceholder = "{code_challenge}"

// checkProviderAuthorizationTemplate validates an advertised authorization
// template against the allowlist for a profile and returns the host, which
// the caller discloses to the participant before sending them to it.
//
// hosts is a parameter rather than a direct reference to the package
// variable so the loopback path below is reachable from a test WITHOUT the
// production allowlist having to contain a loopback host. Callers outside
// tests pass providerAuthorizationHosts.
func checkProviderAuthorizationTemplate(tmpl, profile string, hosts map[string]string) (string, error) {
	if strings.TrimSpace(tmpl) == "" {
		return "", fmt.Errorf("auth: the AS advertises no enrollment_authorization_template")
	}

	want, ok := hosts[profile]
	if !ok {
		// Not "unknown profile" — this profile has no provider
		// authorization host defined, so there is nowhere it is
		// permitted to send anyone.
		return "", fmt.Errorf("auth: no provider authorization host is allowed for source profile %q", profile)
	}

	// Placeholders first: a template carrying {whatever} would be printed
	// with the brace intact and the participant would visit a broken URL —
	// a confusing failure with no diagnosis attached. The contract is
	// exactly one substitution wide, so anything else is refused here where
	// it can be named.
	found := placeholder.FindAllString(tmpl, -1)
	for _, p := range found {
		if p != codeChallengePlaceholder {
			return "", fmt.Errorf("auth: the advertised authorization template carries %s, "+
				"but %s is the only substitution this client makes", p, codeChallengePlaceholder)
		}
	}
	if len(found) == 0 {
		return "", fmt.Errorf("auth: the advertised authorization template carries no %s; "+
			"an authorization with no challenge in it is not PKCE", codeChallengePlaceholder)
	}

	u, err := url.Parse(tmpl)
	if err != nil {
		return "", fmt.Errorf("auth: parse the advertised authorization template: %w", err)
	}
	if u.Scheme != "https" {
		return "", fmt.Errorf("auth: the advertised authorization template is %q, not https; "+
			"a participant is about to authenticate at it", u.Scheme)
	}
	// Same rule, same reason, as the AS base URL: https://openrouter.ai@evil.com
	// is a host check defeated by reading left to right, and Go's error
	// strings retain the username.
	if u.User != nil {
		return "", fmt.Errorf("auth: the advertised authorization template carries userinfo; " +
			"refusing a URL whose apparent host is not its actual host")
	}

	// PKCE downgrade, refused on this side too (§37.2). RFC 7636 §4.3 makes
	// an ABSENT method mean "plain", so absence is the downgrade rather than
	// an omission — which is why this demands the parameter rather than
	// merely objecting to a bad one. The AS hardcodes S256 on its own
	// exchange; enforcing it on both sides of the wire is what turns an
	// opaque provider-side failure, arriving after the participant has
	// already authorized, into one clear message before they are sent.
	if m := u.Query().Get("code_challenge_method"); m != "S256" {
		if m == "" {
			return "", fmt.Errorf("auth: the advertised authorization template names no " +
				"code_challenge_method, which PKCE reads as \"plain\"; only S256 is accepted")
		}
		return "", fmt.Errorf("auth: the advertised authorization template names "+
			"code_challenge_method %q; only S256 is accepted", m)
	}

	host := canonicalHost(u.Hostname())
	if host == "" {
		return "", fmt.Errorf("auth: the advertised authorization template has no host")
	}
	if host != want {
		return "", fmt.Errorf("auth: the AS advertises a provider authorization host %q for "+
			"source profile %s, but this build permits only %q; participants are sent where "+
			"the binary says, not where the document says", host, profile, want)
	}
	// A port is permitted only on loopback, which no production allowlist
	// entry is: this exists so the "never fetched" proof can point a
	// template at a local test server, and it is unreachable with the
	// compiled-in map. The AS base URL carries an equally narrow loopback
	// exemption two files over, for development, on the same reasoning.
	if u.Port() != "" && !isLoopbackHost(host) {
		return "", fmt.Errorf("auth: the advertised authorization template names port %q on %q; "+
			"the allowlisted host is permitted no port", u.Port(), host)
	}

	return host, nil
}

// canonicalHost lowercases and drops a single trailing dot. "OpenRouter.ai"
// and "openrouter.ai." both resolve to the allowlisted host in DNS, so an
// exact byte comparison without this would refuse the first and — worse —
// could be talked into treating the second as a different host.
func canonicalHost(h string) string {
	return strings.TrimSuffix(strings.ToLower(h), ".")
}

// expandChallenge substitutes the S256 challenge into a template that has
// already been checked.
//
// No escaping: the challenge is base64url with no padding, so it is
// URL-safe by construction, and percent-encoding it would produce a value
// the provider would decode back to something else.
func expandChallenge(tmpl, challenge string) string {
	return strings.ReplaceAll(tmpl, codeChallengePlaceholder, challenge)
}
