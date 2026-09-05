package auth

import (
	"crypto/elliptic"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func newStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "state")
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	return s, dir
}

// PX-KEY-001: key generated once, persisted, stable across loads.
func TestDPoPKeyGeneratedOnceAndStable(t *testing.T) {
	s, dir := newStore(t)
	k1, err := s.DPoPKey()
	if err != nil {
		t.Fatal(err)
	}
	if k1.Curve != elliptic.P256() {
		t.Fatal("DPoP key must be P-256 (X-0002: ES256)")
	}
	k2, err := s.DPoPKey()
	if err != nil {
		t.Fatal(err)
	}
	if !k1.Equal(k2) {
		t.Fatal("reload produced a different key")
	}
	tp1, err := Thumbprint(k1)
	if err != nil || tp1 == "" || strings.ContainsAny(tp1, "+/=") {
		t.Fatalf("thumbprint not base64url: %q %v", tp1, err)
	}
	// A different store yields a different installation identity.
	s2, err := OpenStore(filepath.Join(t.TempDir(), "other"))
	if err != nil {
		t.Fatal(err)
	}
	k3, err := s2.DPoPKey()
	if err != nil {
		t.Fatal(err)
	}
	tp3, _ := Thumbprint(k3)
	if tp1 == tp3 {
		t.Fatal("distinct installations produced identical jkt")
	}
	info, err := os.Stat(filepath.Join(dir, "dpop.key"))
	if err != nil || (posixModes && info.Mode().Perm() != 0o600) {
		t.Fatalf("dpop.key perms = %v, want 0600", info.Mode().Perm())
	}
}

// PRIV-008 permission assertion: loose modes refuse the mining plane.
func TestPermissionHazardsRefused(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission semantics (Windows posture is documented best-effort, ADR-0008)")
	}
	s, dir := newStore(t)
	if _, err := s.DPoPKey(); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(dir, "dpop.key")
	if err := os.Chmod(keyPath, 0o644); err != nil { // #nosec G302 -- deliberately loose: the test proves refusal
		t.Fatal(err)
	}
	if _, err := s.DPoPKey(); err == nil || !strings.Contains(err.Error(), "group/world-accessible") {
		t.Fatalf("world-readable key not refused: %v", err)
	}
	if err := os.Chmod(keyPath, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DPoPKey(); err != nil {
		t.Fatalf("restored perms still refused: %v", err)
	}

	// Loose state directory refuses OpenStore entirely.
	looseDir := filepath.Join(t.TempDir(), "loose")
	if err := os.MkdirAll(looseDir, 0o755); err != nil { // #nosec G301 -- deliberately loose: the test proves refusal
		t.Fatal(err)
	}
	if _, err := OpenStore(looseDir); err == nil {
		t.Fatal("group/world-accessible state dir accepted")
	}

	// Symlinked secret refuses.
	target := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.WriteFile(target, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	s2, dir2 := newStore(t)
	if err := os.Symlink(target, filepath.Join(dir2, "refresh.token")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s2.LoadRefreshToken(); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlinked secret not refused: %v", err)
	}
}

func TestRefreshTokenLifecycle(t *testing.T) {
	s, dir := newStore(t)
	if _, ok, err := s.LoadRefreshToken(); err != nil || ok {
		t.Fatalf("fresh store should have no token: ok=%v err=%v", ok, err)
	}
	if err := s.SaveRefreshToken("rt-one"); err != nil {
		t.Fatal(err)
	}
	// Rotation: atomic replace, new value wins.
	if err := s.SaveRefreshToken("rt-two"); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.LoadRefreshToken()
	if err != nil || !ok || got != "rt-two" {
		t.Fatalf("got %q ok=%v err=%v", got, ok, err)
	}
	info, err := os.Stat(filepath.Join(dir, "refresh.token"))
	if err != nil || (posixModes && info.Mode().Perm() != 0o600) {
		t.Fatalf("refresh.token perms = %v, want 0600", info.Mode().Perm())
	}
	if err := s.SaveRefreshToken(""); err == nil {
		t.Fatal("empty refresh token accepted")
	}
	if err := s.DeleteRefreshToken(); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.LoadRefreshToken(); ok {
		t.Fatal("token survived deletion")
	}
	if err := s.DeleteRefreshToken(); err != nil {
		t.Fatalf("idempotent delete failed: %v", err)
	}
}
