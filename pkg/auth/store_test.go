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

// Agent onboarding design §3: the client's cache of its own platform
// identity round-trips, including the fields a resumed poll updates.
func TestSaveLoadAgentRegistrationRoundTrips(t *testing.T) {
	s, _ := newStore(t)
	if _, ok, err := s.LoadAgentRegistration(); err != nil || ok {
		t.Fatalf("fresh store: ok=%v err=%v, want ok=false err=nil", ok, err)
	}
	want := AgentRegistration{
		AgentID:        "agent-1",
		ClaimURL:       "https://platform.nyks.dev/claim/AB12-CD34",
		ClaimCode:      "AB12-CD34",
		Status:         "unclaimed",
		ClaimExpiresAt: "2026-09-16T00:00:00Z",
	}
	if err := s.SaveAgentRegistration(want); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.LoadAgentRegistration()
	if err != nil || !ok || got.AgentID != want.AgentID || got.ClaimURL != want.ClaimURL ||
		got.ClaimCode != want.ClaimCode || got.Status != want.Status || got.ClaimExpiresAt != want.ClaimExpiresAt ||
		len(got.Scopes) != 0 {
		t.Fatalf("got %+v ok=%v err=%v, want %+v", got, ok, err, want)
	}

	// Later overwrites earlier: a resumed poll advances the same record,
	// it does not create a second one.
	claimed := want
	claimed.Status = "claimed"
	claimed.Scopes = []string{"credits", "mining"}
	claimed.LastEnrollmentSlot = "twilight-slot-3"
	claimed.LastEnrollmentAt = "2026-09-09T00:00:00Z"
	if err := s.SaveAgentRegistration(claimed); err != nil {
		t.Fatal(err)
	}
	got, ok, err = s.LoadAgentRegistration()
	if err != nil || !ok {
		t.Fatalf("reload after advance: ok=%v err=%v", ok, err)
	}
	if got.Status != "claimed" || len(got.Scopes) != 2 || got.LastEnrollmentSlot != "twilight-slot-3" {
		t.Fatalf("advance did not persist: %+v", got)
	}
}

// The registration is what a resumed process (a detached connect after a
// later search) reads before making any network call, so it must survive
// closing and reopening the store exactly as the refresh token does.
func TestAgentRegistrationSurvivesAcrossStoreReopens(t *testing.T) {
	_, dir := newStore(t)
	s1, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	rec := AgentRegistration{AgentID: "agent-1", Status: "claimed", Scopes: []string{"mining"}}
	if err := s1.SaveAgentRegistration(rec); err != nil {
		t.Fatal(err)
	}
	s2, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, ok, err := s2.LoadAgentRegistration()
	if err != nil || !ok || got.AgentID != "agent-1" || got.Status != "claimed" {
		t.Fatalf("got %+v ok=%v err=%v", got, ok, err)
	}
	info, err := os.Stat(filepath.Join(dir, "agent.json"))
	if err != nil || (posixModes && info.Mode().Perm() != 0o600) {
		t.Fatalf("agent.json perms = %v, want 0600", info.Mode().Perm())
	}
}

// SavePayoutAddress/LoadPayoutAddress round-trip independently of the
// agent registration — the address is a local mining preference the
// client owns, not platform state a poll can overwrite.
func TestSaveLoadPayoutAddressRoundTrips(t *testing.T) {
	s, _ := newStore(t)
	if _, ok, err := s.LoadPayoutAddress(); err != nil || ok {
		t.Fatalf("fresh store: ok=%v err=%v, want ok=false err=nil", ok, err)
	}
	if err := s.SavePayoutAddress("twilight1uwew6p63453wm0znz723lrneuls4xy29swp89n"); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.LoadPayoutAddress()
	if err != nil || !ok || got != "twilight1uwew6p63453wm0znz723lrneuls4xy29swp89n" {
		t.Fatalf("got %q ok=%v err=%v", got, ok, err)
	}
	if err := s.SavePayoutAddress(""); err == nil {
		t.Fatal("empty payout address accepted")
	}
}

// EpochConflicts is a bounded SET, not a single record — WP2-review
// defect 1: a single record meant a second conflicted epoch clobbered the
// first, and its observations were then never dropped.
func TestEpochConflictsIsASetNotASingleRecord(t *testing.T) {
	s, _ := newStore(t)
	if got, err := s.EpochConflicts(); err != nil || len(got) != 0 {
		t.Fatalf("fresh store: got %+v err=%v, want empty", got, err)
	}
	if err := s.SaveEpochConflict(7, 1042); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveEpochConflict(7, 1043); err != nil {
		t.Fatal(err)
	}
	got, err := s.EpochConflicts()
	if err != nil || len(got) != 2 {
		t.Fatalf("got %+v err=%v, want both 1042 and 1043 on file", got, err)
	}
}

// Recording the same (slot, epoch) conflict twice must not duplicate it —
// a driver that sees ErrEnrollmentConflict on every tick until the target
// rolls over would otherwise grow the set without bound.
func TestSaveEpochConflictDedupesTheSamePair(t *testing.T) {
	s, _ := newStore(t)
	for i := 0; i < 3; i++ {
		if err := s.SaveEpochConflict(7, 1042); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.EpochConflicts()
	if err != nil || len(got) != 1 {
		t.Fatalf("got %+v err=%v, want exactly one entry", got, err)
	}
}

func TestRemoveEpochConflicts(t *testing.T) {
	s, _ := newStore(t)
	if err := s.RemoveEpochConflicts(nil); err != nil {
		t.Fatalf("removing nothing from an absent set: %v", err)
	}
	if err := s.SaveEpochConflict(7, 1042); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveEpochConflict(7, 1043); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveEpochConflicts([]ConflictedEpoch{{SlotID: 7, TargetEpoch: 1042}}); err != nil {
		t.Fatal(err)
	}
	got, err := s.EpochConflicts()
	if err != nil || len(got) != 1 || got[0].TargetEpoch != 1043 {
		t.Fatalf("got %+v err=%v, want only 1043 left", got, err)
	}
}

func TestSaveLoadPayoutBindingHeldRoundTrips(t *testing.T) {
	s, _ := newStore(t)
	if _, ok, err := s.LoadPayoutBindingHeld(); err != nil || ok {
		t.Fatalf("fresh store: ok=%v err=%v, want ok=false err=nil", ok, err)
	}
	if err := s.SavePayoutBindingHeld("twilight1local", "twilight1active", HeldReplacesActive); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.LoadPayoutBindingHeld()
	if err != nil || !ok || got.Local != "twilight1local" || got.Active != "twilight1active" {
		t.Fatalf("got %+v ok=%v err=%v", got, ok, err)
	}
	if err := s.ClearPayoutBindingHeld(); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.LoadPayoutBindingHeld(); err != nil || ok {
		t.Fatalf("after clear: ok=%v err=%v, want ok=false", ok, err)
	}
}

func TestSaveLoadClearRevokePendingRoundTrips(t *testing.T) {
	s, _ := newStore(t)
	if pending, err := s.LoadRevokePending(); err != nil || pending {
		t.Fatalf("fresh store: pending=%v err=%v, want pending=false err=nil", pending, err)
	}
	if err := s.SaveRevokePending(); err != nil {
		t.Fatal(err)
	}
	if pending, err := s.LoadRevokePending(); err != nil || !pending {
		t.Fatalf("after save: pending=%v err=%v, want pending=true", pending, err)
	}
	if err := s.ClearRevokePending(); err != nil {
		t.Fatal(err)
	}
	if pending, err := s.LoadRevokePending(); err != nil || pending {
		t.Fatalf("after clear: pending=%v err=%v, want pending=false", pending, err)
	}
	// Clearing an already-absent marker is not an error — mining enable
	// calls this unconditionally on every enable, marker or not.
	if err := s.ClearRevokePending(); err != nil {
		t.Fatalf("clearing an absent marker: %v", err)
	}
}

// WP2-adversarial-review finding 9: SavePayoutAddress is the one place
// every payout address in the agent-onboarding flow is validated.
func TestSavePayoutAddressRejectsNonBech32(t *testing.T) {
	s, _ := newStore(t)
	for _, bad := range []string{"twilight1abc", "not-an-address", "twilight1", ""} {
		if err := s.SavePayoutAddress(bad); err == nil {
			t.Errorf("SavePayoutAddress(%q) accepted, want a bech32-decode refusal", bad)
		}
	}
}

func TestSavePayoutAddressRejectsWrongHRP(t *testing.T) {
	s, _ := newStore(t)
	// A syntactically valid bech32 string, but for a different chain's
	// prefix — the HRP check is a separate rejection from the decode
	// check above, and needs its own input to exercise it.
	if err := s.SavePayoutAddress("cosmos1qqnfjqxr5w5c60x5xw24k5zqe0shsrtj04kagr"); err == nil {
		t.Fatal("an address with the wrong HRP was accepted")
	}
}

// WP2-adversarial-review finding 18: the conflict set is capped, oldest
// first, and a slot_id reconfiguration is prunable.
func TestEpochConflictsAreBoundedWithOldestFirstEviction(t *testing.T) {
	s, _ := newStore(t)
	for i := uint64(0); i < maxEpochConflicts+10; i++ {
		if err := s.SaveEpochConflict(1, i); err != nil {
			t.Fatal(err)
		}
	}
	set, err := s.EpochConflicts()
	if err != nil {
		t.Fatal(err)
	}
	if len(set) != maxEpochConflicts {
		t.Fatalf("len(set) = %d, want the cap %d", len(set), maxEpochConflicts)
	}
	// Oldest-first eviction: the earliest epochs (0..9) should be gone,
	// the most recent maxEpochConflicts should remain.
	for _, c := range set {
		if c.TargetEpoch < 10 {
			t.Fatalf("epoch %d should have been evicted as the oldest, found in the set", c.TargetEpoch)
		}
	}
}

func TestPruneEpochConflictsForOtherSlots(t *testing.T) {
	s, _ := newStore(t)
	if err := s.SaveEpochConflict(1, 100); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveEpochConflict(2, 200); err != nil {
		t.Fatal(err)
	}
	if err := s.PruneEpochConflictsForOtherSlots(1); err != nil {
		t.Fatal(err)
	}
	set, err := s.EpochConflicts()
	if err != nil {
		t.Fatal(err)
	}
	if len(set) != 1 || set[0].SlotID != 1 {
		t.Fatalf("got %+v, want only the slot-1 entry to survive", set)
	}
}
