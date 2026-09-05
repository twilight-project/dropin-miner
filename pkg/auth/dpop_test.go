package auth

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	dpop "github.com/conductorone/dpop/pkg/dpop"
	jose "github.com/go-jose/go-jose/v4"
)

func testProofer(t *testing.T) *Proofer {
	t.Helper()
	s, err := OpenStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	key, err := s.DPoPKey()
	if err != nil {
		t.Fatal(err)
	}
	p, err := NewProofer(key)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// Interop check: proofs we create validate under the same library's
// server-side validator with the X-0002 constraints (ES256 only).
func TestProofRoundTripsThroughValidator(t *testing.T) {
	p := testProofer(t)
	ctx := context.Background()
	proof, err := p.Proof(ctx, "POST", "https://as.example.com/oauth/token", "", "")
	if err != nil {
		t.Fatal(err)
	}
	v := dpop.NewValidator(
		dpop.WithAllowedSignatureAlgorithms([]jose.SignatureAlgorithm{jose.ES256}),
		dpop.WithMaxClockSkew(30*time.Second),
	)
	claims, err := v.ValidateProof(ctx, proof, "POST", "https://as.example.com/oauth/token")
	if err != nil {
		t.Fatalf("own proof failed validation: %v", err)
	}
	if claims == nil {
		t.Fatal("no claims returned")
	}

	// Wrong htm/htu must fail (AUTH DPoP matrix seeds).
	if _, err := v.ValidateProof(ctx, proof, "GET", "https://as.example.com/oauth/token"); err == nil {
		t.Fatal("wrong htm accepted")
	}
	if _, err := v.ValidateProof(ctx, proof, "POST", "https://as.example.com/other"); err == nil {
		t.Fatal("wrong htu accepted")
	}
}

func TestProofJTIReplayDetectedByStore(t *testing.T) {
	p := testProofer(t)
	ctx := context.Background()
	proof, err := p.Proof(ctx, "POST", "https://as.example.com/oauth/token", "", "")
	if err != nil {
		t.Fatal(err)
	}
	v := dpop.NewValidator(
		dpop.WithAllowedSignatureAlgorithms([]jose.SignatureAlgorithm{jose.ES256}),
		dpop.WithJTIStore(dpop.NewMemoryJTIStore().CheckAndStoreJTI),
	)
	if _, err := v.ValidateProof(ctx, proof, "POST", "https://as.example.com/oauth/token"); err != nil {
		t.Fatalf("first presentation rejected: %v", err)
	}
	if _, err := v.ValidateProof(ctx, proof, "POST", "https://as.example.com/oauth/token"); err == nil {
		t.Fatal("replayed jti accepted")
	}
}

func TestCanonicalHTU(t *testing.T) {
	for raw, want := range map[string]string{ // #nosec G101 -- URLs, not credentials
		"HTTPS://AS.Example.COM/oauth/token?state=x#frag": "https://as.example.com/oauth/token",
		"https://as.example.com/v1/mining/observations":   "https://as.example.com/v1/mining/observations",
		"http://127.0.0.1:8080/oauth/token":               "http://127.0.0.1:8080/oauth/token",
	} {
		got, err := CanonicalHTU(raw)
		if err != nil || got != want {
			t.Errorf("CanonicalHTU(%q) = %q, %v; want %q", raw, got, err, want)
		}
	}
	for _, bad := range []string{"", "/relative/only", "://nope"} {
		if _, err := CanonicalHTU(bad); err == nil {
			t.Errorf("CanonicalHTU(%q) accepted", bad)
		}
	}
	// Proof creation canonicalizes before signing.
	p := testProofer(t)
	proof, err := p.Proof(context.Background(), "POST", "HTTPS://AS.EXAMPLE.COM/oauth/token?x=1", "", "")
	if err != nil {
		t.Fatal(err)
	}
	v := dpop.NewValidator(dpop.WithAllowedSignatureAlgorithms([]jose.SignatureAlgorithm{jose.ES256}))
	if _, err := v.ValidateProof(context.Background(), proof, "POST", "https://as.example.com/oauth/token"); err != nil {
		t.Fatalf("canonicalized proof did not validate against canonical htu: %v", err)
	}
}

func TestJKTStableAcrossProofers(t *testing.T) {
	s, err := OpenStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	key, err := s.DPoPKey()
	if err != nil {
		t.Fatal(err)
	}
	p1, _ := NewProofer(key)
	p2, _ := NewProofer(key)
	if p1.JKT() == "" || p1.JKT() != p2.JKT() {
		t.Fatalf("jkt unstable: %q vs %q", p1.JKT(), p2.JKT())
	}
	tp, err := Thumbprint(key)
	if err != nil || tp != p1.JKT() {
		t.Fatalf("Proofer.JKT %q != Thumbprint %q (%v)", p1.JKT(), tp, err)
	}
}
