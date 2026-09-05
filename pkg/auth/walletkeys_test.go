package auth

// Every hand-rolled layer is pinned by its specification's published test
// vectors, and the whole stack by a cosmjs-published end-to-end vector.
// The vectors are public standards material, not real credentials
// (testdata/README.md spirit: nothing here ever held value).

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

// The SHA-256 of the official BIP39 English wordlist file — any edit to
// the embedded copy, down to a byte, fails here.
func TestBIP39WordlistMatchesTheOfficialSHA256(t *testing.T) {
	sum := sha256.Sum256([]byte(bip39EnglishRaw))
	const want = "2f5eed53a4727b4bf8880d8f3f199efc90e58503646d9ff8eff3a2ed3b24dbda"
	if got := hex.EncodeToString(sum[:]); got != want {
		t.Fatalf("embedded wordlist hash %s, want the official %s", got, want)
	}
	if n := len(bip39Words()); n != 2048 {
		t.Fatalf("wordlist has %d words", n)
	}
}

// Trezor's BIP39 vectors: 256-bit entropy → mnemonic, and mnemonic →
// seed under the vectors' fixed passphrase "TREZOR".
func TestBIP39VectorsRoundTrip(t *testing.T) {
	vectors := []struct{ entropy, mnemonic, seed string }{
		{
			"0000000000000000000000000000000000000000000000000000000000000000",
			"abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon art",
			"bda85446c68413707090a52022edd26a1c9462295029f2e60cd7c4f2bbd3097170af7a4d73245cafa9c3cca8d561a7c3de6f5d4a10be8ed2a5e608d68f92fcc8",
		},
		{
			"7f7f7f7f7f7f7f7f7f7f7f7f7f7f7f7f7f7f7f7f7f7f7f7f7f7f7f7f7f7f7f7f",
			"legal winner thank year wave sausage worth useful legal winner thank year wave sausage worth useful legal winner thank year wave sausage worth title",
			"bc09fca1804f7e69da93c2f2028eb238c227f2e9dda30cd63699232578480a4021b146ad717fbb7e451ce9eb835f43620bf5c514db0f8add49f5d121449d3e87",
		},
		{
			"8080808080808080808080808080808080808080808080808080808080808080",
			"letter advice cage absurd amount doctor acoustic avoid letter advice cage absurd amount doctor acoustic avoid letter advice cage absurd amount doctor acoustic bless",
			"c0c519bd0e91a2ed54357d9d1ebef6f5af218a153624cf4f2da911a0ed8f7a09e2ef61af0aca007096df430022f7a2b6fb91661a9589097069720d015e4e982f",
		},
		{
			"ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff",
			"zoo zoo zoo zoo zoo zoo zoo zoo zoo zoo zoo zoo zoo zoo zoo zoo zoo zoo zoo zoo zoo zoo zoo vote",
			"dd48c104698c30cfe2b6142103248622fb7bb0ff692eebb00089b32d22484e1613912f0a5b694407be899ffd31ed3992c456cdf60f5d4564b8ba3f05a69890ad",
		},
	}
	for _, v := range vectors {
		entropy, err := hex.DecodeString(v.entropy)
		if err != nil {
			t.Fatal(err)
		}
		mnemonic, err := NewWalletMnemonic(entropy)
		if err != nil {
			t.Fatal(err)
		}
		if mnemonic != v.mnemonic {
			t.Errorf("entropy %s...: mnemonic %q, want %q", v.entropy[:8], mnemonic, v.mnemonic)
		}
		if got := hex.EncodeToString(mnemonicSeedWithPassphrase(mnemonic, "TREZOR")); got != v.seed {
			t.Errorf("entropy %s...: seed %s, want %s", v.entropy[:8], got, v.seed)
		}
	}
}

// The RIPEMD-160 paper's own vectors.
func TestRIPEMD160MatchesTheSpecificationVectors(t *testing.T) {
	vectors := map[string]string{
		"":                           "9c1185a5c5e9fc54612808977ee8f548b2258d31",
		"a":                          "0bdc9d2d256b3ee9daae347be6f4dc835a467ffe",
		"abc":                        "8eb208f7e05d987a9b044a8e98c6b087f15a0bfc",
		"message digest":             "5d0689ef49d2fae572b881b123a85ffa21595f36",
		"abcdefghijklmnopqrstuvwxyz": "f71c27109c692c1b56bbdceb5b9d2865b3708dbc",
		"abcdbcdecdefdefgefghfghighijhijkijkljklmklmnlmnomnopnopq":       "12a053384a9c0c88e405a06c27dcf49ada62eb2b",
		"ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789": "b0e20b6e3116640286ed3a87a5713079b21f5189",
		strings.Repeat("1234567890", 8):                                  "9b752e45573d4b39f4dbd3323cab82bf63326bfb",
		strings.Repeat("a", 1_000_000):                                   "52783243c1697bdbe16d37f97f68f08325dc1528",
	}
	for msg, want := range vectors {
		if got := hex.EncodeToString(ripemd160Sum([]byte(msg))); got != want {
			label := msg
			if len(label) > 24 {
				label = label[:24] + "..."
			}
			t.Errorf("ripemd160(%q) = %s, want %s", label, got, want)
		}
	}
}

// BIP-173's own vectors: the valid set decodes, the invalid set refuses,
// and encode is the inverse of decode.
func TestBech32MatchesTheBIP173Vectors(t *testing.T) {
	valid := []string{
		"A12UEL5L",
		"a12uel5l",
		"an83characterlonghumanreadablepartthatcontainsthenumber1andtheexcludedcharactersbio1tt5tgs",
		"abcdef1qpzry9x8gf2tvdw0s3jn54khce6mua7lmqqqxw",
		"1" + "1" + strings.Repeat("q", 82) + "c8247j", // the 90-char-limit vector
		"split1checkupstagehandshakeupstreamerranterredcaperred2y9e3w",
	}
	for _, s := range valid {
		if _, _, err := bech32Decode(s); err != nil {
			t.Errorf("valid vector %q refused: %v", s, err)
		}
	}
	invalid := []string{
		"split1cheo2y9e2w", // invalid character in checksum region
		"split1checkupstagehandshakeupstreamerranterredcaperred2y9e2w", // damaged checksum
		"A12UeL5L",     // mixed case
		"1nwldj5",      // empty HRP
		"pzry9x0s0muk", // no separator
	}
	for _, s := range invalid {
		if _, _, err := bech32Decode(s); err == nil {
			t.Errorf("invalid vector %q accepted", s)
		}
	}

	payload := []byte{0x00, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88, 0x99,
		0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff, 0x01, 0x23, 0x45, 0x67}
	enc, err := bech32Encode(TwilightHRP, payload)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(enc, "twilight1") {
		t.Fatalf("encoded address %q lacks the twilight prefix", enc)
	}
	hrp, back, err := bech32Decode(enc)
	if err != nil || hrp != TwilightHRP || !bytes.Equal(back, payload) {
		t.Fatalf("round trip failed: hrp=%q err=%v", hrp, err)
	}
}

// The end-to-end pin: cosmjs's published wallet vector exercises BIP39
// seed → BIP32 m/44'/118'/0'/0/0 → secp256k1 → SHA256+RIPEMD160 → bech32
// in one assertion, then re-renders the same key under the twilight HRP.
func TestDerivationMatchesTheCosmJSVector(t *testing.T) {
	const (
		mnemonic    = "special sign fit simple patrol salute grocery chicken wheat radar tonight ceiling"
		wantPubKey  = "02baa4ef93f2ce84592a49b1d729c074eab640112522a7a89f7d03ebab21ded7b6"
		wantAddress = "cosmos1jhg0e7s6gn44tfc5k37kr04sznyhedtc9rzys5"
	)
	key, err := DeriveWalletKey(mnemonic)
	if err != nil {
		t.Fatal(err)
	}
	if got := key.PubKeyHex(); got != wantPubKey {
		t.Fatalf("pubkey %s, want %s", got, wantPubKey)
	}
	cosmos, err := key.Address("cosmos")
	if err != nil {
		t.Fatal(err)
	}
	if cosmos != wantAddress {
		t.Fatalf("address %s, want %s", cosmos, wantAddress)
	}
	// Same key, our chain's prefix: identical 20 bytes behind the HRP.
	twilight, err := key.Address(TwilightHRP)
	if err != nil {
		t.Fatal(err)
	}
	_, cosmosBytes, _ := bech32Decode(cosmos)
	hrp, twilightBytes, err := bech32Decode(twilight)
	if err != nil || hrp != TwilightHRP || !bytes.Equal(cosmosBytes, twilightBytes) {
		t.Fatalf("twilight rendering diverged: %q", twilight)
	}
}

func TestKeyfileRoundTripRefusalsAndTamper(t *testing.T) {
	key, err := GenerateWalletKeyForTest()
	if err != nil {
		t.Fatal(err)
	}
	kf, err := SealWalletKey(key, "correct-horse")
	if err != nil {
		t.Fatal(err)
	}

	back, err := OpenWalletKey(kf, "correct-horse")
	if err != nil {
		t.Fatal(err)
	}
	if back.PubKeyHex() != key.PubKeyHex() {
		t.Fatal("round trip changed the key")
	}

	if _, err := OpenWalletKey(kf, "wrong-passphrase"); err == nil {
		t.Fatal("wrong passphrase opened the keyfile")
	}

	// Flip one ciphertext byte: GCM must refuse, not return garbage.
	raw, _ := hex.DecodeString("00")
	_ = raw
	tampered := *kf
	ct := []byte(tampered.Ciphertext)
	ct[0] ^= 1
	tampered.Ciphertext = string(ct)
	if _, err := OpenWalletKey(&tampered, "correct-horse"); err == nil {
		t.Fatal("tampered ciphertext opened cleanly")
	}
}
