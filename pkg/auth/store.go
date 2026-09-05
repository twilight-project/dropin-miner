package auth

// Secret custody per ADR-0008: owner-only files in a 0700 state
// directory. The store owns the auth-plane material — the DPoP
// installation key and the renewable refresh authorization. Every load
// re-verifies permissions and refuses group/world-accessible or
// symlinked paths: custody failures close the MINING plane, never
// inference (the boundary tests keep this package off the request path).

import (
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
	"io/fs"
	"os"
	"path/filepath"

	jose "github.com/go-jose/go-jose/v4"

	"github.com/twilight-project/dropin-miner/pkg/mining/draw"
)

const (
	dpopKeyFile             = "dpop.key"
	participationSecretFile = "participation.secret"
	refreshTokenFile        = "refresh.token"
)

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
	case errors.Is(err, fs.ErrNotExist):
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
	tmp, err := os.CreateTemp(s.dir, refreshTokenFile+".tmp-*")
	if err != nil {
		return fmt.Errorf("auth: stage refresh token: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("auth: chmod refresh token: %w", err)
	}
	if _, err := tmp.WriteString(token); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("auth: write refresh token: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("auth: sync refresh token: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("auth: close refresh token: %w", err)
	}
	if err := os.Rename(tmpName, filepath.Join(s.dir, refreshTokenFile)); err != nil {
		return fmt.Errorf("auth: install refresh token: %w", err)
	}
	return nil
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
	f, err := os.OpenFile(filepath.Join(s.dir, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 -- store-dir + fixed name
	if err != nil {
		return fmt.Errorf("auth: create %s: %w", name, err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("auth: write %s: %w", name, err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("auth: sync %s: %w", name, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("auth: close %s: %w", name, err)
	}
	return nil
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

// enrollmentRecord is the durable pointer to the epoch this installation
// is enrolled in. It is what lets the mining plane spool observations
// with the right delivery context from the first request after a
// restart, before any network round-trip to the AS has happened —
// fail-open means the proxy must never wait on the AS to start serving.
type enrollmentRecord struct {
	SlotID      uint64 `json:"slot_id"`
	TargetEpoch uint64 `json:"target_epoch"`
}

// SaveEnrollment records the enrolled target after a successful
// JoinEpoch. Later epochs overwrite earlier ones: enrollment advances,
// and the receipt files keep the full history (SaveReceipt).
func (s *Store) SaveEnrollment(slotID, targetEpoch uint64) error {
	raw, err := json.Marshal(enrollmentRecord{SlotID: slotID, TargetEpoch: targetEpoch})
	if err != nil {
		return fmt.Errorf("auth: encode enrollment: %w", err)
	}
	// Write-and-rename rather than createExclusive: unlike a receipt,
	// this file legitimately changes every epoch.
	tmp := filepath.Join(s.dir, "enrollment.json.tmp")
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf("auth: write enrollment: %w", err)
	}
	if err := os.Rename(tmp, filepath.Join(s.dir, "enrollment.json")); err != nil {
		return fmt.Errorf("auth: persist enrollment: %w", err)
	}
	return nil
}

// LoadEnrollment returns the enrolled target, ok=false when this
// installation has never joined an epoch.
func (s *Store) LoadEnrollment() (slotID, targetEpoch uint64, ok bool, err error) {
	raw, err := s.readSecret("enrollment.json")
	if errors.Is(err, fs.ErrNotExist) {
		return 0, 0, false, nil
	}
	if err != nil {
		return 0, 0, false, err
	}
	var rec enrollmentRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return 0, 0, false, fmt.Errorf("auth: decode enrollment: %w", err)
	}
	return rec.SlotID, rec.TargetEpoch, true, nil
}
