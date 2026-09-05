// Package auth owns the proxy's AS-facing control plane. In P-I1 that is
// service-metadata discovery (contract §18–§19); the OAuth/DPoP client
// arrives in P-I2 under ADR-0005/0006/0007 — security-protocol libraries
// are importable only from this package (ADR-0010), and nothing on the
// inference path (forward/observe/wire) may import it.
//
// Everything here fails the MINING PLANE closed. Nothing here can touch
// inference: the boundary tests prove forward/observe never reach this
// package.
package auth

import (
	"context"
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

	"github.com/twilight-project/dropin-miner/pkg/wire"
)

// WellKnownPath is the fixed discovery location (§19).
const WellKnownPath = "/.well-known/twilight-mining"

// maxDiscoveryBytes bounds the metadata document read.
const maxDiscoveryBytes = 1 << 20

// DefaultMetadataTTL is the metadata validity period when the operator
// configures none (§19 requires rediscovery on expiry).
const DefaultMetadataTTL = 15 * time.Minute

// DiscoveryConfig is the explicitly configured AS identity (§18: the
// configured base URL is the V1 deployment/recovery discovery rule).
type DiscoveryConfig struct {
	// BaseURL is the AS base URL. HTTPS is mandatory outside local
	// development; plain http is accepted only for loopback hosts.
	BaseURL string
	// ChainID and SlotID are the proxy's configured identity; a
	// discovered document that disagrees is refused (§19).
	ChainID string
	SlotID  uint64
	// TTL is the metadata validity period; DefaultMetadataTTL if zero.
	TTL time.Duration
}

// Discoverer fetches, validates, and caches the Twilight service
// document. All errors are mining-plane errors: the caller keeps the
// mining plane disabled and inference untouched.
type Discoverer struct {
	cfg    DiscoveryConfig
	client *http.Client
	origin *url.URL

	now func() time.Time

	mu        sync.Mutex
	doc       *wire.DiscoveryDocument
	fetchedAt time.Time
}

// NewDiscoverer validates the configured base URL up front: a malformed
// or non-HTTPS public URL never produces a single request.
func NewDiscoverer(cfg DiscoveryConfig) (*Discoverer, error) {
	u, err := url.Parse(cfg.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("auth: parse AS base URL: %w", err)
	}
	if u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, fmt.Errorf("auth: AS base URL must be an absolute http(s) URL")
	}
	// Same rule as the provider upstream: userinfo would be silently
	// emitted as a Basic Authorization header on every fetch, and Go's
	// error strings retain the username (only the password is masked) —
	// with credential-as-username schemes that is a secret in an error.
	if u.User != nil {
		return nil, fmt.Errorf("auth: AS base URL must not carry userinfo")
	}
	if u.Scheme == "http" && !isLoopbackHost(u.Hostname()) {
		return nil, fmt.Errorf("auth: plain http AS URL is only permitted for loopback hosts (§18)")
	}
	if cfg.ChainID == "" {
		return nil, fmt.Errorf("auth: configured chain_id empty")
	}
	if cfg.TTL == 0 {
		cfg.TTL = DefaultMetadataTTL
	}
	d := &Discoverer{cfg: cfg, origin: u, now: time.Now}
	d.client = &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			// Same discipline as the upstream path: no environment
			// proxy may silently interpose on AS identity.
			Proxy: nil,
		},
		// §19: endpoint origins must not silently redirect the proxy to
		// a different Authorization Server identity (AUTH-022). Same-
		// origin redirects are followed (bounded); cross-origin refuses.
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return errors.New("auth: too many discovery redirects")
			}
			if !sameOrigin(req.URL, u) {
				return fmt.Errorf("auth: discovery redirected off-origin to %s://%s — refusing a different AS identity", req.URL.Scheme, req.URL.Host)
			}
			return nil
		},
	}
	return d, nil
}

// Document returns the cached document while it is within its validity
// period, refetching otherwise. A failed refetch does not fall back to
// the expired cache: expiry means rediscovery, and failure means the
// mining plane stays closed (§19).
func (d *Discoverer) Document(ctx context.Context) (*wire.DiscoveryDocument, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.doc != nil && d.now().Sub(d.fetchedAt) < d.cfg.TTL {
		return d.doc, nil
	}
	doc, err := d.fetch(ctx)
	if err != nil {
		d.doc = nil
		return nil, err
	}
	d.doc, d.fetchedAt = doc, d.now()
	return doc, nil
}

// Refresh drops the cache and rediscovers (endpoint-migration path).
func (d *Discoverer) Refresh(ctx context.Context) (*wire.DiscoveryDocument, error) {
	d.mu.Lock()
	d.doc = nil
	d.mu.Unlock()
	return d.Document(ctx)
}

func (d *Discoverer) fetch(ctx context.Context) (*wire.DiscoveryDocument, error) {
	endpoint := d.origin.JoinPath(WellKnownPath).String()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("auth: build discovery request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := d.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("auth: discovery fetch: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("auth: discovery returned status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxDiscoveryBytes+1))
	if err != nil {
		return nil, fmt.Errorf("auth: read discovery body: %w", err)
	}
	if len(body) > maxDiscoveryBytes {
		return nil, fmt.Errorf("auth: discovery document exceeds %d bytes", maxDiscoveryBytes)
	}
	// Unknown fields are tolerated (the AS may extend the document);
	// known fields are validated strictly.
	var doc wire.DiscoveryDocument
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("auth: parse discovery document: %w", err)
	}
	if err := doc.Validate(); err != nil {
		return nil, err
	}
	// §19 pre-use rule: chain/slot identity must match configuration.
	// Mismatch fails the mining plane closed (AUTH-020/021).
	if err := doc.CheckIdentity(d.cfg.ChainID, d.cfg.SlotID); err != nil {
		return nil, err
	}
	return &doc, nil
}

func sameOrigin(a, b *url.URL) bool {
	return a.Scheme == b.Scheme && a.Host == b.Host
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
