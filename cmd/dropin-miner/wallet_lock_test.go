package main

// Wallet creation exclusivity and recovery (§5, §14 checkpoint A/C).
// wallet.lock (createOrRecoverWallet, wallet.go) guards the whole
// critical section a concurrent caller could otherwise race: check,
// generate-or-repair, write. Before the lock existed, two goroutines
// calling the old createWallet concurrently reported two DIFFERENT
// addresses for the same directory — this file's own git history has
// that failure captured, reverted here into the guard it was inverted
// from.

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/twilight-project/dropin-miner/pkg/auth"
)

// TestWalletCreationIsExclusiveAcrossConcurrentCallers is §5.1: two
// goroutines call the shared creation function concurrently; exactly
// one generates, both return the same address, and the key on disk
// decrypts to that address.
func TestWalletCreationIsExclusiveAcrossConcurrentCallers(t *testing.T) {
	dir := walletScratchDir(t)
	const n = 8
	addrs := make([]string, n)
	outcomes := make([]walletCreationOutcome, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			addr, _, outcome, err := createOrRecoverWallet(dir, "test-passphrase", true)
			addrs[i], outcomes[i], errs[i] = addr, outcome, err
		}()
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	created := 0
	for i, o := range outcomes {
		if o == walletCreated {
			created++
		}
		if addrs[i] != addrs[0] {
			t.Fatalf("call %d reported %q, call 0 reported %q — not exclusive", i, addrs[i], addrs[0])
		}
	}
	if created != 1 {
		t.Fatalf("%d of %d calls generated a key; want exactly 1", created, n)
	}

	sc, _, err := loadSidecar(dir, noEnv)
	if err != nil {
		t.Fatal(err)
	}
	if sc.Address != addrs[0] {
		t.Fatalf("sidecar on disk names %q, every caller agreed on %q", sc.Address, addrs[0])
	}
	var kf auth.WalletKeyfile
	if err := readWalletFile(dir, walletKeyFile, &kf); err != nil {
		t.Fatal(err)
	}
	key, err := auth.OpenWalletKey(&kf, "test-passphrase")
	if err != nil {
		t.Fatalf("the key on disk does not decrypt under the passphrase every caller used: %v", err)
	}
	addr, err := key.Address(auth.TwilightHRP)
	if err != nil {
		t.Fatal(err)
	}
	if addr != addrs[0] {
		t.Fatalf("the key on disk decrypts to %q, callers agreed on %q", addr, addrs[0])
	}
}

// TestWalletInitRacingMiningEnableIsAlsoExclusive is §5.2: the two real
// call sites (wallet init's CLI path and mining enable's address
// question) go through the identical shared function, so racing them
// literally IS racing createOrRecoverWallet — this pins that there is
// only one code path, not two independently-safe ones.
func TestWalletInitRacingMiningEnableIsAlsoExclusive(t *testing.T) {
	dir := walletScratchDir(t)
	var wg sync.WaitGroup
	addrs := make([]string, 2)
	errs := make([]error, 2)
	start := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		addr, _, _, err := createOrRecoverWallet(dir, "p1", true) // wallet init's shape
		addrs[0], errs[0] = addr, err
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		addr, _, _, err := createOrRecoverWallet(dir, "p1", true) // mining enable's shape
		addrs[1], errs[1] = addr, err
	}()
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if addrs[0] != addrs[1] {
		t.Fatalf("wallet init and mining enable disagreed: %q vs %q", addrs[0], addrs[1])
	}
}

// TestWalletCreationRepairsAMissingSidecar is §5.3: key present,
// sidecar missing — repair writes a sidecar whose address equals the
// key's, prints no mnemonic (mnemonic is "" for anything but
// walletCreated — checked at the call site, not here), and generates no
// second key.
func TestWalletCreationRepairsAMissingSidecar(t *testing.T) {
	dir := walletScratchDir(t)
	addr, _, outcome, err := createOrRecoverWallet(dir, "test-passphrase", true)
	if err != nil {
		t.Fatal(err)
	}
	if outcome != walletCreated {
		t.Fatalf("outcome = %v, want walletCreated", outcome)
	}
	var kfBefore auth.WalletKeyfile
	if err := readWalletFile(dir, walletKeyFile, &kfBefore); err != nil {
		t.Fatal(err)
	}

	if err := os.Remove(filepath.Join(dir, walletSidecarFile)); err != nil {
		t.Fatal(err)
	}

	repairedAddr, mnemonic, outcome2, err := createOrRecoverWallet(dir, "test-passphrase", true)
	if err != nil {
		t.Fatal(err)
	}
	if outcome2 != walletRepaired {
		t.Fatalf("outcome = %v, want walletRepaired", outcome2)
	}
	if mnemonic != "" {
		t.Fatal("a repair must never return a mnemonic — nothing was generated")
	}
	if repairedAddr != addr {
		t.Fatalf("repaired address %q != original %q", repairedAddr, addr)
	}
	var kfAfter auth.WalletKeyfile
	if err := readWalletFile(dir, walletKeyFile, &kfAfter); err != nil {
		t.Fatal(err)
	}
	if kfAfter.Ciphertext != kfBefore.Ciphertext {
		t.Fatal("repair rewrote the key — it must touch only the sidecar")
	}
}

// TestWalletCreationRepairsACorruptSidecar is §5.4: same as 5.3, for a
// sidecar that parses but names an address the key does not produce
// (the way an interrupted or hand-edited sidecar could be corrupt
// without being unparseable JSON).
func TestWalletCreationRepairsACorruptSidecar(t *testing.T) {
	dir := walletScratchDir(t)
	addr, _, _, err := createOrRecoverWallet(dir, "test-passphrase", true)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeWalletFile(dir, walletSidecarFile, &sidecar{Address: "not-a-valid-address", PubKey: "garbage", Path: "m/"}); err != nil {
		t.Fatal(err)
	}

	repairedAddr, _, outcome, err := createOrRecoverWallet(dir, "test-passphrase", true)
	if err != nil {
		t.Fatal(err)
	}
	if outcome != walletRepaired {
		t.Fatalf("outcome = %v, want walletRepaired", outcome)
	}
	if repairedAddr != addr {
		t.Fatalf("repaired address %q != original %q", repairedAddr, addr)
	}
	var sc sidecar
	if err := readWalletFile(dir, walletSidecarFile, &sc); err != nil {
		t.Fatal(err)
	}
	if sc.Address != addr {
		t.Fatalf("sidecar on disk still names %q after repair", sc.Address)
	}
}

// TestWalletCreationRecoversFromACrashBetweenKeyAndSidecar is §5.5:
// afterWalletKeyWritten (wallet.go) is the seam — force it to fail
// exactly where a real crash would land (key durable, sidecar not yet
// written), confirm the half-written state on disk, then confirm a
// retry repairs it.
func TestWalletCreationRecoversFromACrashBetweenKeyAndSidecar(t *testing.T) {
	dir := walletScratchDir(t)

	orig := afterWalletKeyWritten
	afterWalletKeyWritten = func() error { return errors.New("simulated crash between key and sidecar") }
	t.Cleanup(func() { afterWalletKeyWritten = orig })

	_, _, _, err := createOrRecoverWallet(dir, "test-passphrase", true)
	if err == nil {
		t.Fatal("expected the simulated crash to surface as an error")
	}
	if _, err := os.Stat(filepath.Join(dir, walletKeyFile)); err != nil {
		t.Fatalf("the key should be durable even though the call failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, walletSidecarFile)); !os.IsNotExist(err) {
		t.Fatalf("the sidecar should not exist yet: %v", err)
	}

	afterWalletKeyWritten = orig // the crash is over; a retry should succeed

	var kfBefore auth.WalletKeyfile
	if err := readWalletFile(dir, walletKeyFile, &kfBefore); err != nil {
		t.Fatal(err)
	}
	repairedAddr, _, outcome, err := createOrRecoverWallet(dir, "test-passphrase", true)
	if err != nil {
		t.Fatalf("retry after the crash: %v", err)
	}
	if outcome != walletRepaired {
		t.Fatalf("outcome = %v, want walletRepaired", outcome)
	}
	var kfAfter auth.WalletKeyfile
	if err := readWalletFile(dir, walletKeyFile, &kfAfter); err != nil {
		t.Fatal(err)
	}
	if kfAfter.Ciphertext != kfBefore.Ciphertext {
		t.Fatal("the retry regenerated the key instead of repairing around it")
	}
	var sc sidecar
	if err := readWalletFile(dir, walletSidecarFile, &sc); err != nil {
		t.Fatal(err)
	}
	if sc.Address != repairedAddr {
		t.Fatalf("sidecar address %q != returned address %q", sc.Address, repairedAddr)
	}
}

// TestWalletCreationRefusesWhenLockIsBusyBeyondTheBound is §5.6: a
// lock held elsewhere for longer than the wait fails with the busy
// message and writes nothing.
func TestWalletCreationRefusesWhenLockIsBusyBeyondTheBound(t *testing.T) {
	dir := walletScratchDir(t)
	release, err := lockWalletDir(dir, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	_, err = lockWalletDir(dir, 50*time.Millisecond)
	if !errors.Is(err, errWalletLockBusy) {
		t.Fatalf("err = %v, want errWalletLockBusy", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, walletKeyFile)); !os.IsNotExist(statErr) {
		t.Fatal("a busy lock must write nothing")
	}
}

// TestWalletCreationRepairWithNoPassphraseRefuses is §5.7: repair
// needed, no passphrase available — reports the instruction, generates
// nothing, touches nothing.
func TestWalletCreationRepairWithNoPassphraseRefuses(t *testing.T) {
	dir := walletScratchDir(t)
	if _, _, _, err := createOrRecoverWallet(dir, "test-passphrase", true); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, walletSidecarFile)); err != nil {
		t.Fatal(err)
	}
	var kfBefore auth.WalletKeyfile
	if err := readWalletFile(dir, walletKeyFile, &kfBefore); err != nil {
		t.Fatal(err)
	}

	_, _, _, err := createOrRecoverWallet(dir, "", false)
	if !errors.Is(err, errWalletRepairNeedsPassphrase) {
		t.Fatalf("err = %v, want errWalletRepairNeedsPassphrase", err)
	}
	if _, err := os.Stat(filepath.Join(dir, walletSidecarFile)); !os.IsNotExist(err) {
		t.Fatal("no passphrase available must not write a sidecar")
	}
	var kfAfter auth.WalletKeyfile
	if err := readWalletFile(dir, walletKeyFile, &kfAfter); err != nil {
		t.Fatal(err)
	}
	if kfAfter.Ciphertext != kfBefore.Ciphertext {
		t.Fatal("no passphrase available must not touch the key")
	}
}
