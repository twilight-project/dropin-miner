package draw

// The Phase-0 golden-vector harness (ADR-X-0003): the vendored revision-2
// vector file must match its pinned checksum byte-for-byte before anything
// derives from it. CI stage S7 runs this on every PR (D5 §8.1). The DrawIDV1
// conformance tests (VEC-001/VEC-002) build on this file in P-I1.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// The frozen pin, identical to docs/chain-canon/SHA256SUMS in
// tokendrop-auth-server-design. Changing this constant is a canonical
// protocol change (ADR-X-0003), never a local convenience.
const vectorsSHA256 = "7c9b7036ec3c9435654942f91e47024a8d0078a19f477a55204ef484efc89f1f"

const vectorsRelPath = "../../../testdata/vectors/twilight-tokendrop-draw-v1-golden-vectors-r2.json"

func loadVectors(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Clean(vectorsRelPath))
	if err != nil {
		t.Fatalf("golden vector file missing: %v", err)
	}
	sum := sha256.Sum256(data)
	if got := hex.EncodeToString(sum[:]); got != vectorsSHA256 {
		t.Fatalf("golden vector file checksum mismatch:\n  got  %s\n  want %s\nThe vector file is frozen; a mismatch is a defect to report, not drift to absorb.", got, vectorsSHA256)
	}
	return data
}

func TestGoldenVectorFileIntegrity(t *testing.T) {
	data := loadVectors(t)

	var doc struct {
		Format     string `json:"format"`
		Version    int    `json:"version"`
		Revision   int    `json:"revision"`
		Normative  bool   `json:"normative"`
		Primitives struct {
			DrawIDV1 struct {
				ChainID                string `json:"chain_id"`
				SlotID                 string `json:"slot_id"`
				TargetEpoch            string `json:"target_epoch"`
				ParticipationSecretHex string `json:"participation_secret_hex"`
				ExpectedDrawIDHex      string `json:"expected_draw_id_hex"`
			} `json:"draw_id_v1"`
		} `json:"primitives"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("vector file does not parse: %v", err)
	}
	if doc.Format != "twilight-tokendrop-draw-v1-golden-vectors" || doc.Version != 1 || doc.Revision != 2 || !doc.Normative {
		t.Fatalf("unexpected vector pack identity: format=%q version=%d revision=%d normative=%v",
			doc.Format, doc.Version, doc.Revision, doc.Normative)
	}
	// The primitive DrawIDV1 vector must be present and well-formed; the
	// byte-exact derivation check (VEC-001) lands with the implementation.
	v := doc.Primitives.DrawIDV1
	if len(v.ExpectedDrawIDHex) != 64 || len(v.ParticipationSecretHex) != 64 || v.ChainID == "" {
		t.Fatalf("draw_id_v1 primitive vector malformed: %+v", v)
	}
}
