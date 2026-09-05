package auth

// P-I5: the provider-verification client (contract §37–§43). The proxy
// helps the participant bind a DEDICATED zero-spend OpenRouter key to
// the AS. That key is used only for reconciliation lookups and is
// entirely separate from the inference credential, which stays local to
// provider traffic and is never sent to the AS (§36).

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/twilight-project/dropin-miner/pkg/wire"
)

// Provider verification states as seen by the proxy (§43).
const (
	ProviderStatusNotConfigured = "NOT_CONFIGURED"
	ProviderStatusReady         = "READY"
	ProviderStatusUnavailable   = "UNAVAILABLE"
)

// ProviderBinding is the AS's verification-binding document. It never
// carries key material.
type ProviderBinding struct {
	Provider                     string `json:"provider"`
	SourceProfile                string `json:"source_profile"`
	Status                       string `json:"status"`
	CredentialID                 string `json:"credential_id"`
	KeyFingerprint               string `json:"key_fingerprint"`
	CheckedAt                    string `json:"checked_at"`
	LastCheckedAt                string `json:"last_checked_at"`
	LastSuccessfulVerificationAt string `json:"last_successful_verification_at"`
}

// providerEndpoint expands the discovery template for the V1 provider.
func (m *MiningClient) providerEndpoint(ctx context.Context) (string, error) {
	doc, err := m.discoverer.Document(ctx)
	if err != nil {
		return "", err
	}
	return strings.ReplaceAll(doc.ProviderVerificationEndpointTemplate, "{provider}", "openrouter"), nil
}

// RegisterProviderCredential binds (or replaces) the participant's
// zero-spend verification key. The AS validates before swapping, so a
// rejected key leaves any working binding untouched (§41).
//
// The key is sent once, over the authenticated DPoP-bound channel, and
// is never stored locally, logged, or included in any diagnostic.
func (m *MiningClient) RegisterProviderCredential(ctx context.Context, apiKey string) (*ProviderBinding, error) {
	if apiKey == "" {
		return nil, fmt.Errorf("auth: verification api key is empty")
	}
	endpoint, err := m.providerEndpoint(ctx)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(map[string]string{
		"credential_type": "api_key",
		"api_key":         apiKey,
	})
	if err != nil {
		return nil, fmt.Errorf("auth: encode registration: %w", err)
	}
	resp, err := m.authedRequest(ctx, http.MethodPut, endpoint, body)
	if err != nil {
		// Never wrap the key into an error; authedRequest cannot see it
		// beyond the body it already sent.
		return nil, fmt.Errorf("auth: provider registration request failed")
	}
	defer drainAndClose(resp.Body)
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("auth: read registration response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, joinRefusal(resp.StatusCode, raw)
	}
	var binding ProviderBinding
	if err := json.Unmarshal(raw, &binding); err != nil {
		return nil, fmt.Errorf("auth: parse registration response: %w", err)
	}
	if binding.Status != ProviderStatusReady {
		return nil, fmt.Errorf("auth: provider binding is %s after registration", binding.Status)
	}
	return &binding, nil
}

// ProviderStatus reports the current binding (§43).
func (m *MiningClient) ProviderStatus(ctx context.Context) (*ProviderBinding, error) {
	endpoint, err := m.providerEndpoint(ctx)
	if err != nil {
		return nil, err
	}
	resp, err := m.authedRequest(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("auth: provider status request: %w", err)
	}
	defer drainAndClose(resp.Body)
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("auth: read provider status: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, joinRefusal(resp.StatusCode, raw)
	}
	var binding ProviderBinding
	if err := json.Unmarshal(raw, &binding); err != nil {
		return nil, fmt.Errorf("auth: parse provider status: %w", err)
	}
	return &binding, nil
}

// RemoveProviderCredential unbinds the credential at the AS.
//
// §42: this does NOT revoke or delete the key at OpenRouter. Callers
// surface RevokeAtProviderNotice to the participant so the key is dealt
// with on the provider side too.
func (m *MiningClient) RemoveProviderCredential(ctx context.Context) error {
	endpoint, err := m.providerEndpoint(ctx)
	if err != nil {
		return err
	}
	resp, err := m.authedRequest(ctx, http.MethodDelete, endpoint, nil)
	if err != nil {
		return fmt.Errorf("auth: provider removal request: %w", err)
	}
	defer drainAndClose(resp.Body)
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		return joinRefusal(resp.StatusCode, raw)
	}
	return nil
}

// RevokeAtProviderNotice is the §42 participant-facing wording the CLI
// prints after removal.
const RevokeAtProviderNotice = "The verification key was removed from the Authorization Server. " +
	"It still exists at OpenRouter — delete or revoke it there as well if it should no longer exist."

// AcceptsOpenRouterProfile reports whether this AS accepts OPENROUTER_V1
// observations, which is the only profile a participant verification
// credential means anything under.
//
// §35.1 gives SEARCH_ROUTER_V1 no participant key to ask about: the AS
// verifies with its OWN operator credential and the participant holds
// nothing (MINIS-VER-006). So on a Slot that serves only that profile,
// registering a credential is not merely unnecessary — there is no binding
// for it to establish, and the AS's provider-verification surface refuses to
// serve the profile at all. Asking discovery lets the proxy say that plainly
// instead of relaying a refusal the participant cannot interpret.
func (m *MiningClient) AcceptsOpenRouterProfile(ctx context.Context) (bool, error) {
	doc, err := m.discoverer.Document(ctx)
	if err != nil {
		return false, err
	}
	for _, p := range doc.SourceProfiles {
		if p == wire.SourceProfileOpenRouterV1 {
			return true, nil
		}
	}
	return false, nil
}
