package main

// §7: the send journal and confirmation predicate (REL-15, REL-17),
// against a loopback httptest node that scripts each response.

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func journalTestSetup(t *testing.T, cfg nodeConfig) (dir string, node *fakeNode, env func(string) string) {
	t.Helper()
	node = newFakeNode(t, cfg)
	dir = walletScratchDir(t)
	env = envOf(map[string]string{walletPassphraseEnv: "p-test-1"})
	var out, errOut bytes.Buffer
	if code := cmdWallet([]string{"init", "-dir", dir, "-print-anyway"}, strings.NewReader(""), &out, &errOut, env); code != 0 {
		t.Fatalf("init: %s", errOut.String())
	}
	node.journalPath = filepath.Join(dir, pendingTxFile)
	return dir, node, env
}

func sendArgs(dir, nodeURL string, extra ...string) []string {
	args := []string{"send", "-dir", dir, "-node", nodeURL, "-chain-id", "twilight-devnet-2",
		"-to", "twilight1kl0dn0rtwk46h9zcmazyyrruta290crh93rnlh", "-amount", "1000", "-yes"}
	return append(args, extra...)
}

// §7.1: the journal is written before the broadcast request is observed
// by the node (ordering asserted via the stub recording the journal
// file's existence at request time), then confirmed, then removed.
func TestSendJournalsBeforeBroadcastObservesIt(t *testing.T) {
	dir, node, env := journalTestSetup(t, nodeConfig{
		chainID: "twilight-devnet-2", accountNumber: 5, sequence: 9, balance: "1000000", appearAfter: 1,
	})
	var out, errOut bytes.Buffer
	code := cmdWallet(sendArgs(dir, node.srv.URL), strings.NewReader(""), &out, &errOut, env)
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	node.mu.Lock()
	existed := node.journalExistedAtBroadcast
	node.mu.Unlock()
	if !existed {
		t.Fatal("broadcast was observed before the journal existed on disk")
	}
	if _, err := os.Stat(filepath.Join(dir, pendingTxFile)); !os.IsNotExist(err) {
		t.Fatal("journal should be removed once confirmed")
	}
}

// §7.2: node accepts and responds, but the RESPONSE to broadcast never
// proves it (simulated here via an empty hash, the concrete shape of
// "response dropped/garbled") — exit is the distinct unknown-outcome
// code, journal remains with the hash, stdout/stderr names the hash.
func TestSendKeepsJournalWhenBroadcastOutcomeIsUnknown(t *testing.T) {
	dir, node, env := journalTestSetup(t, nodeConfig{
		chainID: "twilight-devnet-2", accountNumber: 5, sequence: 9, balance: "1000000", emptyHash: true,
	})
	var out, errOut bytes.Buffer
	code := cmdWallet(sendArgs(dir, node.srv.URL), strings.NewReader(""), &out, &errOut, env)
	if code != exitOutcomeUnknown {
		t.Fatalf("exit %d, want exitOutcomeUnknown (%d): %s", code, exitOutcomeUnknown, errOut.String())
	}
	p, err := loadPendingTx(dir)
	if err != nil || p == nil {
		t.Fatalf("journal should remain: %v %v", p, err)
	}
	if !strings.Contains(errOut.String(), p.Hash) || !strings.Contains(errOut.String(), "outcome unknown") {
		t.Errorf("stderr should name the hash and say outcome unknown: %q", errOut.String())
	}
}

// §7.3: rerun after 2 (here: after an unknown-outcome send), node now
// reports the hash in a block — reported as confirmed, journal
// removed, no second broadcast.
func TestRerunAfterUnknownOutcomeConfirmsWithoutRebroadcasting(t *testing.T) {
	node := newFakeNode(t, nodeConfig{
		chainID: "twilight-devnet-2", accountNumber: 5, sequence: 9, balance: "1000000",
	})
	dir := walletScratchDir(t)
	env := envOf(map[string]string{walletPassphraseEnv: "p-test-1"})
	var out, errOut bytes.Buffer
	if code := cmdWallet([]string{"init", "-dir", dir, "-print-anyway"}, strings.NewReader(""), &out, &errOut, env); code != 0 {
		t.Fatalf("init: %s", errOut.String())
	}

	// Manufacture a journal directly — cleaner than forcing broadcast to
	// look like a lost response and then flipping the node's own
	// behavior mid-test.
	txRaw := []byte("fixed-test-tx-bytes-for-rerun")
	hash := txHash(txRaw)
	if err := writePendingTx(dir, pendingTx{
		Hash: hash, TxRawB64: b64(txRaw), To: "twilight1kl0dn0rtwk46h9zcmazyyrruta290crh93rnlh",
		Amount: "1000", Denom: "utwlt", ChainID: "twilight-devnet-2", CreatedAt: nowRFC3339(),
	}); err != nil {
		t.Fatal(err)
	}
	node.mu.Lock()
	node.lastTx = string(txRaw) // the node "already has" this tx from the lost-response attempt
	node.mu.Unlock()

	out.Reset()
	errOut.Reset()
	// balance is enough to trigger resolvePendingTx's opportunistic check.
	code := cmdWallet([]string{"balance", "-dir", dir, "-node", node.srv.URL, "-chain-id", "twilight-devnet-2"},
		strings.NewReader(""), &out, &errOut, env)
	if code != exitOK {
		t.Fatalf("balance exit %d: %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "confirmed") {
		t.Errorf("output should report the resolved journal as confirmed: %q", out.String())
	}
	if _, err := os.Stat(filepath.Join(dir, pendingTxFile)); !os.IsNotExist(err) {
		t.Fatal("journal should be removed once confirmed")
	}
	node.mu.Lock()
	bc := node.broadcastCount
	node.mu.Unlock()
	if bc != 0 {
		t.Fatalf("resolving an already-confirmed journal must not broadcast; broadcastCount=%d", bc)
	}
}

// §7.4: rerun after 2, node does not know the hash — the SAME tx_raw
// bytes are re-broadcast (stub compares bytes), never a fresh
// signature.
func TestRerunWhenNodeDoesNotKnowTheHashRebroadcastsSameBytes(t *testing.T) {
	node := newFakeNode(t, nodeConfig{
		chainID: "twilight-devnet-2", accountNumber: 5, sequence: 9, balance: "1000000",
	})
	dir := walletScratchDir(t)
	env := envOf(map[string]string{walletPassphraseEnv: "p-test-1"})
	var out, errOut bytes.Buffer
	if code := cmdWallet([]string{"init", "-dir", dir, "-print-anyway"}, strings.NewReader(""), &out, &errOut, env); code != 0 {
		t.Fatalf("init: %s", errOut.String())
	}

	txRaw := []byte("fixed-test-tx-bytes-not-yet-seen")
	hash := txHash(txRaw)
	if err := writePendingTx(dir, pendingTx{
		Hash: hash, TxRawB64: b64(txRaw), To: "twilight1kl0dn0rtwk46h9zcmazyyrruta290crh93rnlh",
		Amount: "1000", Denom: "utwlt", ChainID: "twilight-devnet-2", CreatedAt: nowRFC3339(),
	}); err != nil {
		t.Fatal(err)
	}
	// node has NOT seen this tx (lastTx unset, appearAfter=0 means /tx
	// always answers "not found" until a broadcast sets lastTx).

	out.Reset()
	errOut.Reset()
	code := cmdWallet([]string{"balance", "-dir", dir, "-node", node.srv.URL, "-chain-id", "twilight-devnet-2"},
		strings.NewReader(""), &out, &errOut, env)
	if code != exitOK {
		t.Fatalf("balance exit %d: %s", code, errOut.String())
	}
	node.mu.Lock()
	gotTx, bc := node.lastTx, node.broadcastCount
	node.mu.Unlock()
	if bc != 1 {
		t.Fatalf("expected exactly one re-broadcast, got %d", bc)
	}
	if gotTx != string(txRaw) {
		t.Fatalf("re-broadcast bytes = %q, want the original journaled bytes %q — a fresh signature would differ", gotTx, string(txRaw))
	}
	if _, err := os.Stat(filepath.Join(dir, pendingTxFile)); err != nil {
		t.Fatal("journal should remain — still unresolved")
	}
}

// §7.5: a pending journal exists and send is invoked for a new
// payment — refused until resolved.
func TestSendRefusesANewPaymentWhilePendingJournalExists(t *testing.T) {
	node := newFakeNode(t, nodeConfig{chainID: "twilight-devnet-2", accountNumber: 5, sequence: 9})
	dir := walletScratchDir(t)
	env := envOf(map[string]string{walletPassphraseEnv: "p-test-1"})
	var out, errOut bytes.Buffer
	if code := cmdWallet([]string{"init", "-dir", dir, "-print-anyway"}, strings.NewReader(""), &out, &errOut, env); code != 0 {
		t.Fatalf("init: %s", errOut.String())
	}
	txRaw := []byte("unresolved-tx")
	if err := writePendingTx(dir, pendingTx{
		Hash: txHash(txRaw), TxRawB64: b64(txRaw), To: "twilight1kl0dn0rtwk46h9zcmazyyrruta290crh93rnlh",
		Amount: "1", Denom: "utwlt", ChainID: "twilight-devnet-2", CreatedAt: nowRFC3339(),
	}); err != nil {
		t.Fatal(err)
	}

	out.Reset()
	errOut.Reset()
	code := cmdWallet(sendArgs(dir, node.srv.URL, "-to", "twilight1kl0dn0rtwk46h9zcmazyyrruta290crh93rnlh"),
		strings.NewReader(""), &out, &errOut, env)
	if code == exitOK {
		t.Fatal("a new send while a journal is pending must not silently succeed as a fresh payment")
	}
	node.mu.Lock()
	bc := node.broadcastCount
	node.mu.Unlock()
	// Only the pending-resolution's own re-broadcast (of the OLD bytes)
	// is allowed here — never a broadcast of a freshly-built payment.
	if bc > 1 {
		t.Fatalf("more than one broadcast happened; a new payment must never be constructed: %d", bc)
	}
}

// §7.6: -abandon-pending removes the journal and prints the hash.
func TestAbandonPendingRemovesTheJournalAndPrintsItsHash(t *testing.T) {
	dir := walletScratchDir(t)
	env := envOf(map[string]string{walletPassphraseEnv: "p-test-1"})
	var out, errOut bytes.Buffer
	if code := cmdWallet([]string{"init", "-dir", dir, "-print-anyway"}, strings.NewReader(""), &out, &errOut, env); code != 0 {
		t.Fatalf("init: %s", errOut.String())
	}
	txRaw := []byte("to-be-abandoned")
	hash := txHash(txRaw)
	if err := writePendingTx(dir, pendingTx{
		Hash: hash, TxRawB64: b64(txRaw), To: "x", Amount: "1", Denom: "utwlt",
		ChainID: "twilight-devnet-2", CreatedAt: nowRFC3339(),
	}); err != nil {
		t.Fatal(err)
	}

	out.Reset()
	errOut.Reset()
	code := cmdWallet([]string{"send", "-dir", dir, "-abandon-pending"}, strings.NewReader(""), &out, &errOut, env)
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), hash) {
		t.Errorf("output should print the abandoned hash: %q", out.String())
	}
	if _, err := os.Stat(filepath.Join(dir, pendingTxFile)); !os.IsNotExist(err) {
		t.Fatal("journal should be gone")
	}
}

// §7.7: broadcast response with an empty hash, or a hash that does not
// match — unknown outcome, journal kept. (Empty is covered by
// TestSendKeepsJournalWhenBroadcastOutcomeIsUnknown above; this covers
// the mismatched-hash half.)
func TestSendKeepsJournalWhenBroadcastHashDoesNotMatch(t *testing.T) {
	dir, node, env := journalTestSetup(t, nodeConfig{
		chainID: "twilight-devnet-2", accountNumber: 5, sequence: 9, balance: "1000000", wrongHash: true,
	})
	var out, errOut bytes.Buffer
	code := cmdWallet(sendArgs(dir, node.srv.URL), strings.NewReader(""), &out, &errOut, env)
	if code != exitOutcomeUnknown {
		t.Fatalf("exit %d, want exitOutcomeUnknown: %s", code, errOut.String())
	}
	if p, err := loadPendingTx(dir); err != nil || p == nil {
		t.Fatalf("journal should remain: %v %v", p, err)
	}
}

// §7.8: waitForTx response with empty height, zero height, missing
// code, or a mismatched hash — none of them confirmed.
func TestWaitForTxTreatsIncompleteResponsesAsNotYet(t *testing.T) {
	cases := []struct {
		name string
		cfg  func(cfg *nodeConfig)
	}{
		{"empty height", func(cfg *nodeConfig) { cfg.emptyHeight = true }},
		{"zero height", func(cfg *nodeConfig) { cfg.zeroHeight = true }},
		{"missing code", func(cfg *nodeConfig) { cfg.omitDeliverCode = true }},
		{"mismatched hash", func(cfg *nodeConfig) { cfg.wrongTxHash = true }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := nodeConfig{chainID: "twilight-devnet-2", accountNumber: 5, sequence: 9, balance: "1000000", appearAfter: 0}
			c.cfg(&cfg)
			node := newFakeNode(t, cfg)

			ctx, cancel := signalContext()
			defer cancel()
			c := newRPCClient(node.srv.URL)
			_, _, confirmed, err := c.queryTxOnce(ctx, "AAAA0000AAAA0000AAAA0000AAAA0000AAAA0000AAAA0000AAAA0000AAAA0000")
			if err != nil {
				t.Fatalf("queryTxOnce: %v", err)
			}
			if confirmed {
				t.Fatal("an incomplete response must not read as confirmed")
			}
		})
	}
}

// §7.9: node chain id differs from the configured one — refused before
// signing, nothing journaled.
func TestSendRefusesWhenNodeChainIDDiffersBeforeSigning(t *testing.T) {
	node := newFakeNode(t, nodeConfig{chainID: "some-other-chain", accountNumber: 5, sequence: 9, balance: "1000000"})
	dir := walletScratchDir(t)
	env := envOf(map[string]string{walletPassphraseEnv: "p-test-1"})
	var out, errOut bytes.Buffer
	if code := cmdWallet([]string{"init", "-dir", dir, "-print-anyway"}, strings.NewReader(""), &out, &errOut, env); code != 0 {
		t.Fatalf("init: %s", errOut.String())
	}
	out.Reset()
	errOut.Reset()
	code := cmdWallet(sendArgs(dir, node.srv.URL), strings.NewReader(""), &out, &errOut, env)
	if code == exitOK {
		t.Fatal("a chain-id mismatch must refuse")
	}
	if !strings.Contains(errOut.String(), "twilight-devnet-2") || !strings.Contains(errOut.String(), "some-other-chain") {
		t.Errorf("stderr should name both chain ids: %q", errOut.String())
	}
	if _, err := os.Stat(filepath.Join(dir, pendingTxFile)); !os.IsNotExist(err) {
		t.Fatal("nothing should be journaled when the chain id check refuses before signing")
	}
	node.mu.Lock()
	bc := node.broadcastCount
	node.mu.Unlock()
	if bc != 0 {
		t.Fatalf("nothing should ever be broadcast: %d", bc)
	}
}

// §7.10: node URL plain http non-loopback without -insecure-node is
// refused; with the flag it proceeds and warns.
func TestSendRefusesPlainHTTPNonLoopbackWithoutInsecureFlag(t *testing.T) {
	dir := walletScratchDir(t)
	env := envOf(map[string]string{walletPassphraseEnv: "p-test-1"})
	var out, errOut bytes.Buffer
	if code := cmdWallet([]string{"init", "-dir", dir, "-print-anyway"}, strings.NewReader(""), &out, &errOut, env); code != 0 {
		t.Fatalf("init: %s", errOut.String())
	}

	out.Reset()
	errOut.Reset()
	code := cmdWallet(sendArgs(dir, "http://203.0.113.5:26657"), strings.NewReader(""), &out, &errOut, env)
	if code == exitOK {
		t.Fatal("a plain http non-loopback node must be refused by default")
	}
	if !strings.Contains(errOut.String(), "insecure-node") {
		t.Errorf("stderr should point at -insecure-node: %q", errOut.String())
	}
}

func TestValidateNodeURLAcceptsPlainHTTPNonLoopbackWithInsecureFlag(t *testing.T) {
	var stderr bytes.Buffer
	if err := validateNodeURL("http://203.0.113.5:26657", true, &stderr); err != nil {
		t.Fatalf("with -insecure-node this should proceed: %v", err)
	}
	if !strings.Contains(stderr.String(), "insecure-node") {
		t.Errorf("stderr should warn: %q", stderr.String())
	}
}

// §7.11: node rejects with code != 0 — journal removed, code and log
// reported.
func TestSendRejectionRemovesTheJournal(t *testing.T) {
	dir, node, env := journalTestSetup(t, nodeConfig{
		chainID: "twilight-devnet-2", accountNumber: 5, sequence: 9,
		broadcastCode: 5, broadcastLog: "insufficient funds",
	})
	var out, errOut bytes.Buffer
	code := cmdWallet(sendArgs(dir, node.srv.URL), strings.NewReader(""), &out, &errOut, env)
	if code != exitChainRejected {
		t.Fatalf("exit %d, want exitChainRejected: %s", code, errOut.String())
	}
	if !strings.Contains(errOut.String(), "insufficient funds") {
		t.Errorf("stderr should carry the chain's reason: %q", errOut.String())
	}
	if _, err := os.Stat(filepath.Join(dir, pendingTxFile)); !os.IsNotExist(err) {
		t.Fatal("a rejected transaction's journal should be removed — nothing left to resolve")
	}
}
