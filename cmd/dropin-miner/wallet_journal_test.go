package main

// The send journal and confirmation predicate, against a loopback
// httptest node that scripts each response.

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
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

// TestRerunWithInsufficientEvidenceDoesNotRebroadcast: the node HAS a
// response about this hash (found=true — not the "not found" case
// TestRerunWhenNodeDoesNotKnowTheHashRebroadcastsSameBytes covers), but
// it falls short of the confirmation predicate (here: no tx_result.code
// at all). That is not the same fact as "the node has never heard of
// this," and must not be treated as license to re-send: the journal
// stays, nothing is rebroadcast, and the report says the evidence is
// incomplete rather than claiming either outcome.
func TestRerunWithInsufficientEvidenceDoesNotRebroadcast(t *testing.T) {
	node := newFakeNode(t, nodeConfig{
		chainID: "twilight-devnet-2", accountNumber: 5, sequence: 9, balance: "1000000",
		appearAfter: 0, omitDeliverCode: true,
	})
	dir := walletScratchDir(t)
	env := envOf(map[string]string{walletPassphraseEnv: "p-test-1"})
	var out, errOut bytes.Buffer
	if code := cmdWallet([]string{"init", "-dir", dir, "-print-anyway"}, strings.NewReader(""), &out, &errOut, env); code != 0 {
		t.Fatalf("init: %s", errOut.String())
	}

	txRaw := []byte("fixed-test-tx-bytes-partial-evidence")
	hash := txHash(txRaw)
	if err := writePendingTx(dir, pendingTx{
		Hash: hash, TxRawB64: b64(txRaw), To: "twilight1kl0dn0rtwk46h9zcmazyyrruta290crh93rnlh",
		Amount: "1000", Denom: "utwlt", ChainID: "twilight-devnet-2", CreatedAt: nowRFC3339(),
	}); err != nil {
		t.Fatal(err)
	}
	node.mu.Lock()
	node.lastTx = string(txRaw) // the node has SEEN it — /tx will answer, just not with a code
	node.mu.Unlock()

	out.Reset()
	errOut.Reset()
	code := cmdWallet([]string{"balance", "-dir", dir, "-node", node.srv.URL, "-chain-id", "twilight-devnet-2"},
		strings.NewReader(""), &out, &errOut, env)
	if code != exitOK {
		t.Fatalf("balance exit %d: %s", code, errOut.String())
	}
	node.mu.Lock()
	bc := node.broadcastCount
	node.mu.Unlock()
	if bc != 0 {
		t.Fatalf("insufficient evidence must not trigger a rebroadcast; broadcastCount=%d", bc)
	}
	if _, err := os.Stat(filepath.Join(dir, pendingTxFile)); err != nil {
		t.Fatal("journal should remain — still unresolved")
	}
	if !strings.Contains(out.String(), "does not yet prove inclusion") {
		t.Errorf("output should say the evidence is incomplete, not claim an outcome: %q", out.String())
	}
}

// TestRerunRebroadcastRejectionResolvesTheJournal: the node has never
// heard of the hash (found=false), so a re-broadcast of the same bytes
// is attempted, exactly as
// TestRerunWhenNodeDoesNotKnowTheHashRebroadcastsSameBytes — but this
// time the node's own response to THAT re-broadcast is an explicit
// rejection (code != 0). That is proof, not silence: the journal is
// resolved (removed) and the code and log are reported, the same fact
// walletSend's own first-broadcast rejection reports, reached this time
// on a re-send.
func TestRerunRebroadcastRejectionResolvesTheJournal(t *testing.T) {
	node := newFakeNode(t, nodeConfig{
		chainID: "twilight-devnet-2", accountNumber: 5, sequence: 9, balance: "1000000",
		broadcastCode: 7, broadcastLog: "insufficient funds on re-check",
	})
	dir := walletScratchDir(t)
	env := envOf(map[string]string{walletPassphraseEnv: "p-test-1"})
	var out, errOut bytes.Buffer
	if code := cmdWallet([]string{"init", "-dir", dir, "-print-anyway"}, strings.NewReader(""), &out, &errOut, env); code != 0 {
		t.Fatalf("init: %s", errOut.String())
	}

	txRaw := []byte("fixed-test-tx-bytes-rejected-on-rebroadcast")
	hash := txHash(txRaw)
	if err := writePendingTx(dir, pendingTx{
		Hash: hash, TxRawB64: b64(txRaw), To: "twilight1kl0dn0rtwk46h9zcmazyyrruta290crh93rnlh",
		Amount: "1000", Denom: "utwlt", ChainID: "twilight-devnet-2", CreatedAt: nowRFC3339(),
	}); err != nil {
		t.Fatal(err)
	}
	// node has NOT seen this tx: /tx answers "not found" until the
	// re-broadcast below sets lastTx, so resolvePendingTx takes the
	// found=false branch and re-sends.

	out.Reset()
	errOut.Reset()
	code := cmdWallet([]string{"balance", "-dir", dir, "-node", node.srv.URL, "-chain-id", "twilight-devnet-2"},
		strings.NewReader(""), &out, &errOut, env)
	if code != exitOK {
		t.Fatalf("balance exit %d: %s", code, errOut.String())
	}
	node.mu.Lock()
	bc := node.broadcastCount
	node.mu.Unlock()
	if bc != 1 {
		t.Fatalf("expected exactly one re-broadcast, got %d", bc)
	}
	if _, err := os.Stat(filepath.Join(dir, pendingTxFile)); !os.IsNotExist(err) {
		t.Fatal("journal should be removed once the re-broadcast is explicitly rejected")
	}
	if !strings.Contains(out.String(), "rejected on re-broadcast") || !strings.Contains(out.String(), "7") ||
		!strings.Contains(out.String(), "insufficient funds on re-check") {
		t.Errorf("output should report the rejection code and log: %q", out.String())
	}
}

// TestConcurrentSendsJournalExactlyOnePayment is the send-side analog
// of TestWalletCreationIsExclusiveAcrossConcurrentCallers
// (wallet_lock_test.go): wallet.lock now serializes wallet send's
// pending-check-through-journal-write span (wallet.go), so two sends
// racing for the same wallet directory must never both commit to
// constructing a brand new payment. Lock ordering makes this
// deterministic, not merely probable: whichever call acquires the lock
// second cannot observe "no journal yet," because the first call's
// journal write strictly happens-before its own lock release, which
// strictly happens-before the second call's acquire — so the second
// call is guaranteed to find the first's journal and resolve or wait on
// it, never build its own.
//
// Counted via beforeJournalingANewPayment rather than broadcastCount:
// resolving a pending journal can legitimately re-broadcast the SAME
// bytes (TestRerunWhenNodeDoesNotKnowTheHashRebroadcastsSameBytes), and
// that must not be mistaken for a second, independent payment. Because
// only one payment is ever journaled, there is only one journal for
// EITHER call to act on — whatever the second call's own resolution
// attempt does with it (confirm it, find it still unresolved, even see
// it rejected on re-broadcast) is an outcome for that ONE journal, never
// a stray removal of some other, second one, because a second one never
// existed.
func TestConcurrentSendsJournalExactlyOnePayment(t *testing.T) {
	// emptyHash blanks only the broadcast_tx_sync RESPONSE's hash, not
	// what the fake node actually learned — so appearAfter must ALSO
	// stay high, or a resolver's own /tx query genuinely confirms the
	// winner's transaction (the node really did see it) and removes the
	// journal, legitimately opening the door for the next contender to
	// build its own fresh payment. Both together keep the journal
	// unresolved for the whole test: no contender, whenever it actually
	// gets its turn at the lock, can ever mistake an empty journal
	// directory for "go ahead, build a new payment."
	node := newFakeNode(t, nodeConfig{
		chainID: "twilight-devnet-2", accountNumber: 5, sequence: 9, balance: "1000000",
		emptyHash: true, appearAfter: 1000,
	})
	dir := walletScratchDir(t)
	env := envOf(map[string]string{walletPassphraseEnv: "p-test-1"})
	var out, errOut bytes.Buffer
	if code := cmdWallet([]string{"init", "-dir", dir, "-print-anyway"}, strings.NewReader(""), &out, &errOut, env); code != 0 {
		t.Fatalf("init: %s", errOut.String())
	}

	var newPayments int32
	orig := beforeJournalingANewPayment
	beforeJournalingANewPayment = func() { atomic.AddInt32(&newPayments, 1) }
	t.Cleanup(func() { beforeJournalingANewPayment = orig })

	const n = 4
	var wg sync.WaitGroup
	start := make(chan struct{})
	codes := make([]int, n)
	for i := 0; i < n; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			var o, e bytes.Buffer
			codes[i] = cmdWallet(sendArgs(dir, node.srv.URL), strings.NewReader(""), &o, &e, env)
		}()
	}
	close(start)
	wg.Wait()

	if got := atomic.LoadInt32(&newPayments); got != 1 {
		t.Fatalf("exactly one send should have journaled a NEW payment, got %d", got)
	}
	for i, code := range codes {
		if code != exitOK && code != exitOutcomeUnknown {
			t.Errorf("call %d exited %d, want exitOK or exitOutcomeUnknown", i, code)
		}
	}
	// Whatever state the journal ends in (resolved and gone, or still
	// present because nothing yet proved an outcome), it must still be
	// coherent — not partially written, not corrupt from two callers
	// racing a write.
	if _, err := loadPendingTx(dir); err != nil {
		t.Fatalf("journal, if present, must still be readable and well-formed: %v", err)
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
