package auth

// RFC 9449 proof creation for the installation key, under the X-0002
// interoperability profile: ES256 only, canonical htu (scheme+host+path,
// no query/fragment, lowercased scheme/host), ath bound whenever a token
// accompanies the request, nonce echoed on server challenge.

import (
	"context"
	"crypto/ecdsa"
	"fmt"
	"net/url"
	"strings"

	dpop "github.com/conductorone/dpop/pkg/dpop"
	jose "github.com/go-jose/go-jose/v4"
)

// Proofer creates DPoP proofs for the installation key.
type Proofer struct {
	inner *dpop.Proofer
	jkt   string
}

// NewProofer wraps the installation's private key (Store.DPoPKey).
func NewProofer(key *ecdsa.PrivateKey) (*Proofer, error) {
	jkt, err := Thumbprint(key)
	if err != nil {
		return nil, err
	}
	inner, err := dpop.NewProofer(&jose.JSONWebKey{
		Key:       key,
		Algorithm: string(jose.ES256),
		Use:       "sig",
	})
	if err != nil {
		return nil, fmt.Errorf("auth: construct DPoP proofer: %w", err)
	}
	return &Proofer{inner: inner, jkt: jkt}, nil
}

// JKT is the RFC 7638 thumbprint the AS binds this installation to.
func (p *Proofer) JKT() string { return p.jkt }

// Proof creates a proof for method+htu. accessToken, when non-empty,
// binds the proof to the presented token (ath — required on resource
// requests per X-0002). nonce, when non-empty, echoes a server
// DPoP-Nonce challenge.
func (p *Proofer) Proof(ctx context.Context, method, htu, accessToken, nonce string) (string, error) {
	canonical, err := CanonicalHTU(htu)
	if err != nil {
		return "", err
	}
	opts := []dpop.ProofOption{}
	if accessToken != "" {
		opts = append(opts, dpop.WithAccessToken(accessToken))
	}
	if nonce != "" {
		opts = append(opts, dpop.WithNonceFunc(func() (string, error) { return nonce, nil }))
	}
	proof, err := p.inner.CreateProof(ctx, method, canonical, opts...)
	if err != nil {
		return "", fmt.Errorf("auth: create DPoP proof: %w", err)
	}
	return proof, nil
}

// CanonicalHTU normalizes a URL to the X-0002 htu form: lowercase
// scheme and host, no query, no fragment, path preserved. Both sides
// canonicalize before comparing, so library-level htu quirks cannot
// cause interop drift.
func CanonicalHTU(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("auth: parse htu: %w", err)
	}
	if u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("auth: htu must be absolute")
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	u.RawQuery = ""
	u.Fragment = ""
	u.User = nil
	return u.String(), nil
}
