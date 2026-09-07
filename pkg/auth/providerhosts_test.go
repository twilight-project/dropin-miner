package auth

// The allowlist is a trust anchor, so this suite is written the way D2 §2.0
// asks: enumerate where a participant could be sent and prove the
// destructive destinations unreachable, rather than checking that the good
// case works and inferring the rest.

import (
	"net/url"
	"strings"
	"testing"

	"github.com/twilight-project/dropin-miner/pkg/wire"
)

const goodTemplate = "https://openrouter.ai/auth?code_challenge={code_challenge}&code_challenge_method=S256"

// prod is the compiled-in allowlist, named so a test that means "the real
// one" reads differently from a test that supplies its own.
var prod = providerAuthorizationHosts

// THE PORT EXEMPTION IS UNREACHABLE IN PRODUCTION, AND THIS IS WHERE THAT
// STOPS BEING A COMMENT.
//
// checkProviderAuthorizationTemplate permits a port only on a loopback
// host, so the whole relaxation rests on no production entry being loopback.
// That property is invisible at the call site and easy to break years later
// by adding a profile — which is exactly the kind of safety that has to be
// asserted over the domain rather than argued in prose.
func TestNoLoopbackInTheProductionAllowlist(t *testing.T) {
	if len(prod) == 0 {
		t.Fatal("the production allowlist is empty; no door can be offered at all")
	}
	for profile, host := range prod {
		if host == "" {
			t.Errorf("profile %s allows an empty host, which would match a template with no host", profile)
			continue
		}
		if isLoopbackHost(host) {
			t.Errorf("profile %s allows loopback host %q: the port exemption in "+
				"checkProviderAuthorizationTemplate exists only because no production entry is "+
				"loopback, and this entry makes a test affordance reachable in a release", profile, host)
		}
		if host != canonicalHost(host) {
			t.Errorf("profile %s allows %q, which is not canonical (%q); the comparison "+
				"canonicalizes the ADVERTISED host, so a non-canonical entry can never match",
				profile, host, canonicalHost(host))
		}
		// A scheme, port, or path smuggled into an entry would compare
		// against Hostname() and silently never match — a permanently
		// closed door that looks open.
		if strings.ContainsAny(host, ":/@") {
			t.Errorf("profile %s allows %q, which is not a bare host", profile, host)
		}
	}
}

// The launch profile is present, and the profile that has no participant
// provider credential at all is absent.
func TestTheProductionAllowlistCoversOpenRouterAndNothingElse(t *testing.T) {
	if got := prod[wire.SourceProfileOpenRouterV1]; got != "openrouter.ai" {
		t.Errorf("OPENROUTER_V1 authorization host = %q, want openrouter.ai", got)
	}
	// §35.1: under SEARCH_ROUTER_V1 the AS verifies with its own operator
	// credential and the participant holds nothing, so there is no
	// authorization to send anyone to.
	if got, ok := prod[wire.SourceProfileSearchRouterV1]; ok {
		t.Errorf("SEARCH_ROUTER_V1 allows authorization host %q, but that profile gives the "+
			"participant no provider credential to authorize (§35.1)", got)
	}
}

func TestTheAdvertisedHostMustBeTheAllowlistedOne(t *testing.T) {
	for _, tc := range []struct{ name, tmpl string }{
		{"a different provider", "https://evil.com/auth?code_challenge={code_challenge}&code_challenge_method=S256"},
		{"a lookalike subdomain", "https://openrouter.ai.evil.com/auth?code_challenge={code_challenge}&code_challenge_method=S256"},
		{"a subdomain of the real host", "https://auth.openrouter.ai/auth?code_challenge={code_challenge}&code_challenge_method=S256"},
		{"a unicode homograph", "https://openrouter.аi/auth?code_challenge={code_challenge}&code_challenge_method=S256"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := checkProviderAuthorizationTemplate(tc.tmpl, wire.SourceProfileOpenRouterV1, prod)
			if err == nil {
				t.Fatalf("accepted %s: %s", tc.name, tc.tmpl)
			}
			// The refusal names BOTH, because "refused" alone leaves an
			// operator with a document and no idea which half is wrong.
			if !strings.Contains(err.Error(), "openrouter.ai") {
				t.Errorf("the refusal does not name the permitted host: %v", err)
			}
		})
	}
}

// Userinfo, from both directions, because only one of them is actually the
// userinfo check's to catch and a test that conflated them would prove
// nothing.
//
// https://openrouter.ai@evil.com is the string that defeats a host check
// written by someone reading left to right — but the host comparison
// already refuses it, since Hostname() is evil.com. Deleting the userinfo
// check leaves that case passing.
//
// https://evil.com@openrouter.ai is the one the check exists for: the host
// IS the allowlisted host, so everything else here waves it through, and
// what it carries is a credential that becomes a Basic Authorization header
// on any request and a username retained verbatim in Go's error strings.
// That is the same reasoning NewDiscoverer refuses userinfo on the AS base
// URL.
func TestUserinfoIsRefused(t *testing.T) {
	for _, tc := range []struct{ name, tmpl, host string }{
		{
			"an apparent host that is not the actual host",
			"https://openrouter.ai@evil.com/auth?code_challenge={code_challenge}&code_challenge_method=S256",
			"evil.com",
		},
		{
			"userinfo on the allowlisted host itself",
			"https://evil.com@openrouter.ai/auth?code_challenge={code_challenge}&code_challenge_method=S256",
			"openrouter.ai",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Confirm the premise rather than assume it.
			u, err := url.Parse(tc.tmpl)
			if err != nil {
				t.Fatal(err)
			}
			if u.Hostname() != tc.host {
				t.Fatalf("premise wrong: Hostname() = %q, want %q", u.Hostname(), tc.host)
			}
			if u.User == nil {
				t.Fatal("premise wrong: no userinfo parsed")
			}

			if _, err := checkProviderAuthorizationTemplate(tc.tmpl, wire.SourceProfileOpenRouterV1, prod); err == nil {
				t.Fatalf("accepted a template carrying userinfo: %s", tc.tmpl)
			}
		})
	}
}

func TestOnlyHTTPS(t *testing.T) {
	for _, tmpl := range []string{
		"http://openrouter.ai/auth?code_challenge={code_challenge}&code_challenge_method=S256",
		"ftp://openrouter.ai/auth?code_challenge={code_challenge}&code_challenge_method=S256",
	} {
		if _, err := checkProviderAuthorizationTemplate(tmpl, wire.SourceProfileOpenRouterV1, prod); err == nil {
			t.Errorf("accepted %q; a participant is about to authenticate at it", tmpl)
		}
	}
}

// One substitution wide. An unsubstituted {whatever} reaches the
// participant with the brace intact and they visit a broken URL.
func TestPlaceholdersAreExactlyOneSubstitutionWide(t *testing.T) {
	for _, tc := range []struct{ name, tmpl string }{
		{"an unknown placeholder", "https://openrouter.ai/auth?code_challenge={code_challenge}&state={state}&code_challenge_method=S256"},
		{"a misspelled one", "https://openrouter.ai/auth?code_challenge={challenge}&code_challenge_method=S256"},
		{"none at all", "https://openrouter.ai/auth?code_challenge_method=S256"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := checkProviderAuthorizationTemplate(tc.tmpl, wire.SourceProfileOpenRouterV1, prod); err == nil {
				t.Fatalf("accepted %s: %s", tc.name, tc.tmpl)
			}
		})
	}
	// Repetition is not a violation: the same challenge twice substitutes
	// consistently and is merely unusual.
	const twice = "https://openrouter.ai/auth?code_challenge={code_challenge}&echo={code_challenge}&code_challenge_method=S256"
	if _, err := checkProviderAuthorizationTemplate(twice, wire.SourceProfileOpenRouterV1, prod); err != nil {
		t.Errorf("refused a template repeating the one permitted placeholder: %v", err)
	}
}

// §37.2's downgrade rule, enforced on the participant-facing side.
//
// An ABSENT method is the downgrade, not an omission: RFC 7636 §4.3 reads
// absence as "plain". Without this the participant authorizes and the
// failure arrives later as an opaque provider error at the AS.
func TestCodeChallengeMethodMustBeS256(t *testing.T) {
	for _, tc := range []struct{ name, tmpl string }{
		{"absent, which PKCE reads as plain", "https://openrouter.ai/auth?code_challenge={code_challenge}"},
		{"explicitly plain", "https://openrouter.ai/auth?code_challenge={code_challenge}&code_challenge_method=plain"},
		{"lowercased", "https://openrouter.ai/auth?code_challenge={code_challenge}&code_challenge_method=s256"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := checkProviderAuthorizationTemplate(tc.tmpl, wire.SourceProfileOpenRouterV1, prod)
			if err == nil {
				t.Fatalf("accepted a template with code_challenge_method %s", tc.name)
			}
			if !strings.Contains(err.Error(), "S256") {
				t.Errorf("the refusal does not name what is required: %v", err)
			}
		})
	}
}

// A profile with no entry has nowhere it is permitted to send anyone.
func TestAProfileWithNoEntryIsRefused(t *testing.T) {
	if _, err := checkProviderAuthorizationTemplate(goodTemplate, wire.SourceProfileSearchRouterV1, prod); err == nil {
		t.Fatal("accepted an authorization template for a profile with no allowlisted host")
	}
	if _, err := checkProviderAuthorizationTemplate(goodTemplate, "MADE_UP_V9", prod); err == nil {
		t.Fatal("accepted an authorization template for an unknown profile")
	}
}

func TestAbsenceIsRefusedHereToo(t *testing.T) {
	for _, tmpl := range []string{"", "   "} {
		if _, err := checkProviderAuthorizationTemplate(tmpl, wire.SourceProfileOpenRouterV1, prod); err == nil {
			t.Errorf("accepted an empty template %q", tmpl)
		}
	}
}

// DNS is case-insensitive and tolerates one trailing dot; an exact byte
// comparison would refuse the first and could be argued into treating the
// second as a different host.
func TestHostComparisonFollowsDNS(t *testing.T) {
	for _, tmpl := range []string{
		"https://OpenRouter.AI/auth?code_challenge={code_challenge}&code_challenge_method=S256",
		"https://openrouter.ai./auth?code_challenge={code_challenge}&code_challenge_method=S256",
	} {
		host, err := checkProviderAuthorizationTemplate(tmpl, wire.SourceProfileOpenRouterV1, prod)
		if err != nil {
			t.Errorf("refused %q, which is the allowlisted host in DNS: %v", tmpl, err)
			continue
		}
		if host != "openrouter.ai" {
			t.Errorf("host = %q, want the canonical openrouter.ai", host)
		}
	}
}

// The port allowance, from both sides: refused on the production anchor,
// permitted on loopback only when a caller supplies a loopback map — which
// TestNoLoopbackInTheProductionAllowlist proves no release does.
func TestPortIsPermittedOnLoopbackOnly(t *testing.T) {
	const withPort = "https://openrouter.ai:8443/auth?code_challenge={code_challenge}&code_challenge_method=S256"
	if _, err := checkProviderAuthorizationTemplate(withPort, wire.SourceProfileOpenRouterV1, prod); err == nil {
		t.Error("accepted a port on the allowlisted production host")
	}

	local := map[string]string{wire.SourceProfileOpenRouterV1: "127.0.0.1"}
	const loopback = "https://127.0.0.1:8443/auth?code_challenge={code_challenge}&code_challenge_method=S256"
	if _, err := checkProviderAuthorizationTemplate(loopback, wire.SourceProfileOpenRouterV1, local); err != nil {
		t.Errorf("refused a loopback template under a loopback map; the never-fetched proof "+
			"depends on this being reachable from a test: %v", err)
	}
}

func TestTheGoodTemplateIsAccepted(t *testing.T) {
	host, err := checkProviderAuthorizationTemplate(goodTemplate, wire.SourceProfileOpenRouterV1, prod)
	if err != nil {
		t.Fatalf("refused the measured live template: %v", err)
	}
	if host != "openrouter.ai" {
		t.Errorf("host = %q, want openrouter.ai", host)
	}
}

// Substitution leaves a URL that parses, with the challenge readable as a
// query value rather than as escaped text.
func TestExpandChallengeProducesAUsableURL(t *testing.T) {
	// base64url, no padding: what S256ChallengeFromVerifier returns, and
	// deliberately including the two characters that distinguish it from
	// standard base64.
	const challenge = "abc-DEF_123ghi"

	got := expandChallenge(goodTemplate, challenge)
	if strings.Contains(got, codeChallengePlaceholder) {
		t.Fatalf("the placeholder survived substitution: %s", got)
	}
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("the substituted URL does not parse: %v", err)
	}
	if v := u.Query().Get("code_challenge"); v != challenge {
		t.Errorf("code_challenge = %q, want %q (percent-encoding it would make the provider "+
			"decode it back to something else)", v, challenge)
	}
	if v := u.Query().Get("code_challenge_method"); v != "S256" {
		t.Errorf("code_challenge_method = %q after substitution", v)
	}
}
