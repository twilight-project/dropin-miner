package auth

// dpopTransport injects a fresh DPoP proof into every outgoing request
// and transparently answers a server nonce challenge (RFC 9449 §8) by
// retrying ONCE with the supplied nonce. It wraps the HTTP client that
// x/oauth2 uses, so every token-endpoint call the library makes is
// sender-constrained without hand-rolling token mechanics (ADR-0005).

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
)

type dpopTransport struct {
	base    http.RoundTripper
	proofer *Proofer

	mu    sync.Mutex
	nonce string // last server-issued nonce, per AS (one AS per client)
}

func newDPoPTransport(proofer *Proofer) *dpopTransport {
	return &dpopTransport{base: &http.Transport{Proxy: nil}, proofer: proofer}
}

func (t *dpopTransport) currentNonce() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.nonce
}

func (t *dpopTransport) storeNonce(n string) {
	if n == "" {
		return
	}
	t.mu.Lock()
	t.nonce = n
	t.mu.Unlock()
}

func (t *dpopTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.attempt(req, t.currentNonce())
	if err != nil {
		return nil, err
	}
	// Nonce challenge: remember it and retry once (requires a
	// replayable body).
	if nonce := resp.Header.Get("DPoP-Nonce"); nonce != "" {
		t.storeNonce(nonce)
		if (resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnauthorized) &&
			req.GetBody != nil {
			_ = resp.Body.Close()
			return t.attempt(req, nonce)
		}
	}
	return resp, nil
}

func (t *dpopTransport) attempt(req *http.Request, nonce string) (*http.Response, error) {
	clone := req.Clone(req.Context())
	if req.GetBody != nil {
		body, err := req.GetBody()
		if err != nil {
			return nil, err
		}
		clone.Body = body
	} else if req.Body != nil && clone.Body == nil {
		return nil, errors.New("auth: request body is not replayable")
	}
	proof, err := t.proofer.Proof(req.Context(), clone.Method, clone.URL.String(), accessTokenFrom(clone), nonce)
	if err != nil {
		return nil, err
	}
	clone.Header.Set("DPoP", proof)
	return t.base.RoundTrip(clone)
}

// accessTokenFrom binds ath when the request presents a DPoP-scheme
// access token (resource requests; token-endpoint calls carry none).
func accessTokenFrom(req *http.Request) string {
	authz := req.Header.Get("Authorization")
	if v, ok := strings.CutPrefix(authz, "DPoP "); ok {
		return v
	}
	return ""
}

// drainAndClose is a helper for error paths reading small bodies.
func drainAndClose(body io.ReadCloser) {
	_, _ = io.Copy(io.Discard, io.LimitReader(body, 1<<16))
	_ = body.Close()
}
