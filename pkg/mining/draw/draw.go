package draw

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
)

const (
	// SecretSize is the exact participation_secret length (r5 §38).
	SecretSize = 32
	// IDSize is the exact DrawIDV1 output length (r5 §38).
	IDSize = 32
)

// Domain tags are exact ASCII bytes, no trailing NUL (r5 §37).
const (
	domainDrawID       = "TWILIGHT/TOKENDROP/DRAW-ID/V1"
	domainCandidateSet = "TWILIGHT/TOKENDROP/CANDIDATE-SET/V1"
)

var (
	ErrSecretSize     = errors.New("draw: participation_secret must be exactly 32 bytes")
	ErrChainIDTooLong = errors.New("draw: chain_id exceeds 65535 UTF-8 bytes")
)

// ID is a DrawIDV1 value: exactly 32 raw bytes. Hex is presentation
// only; preimages always use the raw bytes (r5 §36).
type ID [IDSize]byte

// Hex returns the 64-character lowercase hexadecimal presentation.
func (id ID) Hex() string { return hex.EncodeToString(id[:]) }

// IDFromHex decodes the 64-character lowercase transport form (HEX32).
func IDFromHex(s string) (ID, error) {
	var id ID
	if len(s) != 2*IDSize || bytes.ContainsFunc([]byte(s), func(r rune) bool {
		return (r < '0' || r > '9') && (r < 'a' || r > 'f')
	}) {
		return id, fmt.Errorf("draw: draw_id must be 64 lowercase hex characters")
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		return id, fmt.Errorf("draw: decode draw_id: %w", err)
	}
	copy(id[:], b)
	return id, nil
}

// Secret is the local 32-byte participation secret. It MUST remain
// local: never transmitted to the AS, never logged, never included in
// diagnostics. Every fmt verb (including %x and %#v) renders a redaction
// marker instead of the bytes.
type Secret struct{ b [SecretSize]byte }

// NewSecret draws a fresh participation secret from the CSPRNG.
func NewSecret() (*Secret, error) {
	s := new(Secret)
	if _, err := rand.Read(s.b[:]); err != nil {
		return nil, fmt.Errorf("draw: generate participation_secret: %w", err)
	}
	return s, nil
}

// SecretFromBytes copies exactly 32 bytes into a Secret.
func SecretFromBytes(b []byte) (*Secret, error) {
	if len(b) != SecretSize {
		return nil, ErrSecretSize
	}
	s := new(Secret)
	copy(s.b[:], b)
	return s, nil
}

// Bytes exposes the raw secret for persistence (ADR-0008 owner-only
// files). Callers must not let the slice reach any log or wire path.
func (s *Secret) Bytes() []byte { return s.b[:] }

// Format implements fmt.Formatter for every verb, so no fmt path can
// print the secret bytes.
func (s Secret) Format(f fmt.State, _ rune) {
	_, _ = io.WriteString(f, "participation_secret[redacted]")
}

// DrawID computes DrawIDV1 (r5 §38):
//
//	HMAC_SHA256(secret, DOMAIN_DRAW_ID ‖ ChainIDEncoding(chain_id) ‖
//	            U64BE(slot_id) ‖ U64BE(target_epoch))
func (s *Secret) DrawID(chainID string, slotID, targetEpoch uint64) (ID, error) {
	var id ID
	enc, err := chainIDEncoding(chainID)
	if err != nil {
		return id, err
	}
	mac := hmac.New(sha256.New, s.b[:])
	mac.Write([]byte(domainDrawID))
	mac.Write(enc)
	mac.Write(u64be(slotID))
	mac.Write(u64be(targetEpoch))
	copy(id[:], mac.Sum(nil))
	return id, nil
}

// VerifyCanonical checks CandidateList canonicality (r5 §19): strictly
// increasing raw-byte lexicographic order, duplicates forbidden. A
// non-canonical list MUST be rejected, never sorted, deduplicated, or
// otherwise repaired.
func VerifyCanonical(drawIDs []ID) error {
	for i := 1; i < len(drawIDs); i++ {
		switch c := bytes.Compare(drawIDs[i-1][:], drawIDs[i][:]); {
		case c == 0:
			return fmt.Errorf("draw: candidate list has duplicate draw_id at index %d", i)
		case c > 0:
			return fmt.Errorf("draw: candidate list not strictly increasing at index %d", i)
		}
	}
	return nil
}

// CandidateSetHash computes CandidateSetHashV1 (r5 §39) over a canonical
// draw_id sequence. The empty list is valid and hashes deterministically.
func CandidateSetHash(chainID string, slotID, targetEpoch uint64, drawIDs []ID) ([32]byte, error) {
	var out [32]byte
	if err := VerifyCanonical(drawIDs); err != nil {
		return out, err
	}
	enc, err := chainIDEncoding(chainID)
	if err != nil {
		return out, err
	}
	h := sha256.New()
	h.Write([]byte(domainCandidateSet))
	h.Write(enc)
	h.Write(u64be(slotID))
	h.Write(u64be(targetEpoch))
	h.Write(u64be(uint64(len(drawIDs))))
	for i := range drawIDs {
		h.Write(drawIDs[i][:])
	}
	copy(out[:], h.Sum(nil))
	return out, nil
}

// chainIDEncoding is ChainIDEncoding (r5 §36): U16BE(len(UTF8)) ‖ UTF8,
// no normalization; >65535 UTF-8 bytes is invalid.
func chainIDEncoding(chainID string) ([]byte, error) {
	n := len(chainID)
	if n > 0xFFFF {
		return nil, ErrChainIDTooLong
	}
	out := make([]byte, 2+n)
	binary.BigEndian.PutUint16(out, uint16(n)) // #nosec G115 -- bounded to 0xFFFF above
	copy(out[2:], chainID)
	return out, nil
}

func u64be(v uint64) []byte {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], v)
	return b[:]
}
