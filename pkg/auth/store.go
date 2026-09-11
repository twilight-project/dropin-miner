package auth

// Secret custody per ADR-0008: owner-only files in a 0700 state
// directory. The store owns the auth-plane material — the DPoP
// installation key and the renewable refresh authorization. Every load
// re-verifies permissions and refuses group/world-accessible or
// symlinked paths: custody failures close the MINING plane, never
// inference (the boundary tests keep this package off the request path).

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/twilight-project/dropin-miner/pkg/fsx"

	jose "github.com/go-jose/go-jose/v4"

	"github.com/twilight-project/dropin-miner/pkg/mining/draw"
)

const (
	dpopKeyFile             = "dpop.key"
	participationSecretFile = "participation.secret"
	refreshTokenFile        = "refresh.token"
	registrationPendingFile = "registration_pending.json"
)

// ErrAgentRegistrationCorrupt identifies an undecodable agent.json whose
// contents may still represent a live platform identity.
var ErrAgentRegistrationCorrupt = errors.New("auth: agent registration is corrupt")

// Store is the per-installation secret directory.
type Store struct {
	dir string
}

// OpenStore creates or opens the state directory ([mining] state_dir):
// created 0700, and refused if it is a symlink or group/world-accessible
// — the same hazard discipline as the Unix-socket listener.
func OpenStore(dir string) (*Store, error) {
	if dir == "" {
		return nil, errors.New("auth: state_dir is empty")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil { // #nosec G703 -- operator-configured state dir, validated just below
		return nil, fmt.Errorf("auth: create state dir: %w", err)
	}
	return openExistingStore(dir)
}

// OpenStoreExisting opens an already-existing state directory without
// creating anything. It is the inspection path for commands such as doctor
// and status, and for search-side state reads: diagnosis must not manufacture
// custody state merely because a participant asked a question.
func OpenStoreExisting(dir string) (*Store, error) {
	if dir == "" {
		return nil, errors.New("auth: state_dir is empty")
	}
	return openExistingStore(dir)
}

func openExistingStore(dir string) (*Store, error) {
	info, err := os.Lstat(dir) // #nosec G703 -- same validated operator path
	if err != nil {
		return nil, fmt.Errorf("auth: stat state dir: %w", err)
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		return nil, fmt.Errorf("auth: state dir is a symlink; refusing")
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("auth: state dir is not a directory")
	}
	if posixModes && info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("auth: state dir is group/world-accessible (%04o); refusing", info.Mode().Perm())
	}
	return &Store{dir: dir}, nil
}

// DPoPKey loads the installation's DPoP private key, generating it on
// first use (contract §15: created at first authorization setup, local
// to the installation forever). ES256 (ECDSA P-256) per the X-0002
// interoperability profile; PKCS#8 PEM on disk.
func (s *Store) DPoPKey() (*ecdsa.PrivateKey, error) {
	return s.dpopKey(true)
}

// DPoPKeyExisting loads the installation key without generating one. This is
// the only key path available to observational commands; a missing key means
// local authorization is incomplete, not that diagnosis should create it.
func (s *Store) DPoPKeyExisting() (*ecdsa.PrivateKey, error) {
	return s.dpopKey(false)
}

func (s *Store) dpopKey(generate bool) (*ecdsa.PrivateKey, error) {
	raw, err := s.readSecret(dpopKeyFile)
	switch {
	case err == nil:
		block, _ := pem.Decode(raw)
		if block == nil || block.Type != "PRIVATE KEY" {
			return nil, errors.New("auth: dpop.key is not a PKCS#8 PEM private key")
		}
		parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("auth: parse dpop.key: %w", err)
		}
		key, ok := parsed.(*ecdsa.PrivateKey)
		if !ok || key.Curve != elliptic.P256() {
			return nil, errors.New("auth: dpop.key is not an ECDSA P-256 key (X-0002 profile requires ES256)")
		}
		return key, nil
	case errors.Is(err, fs.ErrNotExist) && generate:
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return nil, fmt.Errorf("auth: generate DPoP key: %w", err)
		}
		der, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			return nil, fmt.Errorf("auth: encode DPoP key: %w", err)
		}
		pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
		if err := s.createExclusive(dpopKeyFile, pemBytes); err != nil {
			return nil, err
		}
		return key, nil
	default:
		return nil, err
	}
}

// Thumbprint is the RFC 7638 JWK SHA-256 thumbprint of the key's public
// half, base64url — the `jkt` value the AS binds the installation to.
func Thumbprint(key *ecdsa.PrivateKey) (string, error) {
	tp, err := (&jose.JSONWebKey{Key: key.Public()}).Thumbprint(crypto.SHA256)
	if err != nil {
		return "", fmt.Errorf("auth: compute JWK thumbprint: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(tp), nil
}

// SaveRefreshToken durably replaces the renewable refresh authorization
// (rotation makes this a frequent, must-not-torn write): tmp file 0600
// in the same directory, then atomic rename.
func (s *Store) SaveRefreshToken(token string) error {
	if token == "" {
		return errors.New("auth: refusing to store an empty refresh token")
	}
	return fsx.WriteFileAtomic(s.dir, refreshTokenFile, []byte(token), 0o600)
}

// LoadRefreshToken returns the stored refresh authorization, or ok=false
// when none exists yet.
func (s *Store) LoadRefreshToken() (token string, ok bool, err error) {
	raw, err := s.readSecret(refreshTokenFile)
	if errors.Is(err, fs.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return string(raw), true, nil
}

// DeleteRefreshToken removes the refresh authorization (revocation /
// family-compromise handling).
func (s *Store) DeleteRefreshToken() error {
	err := os.Remove(filepath.Join(s.dir, refreshTokenFile))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("auth: delete refresh token: %w", err)
	}
	return nil
}

// readSecret loads a secret file, re-verifying on EVERY load that it is
// a regular, owner-only file (ADR-0008: a group/world-readable secret
// refuses mining startup).
func (s *Store) readSecret(name string) ([]byte, error) {
	path := filepath.Join(s.dir, name)
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err // fs.ErrNotExist flows through for first-use
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		return nil, fmt.Errorf("auth: %s is a symlink; refusing", name)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("auth: %s is not a regular file; refusing", name)
	}
	if posixModes && info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("auth: %s is group/world-accessible (%04o); refusing mining startup", name, info.Mode().Perm())
	}
	raw, err := os.ReadFile(path) // #nosec G304 -- path is store-dir + fixed name
	if err != nil {
		return nil, fmt.Errorf("auth: read %s: %w", name, err)
	}
	return raw, nil
}

// createExclusive writes a brand-new secret file 0600 via O_CREAT|O_EXCL
// — first generation must never clobber concurrent creation.
func (s *Store) createExclusive(name string, data []byte) error {
	return fsx.WriteFileExclusive(s.dir, name, data, 0o600)
}

// saveStateFile durably REPLACES name's content — SaveRefreshToken's exact
// idiom (WP2-adversarial-review finding 5), and now the only way any
// mutable state file in this store is written: a random-suffixed temp file
// via CreateTemp (never a fixed name — a fixed name is itself a race
// between two writers, and a leftover from a killed process would
// otherwise get silently published by the next successful write), 0600
// before any content lands in it, fsync'd, then renamed over the final
// name. Any failure removes the temp file rather than leaving it for a
// later rename to publish by accident.
func (s *Store) saveStateFile(name string, data []byte) error {
	return fsx.WriteFileAtomic(s.dir, name, data, 0o600)
}

// ParticipationSecret loads the installation's 32-byte participation
// secret, generating it on first use (ADR-0008: participation.secret,
// 0600, exclusive-create; the derivation lives in internal/mining/draw
// and the raw bytes never leave the custody path).
func (s *Store) ParticipationSecret() (*draw.Secret, error) {
	raw, err := s.readSecret(participationSecretFile)
	switch {
	case err == nil:
		secret, err := draw.SecretFromBytes(raw)
		if err != nil {
			return nil, fmt.Errorf("auth: participation.secret: %w", err)
		}
		return secret, nil
	case errors.Is(err, fs.ErrNotExist):
		secret, err := draw.NewSecret()
		if err != nil {
			return nil, err
		}
		if err := s.createExclusive(participationSecretFile, secret.Bytes()); err != nil {
			return nil, err
		}
		return secret, nil
	default:
		return nil, err
	}
}

// SaveReceipt persists an enrollment receipt's exact bytes (contract
// §22: the proxy retains the signed receipt as durable evidence).
func (s *Store) SaveReceipt(slotID, targetEpoch uint64, compactJWS string) error {
	name := fmt.Sprintf("receipt-%d-%d.jws", slotID, targetEpoch)
	if err := s.createExclusive(name, []byte(compactJWS)); err != nil {
		if errors.Is(err, fs.ErrExist) {
			existing, rerr := s.readSecret(name)
			if rerr == nil && string(existing) == compactJWS {
				return nil // idempotent replay of identical bytes
			}
			return fmt.Errorf("auth: receipt for %d/%d already stored with different bytes", slotID, targetEpoch)
		}
		return err
	}
	return nil
}

// LoadReceipt returns the stored receipt bytes, ok=false when absent.
func (s *Store) LoadReceipt(slotID, targetEpoch uint64) (string, bool, error) {
	raw, err := s.readSecret(fmt.Sprintf("receipt-%d-%d.jws", slotID, targetEpoch))
	if errors.Is(err, fs.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return string(raw), true, nil
}

// AgentRegistration is this installation's identity on the search
// platform (agent onboarding design §3): what register minted, what the
// claim did to it, and what the last enrollment call recorded. It is the
// client's cache of the platform's own state, re-verified against
// GET /v1/agents/{id} on every poll — never trusted as an authority on
// its own, only as what to resume from.
type AgentRegistration struct {
	AgentID            string   `json:"agent_id"`
	ClaimURL           string   `json:"claim_url"`
	ClaimCode          string   `json:"claim_code"`
	Status             string   `json:"status"` // "unclaimed" | "claimed" | "expired"
	Scopes             []string `json:"scopes,omitempty"`
	ClaimExpiresAt     string   `json:"claim_expires_at,omitempty"`
	LastEnrollmentSlot string   `json:"last_enrollment_slot,omitempty"`
	LastEnrollmentAt   string   `json:"last_enrollment_at,omitempty"`
	// SlotRefusal is set when the platform offered more than one mining
	// slot and mining.platform_slot did not resolve one (WP2-adversarial-
	// review finding 15): persisted so shouldResume stops spawning a
	// resume that can only ever hit the same refusal again, and so status
	// can name it explicitly instead of the participant discovering it
	// only from a resume's silent no-op. Cleared the moment a slot
	// actually resolves (config changes, or the platform stops offering
	// more than one).
	SlotRefusal string `json:"slot_refusal,omitempty"`
}

// SaveAgentRegistration persists the platform identity, overwriting
// whatever was there. Like SaveEnrollment, this record legitimately
// advances through unclaimed -> claimed -> enrolled, so it is
// write-and-rename rather than createExclusive.
func (s *Store) SaveAgentRegistration(rec AgentRegistration) error {
	raw, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("auth: encode agent registration: %w", err)
	}
	return s.saveStateFile("agent.json", raw)
}

// LoadAgentRegistration returns the stored platform identity, ok=false
// when this installation has never registered.
func (s *Store) LoadAgentRegistration() (rec AgentRegistration, ok bool, err error) {
	raw, err := s.readSecret("agent.json")
	if errors.Is(err, fs.ErrNotExist) {
		return AgentRegistration{}, false, nil
	}
	if err != nil {
		return AgentRegistration{}, false, err
	}
	if err := json.Unmarshal(raw, &rec); err != nil {
		return AgentRegistration{}, false, fmt.Errorf("%w: %v", ErrAgentRegistrationCorrupt, err)
	}
	return rec, true, nil
}

// PreserveCorruptAgentRegistration moves an undecodable agent record aside
// without deleting it. Callers use this only after deciding that a fresh
// registration is permitted; an existing platform key must cause a refusal
// instead, because the unreadable record may be the identity that key serves.
func (s *Store) PreserveCorruptAgentRegistration() error {
	path := filepath.Join(s.dir, "agent.json")
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&fs.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return errors.New("auth: agent registration is not a regular file")
	}
	if posixModes && info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("auth: agent registration is readable by others (%04o); refusing", info.Mode().Perm())
	}
	backup := filepath.Join(s.dir, "agent.json.corrupt")
	if _, err := os.Lstat(backup); err == nil {
		return errors.New("auth: corrupt agent registration evidence already exists")
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := os.Rename(path, backup); err != nil {
		return fmt.Errorf("auth: preserve corrupt agent registration: %w", err)
	}
	return nil
}

// PendingRegistration is the durable hand-off between a validated remote
// Register response and the local publication of the platform key and agent
// record. It is intentionally separate from AgentRegistration: the key is
// secret material and this record only exists while local publication is
// incomplete.
type PendingRegistration struct {
	V               int    `json:"v"`
	AgentID         string `json:"agent_id"`
	Key             string `json:"key"`
	ClaimURL        string `json:"claim_url"`
	ClaimCode       string `json:"claim_code"`
	ClaimExpiresAt  string `json:"claim_expires_at"`
	Status          string `json:"status"`
	PollIntervalMS  int64  `json:"poll_interval_ms,omitempty"`
	ReplaceExpired  bool   `json:"replace_expired,omitempty"`
	PreviousAgentID string `json:"previous_agent_id,omitempty"`
	PreviousKey     string `json:"previous_key,omitempty"`
}

const pendingRegistrationVersion = 1

func (s *Store) SavePendingRegistration(rec PendingRegistration) error {
	rec.V = pendingRegistrationVersion
	if err := validatePendingRegistration(rec); err != nil {
		return err
	}
	raw, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("auth: encode pending registration: %w", err)
	}
	return s.saveStateFile(registrationPendingFile, raw)
}

func (s *Store) LoadPendingRegistration() (rec PendingRegistration, ok bool, err error) {
	raw, err := s.readSecret(registrationPendingFile)
	if errors.Is(err, fs.ErrNotExist) {
		return PendingRegistration{}, false, nil
	}
	if err != nil {
		return PendingRegistration{}, false, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&rec); err != nil {
		return PendingRegistration{}, false, fmt.Errorf("auth: decode pending registration: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return PendingRegistration{}, false, errors.New("auth: decode pending registration: trailing data")
		}
		return PendingRegistration{}, false, fmt.Errorf("auth: decode pending registration: trailing data: %w", err)
	}
	if err := validatePendingRegistration(rec); err != nil {
		return PendingRegistration{}, false, err
	}
	return rec, true, nil
}

// ClearPendingRegistration removes the transient journal only after both
// local publication targets have been verified. Reuse readSecret first so a
// symlink or unsafe journal is never removed as if it were our state file.
func (s *Store) ClearPendingRegistration() error {
	if _, err := s.readSecret(registrationPendingFile); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	if err := os.Remove(filepath.Join(s.dir, registrationPendingFile)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("auth: clear pending registration: %w", err)
	}
	return nil
}

func validatePendingRegistration(rec PendingRegistration) error {
	if rec.V != pendingRegistrationVersion {
		return fmt.Errorf("auth: pending registration has unsupported version %d", rec.V)
	}
	if !boundedNonEmpty(rec.AgentID, 1, 512) || !boundedNonEmpty(rec.Key, 1, 4096) ||
		!boundedNonEmpty(rec.ClaimURL, 1, 4096) || !bounded(rec.ClaimCode, 0, 512) ||
		!bounded(rec.ClaimExpiresAt, 0, 128) || rec.Status != "unclaimed" ||
		rec.PollIntervalMS < 0 || rec.PollIntervalMS > int64((24*time.Hour)/time.Millisecond) ||
		(rec.ReplaceExpired && (!boundedNonEmpty(rec.PreviousAgentID, 1, 512) || !boundedNonEmpty(rec.PreviousKey, 1, 4096))) ||
		(!rec.ReplaceExpired && (rec.PreviousAgentID != "" || rec.PreviousKey != "")) {
		return errors.New("auth: pending registration fields are invalid")
	}
	return nil
}

func bounded(value string, min, max int) bool {
	n := len(value)
	return n >= min && n <= max
}

func boundedNonEmpty(value string, min, max int) bool {
	return bounded(value, min, max)
}

// SavePayoutAddress persists the payout address decided at the terminal
// (agent onboarding design §5.5): typed directly, or the address of a
// wallet just created. It is kept in its own file rather than folded into
// agent.json — one concern per file, matching dpop.key/refresh.token/
// enrollment.json/receipt-*.jws — because the address is a local mining
// preference the client owns outright, while agent.json mirrors platform
// state that a poll can overwrite. A detached resume reads this file as
// the one thing it needs to declare a payout unattended once enrollment
// succeeds; it never needs to know how the address was decided.
// validatePayoutAddress is the one place every payout address in this
// flow is checked (WP2-adversarial-review finding 9): a terminal-typed
// answer, mining.payout_address from config, and — redundantly but
// harmlessly, since it is already valid by construction — a freshly
// created wallet's own address all funnel through SavePayoutAddress, so
// validating here structurally covers all three without relying on each
// call site to remember to.
func validatePayoutAddress(address string) error {
	hrp, _, err := DecodeBech32Address(address)
	if err != nil {
		return fmt.Errorf("auth: payout address %q does not decode as bech32: %w", address, err)
	}
	if hrp != TwilightHRP {
		return fmt.Errorf("auth: payout address %q has prefix %q, want %q", address, hrp, TwilightHRP)
	}
	return nil
}

func (s *Store) SavePayoutAddress(address string) error {
	if address == "" {
		return errors.New("auth: refusing to store an empty payout address")
	}
	if err := validatePayoutAddress(address); err != nil {
		return err
	}
	raw, err := json.Marshal(struct {
		Address string `json:"address"`
	}{Address: address})
	if err != nil {
		return fmt.Errorf("auth: encode payout address: %w", err)
	}
	return s.saveStateFile("payout.json", raw)
}

// LoadPayoutAddress returns the stored address, ok=false when none has
// been decided yet.
func (s *Store) LoadPayoutAddress() (address string, ok bool, err error) {
	raw, err := s.readSecret("payout.json")
	if errors.Is(err, fs.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	var rec struct {
		Address string `json:"address"`
	}
	if err := json.Unmarshal(raw, &rec); err != nil {
		return "", false, fmt.Errorf("auth: decode payout address: %w", err)
	}
	return rec.Address, true, nil
}

// ConflictedEpoch is one (slot, epoch) pair another installation of this
// participant is confirmed to hold — WP4b (design aba1245 §2.3/§5.5).
// This installation never obtained a capability for it (ErrEnrollmentConflict
// on join, or ErrProxyBindingMismatch on exchange) and never will: the AS's
// current target moving past TargetEpoch is what proves the observations
// this installation queued for it are worthless, not a clock. That is the
// finding that replaced the first version of this record, which was a
// single CapabilityDeadline-gated slot: the deadline only exists when this
// installation itself once held the epoch before losing it, which is the
// RARE case (ErrProxyBindingMismatch after a prior success) — the common
// case (ErrEnrollmentConflict on join, never held at all) has no deadline
// to compare against and the drop never fired. A single record also meant
// a second conflicted epoch silently clobbered the first's.
type ConflictedEpoch struct {
	SlotID      uint64 `json:"slot_id"`
	TargetEpoch uint64 `json:"target_epoch"`
}

// maxEpochConflicts bounds the conflict set (WP2-adversarial-review
// finding 18): normally each entry is short-lived — RemoveEpochConflicts
// drops it as soon as a flush sees the AS's target move past it — but a
// slot_id the current configuration no longer names can never reach that
// removal path, so without a cap a long-lived installation whose slot_id
// changed repeatedly could accumulate entries forever.
const maxEpochConflicts = 64

// SaveEpochConflict records that ErrEnrollmentConflict or
// ErrProxyBindingMismatch was seen for (slotID, targetEpoch): another
// installation of this participant holds it. Adds to the bounded set; a
// pair already on file is left alone rather than duplicated. Past
// maxEpochConflicts the oldest entry (index 0 — the set is always
// appended to, never reordered) is evicted to make room, on the
// reasoning that a conflict this installation cannot even remember
// having queued observations against is one it has already lost track
// of usefully anyway.
func (s *Store) SaveEpochConflict(slotID, targetEpoch uint64) error {
	set, err := s.loadEpochConflicts()
	if err != nil {
		return err
	}
	for _, c := range set {
		if c.SlotID == slotID && c.TargetEpoch == targetEpoch {
			return nil
		}
	}
	set = append(set, ConflictedEpoch{SlotID: slotID, TargetEpoch: targetEpoch})
	if len(set) > maxEpochConflicts {
		set = set[len(set)-maxEpochConflicts:]
	}
	return s.saveEpochConflicts(set)
}

// EpochConflicts returns every (slot, epoch) pair this installation knows
// another installation of this participant holds. Bounded (maxEpochConflicts):
// an entry leaves the set as soon as a flush observes the AS's current target
// has moved past it (RemoveEpochConflicts), which happens on ordinary epoch
// rollover regardless of whether this installation had anything queued for
// it.
func (s *Store) EpochConflicts() ([]ConflictedEpoch, error) {
	return s.loadEpochConflicts()
}

// PruneEpochConflictsForOtherSlots drops every entry whose SlotID is not
// currentSlotID (WP2-adversarial-review finding 18): a slot_id
// reconfiguration strands whatever conflicts were recorded under the old
// one — the state-based drop in flush.go only ever compares against the
// CURRENT slot's target epoch, so an entry for a different slot can never
// be resolved by ordinary epoch advancement and would sit in the set
// forever without this.
func (s *Store) PruneEpochConflictsForOtherSlots(currentSlotID uint64) error {
	set, err := s.loadEpochConflicts()
	if err != nil {
		return err
	}
	kept := set[:0]
	for _, c := range set {
		if c.SlotID == currentSlotID {
			kept = append(kept, c)
		}
	}
	if len(kept) == len(set) {
		return nil // nothing pruned; skip the write
	}
	return s.saveEpochConflicts(kept)
}

// RemoveEpochConflicts drops the named pairs from the set — called once
// their queued observations have been acted on (dropped, or found already
// gone), never speculatively: a caller that removed a pair before
// processing it and then failed to open the spool would lose the record
// that anything needed doing.
func (s *Store) RemoveEpochConflicts(done []ConflictedEpoch) error {
	if len(done) == 0 {
		return nil
	}
	set, err := s.loadEpochConflicts()
	if err != nil {
		return err
	}
	kept := set[:0]
	for _, c := range set {
		remove := false
		for _, d := range done {
			if c.SlotID == d.SlotID && c.TargetEpoch == d.TargetEpoch {
				remove = true
				break
			}
		}
		if !remove {
			kept = append(kept, c)
		}
	}
	return s.saveEpochConflicts(kept)
}

func (s *Store) saveEpochConflicts(set []ConflictedEpoch) error {
	if set == nil {
		set = []ConflictedEpoch{}
	}
	raw, err := json.Marshal(set)
	if err != nil {
		return fmt.Errorf("auth: encode epoch conflicts: %w", err)
	}
	return s.saveStateFile("epoch_conflicts.json", raw)
}

func (s *Store) loadEpochConflicts() ([]ConflictedEpoch, error) {
	raw, err := s.readSecret("epoch_conflicts.json")
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var set []ConflictedEpoch
	if err := json.Unmarshal(raw, &set); err != nil {
		return nil, fmt.Errorf("auth: decode epoch conflicts: %w", err)
	}
	return set, nil
}

// PayoutBindingHeld is what status shows when connect declined to declare
// a payout address unattended because the AS already has a DIFFERENT
// address active for this participant (agent onboarding design §5.5:
// "declares nothing ... status reports both addresses and that changing
// the binding is an operator-activated change"). Cleared once the two
// addresses agree (a later read-before-declare finds Active == Local, or
// an operator activates the change and a later read reflects it).
type PayoutBindingHeld struct {
	Local  string `json:"local"`
	Active string `json:"active"`
	// HeldFor is the AS's reason (HeldReplacesActive, HeldAddressInUse —
	// payout.go), or empty when this hold came from connect's own
	// read-before-declare pre-check (PayoutStanding showing a different
	// active address) rather than the declare call's own response.
	HeldFor string `json:"held_for,omitempty"`
}

// SavePayoutBindingHeld records that declaration was skipped because the
// AS's active address differs from the one this installation would
// declare.
func (s *Store) SavePayoutBindingHeld(local, active string, heldFor string) error {
	raw, err := json.Marshal(PayoutBindingHeld{Local: local, Active: active, HeldFor: heldFor})
	if err != nil {
		return fmt.Errorf("auth: encode payout binding held: %w", err)
	}
	return s.saveStateFile("payout_binding_held.json", raw)
}

// LoadPayoutBindingHeld returns the stored held-binding note, ok=false
// when declaration has never been held (or the hold has been cleared).
func (s *Store) LoadPayoutBindingHeld() (rec PayoutBindingHeld, ok bool, err error) {
	raw, err := s.readSecret("payout_binding_held.json")
	if errors.Is(err, fs.ErrNotExist) {
		return PayoutBindingHeld{}, false, nil
	}
	if err != nil {
		return PayoutBindingHeld{}, false, err
	}
	if err := json.Unmarshal(raw, &rec); err != nil {
		return PayoutBindingHeld{}, false, fmt.Errorf("auth: decode payout binding held: %w", err)
	}
	return rec, true, nil
}

// ClearPayoutBindingHeld removes the held-binding note once the addresses
// agree again.
func (s *Store) ClearPayoutBindingHeld() error {
	err := os.Remove(filepath.Join(s.dir, "payout_binding_held.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// SavePayoutDeclared records that address was confirmed ACTIVE for this
// participant — either just declared successfully, or found already
// matching a read-before-declare — so a later poll with nothing new to say
// can skip the AS round trip entirely (WP2-review defect 2's "avoid
// redundant declare calls" question) and so shouldResume (connect.go) can
// tell, from disk alone, whether an on-file address still needs acting on.
// A DIFFERENT address later saved via SavePayoutAddress makes this stale by
// construction — callers compare the two rather than clearing this file.
func (s *Store) SavePayoutDeclared(address string) error {
	if address == "" {
		return errors.New("auth: refusing to record an empty address as declared")
	}
	raw, err := json.Marshal(struct {
		Address string `json:"address"`
	}{Address: address})
	if err != nil {
		return fmt.Errorf("auth: encode payout declared: %w", err)
	}
	return s.saveStateFile("payout_declared.json", raw)
}

// LoadPayoutDeclared returns the address last confirmed active, ok=false
// when nothing has ever been declared or confirmed from this installation.
func (s *Store) LoadPayoutDeclared() (address string, ok bool, err error) {
	raw, err := s.readSecret("payout_declared.json")
	if errors.Is(err, fs.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	var rec struct {
		Address string `json:"address"`
	}
	if err := json.Unmarshal(raw, &rec); err != nil {
		return "", false, fmt.Errorf("auth: decode payout declared: %w", err)
	}
	return rec.Address, true, nil
}

// SaveMiningEnabled persists the explicit runtime mining decision. The
// versioned record is the authority after it exists; configuration may seed
// this file during an approved onboarding decision but never overrides it.
func (s *Store) SaveMiningEnabled(enabled bool) error {
	raw, err := json.Marshal(struct {
		Version int  `json:"version"`
		Enabled bool `json:"enabled"`
	}{Version: miningDecisionVersion, Enabled: enabled})
	if err != nil {
		return fmt.Errorf("auth: encode mining decision: %w", err)
	}
	if err := s.saveStateFile("mining_decision.json", raw); err != nil {
		return err
	}
	// Health is diagnostic only. A successful explicit decision write proves
	// this component healthy; failure to clear the diagnostic must not make
	// the participant's decision write fail.
	_ = s.ClearHealth(HealthDecision)
	return nil
}

// LoadMiningEnabled is the compatibility convenience wrapper. Callers that
// need to distinguish undecided from degraded must use ReadMiningDecision.
func (s *Store) LoadMiningEnabled() (enabled bool, ok bool, err error) {
	decision := s.ReadMiningDecision()
	if decision.Err != nil {
		return false, decision.Present, decision.Err
	}
	return decision.Enabled, decision.Present, nil
}

// SaveRevokePending marks that `mining disable` stopped mining locally
// but could not confirm the AS accepted the self-service revocation
// (network down, AS unreachable) — the family may still be live there.
// A later flush or resume retries it and clears this marker on success;
// nothing here ever depends on the marker to decide whether mining is
// active locally — mining_decision.json alone (miningActive) already
// settled that, unconditionally, before this file is ever written.
func (s *Store) SaveRevokePending() error {
	return s.saveStateFile("revoke_pending.json", []byte(`{"pending":true}`))
}

// LoadRevokePending reports whether an AS-side revocation from a past
// `mining disable` is still outstanding.
func (s *Store) LoadRevokePending() (pending bool, err error) {
	_, err = s.readSecret("revoke_pending.json")
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// ClearRevokePending removes the marker once the AS confirms the
// revocation, or once a fresh `mining enable` mints a new family that
// supersedes whatever the marker was tracking (mining.go: enabling is
// what makes the old family's fate moot, not this file surviving to be
// retried against the new one).
func (s *Store) ClearRevokePending() error {
	err := os.Remove(filepath.Join(s.dir, "revoke_pending.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}
