package auth

// P-I4: the Participation Capability client (contract §28–§34). The
// proxy trades its normal authorization for a short-lived, slot- and
// epoch-scoped capability, treats it as opaque, and publishes it into
// the scope holder the request path snapshots. There is no refresh
// token: renewal repeats the exchange, which forces the AS to
// re-evaluate authorization every time (§31).

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/twilight-project/dropin-miner/pkg/mining/scope"
)

const (
	grantTypeTokenExchange = "urn:ietf:params:oauth:grant-type:token-exchange" // #nosec G101 -- RFC 8693 grant-type URN
	tokenTypeAccessToken   = "urn:ietf:params:oauth:token-type:access_token"   // #nosec G101 -- RFC 8693 token-type URN
	// CapabilityScope is the only scope a capability carries (§32).
	CapabilityScope = "mining:observation.submit"
	// renewalMargin re-exchanges slightly before expiry so an
	// in-flight request never starts with a capability about to die.
	renewalMargin = 30 * time.Second
)

// CapabilityClient obtains and refreshes Participation Capabilities.
type CapabilityClient struct {
	mining *MiningClient
	holder *scope.Holder

	mu      sync.Mutex
	current *scope.Context
}

// NewCapabilityClient publishes into holder, which the forwarding path
// snapshots at request start.
func NewCapabilityClient(m *MiningClient, holder *scope.Holder) *CapabilityClient {
	return &CapabilityClient{mining: m, holder: holder}
}

// capabilityResponse is the §31 token-exchange response.
type capabilityResponse struct {
	AccessToken     string `json:"access_token"`
	TokenType       string `json:"token_type"`
	ExpiresIn       int64  `json:"expires_in"`
	Scope           string `json:"scope"`
	IssuedTokenType string `json:"issued_token_type"`
	RefreshToken    string `json:"refresh_token"`
	SlotID          string `json:"twilight_slot_id"`
	TargetEpoch     string `json:"twilight_target_epoch"`
	Deadline        string `json:"observation_submission_deadline"`
	Error           string `json:"error"`
	Description     string `json:"error_description"`
	TwilightError   string `json:"twilight_error"`
}

// Ensure returns a usable capability for targetEpoch, exchanging only
// when the cached one is missing, expiring, or for another target.
// Failure leaves the previous context untouched at the holder unless it
// is itself unusable — mining degrades, inference never notices.
func (c *CapabilityClient) Ensure(ctx context.Context, targetEpoch uint64) (*scope.Context, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()
	if c.current != nil && c.current.TargetEpoch == targetEpoch &&
		now.Add(renewalMargin).Before(c.current.ExpiresAt) {
		return c.current, nil
	}
	fresh, err := c.exchange(ctx, targetEpoch)
	if err != nil {
		if c.current != nil && !c.current.Valid(now) {
			c.current = nil
			c.holder.Clear()
		}
		return nil, err
	}
	c.current = fresh
	c.holder.Set(fresh)
	return fresh, nil
}

// Clear drops participation locally (revocation, epoch rollover).
func (c *CapabilityClient) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.current = nil
	c.holder.Clear()
}

// exchange performs the RFC 8693 request. The forbidden inputs of §29
// (receipt, draw_id, participation_secret, payout address, actor token)
// are absent by construction: they are never added to this form.
func (c *CapabilityClient) exchange(ctx context.Context, targetEpoch uint64) (*scope.Context, error) {
	doc, err := c.mining.discoverer.Document(ctx)
	if err != nil {
		return nil, err
	}
	meta, err := c.mining.discoverer.OAuthMetadata(ctx)
	if err != nil {
		return nil, err
	}
	tok, err := c.mining.oauth.Refresh(ctx)
	if err != nil {
		return nil, err
	}
	slotID := c.mining.discoverer.cfg.SlotID

	form := url.Values{
		"grant_type":            {grantTypeTokenExchange},
		"subject_token":         {tok.AccessToken},
		"subject_token_type":    {tokenTypeAccessToken},
		"requested_token_type":  {tokenTypeAccessToken},
		"resource":              {doc.ParticipationResource},
		"scope":                 {CapabilityScope},
		"client_id":             {ClientID},
		"twilight_slot_id":      {strconv.FormatUint(slotID, 10)},
		"twilight_target_epoch": {strconv.FormatUint(targetEpoch, 10)},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, meta.TokenEndpoint,
		strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("auth: build exchange request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := (&http.Client{Transport: c.mining.oauth.transport, Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("auth: capability exchange: %w", err)
	}
	defer drainAndClose(resp.Body)
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("auth: read exchange response: %w", err)
	}
	var out capabilityResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("auth: parse exchange response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		// The subject token this exchange presented came from the
		// access-token cache, so a refusal that names IT has to drop
		// the cache or the same dead token is offered again every tick
		// until it nominally expires.
		//
		// Narrowly, though. A Twilight refusal — not joined, no
		// capability for this epoch, gate not passed — is the ordinary
		// state before enrollment and on every tick until a join
		// succeeds. Invalidating on those would rotate the refresh
		// token once a minute in exactly the case the cache exists to
		// stop rotating in, which is why TwilightError is checked
		// first and never reaches here.
		if out.TwilightError != "" {
			return nil, fmt.Errorf("auth: capability refused (%s): %s", out.TwilightError, out.Description)
		}
		if resp.StatusCode == http.StatusUnauthorized || out.Error == "invalid_grant" || out.Error == "invalid_token" {
			c.mining.oauth.InvalidateAccessToken()
		}
		return nil, fmt.Errorf("auth: capability refused (%d %s): %s", resp.StatusCode, out.Error, out.Description)
	}
	if out.AccessToken == "" || out.ExpiresIn <= 0 {
		return nil, fmt.Errorf("auth: capability response incomplete")
	}
	if out.Scope != "" && out.Scope != CapabilityScope {
		return nil, fmt.Errorf("auth: capability carries unexpected scope %q", out.Scope)
	}
	// §31: a capability never carries a refresh token — refuse one
	// rather than ever storing it.
	if out.RefreshToken != "" {
		return nil, fmt.Errorf("auth: capability unexpectedly carried a refresh token")
	}
	gotEpoch, err := strconv.ParseUint(out.TargetEpoch, 10, 64)
	if err != nil || gotEpoch != targetEpoch {
		return nil, fmt.Errorf("auth: capability scoped to epoch %q, requested %d", out.TargetEpoch, targetEpoch)
	}
	if out.SlotID != strconv.FormatUint(slotID, 10) {
		return nil, fmt.Errorf("auth: capability scoped to slot %q, configured %d", out.SlotID, slotID)
	}

	deadline := time.Time{}
	if out.Deadline != "" {
		if deadline, err = time.Parse(time.RFC3339, out.Deadline); err != nil {
			return nil, fmt.Errorf("auth: capability deadline: %w", err)
		}
	}
	return &scope.Context{
		SlotID:      slotID,
		TargetEpoch: targetEpoch,
		Capability:  out.AccessToken,
		ExpiresAt:   time.Now().Add(time.Duration(out.ExpiresIn) * time.Second),
		Deadline:    deadline,
	}, nil
}
