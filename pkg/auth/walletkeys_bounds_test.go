package auth

// OpenWalletKey validates every field before any crypto touches
// it. Before this file's guard existed, a wrong-length nonce panicked
// inside gcm.Open and an out-of-range Iterations spun the KDF with no
// ceiling — this table proves both are now bounded errors, in bounded
// time, naming the field, never a panic.

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"testing"
	"time"
)

// recoverPanic turns a panic inside fn into a test failure with a
// message, rather than a crashed test binary — the mechanism §6 asks
// for ("use a deferred recover in the test to turn a panic into a
// failure").
func recoverPanic(t *testing.T, fn func()) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("OpenWalletKey panicked: %v", r)
		}
	}()
	fn()
}

func validKeyfileForBoundsTest(t *testing.T) (*WalletKeyfile, string) {
	t.Helper()
	key, err := GenerateWalletKeyForTest()
	if err != nil {
		t.Fatal(err)
	}
	kf, err := SealWalletKey(key, "bounds-test-passphrase")
	if err != nil {
		t.Fatal(err)
	}
	return kf, "bounds-test-passphrase"
}

func TestOpenWalletKeyBoundsEveryFieldBeforeAnyCrypto(t *testing.T) {
	base, passphrase := validKeyfileForBoundsTest(t)

	cases := []struct {
		name   string
		mutate func(kf *WalletKeyfile)
	}{
		{"nonce too short", func(kf *WalletKeyfile) { kf.Nonce = base64.StdEncoding.EncodeToString(make([]byte, 4)) }},
		{"nonce too long", func(kf *WalletKeyfile) { kf.Nonce = base64.StdEncoding.EncodeToString(make([]byte, 64)) }},
		{"salt empty", func(kf *WalletKeyfile) { kf.Salt = "" }},
		{"salt oversized", func(kf *WalletKeyfile) { kf.Salt = base64.StdEncoding.EncodeToString(make([]byte, 4096)) }},
		{"ciphertext shorter than the tag", func(kf *WalletKeyfile) {
			kf.Ciphertext = base64.StdEncoding.EncodeToString(make([]byte, 4))
		}},
		{"ciphertext oversized", func(kf *WalletKeyfile) {
			kf.Ciphertext = base64.StdEncoding.EncodeToString(make([]byte, 10_000_000))
		}},
		{"iterations zero", func(kf *WalletKeyfile) { kf.Iterations = 0 }},
		{"iterations negative", func(kf *WalletKeyfile) { kf.Iterations = -1 }},
		{"iterations below floor", func(kf *WalletKeyfile) { kf.Iterations = WalletKeyfileIterations - 1 }},
		{"iterations above ceiling", func(kf *WalletKeyfile) { kf.Iterations = keyfileIterationsCeiling + 1 }},
		{"wrong version", func(kf *WalletKeyfile) { kf.Version = 2 }},
		{"wrong kdf name", func(kf *WalletKeyfile) { kf.KDF = "argon2id" }},
		{"invalid base64 salt", func(kf *WalletKeyfile) { kf.Salt = "not base64!!" }},
		{"invalid base64 nonce", func(kf *WalletKeyfile) { kf.Nonce = "not base64!!" }},
		{"invalid base64 ciphertext", func(kf *WalletKeyfile) { kf.Ciphertext = "not base64!!" }},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			kf := *base
			c.mutate(&kf)

			done := make(chan struct{})
			var openErr error
			go func() {
				defer close(done)
				recoverPanic(t, func() {
					_, openErr = OpenWalletKey(&kf, passphrase)
				})
			}()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("OpenWalletKey did not return within 5s — an unbounded KDF or similar is not refusing in bounded time")
			}
			if openErr == nil {
				t.Fatal("expected a bounded error, got nil (the mutated field was accepted)")
			}
		})
	}
}

// The huge-iterations case at ~5x the ceiling, timed on its own: proves
// the ceiling check fires BEFORE the KDF runs, not that the KDF merely
// finishes quickly enough by chance. A version without the ceiling
// check would still eventually return (PBKDF2 terminates), just slowly
// and only after doing the expensive work — this asserts near-instant
// refusal, which only the bounds check produces.
func TestOpenWalletKeyRefusesAboveTheCeilingWithoutRunningTheKDF(t *testing.T) {
	base, passphrase := validKeyfileForBoundsTest(t)
	kf := *base
	kf.Iterations = keyfileIterationsCeiling * 10

	start := time.Now()
	if _, err := OpenWalletKey(&kf, passphrase); err == nil {
		t.Fatal("expected a bounded error")
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("OpenWalletKey took %s to refuse an out-of-range iterations count — the ceiling check is not running before the KDF", elapsed)
	}
}

// §6: one test decrypts a keyfile produced by the CURRENT SealWalletKey.
func TestOpenWalletKeyDecryptsAFreshlySealedKeyfile(t *testing.T) {
	kf, passphrase := validKeyfileForBoundsTest(t)
	if _, err := OpenWalletKey(kf, passphrase); err != nil {
		t.Fatalf("a keyfile this same code just sealed did not decrypt: %v", err)
	}
}

// §6: one test decrypts a COMMITTED fixture produced before this PR —
// pkg/auth/testdata/wallet_keyfile_fixture.json, sealed under
// SealWalletKey before the bounds check existed, so this pins that the
// new floor/ceiling never reject a file the unmodified code already
// produced. Regenerating this fixture defeats its own purpose.
func TestOpenWalletKeyDecryptsTheCommittedPreExistingFixture(t *testing.T) {
	raw, err := os.ReadFile("testdata/wallet_keyfile_fixture.json")
	if err != nil {
		t.Fatal(err)
	}
	var kf WalletKeyfile
	if err := json.Unmarshal(raw, &kf); err != nil {
		t.Fatal(err)
	}
	key, err := OpenWalletKey(&kf, "fixture-passphrase")
	if err != nil {
		t.Fatalf("the pre-existing fixture no longer decrypts: %v", err)
	}
	addr, err := key.Address(TwilightHRP)
	if err != nil {
		t.Fatal(err)
	}
	const wantAddr = "twilight1xjwyyyy4s4rjxq7uv7j90vnqgecgwj0lwu9mkz"
	if addr != wantAddr {
		t.Fatalf("fixture decrypted to %q, want %q — the fixture or the derivation changed", addr, wantAddr)
	}
}
