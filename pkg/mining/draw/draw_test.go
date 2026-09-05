package draw

// VEC-001/VEC-002: byte-exact conformance of DrawIDV1 and
// CandidateSetHashV1 against the checksum-pinned revision-2 golden
// vectors (ADR-X-0003). Test names carry "Vector" so CI stage S7
// (-run 'Vector') selects them on every PR.

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"
)

type vectorPack struct {
	Primitives struct {
		DrawIDV1 struct {
			ChainID                string `json:"chain_id"`
			SlotID                 string `json:"slot_id"`
			TargetEpoch            string `json:"target_epoch"`
			ParticipationSecretHex string `json:"participation_secret_hex"`
			ExpectedDrawIDHex      string `json:"expected_draw_id_hex"`
		} `json:"draw_id_v1"`
		CandidateSetHashV1      candidateSetVector `json:"candidate_set_hash_v1"`
		CandidateSetHashEmptyV1 candidateSetVector `json:"candidate_set_hash_empty_v1"`
	} `json:"primitives"`
}

type candidateSetVector struct {
	ChainID                     string   `json:"chain_id"`
	SlotID                      string   `json:"slot_id"`
	TargetEpoch                 string   `json:"target_epoch"`
	DrawIDsHex                  []string `json:"draw_ids_hex"`
	ExpectedCandidateSetHashHex string   `json:"expected_candidate_set_hash_hex"`
}

func parseVectors(t *testing.T) vectorPack {
	t.Helper()
	var pack vectorPack
	if err := json.Unmarshal(loadVectors(t), &pack); err != nil {
		t.Fatalf("vector file does not parse: %v", err)
	}
	return pack
}

func mustU64(t *testing.T, s string) uint64 {
	t.Helper()
	v, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		t.Fatalf("vector integer %q: %v", s, err)
	}
	return v
}

// VEC-001
func TestVectorDrawIDV1(t *testing.T) {
	v := parseVectors(t).Primitives.DrawIDV1
	secretBytes, err := hex.DecodeString(v.ParticipationSecretHex)
	if err != nil {
		t.Fatal(err)
	}
	secret, err := SecretFromBytes(secretBytes)
	if err != nil {
		t.Fatal(err)
	}
	id, err := secret.DrawID(v.ChainID, mustU64(t, v.SlotID), mustU64(t, v.TargetEpoch))
	if err != nil {
		t.Fatal(err)
	}
	if id.Hex() != v.ExpectedDrawIDHex {
		t.Fatalf("DrawIDV1 mismatch:\n  got  %s\n  want %s\nThe vectors are frozen; this is an implementation defect.", id.Hex(), v.ExpectedDrawIDHex)
	}
}

// VEC-002
func TestVectorCandidateSetHashV1(t *testing.T) {
	pack := parseVectors(t)
	for name, v := range map[string]candidateSetVector{
		"three_candidates": pack.Primitives.CandidateSetHashV1,
		"empty_set":        pack.Primitives.CandidateSetHashEmptyV1,
	} {
		t.Run(name, func(t *testing.T) {
			ids := make([]ID, len(v.DrawIDsHex))
			for i, h := range v.DrawIDsHex {
				id, err := IDFromHex(h)
				if err != nil {
					t.Fatal(err)
				}
				ids[i] = id
			}
			got, err := CandidateSetHash(v.ChainID, mustU64(t, v.SlotID), mustU64(t, v.TargetEpoch), ids)
			if err != nil {
				t.Fatal(err)
			}
			if hex.EncodeToString(got[:]) != v.ExpectedCandidateSetHashHex {
				t.Fatalf("CandidateSetHashV1 mismatch (%s):\n  got  %x\n  want %s", name, got, v.ExpectedCandidateSetHashHex)
			}
		})
	}
}

// §19: a verifier rejects non-canonical lists and never repairs them.
func TestVectorCandidateListCanonicality(t *testing.T) {
	v := parseVectors(t).Primitives.CandidateSetHashV1
	ids := make([]ID, len(v.DrawIDsHex))
	for i, h := range v.DrawIDsHex {
		id, err := IDFromHex(h)
		if err != nil {
			t.Fatal(err)
		}
		ids[i] = id
	}
	slot, epoch := mustU64(t, v.SlotID), mustU64(t, v.TargetEpoch)

	reversed := []ID{ids[2], ids[1], ids[0]}
	if _, err := CandidateSetHash(v.ChainID, slot, epoch, reversed); err == nil {
		t.Fatal("descending candidate list accepted; §19 requires rejection, not repair")
	}
	duplicated := []ID{ids[0], ids[0], ids[1]}
	if _, err := CandidateSetHash(v.ChainID, slot, epoch, duplicated); err == nil {
		t.Fatal("duplicate draw_id accepted; §19 forbids duplicates")
	}
}

func TestChainIDEncodingLimits(t *testing.T) {
	secret, err := NewSecret()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := secret.DrawID(strings.Repeat("a", 0x10000), 1, 1); err != ErrChainIDTooLong {
		t.Fatalf("oversized chain_id not rejected: %v", err)
	}
	if _, err := secret.DrawID(strings.Repeat("a", 0xFFFF), 1, 1); err != nil {
		t.Fatalf("65535-byte chain_id must be valid: %v", err)
	}
}

func TestSecretSizeEnforced(t *testing.T) {
	for _, n := range []int{0, 16, 31, 33, 64} {
		if _, err := SecretFromBytes(make([]byte, n)); err != ErrSecretSize {
			t.Fatalf("secret of %d bytes not rejected: %v", n, err)
		}
	}
}

// PRIV: no fmt verb may reveal secret bytes.
func TestSecretNeverFormats(t *testing.T) {
	raw := make([]byte, SecretSize)
	for i := range raw {
		raw[i] = byte(i + 1)
	}
	secret, err := SecretFromBytes(raw)
	if err != nil {
		t.Fatal(err)
	}
	leak := hex.EncodeToString(raw[:4]) // any prefix of the bytes
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X", "%d"} {
		for _, arg := range []any{secret, *secret} {
			out := fmt.Sprintf(verb, arg)
			if strings.Contains(out, leak) || strings.Contains(out, "1 2 3") {
				t.Fatalf("verb %s leaked secret bytes: %q", verb, out)
			}
			if !strings.Contains(out, "redacted") {
				t.Fatalf("verb %s did not redact: %q", verb, out)
			}
		}
	}
}

func TestIDFromHexRejectsNonCanonicalText(t *testing.T) {
	valid := strings.Repeat("ab", 32)
	if _, err := IDFromHex(valid); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{
		strings.Repeat("AB", 32),       // uppercase is not the transport form
		strings.Repeat("ab", 31),       // short
		strings.Repeat("ab", 33),       // long
		strings.Repeat("g", 64),        // non-hex
		"0x" + strings.Repeat("a", 62), // prefixed
	} {
		if _, err := IDFromHex(bad); err == nil {
			t.Fatalf("IDFromHex accepted %q", bad)
		}
	}
}
