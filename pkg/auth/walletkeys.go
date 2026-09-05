package auth

// Wallet key material: BIP39 mnemonic, BIP32 derivation on the Cosmos
// path, the twilight bech32 address, and the encrypted keyfile.
//
// This lives in internal/auth because that is the ADR-0010 boundary: the
// one package permitted to import security libraries, behind an audited
// surface the composition root reaches through. The curve implementation
// (decred/dcrd/dcrec/secp256k1) joins the admitted set with the same
// standing as jose and dpop — elliptic-curve arithmetic is the piece
// where a hand-rolled implementation would be a liability, not a
// discipline.
//
// Everything else is hand-rolled against public specifications, because
// the repo bans the Cosmos SDK module tree outright (AGENTS.md;
// TestNoChainImportsAnywhere) and the pieces are small: bech32 is
// BIP-173, derivation is BIP32/BIP44, the mnemonic is BIP39, and
// RIPEMD-160 is implemented below rather than importing the deprecated
// x/crypto package for it. Each layer is pinned by its specification's
// published test vectors in walletkeys_test.go, and the full stack by a
// cosmjs-published end-to-end vector.

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/sha512"
	_ "embed"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
)

// TwilightHRP is the chain's bech32 human-readable part. The devnet pays
// twilight1... destinations, and the AS validates admissibility against
// the chain, so a wrong prefix fails loudly at registration.
const TwilightHRP = "twilight"

// cosmosHDPath is BIP44 for the Cosmos coin type: m/44'/118'/0'/0/0.
// The ecosystem-standard path, so the printed mnemonic recovers this
// exact account in any standard Cosmos wallet.
var cosmosHDPath = [5]uint32{44 | hardened, 118 | hardened, 0 | hardened, 0, 0}

// WalletHDPath renders the derivation path for display.
const WalletHDPath = "m/44'/118'/0'/0/0"

const hardened = 0x80000000

//go:embed bip39_english.txt
var bip39EnglishRaw string

// WalletKey is a derived signing key. The private scalar never leaves
// this package; callers get the public key, the address, and (for the
// send path) signatures.
type WalletKey struct {
	priv *secp256k1.PrivateKey
}

// ---- BIP39 ----

func bip39Words() []string {
	return strings.Split(strings.TrimSpace(bip39EnglishRaw), "\n")
}

// NewWalletMnemonic converts 256 bits of entropy into 24 words: the
// 8-bit SHA-256 checksum is appended and the 264 bits are read as 24
// 11-bit word indexes (BIP39).
func NewWalletMnemonic(entropy []byte) (string, error) {
	if len(entropy) != 32 {
		return "", fmt.Errorf("wallet: entropy must be 32 bytes, got %d", len(entropy))
	}
	words := bip39Words()
	if len(words) != 2048 {
		return "", fmt.Errorf("wallet: embedded wordlist has %d words, want 2048", len(words))
	}
	sum := sha256.Sum256(entropy)
	// 33 bytes = 264 bits = 24 * 11.
	bits := make([]byte, 0, 33)
	bits = append(bits, entropy...)
	bits = append(bits, sum[0])

	out := make([]string, 24)
	for i := range out {
		var idx uint16
		for b := 0; b < 11; b++ {
			bitPos := i*11 + b
			byteIdx, bitIdx := bitPos/8, 7-(bitPos%8)
			idx <<= 1
			idx |= uint16(bits[byteIdx]>>bitIdx) & 1
		}
		out[i] = words[idx]
	}
	return strings.Join(out, " "), nil
}

// mnemonicSeed is BIP39's seed derivation. The BIP39 passphrase is empty
// on purpose: the recovery phrase alone must recover the account in any
// standard wallet. Confidentiality at rest comes from the keyfile
// encryption, which is a different secret with a different job.
func mnemonicSeed(mnemonic string) []byte {
	return mnemonicSeedWithPassphrase(mnemonic, "")
}

// mnemonicSeedWithPassphrase exists so the BIP39 specification vectors
// (which fix the passphrase "TREZOR") can pin the derivation.
func mnemonicSeedWithPassphrase(mnemonic, passphrase string) []byte {
	seed, err := pbkdf2.Key(sha512.New, mnemonic, []byte("mnemonic"+passphrase), 2048, 64)
	if err != nil {
		// Unreachable with fixed, valid parameters.
		panic(err)
	}
	return seed
}

// ---- BIP32 on the Cosmos path ----

// DeriveWalletKey walks m/44'/118'/0'/0/0 from the mnemonic's seed.
func DeriveWalletKey(mnemonic string) (*WalletKey, error) {
	seed := mnemonicSeed(mnemonic)
	mac := hmac.New(sha512.New, []byte("Bitcoin seed"))
	_, _ = mac.Write(seed)
	i := mac.Sum(nil)
	key, chain := i[:32], i[32:]

	var k secp256k1.ModNScalar
	if overflow := k.SetByteSlice(key); overflow || k.IsZero() {
		return nil, errors.New("wallet: invalid master key from seed")
	}

	for _, index := range cosmosHDPath {
		mac := hmac.New(sha512.New, chain)
		if index >= hardened {
			var ser [32]byte
			k.PutBytes(&ser)
			_, _ = mac.Write([]byte{0})
			_, _ = mac.Write(ser[:])
		} else {
			priv := secp256k1.NewPrivateKey(&k)
			_, _ = mac.Write(priv.PubKey().SerializeCompressed())
		}
		var idx [4]byte
		binary.BigEndian.PutUint32(idx[:], index)
		_, _ = mac.Write(idx[:])
		i := mac.Sum(nil)

		var il secp256k1.ModNScalar
		if overflow := il.SetByteSlice(i[:32]); overflow {
			return nil, errors.New("wallet: derivation produced an out-of-range child (retry with new entropy)")
		}
		il.Add(&k)
		if il.IsZero() {
			return nil, errors.New("wallet: derivation produced a zero child key (retry with new entropy)")
		}
		k = il
		chain = i[32:]
	}
	return &WalletKey{priv: secp256k1.NewPrivateKey(&k)}, nil
}

// PubKeyCompressed is the 33-byte SEC1 form.
func (w *WalletKey) PubKeyCompressed() []byte {
	return w.priv.PubKey().SerializeCompressed()
}

// PubKeyHex renders the compressed public key for the sidecar.
func (w *WalletKey) PubKeyHex() string {
	return hex.EncodeToString(w.PubKeyCompressed())
}

// Address is the Cosmos convention: bech32(RIPEMD160(SHA256(pub))).
func (w *WalletKey) Address(hrp string) (string, error) {
	return AddressFromPubKey(hrp, w.PubKeyCompressed())
}

// AddressFromPubKey derives a bech32 account address from a compressed
// public key under the given prefix.
func AddressFromPubKey(hrp string, compressed []byte) (string, error) {
	sha := sha256.Sum256(compressed)
	return bech32Encode(hrp, ripemd160Sum(sha[:]))
}

// DecodeBech32Address validates a bech32 string (checksum, case, length)
// and returns its prefix and payload — the send path's -to validation.
func DecodeBech32Address(s string) (hrp string, payload []byte, err error) {
	return bech32Decode(s)
}

// ---- bech32 (BIP-173, classic checksum — what Cosmos addresses use) ----

const bech32Charset = "qpzry9x8gf2tvdw0s3jn54khce6mua7l"

var bech32Gen = [5]uint32{0x3b6a57b2, 0x26508e6d, 0x1ea119fa, 0x3d4233dd, 0x2a1462b3}

func bech32Polymod(values []byte) uint32 {
	chk := uint32(1)
	for _, v := range values {
		top := chk >> 25
		chk = (chk&0x1ffffff)<<5 ^ uint32(v)
		for i := 0; i < 5; i++ {
			if (top>>i)&1 == 1 {
				chk ^= bech32Gen[i]
			}
		}
	}
	return chk
}

func bech32HRPExpand(hrp string) []byte {
	out := make([]byte, 0, len(hrp)*2+1)
	for _, c := range hrp {
		// The decoder has already refused anything outside 33..126, so
		// the rune fits a byte by construction.
		out = append(out, byte(c&0x7f)>>5)
	}
	out = append(out, 0)
	for _, c := range hrp {
		out = append(out, byte(c&0x1f))
	}
	return out
}

// bech32Encode encodes arbitrary bytes (converted 8→5 bits, padded) under
// hrp with the classic BIP-173 checksum.
func bech32Encode(hrp string, data []byte) (string, error) {
	five, err := convertBits(data, 8, 5, true)
	if err != nil {
		return "", err
	}
	values := append(bech32HRPExpand(hrp), five...)
	polymod := bech32Polymod(append(values, 0, 0, 0, 0, 0, 0)) ^ 1
	var b strings.Builder
	b.WriteString(hrp)
	b.WriteByte('1')
	for _, v := range five {
		b.WriteByte(bech32Charset[v])
	}
	for i := 0; i < 6; i++ {
		b.WriteByte(bech32Charset[polymod>>(5*(5-i))&31])
	}
	return b.String(), nil
}

// bech32Decode validates checksum and case rules and returns hrp + bytes.
func bech32Decode(s string) (string, []byte, error) {
	if strings.ToLower(s) != s && strings.ToUpper(s) != s {
		return "", nil, errors.New("mixed case")
	}
	s = strings.ToLower(s)
	sep := strings.LastIndexByte(s, '1')
	if sep < 1 || sep+7 > len(s) || len(s) > 90 {
		return "", nil, errors.New("malformed bech32 string")
	}
	hrp, rest := s[:sep], s[sep+1:]
	for _, c := range hrp {
		if c < 33 || c > 126 {
			return "", nil, errors.New("invalid character in prefix")
		}
	}
	data := make([]byte, len(rest))
	for i, c := range rest {
		idx := strings.IndexRune(bech32Charset, c)
		if idx < 0 {
			return "", nil, errors.New("invalid character in data part")
		}
		data[i] = byte(idx & 0x1f) // the charset has 32 entries
	}
	if bech32Polymod(append(bech32HRPExpand(hrp), data...)) != 1 {
		return "", nil, errors.New("checksum mismatch")
	}
	decoded, err := convertBits(data[:len(data)-6], 5, 8, false)
	if err != nil {
		return "", nil, err
	}
	return hrp, decoded, nil
}

func convertBits(data []byte, from, to uint, pad bool) ([]byte, error) {
	var acc, bits uint
	maxv := uint(1)<<to - 1
	out := make([]byte, 0, len(data)*int(from)/int(to)+1)
	for _, b := range data {
		if uint(b)>>from != 0 {
			return nil, fmt.Errorf("invalid data range: %d", b)
		}
		acc = acc<<from | uint(b)
		bits += from
		for bits >= to {
			bits -= to
			out = append(out, byte(acc>>bits&maxv&0xff))
		}
	}
	if pad {
		if bits > 0 {
			out = append(out, byte(acc<<(to-bits)&maxv&0xff))
		}
	} else if bits >= from || acc<<(to-bits)&maxv != 0 {
		return nil, errors.New("invalid padding")
	}
	return out, nil
}

// ---- RIPEMD-160 ----
//
// Implemented from the specification (Dobbertin, Bosselaers, Preneel
// 1996) because the only Go implementation lives in a deprecated
// x/crypto package this module does not otherwise need. Pinned by the
// paper's own test vectors in walletkeys_test.go.

var (
	rmdR = [80]byte{
		0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15,
		7, 4, 13, 1, 10, 6, 15, 3, 12, 0, 9, 5, 2, 14, 11, 8,
		3, 10, 14, 4, 9, 15, 8, 1, 2, 7, 0, 6, 13, 11, 5, 12,
		1, 9, 11, 10, 0, 8, 12, 4, 13, 3, 7, 15, 14, 5, 6, 2,
		4, 0, 5, 9, 7, 12, 2, 10, 14, 1, 3, 8, 11, 6, 15, 13,
	}
	rmdRp = [80]byte{
		5, 14, 7, 0, 9, 2, 11, 4, 13, 6, 15, 8, 1, 10, 3, 12,
		6, 11, 3, 7, 0, 13, 5, 10, 14, 15, 8, 12, 4, 9, 1, 2,
		15, 5, 1, 3, 7, 14, 6, 9, 11, 8, 12, 2, 10, 0, 4, 13,
		8, 6, 4, 1, 3, 11, 15, 0, 5, 12, 2, 13, 9, 7, 10, 14,
		12, 15, 10, 4, 1, 5, 8, 7, 6, 2, 13, 14, 0, 3, 9, 11,
	}
	rmdS = [80]byte{
		11, 14, 15, 12, 5, 8, 7, 9, 11, 13, 14, 15, 6, 7, 9, 8,
		7, 6, 8, 13, 11, 9, 7, 15, 7, 12, 15, 9, 11, 7, 13, 12,
		11, 13, 6, 7, 14, 9, 13, 15, 14, 8, 13, 6, 5, 12, 7, 5,
		11, 12, 14, 15, 14, 15, 9, 8, 9, 14, 5, 6, 8, 6, 5, 12,
		9, 15, 5, 11, 6, 8, 13, 12, 5, 12, 13, 14, 11, 8, 5, 6,
	}
	rmdSp = [80]byte{
		8, 9, 9, 11, 13, 15, 15, 5, 7, 7, 8, 11, 14, 14, 12, 6,
		9, 13, 15, 7, 12, 8, 9, 11, 7, 7, 12, 7, 6, 15, 13, 11,
		9, 7, 15, 11, 8, 6, 6, 14, 12, 13, 5, 14, 13, 13, 7, 5,
		15, 5, 8, 11, 14, 14, 6, 14, 6, 9, 12, 9, 12, 5, 15, 8,
		8, 5, 12, 9, 12, 5, 14, 6, 8, 13, 6, 5, 15, 13, 11, 11,
	}
)

func rmdF(j int, x, y, z uint32) uint32 {
	switch {
	case j < 16:
		return x ^ y ^ z
	case j < 32:
		return (x & y) | (^x & z)
	case j < 48:
		return (x | ^y) ^ z
	case j < 64:
		return (x & z) | (y & ^z)
	default:
		return x ^ (y | ^z)
	}
}

func rmdK(j int) uint32 {
	return [5]uint32{0x00000000, 0x5a827999, 0x6ed9eba1, 0x8f1bbcdc, 0xa953fd4e}[j/16]
}

func rmdKp(j int) uint32 {
	return [5]uint32{0x50a28be6, 0x5c4dd124, 0x6d703ef3, 0x7a6d76e9, 0x00000000}[j/16]
}

func rol(x uint32, n byte) uint32 { return x<<n | x>>(32-n) }

// ripemd160Sum hashes msg with RIPEMD-160.
func ripemd160Sum(msg []byte) []byte {
	h := [5]uint32{0x67452301, 0xefcdab89, 0x98badcfe, 0x10325476, 0xc3d2e1f0}

	// Padding: 0x80, zeros, 64-bit little-endian bit length.
	padded := make([]byte, 0, len(msg)+72)
	padded = append(padded, msg...)
	padded = append(padded, 0x80)
	for len(padded)%64 != 56 {
		padded = append(padded, 0)
	}
	padded = binary.LittleEndian.AppendUint64(padded, uint64(len(msg))*8)

	var x [16]uint32
	for block := 0; block < len(padded); block += 64 {
		for i := range x {
			x[i] = binary.LittleEndian.Uint32(padded[block+i*4:])
		}
		a, b, c, d, e := h[0], h[1], h[2], h[3], h[4]
		ap, bp, cp, dp, ep := h[0], h[1], h[2], h[3], h[4]
		for j := 0; j < 80; j++ {
			t := rol(a+rmdF(j, b, c, d)+x[rmdR[j]]+rmdK(j), rmdS[j]) + e
			a, e, d, c, b = e, d, rol(c, 10), b, t
			t = rol(ap+rmdF(79-j, bp, cp, dp)+x[rmdRp[j]]+rmdKp(j), rmdSp[j]) + ep
			ap, ep, dp, cp, bp = ep, dp, rol(cp, 10), bp, t
		}
		t := h[1] + c + dp
		h[1] = h[2] + d + ep
		h[2] = h[3] + e + ap
		h[3] = h[4] + a + bp
		h[4] = h[0] + b + cp
		h[0] = t
	}

	out := make([]byte, 20)
	for i, v := range h {
		binary.LittleEndian.PutUint32(out[i*4:], v)
	}
	return out
}

// ---- keyfile ----

// WalletKeyfileIterations is the PBKDF2-SHA256 count for the keyfile
// KDF; OWASP's 2023+ floor.
const WalletKeyfileIterations = 600_000

// WalletKeyfile is the encrypted-at-rest form of the derived private
// key. The mnemonic is NEVER stored in any form — the file holds only
// the derived key, sealed with AES-256-GCM under a passphrase-derived
// key.
type WalletKeyfile struct {
	Version    int    `json:"version"`
	KDF        string `json:"kdf"`
	Iterations int    `json:"iterations"`
	Salt       string `json:"salt"`
	Nonce      string `json:"nonce"`
	Ciphertext string `json:"ciphertext"`
}

func keyfileKDF(passphrase string, salt []byte, iterations int) []byte {
	if iterations < 1 {
		iterations = WalletKeyfileIterations
	}
	k, err := pbkdf2.Key(sha256.New, passphrase, salt, iterations, 32)
	if err != nil {
		panic(err) // unreachable with fixed, valid parameters
	}
	return k
}

// SealWalletKey encrypts the private key under the passphrase.
func SealWalletKey(w *WalletKey, passphrase string) (*WalletKeyfile, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, fmt.Errorf("wallet: entropy: %w", err)
	}
	block, err := aes.NewCipher(keyfileKDF(passphrase, salt, WalletKeyfileIterations))
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("wallet: entropy: %w", err)
	}
	ct := gcm.Seal(nil, nonce, w.priv.Serialize(), nil)
	return &WalletKeyfile{
		Version:    1,
		KDF:        "pbkdf2-sha256",
		Iterations: WalletKeyfileIterations,
		Salt:       base64.StdEncoding.EncodeToString(salt),
		Nonce:      base64.StdEncoding.EncodeToString(nonce),
		Ciphertext: base64.StdEncoding.EncodeToString(ct),
	}, nil
}

// OpenWalletKey decrypts a keyfile. The iteration count RECORDED IN THE
// FILE is honored so a future floor raise still opens old files;
// SealWalletKey always writes the current constant.
func OpenWalletKey(kf *WalletKeyfile, passphrase string) (*WalletKey, error) {
	if kf.Version != 1 || kf.KDF != "pbkdf2-sha256" {
		return nil, fmt.Errorf("wallet: unsupported keyfile version %d / kdf %q", kf.Version, kf.KDF)
	}
	salt, err := base64.StdEncoding.DecodeString(kf.Salt)
	if err != nil {
		return nil, fmt.Errorf("wallet: keyfile salt: %w", err)
	}
	nonce, err := base64.StdEncoding.DecodeString(kf.Nonce)
	if err != nil {
		return nil, fmt.Errorf("wallet: keyfile nonce: %w", err)
	}
	ct, err := base64.StdEncoding.DecodeString(kf.Ciphertext)
	if err != nil {
		return nil, fmt.Errorf("wallet: keyfile ciphertext: %w", err)
	}
	block, err := aes.NewCipher(keyfileKDF(passphrase, salt, kf.Iterations))
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	plain, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return nil, errors.New("wallet: wrong passphrase or corrupted keyfile")
	}
	defer func() {
		for i := range plain {
			plain[i] = 0
		}
	}()
	if len(plain) != 32 {
		return nil, errors.New("wallet: keyfile holds an unexpected key length")
	}
	return &WalletKey{priv: secp256k1.PrivKeyFromBytes(plain)}, nil
}

// GenerateWalletKeyForTest returns a random key; test fixtures only.
func GenerateWalletKeyForTest() (*WalletKey, error) {
	priv, err := secp256k1.GeneratePrivateKey()
	if err != nil {
		return nil, err
	}
	return &WalletKey{priv: priv}, nil
}

// SignDigest produces the 64-byte r||s signature Cosmos SIGN_MODE_DIRECT
// expects over a 32-byte digest (the SHA-256 of the SignDoc).
//
// The signature is deterministic (RFC 6979), which is what makes a
// golden-bytes test of a whole transaction possible: the same key, the
// same document and the same sequence produce the same bytes forever.
// Low-S normalization is the library's, and is what the chain's
// verifier requires.
func (w *WalletKey) SignDigest(digest []byte) ([]byte, error) {
	if len(digest) != 32 {
		return nil, fmt.Errorf("wallet: digest must be 32 bytes, got %d", len(digest))
	}
	sig := ecdsa.SignCompact(w.priv, digest, true)
	// SignCompact returns [recovery byte || R || S]; the Cosmos wire form
	// is R||S with no recovery byte.
	if len(sig) != 65 {
		return nil, fmt.Errorf("wallet: unexpected signature length %d", len(sig))
	}
	out := make([]byte, 64)
	copy(out, sig[1:])
	return out, nil
}
