package auth

// P-I3: the JoinEpoch/status client, and the current-target endpoint
// that tells the proxy WHICH epoch to join. Derives DrawIDV1 from the
// custody secret, submits enrollment (contract §20–§21), verifies the
// returned receipt against the AS's published receipt keys BEFORE
// trusting it (§24), and stores the exact bytes (§22). The mining plane
// fails closed on every error; nothing here can touch inference.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

// ReceiptTyp is the only accepted receipt JWS typ (contract §24).
const ReceiptTyp = "twilight-enrollment-receipt+jwt"

// MiningClient drives the AS mining control plane for one installation.
type MiningClient struct {
	discoverer *Discoverer
	oauth      *OAuthClient
	store      *Store
}

func NewMiningClient(d *Discoverer, oc *OAuthClient, store *Store) *MiningClient {
	return &MiningClient{discoverer: d, oauth: oc, store: store}
}

// The two join_status answers that mean this installation is enrolled
// in a target (§21). Anything else means it is not — including the
// empty string, which is what an AS that omits the field says.
const (
	JoinAccepted        = "ACCEPTED"
	JoinAlreadyAccepted = "ALREADY_ACCEPTED"
)

// JoinHeld reports whether an EpochStatus.JoinStatus means this
// installation already holds a place in the target. It exists because
// `joinable` alone cannot be read that way: the AS reports
// joinable=false both for "you are already in" and for "enrollment
// closed without you".
func JoinHeld(joinStatus string) bool {
	return joinStatus == JoinAccepted || joinStatus == JoinAlreadyAccepted
}

// JoinResult is the accepted enrollment from the proxy's perspective.
type JoinResult struct {
	Status  string // ACCEPTED | ALREADY_ACCEPTED
	DrawID  string
	Receipt string // verified compact JWS, stored durably
}

// EpochStatus is the §68 subset the proxy consumes.
type EpochStatus struct {
	SlotID              string `json:"slot_id"`
	TargetEpoch         string `json:"target_epoch"`
	DistributionMode    string `json:"distribution_mode"`
	Phase               string `json:"phase"`
	JoinStatus          string `json:"join_status"`
	Joinable            bool   `json:"joinable"`
	ReceiptID           string `json:"receipt_id"`
	ParticipationStatus string `json:"participation_status"`
	CapabilityAvailable bool   `json:"capability_available"`
}

// authedRequest performs a mining-plane request: a valid access token —
// cached while it lasts, rotated when it does not — and a DPoP proof with
// ath bound by the transport.
//
// THE 401 BRANCH BELOW IS NOT OPTIONAL, and the reason is worth stating
// because it is the cost of caching at all. Before the cache every call
// rotated, so a token the AS rejected was never seen twice: the Slot 3 run
// of 2026-08-27 hit a 401 "missing scope mining:join" and the immediate
// retry succeeded, precisely because the retry carried a different token.
// A cache without this branch would re-offer the rejected one for the rest
// of its nominal lifetime and turn that transient into a fifteen-minute
// outage — a regression introduced by the fix.
func (m *MiningClient) authedRequest(ctx context.Context, method, endpoint string, body []byte) (*http.Response, error) {
	tok, err := m.oauth.Refresh(ctx)
	if err != nil {
		return nil, err
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return nil, fmt.Errorf("auth: build mining request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "DPoP "+tok.AccessToken)
	resp, err := (&http.Client{Transport: m.oauth.transport, Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		m.oauth.InvalidateAccessToken()
	}
	return resp, nil
}

// JoinEpoch derives the draw ID and enrolls for targetEpoch. The same
// derived draw_id is sent regardless of distribution mode (§21), and
// the participation secret itself never leaves the custody path.
func (m *MiningClient) JoinEpoch(ctx context.Context, targetEpoch uint64) (*JoinResult, error) {
	doc, err := m.discoverer.Document(ctx)
	if err != nil {
		return nil, err
	}
	chainID := m.discoverer.cfg.ChainID
	slotID := m.discoverer.cfg.SlotID

	secret, err := m.store.ParticipationSecret()
	if err != nil {
		return nil, err
	}
	id, err := secret.DrawID(chainID, slotID, targetEpoch)
	if err != nil {
		return nil, err
	}
	drawID := id.Hex()

	endpoint := expandTemplate(doc.JoinEpochEndpointTemplate, slotID, targetEpoch)
	body, err := json.Marshal(map[string]string{"chain_id": chainID, "draw_id": drawID})
	if err != nil {
		return nil, fmt.Errorf("auth: encode join body: %w", err)
	}
	resp, err := m.authedRequest(ctx, http.MethodPost, endpoint, body)
	if err != nil {
		return nil, fmt.Errorf("auth: join request: %w", err)
	}
	defer drainAndClose(resp.Body)

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("auth: read join response: %w", err)
	}
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return nil, joinRefusal(resp.StatusCode, raw)
	}
	var result struct {
		Status  string `json:"status"`
		Receipt string `json:"receipt"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("auth: parse join response: %w", err)
	}
	if result.Receipt == "" {
		return nil, errors.New("auth: join accepted without a receipt")
	}
	// Verify BEFORE storing or trusting (§24).
	if err := m.VerifyReceipt(ctx, result.Receipt, chainID, slotID, targetEpoch, drawID); err != nil {
		return nil, err
	}
	if err := m.store.SaveReceipt(slotID, targetEpoch, result.Receipt); err != nil {
		return nil, err
	}
	return &JoinResult{Status: result.Status, DrawID: drawID, Receipt: result.Receipt}, nil
}

// Status fetches the §68 document; `joinable` is the single
// authoritative join signal (never inferred from phase).
func (m *MiningClient) Status(ctx context.Context, targetEpoch uint64) (*EpochStatus, error) {
	doc, err := m.discoverer.Document(ctx)
	if err != nil {
		return nil, err
	}
	endpoint := expandTemplate(doc.EpochStatusEndpointTemplate, m.discoverer.cfg.SlotID, targetEpoch)
	resp, err := m.authedRequest(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("auth: status request: %w", err)
	}
	defer drainAndClose(resp.Body)
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("auth: read status response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, joinRefusal(resp.StatusCode, raw)
	}
	var st EpochStatus
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, fmt.Errorf("auth: parse status: %w", err)
	}
	return &st, nil
}

// NoOpenTarget is the status word the AS uses when it has nothing
// joinable: it answers 200 with a null target rather than 404, so a
// client can tell "poll later" apart from "cannot answer".
const NoOpenTarget = "NO_OPEN_TARGET"

// ErrNoCurrentTargetEndpoint reports that this AS does not advertise
// current_target_endpoint_template. The key is optional in the
// discovery document (§19), so its absence means the AS predates the
// current-target endpoint — not that the proxy should build the URL
// itself. Guessing would send a DPoP access token at a path the AS
// never published, so the caller degrades instead.
var ErrNoCurrentTargetEndpoint = errors.New("auth: this AS advertises no current_target_endpoint_template")

// MiningTarget is what the AS says is open for this slot right now.
//
// Joinable, JoinStatus and Phase describe the target as it was when the
// AS named it: context for a log line, not a decision. The authoritative
// join signal remains EpochStatus (§68), asked for per target right
// before joining — a listing can be a moment stale, and enrollment
// windows close.
type MiningTarget struct {
	SlotID           uint64
	TargetEpoch      uint64
	Joinable         bool
	DistributionMode string
	JoinStatus       string
	Phase            string
}

// currentTargetEnvelope is the endpoint's two-shaped answer: a target,
// or an explicit "nothing open".
type currentTargetEnvelope struct {
	Status string             `json:"status"`
	Target *currentTargetBody `json:"target"`
}

type currentTargetBody struct {
	SlotID           decimalU64 `json:"slot_id"`
	TargetEpoch      decimalU64 `json:"target_epoch"`
	Joinable         bool       `json:"joinable"`
	DistributionMode string     `json:"distribution_mode"`
	JoinStatus       string     `json:"join_status"`
	Phase            string     `json:"phase"`
}

// CurrentTarget asks the AS which target this slot may join now. It is
// the epoch source the daemon drives from; nothing in this repository
// derives an epoch locally, and a chain client is forbidden here.
//
// Authorization is the ordinary DPoP access token (scope mining:read),
// NOT a Participation Capability: asking what to join cannot require
// having joined.
//
// The two "no epoch" answers are kept apart on purpose, because they
// call for opposite behavior:
//
//	(nil, nil)  the AS answered, and nothing is joinable. Poll later;
//	            this is not a failure and must not be logged as one.
//	(nil, err)  the AS could not answer. Degrade to no capability.
func (m *MiningClient) CurrentTarget(ctx context.Context) (*MiningTarget, error) {
	doc, err := m.discoverer.Document(ctx)
	if err != nil {
		return nil, err
	}
	if doc.CurrentTargetEndpointTemplate == "" {
		return nil, ErrNoCurrentTargetEndpoint
	}
	slotID := m.discoverer.cfg.SlotID
	// Only {slot_id} is expanded: there is no epoch to substitute here,
	// since the epoch is precisely what the answer carries.
	endpoint := expandSlot(doc.CurrentTargetEndpointTemplate, slotID)

	resp, err := m.authedRequest(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("auth: current-target request: %w", err)
	}
	defer drainAndClose(resp.Body)
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("auth: read current-target response: %w", err)
	}
	// Any non-200 is "cannot answer", 404 included: the AS spends a 200
	// on "nothing open", so it has no reason to use a status code for
	// it, and reading one as "nothing open" would hide a broken route.
	if resp.StatusCode != http.StatusOK {
		return nil, joinRefusal(resp.StatusCode, raw)
	}
	var env currentTargetEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("auth: parse current target: %w", err)
	}
	// An absent target IS the "nothing open" answer, whatever word
	// accompanies it; only NO_OPEN_TARGET is frozen, so a status the
	// proxy does not know must not become an error. A contradictory
	// answer (NO_OPEN_TARGET carrying a target) is read the closed way.
	if env.Target == nil || env.Status == NoOpenTarget {
		return nil, nil
	}
	// Same fail-closed identity rule as §19 discovery and §31 capability
	// scoping: a target for another slot is never this installation's.
	if uint64(env.Target.SlotID) != slotID {
		return nil, fmt.Errorf("auth: current target names slot %d, configured %d", uint64(env.Target.SlotID), slotID)
	}
	return &MiningTarget{
		SlotID:           uint64(env.Target.SlotID),
		TargetEpoch:      uint64(env.Target.TargetEpoch),
		Joinable:         env.Target.Joinable,
		DistributionMode: env.Target.DistributionMode,
		JoinStatus:       env.Target.JoinStatus,
		Phase:            env.Target.Phase,
	}, nil
}

// VerifyReceipt enforces §24 verification: EdDSA only, exact typ,
// kid resolved ONLY within the receipt-key namespace of the published
// JWKS, and payload claims matching the enrollment context.
func (m *MiningClient) VerifyReceipt(ctx context.Context, compact, chainID string, slotID, targetEpoch uint64, drawID string) error {
	jws, err := jose.ParseSigned(compact, []jose.SignatureAlgorithm{jose.EdDSA})
	if err != nil {
		return fmt.Errorf("auth: parse receipt: %w", err)
	}
	hdr := jws.Signatures[0].Protected
	if typ, _ := hdr.ExtraHeaders[jose.HeaderType].(string); typ != ReceiptTyp {
		return fmt.Errorf("auth: receipt typ %q rejected", typ)
	}
	if !strings.HasPrefix(hdr.KeyID, "receipt-") {
		return fmt.Errorf("auth: receipt kid %q outside the receipt-key namespace", hdr.KeyID)
	}
	pub, err := m.receiptKey(ctx, hdr.KeyID)
	if err != nil {
		return err
	}
	payload, err := jws.Verify(pub)
	if err != nil {
		return fmt.Errorf("auth: receipt signature invalid: %w", err)
	}
	var claims struct {
		ReceiptVersion int    `json:"receipt_version"`
		Issuer         string `json:"iss"`
		ChainID        string `json:"chain_id"`
		SlotID         string `json:"slot_id"`
		TargetEpoch    string `json:"target_epoch"`
		DrawID         string `json:"draw_id"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return fmt.Errorf("auth: parse receipt claims: %w", err)
	}
	if claims.ReceiptVersion != 1 ||
		claims.ChainID != chainID ||
		claims.SlotID != strconv.FormatUint(slotID, 10) ||
		claims.TargetEpoch != strconv.FormatUint(targetEpoch, 10) ||
		claims.DrawID != drawID {
		return fmt.Errorf("auth: receipt claims do not match the enrollment context")
	}
	return nil
}

// receiptKey resolves a receipt kid from the AS JWKS, restricted to
// Ed25519 keys in the receipt namespace.
func (m *MiningClient) receiptKey(ctx context.Context, kid string) (ed25519.PublicKey, error) {
	meta, err := m.discoverer.OAuthMetadata(ctx)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, meta.JWKSURI, nil)
	if err != nil {
		return nil, fmt.Errorf("auth: build jwks request: %w", err)
	}
	resp, err := m.discoverer.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("auth: fetch jwks: %w", err)
	}
	defer drainAndClose(resp.Body)
	var set jose.JSONWebKeySet
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxDiscoveryBytes)).Decode(&set); err != nil {
		return nil, fmt.Errorf("auth: parse jwks: %w", err)
	}
	for _, k := range set.Key(kid) {
		if pub, ok := k.Key.(ed25519.PublicKey); ok && strings.HasPrefix(k.KeyID, "receipt-") {
			return pub, nil
		}
	}
	return nil, fmt.Errorf("auth: receipt kid %q not resolvable in the published receipt-key set", kid)
}

// joinRefusal maps the stable §26 envelope into an error.
func joinRefusal(status int, raw []byte) error {
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &env); err == nil && env.Error.Code != "" {
		return fmt.Errorf("auth: AS refused (%d %s): %s", status, env.Error.Code, env.Error.Message)
	}
	return fmt.Errorf("auth: AS refused with status %d", status)
}

func expandSlot(tmpl string, slotID uint64) string {
	return strings.ReplaceAll(tmpl, "{slot_id}", strconv.FormatUint(slotID, 10))
}

func expandTemplate(tmpl string, slotID, targetEpoch uint64) string {
	return strings.ReplaceAll(expandSlot(tmpl, slotID), "{target_epoch}", strconv.FormatUint(targetEpoch, 10))
}

// decimalU64 decodes a 64-bit identifier from the contract's decimal
// STRING form ("1042" — chosen so no JSON consumer can round it through
// a float and lose precision), and tolerates a bare JSON number as well.
// Tolerating both costs nothing here, while refusing the number form
// would strand the driver in permanent degradation over an encoding
// detail it does not depend on.
type decimalU64 uint64

func (d *decimalU64) UnmarshalJSON(b []byte) error {
	s := string(b)
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		s = s[1 : len(s)-1]
	}
	v, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return fmt.Errorf("auth: %q is not an unsigned decimal identifier", s)
	}
	*d = decimalU64(v)
	return nil
}
