// Package platform is the client for the search platform's public
// control plane (agent onboarding design, search-platform-agent-
// onboarding-design.md §5.1-§5.3): register, poll, enroll. It is a
// separate package from pkg/auth on purpose — two contracts, two owners.
// pkg/auth implements the AS contract (owned by
// tokendrop-auth-server-design); this implements platform.nyks.dev's
// contract (owned by search-router, this design doc). Nothing here
// touches the AS; RedeemEnrollmentAssertion (pkg/auth) still does that,
// unchanged, once this package hands back a token.
package platform

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/twilight-project/dropin-miner/pkg/auth"
)

const (
	clientTimeout = 30 * time.Second
	maxBodyBytes  = 1 << 20

	// minPollInterval floors whatever a register response advertises in
	// poll.interval_s. A misbehaving or malicious control plane
	// advertising 0 — or a stub that forgot to set it — must not turn
	// connect's poll into a tight loop against a real service.
	minPollInterval = 2 * time.Second
	// maxPollInterval ceilings the same value (WP2-adversarial-review
	// finding 11): connect's own foreground budget is a few minutes, and
	// a detached resume runs after every search regardless — an
	// advertised interval_s in the hours (or a bogus 100000) must not
	// park a poll for a day. The caller's own remaining budget still
	// bounds the actual sleep further; this is the platform-facing half
	// of that bound.
	maxPollInterval = 60 * time.Second
)

// Known refusal codes from §5.3's enroll route and §6's threat list. A
// caller (cmd/dropin-miner/connect.go) branches on these rather than
// parsing prose — e.g. re-printing the claim URL specifically on
// CodeNotClaimed.
const (
	CodeNotClaimed             = "not_claimed"
	CodeMiningNotGranted       = "mining_not_granted"
	CodeAttributionUnavailable = "attribution_unavailable"
	CodeUnknownSlot            = "unknown_slot"
)

// ErrAgentNotFound is Status's answer to a 404: an unknown agent, or a
// key that does not belong to it — the design deliberately gives both
// the same answer (§5.2), one no oracle.
var ErrAgentNotFound = errors.New("platform: agent not found")

// newPlatformClient is the only kind of http.Client this package may
// construct, mirroring pkg/auth/credentialclient.go's
// newCredentialClient in shape: bounded, same-origin redirects via
// auth.SameOriginRedirects (exported by that package precisely for
// reuse like this — see its own doc comment) because Status and Enroll
// carry the participant's freshly-minted sr- key in Authorization, even
// though Register itself carries no credential yet.
// TestNoBareHTTPClientInThisPackage enforces this module-wide, the same
// sweep cmd/dropin-miner/boundary_test.go runs.
func newPlatformClient() *http.Client {
	return &http.Client{
		Timeout:       clientTimeout,
		CheckRedirect: auth.SameOriginRedirects,
	}
}

// Client talks to one platform base URL (config.Platform.BaseURL).
type Client struct {
	baseURL string
	http    *http.Client
}

// New builds a Client. baseURL is trusted as already validated
// (https-or-loopback, invariant 5) by pkg/config; this package does not
// re-validate it.
func New(baseURL string) *Client {
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), http: newPlatformClient()}
}

// Registration is what Register returns (§5.1), flattened and with the
// poll interval already floor-clamped.
type Registration struct {
	AgentID        string
	Key            string
	ClaimURL       string
	ClaimCode      string
	ClaimExpiresAt string
	PollInterval   time.Duration
	Tier           string
}

type registerWireResponse struct {
	AgentID        string `json:"agent_id"`
	Key            string `json:"key"`
	ClaimURL       string `json:"claim_url"`
	ClaimCode      string `json:"claim_code"`
	ClaimExpiresAt string `json:"claim_expires_at"`
	Poll           struct {
		URL       string `json:"url"`
		IntervalS int    `json:"interval_s"`
	} `json:"poll"`
	Tier string `json:"tier"`
}

// Register calls POST /v1/agents/register. name and requestedScopes are
// both optional; per §5.1, "a client that has nothing to say omits the
// fields rather than inventing values" — so both are left out of the
// request body entirely when empty, not sent as "" / [].
// requestedScopes is a hint only (the claim page's pre-tick); it grants
// nothing on its own.
func (c *Client) Register(ctx context.Context, name string, requestedScopes []string) (*Registration, error) {
	body := map[string]any{}
	if name != "" {
		body["name"] = name
	}
	if len(requestedScopes) > 0 {
		body["requested_scopes"] = requestedScopes
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("platform: encode register request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/agents/register", bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("platform: build register request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("platform: register request failed: %w", err)
	}
	defer drainAndClose(resp.Body)
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return nil, fmt.Errorf("platform: read register response: %w", err)
	}
	if resp.StatusCode != http.StatusCreated {
		return nil, refusal(resp.StatusCode, data)
	}
	var wire registerWireResponse
	if err := json.Unmarshal(data, &wire); err != nil {
		return nil, fmt.Errorf("platform: parse register response: %w", err)
	}
	if wire.AgentID == "" || wire.Key == "" || wire.ClaimURL == "" {
		return nil, errors.New("platform: register response missing agent_id, key or claim_url")
	}
	if err := validateClaimURL(wire.ClaimURL, c.baseURL); err != nil {
		return nil, err
	}
	if hasControlChar(wire.ClaimCode) {
		return nil, errors.New("platform: claim_code contains a control character; refusing")
	}
	return &Registration{
		AgentID:        wire.AgentID,
		Key:            wire.Key,
		ClaimURL:       wire.ClaimURL,
		ClaimCode:      wire.ClaimCode,
		ClaimExpiresAt: wire.ClaimExpiresAt,
		Tier:           wire.Tier,
		PollInterval:   clampPollInterval(time.Duration(wire.Poll.IntervalS) * time.Second),
	}, nil
}

func clampPollInterval(d time.Duration) time.Duration {
	if d < minPollInterval {
		return minPollInterval
	}
	if d > maxPollInterval {
		return maxPollInterval
	}
	return d
}

// controlCharPattern is any C0 control character (including \n, \r, \t)
// or DEL — none legitimately appears in a claim URL, a claim code, or a
// slot name. Not a full sanitizer: a REFUSAL, not a strip, because a
// platform response containing one is not a shape this client trusts
// enough to guess what was meant (WP2-adversarial-review finding 12).
func hasControlChar(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

// isLoopbackHostname mirrors pkg/config's and pkg/auth's own
// isLoopbackHost — each package that validates a URL's host keeps this
// tiny check locally rather than importing another package for four
// lines (the existing house convention: see pkg/auth/discovery.go and
// pkg/config/config.go's own copies).
func isLoopbackHostname(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// validateClaimURL enforces invariant 12: the claim URL a register
// response hands back is printed to a terminal and persisted to disk
// verbatim — a control character in it could forge the client's own
// output, and an off-origin URL could point a participant at a page
// that is not actually this platform's. Absolute HTTPS (or loopback,
// matching this codebase's http(s)-or-loopback convention elsewhere),
// origin exactly equal to baseURL's own.
func validateClaimURL(raw, baseURL string) error {
	if hasControlChar(raw) {
		return errors.New("platform: claim_url contains a control character; refusing")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("platform: claim_url does not parse: %w", err)
	}
	if !u.IsAbs() {
		return errors.New("platform: claim_url is not an absolute URL")
	}
	httpsOrLoopback := u.Scheme == "https" || (u.Scheme == "http" && isLoopbackHostname(u.Hostname()))
	if !httpsOrLoopback {
		return fmt.Errorf("platform: claim_url scheme %q is not https (or http on loopback)", u.Scheme)
	}
	base, err := url.Parse(baseURL)
	if err != nil {
		return fmt.Errorf("platform: configured base URL does not parse: %w", err)
	}
	if u.Scheme != base.Scheme || u.Host != base.Host {
		return fmt.Errorf("platform: claim_url origin %s://%s does not match the configured platform.base_url origin %s://%s",
			u.Scheme, u.Host, base.Scheme, base.Host)
	}
	return nil
}

// AgentStatus is what Status returns (§5.2), flattened.
type AgentStatus struct {
	Status             string // "unclaimed" | "claimed" | "expired"
	Scopes             []string
	ClaimExpiresAt     string
	ClaimedAt          string
	MiningAvailable    bool
	MiningSlots        []string
	LastEnrollmentSlot string
	LastEnrollmentAt   string
	// ParticipantHasOtherMiningAgent is true when this agent's owning
	// participant already has the mining scope granted on a DIFFERENT
	// claimed agent (design f0ddb69 §2.3/§5.5: several agents per
	// participant draw one share; only one holds a given epoch, so
	// enabling mining on more than one is wasteful and noisy, not
	// forbidden). Only the platform can know this — it sees every agent
	// under the participant's org, which no single installation does —
	// so this field does not exist in §5.2's literal spec text and is an
	// assumption pending WP1 confirmation, flagged where it is consumed
	// (cmd/dropin-miner/mining.go's askMiningQuestion).
	ParticipantHasOtherMiningAgent bool
}

// HasScope reports whether scope was granted at the claim.
func (s *AgentStatus) HasScope(scope string) bool {
	for _, sc := range s.Scopes {
		if sc == scope {
			return true
		}
	}
	return false
}

// Status calls GET /v1/agents/{id}, the client's poll. Authentication
// here is diagnostic (§5.2): the key must belong to the agent, but an
// expired-unclaimed key still authenticates so the client can learn
// "expired" and stop polling — only a revoked key, or an unknown
// agent/key pairing, answers 404.
func (c *Client) Status(ctx context.Context, agentID, key string) (*AgentStatus, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/agents/"+url.PathEscape(agentID), nil)
	if err != nil {
		return nil, fmt.Errorf("platform: build status request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("platform: status request failed: %w", err)
	}
	defer drainAndClose(resp.Body)
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return nil, fmt.Errorf("platform: read status response: %w", err)
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrAgentNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return nil, refusal(resp.StatusCode, data)
	}
	var wire struct {
		Status         string   `json:"status"`
		Scopes         []string `json:"scopes"`
		ClaimExpiresAt string   `json:"claim_expires_at"`
		ClaimedAt      string   `json:"claimed_at"`
		Mining         struct {
			Available      bool     `json:"available"`
			Slots          []string `json:"slots"`
			LastEnrollment *struct {
				Slot     string `json:"slot"`
				MintedAt string `json:"minted_at"`
			} `json:"last_enrollment"`
			// ParticipantHasOtherAgent: see AgentStatus's own doc comment.
			ParticipantHasOtherAgent bool `json:"participant_has_other_agent"`
		} `json:"mining"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return nil, fmt.Errorf("platform: parse status response: %w", err)
	}
	if wire.Status == "" {
		return nil, errors.New("platform: status response carried no status")
	}
	// WP2-adversarial-review finding 13: an unrecognized status (a typo,
	// a future value this build predates, or a hostile response) must
	// not be trusted as anything more specific than "keep polling" — the
	// alternative, falling through the switch statements callers build
	// on this value, has previously let an unrecognized string like
	// "PENDING_REVIEW" reach the enroll-and-redeem path. unclaimed is
	// the one status every caller already treats as "nothing to do yet,
	// try again later," which is the correct, safe default here.
	switch wire.Status {
	case "unclaimed", "claimed", "expired":
	default:
		wire.Status = "unclaimed"
		wire.Scopes = nil
	}
	for _, slot := range wire.Mining.Slots {
		if hasControlChar(slot) {
			return nil, errors.New("platform: a slot name in the status response contains a control character; refusing")
		}
	}
	out := &AgentStatus{
		Status:                         wire.Status,
		Scopes:                         wire.Scopes,
		ClaimExpiresAt:                 wire.ClaimExpiresAt,
		ClaimedAt:                      wire.ClaimedAt,
		MiningAvailable:                wire.Mining.Available,
		MiningSlots:                    wire.Mining.Slots,
		ParticipantHasOtherMiningAgent: wire.Mining.ParticipantHasOtherAgent,
	}
	if wire.Mining.LastEnrollment != nil {
		out.LastEnrollmentSlot = wire.Mining.LastEnrollment.Slot
		out.LastEnrollmentAt = wire.Mining.LastEnrollment.MintedAt
	}
	return out, nil
}

// Enroll calls POST /v1/agents/enroll, returning the enrollment token
// exactly as the AS receives it (§5.3, §7) — the caller
// (cmd/dropin-miner/connect.go) redeems it via
// auth.OAuthClient.RedeemEnrollmentAssertion, unchanged.
//
// The response's token field name is not given literally in §5.3 (only
// "the enrollment token exactly as the /mining page mints it today" is
// specified); {"token": "..."} is assumed as the minimal natural shape,
// matching how §5.1's response IS given literally. Confirm against WP1
// once it lands.
func (c *Client) Enroll(ctx context.Context, agentID, key, slot string) (string, error) {
	raw, err := json.Marshal(map[string]string{"slot": slot})
	if err != nil {
		return "", fmt.Errorf("platform: encode enroll request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/agents/enroll", bytes.NewReader(raw))
	if err != nil {
		return "", fmt.Errorf("platform: build enroll request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("platform: enroll request failed: %w", err)
	}
	defer drainAndClose(resp.Body)
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return "", fmt.Errorf("platform: read enroll response: %w", err)
	}
	// The scope check this line depends on lives on the SERVER (§5.3:
	// "Requires: ... mining in scopes"). This client only surfaces
	// whatever status/code the server sent — it does not re-derive the
	// decision. That is deliberate: a client-side "is mining granted"
	// check here would be a second, driftable copy of the server's own
	// gate (agent onboarding design §6.2: "checks the scope on the key
	// that authenticated, never a request field").
	if resp.StatusCode != http.StatusOK {
		return "", refusal(resp.StatusCode, data)
	}
	var wire struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return "", fmt.Errorf("platform: parse enroll response: %w", err)
	}
	if wire.Token == "" {
		return "", errors.New("platform: enroll response carried no token")
	}
	return wire.Token, nil
}

// RefusalError is a structured refusal from the platform's public
// routes: a machine-readable code plus the message, so a caller can act
// on WHICH refusal this is rather than parsing prose. The envelope
// shape ({"error":{"code","message"}}) is assumed to match the AS's own
// (pkg/auth's joinRefusal) since the design doc does not specify a
// different one and this client already borrows an AS convention
// elsewhere (auth.SameOriginRedirects).
type RefusalError struct {
	Status  int
	Code    string
	Message string
}

func (e *RefusalError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("platform: refused (%d %s): %s", e.Status, e.Code, e.Message)
	}
	return fmt.Sprintf("platform: refused with status %d", e.Status)
}

func refusal(status int, raw []byte) error {
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &env); err == nil && env.Error.Code != "" {
		return &RefusalError{Status: status, Code: env.Error.Code, Message: env.Error.Message}
	}
	return &RefusalError{Status: status}
}

func drainAndClose(body io.ReadCloser) {
	_, _ = io.Copy(io.Discard, io.LimitReader(body, 1<<16))
	_ = body.Close()
}
