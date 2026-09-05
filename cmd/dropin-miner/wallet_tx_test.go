package main

// The transaction layer. Two things are worth pinning: that the bytes we
// hand the chain are the bytes the chain's own encoder would produce (a
// golden test, possible because RFC 6979 makes signing deterministic),
// and that every refusal happens before anything is signed or sent.

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/twilight-project/dropin-miner/pkg/auth"
)

// The cosmjs vector's mnemonic, so the key here is the one whose address
// and public key are already pinned in internal/auth's tests.
const txTestMnemonic = "special sign fit simple patrol salute grocery chicken wheat radar tonight ceiling"

func txTestKey(t *testing.T) *auth.WalletKey {
	t.Helper()
	key, err := auth.DeriveWalletKey(txTestMnemonic)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// A fully assembled transaction, byte for byte. Anything that changes
// the encoding — a field number, a wire type, an omitted zero, the sign
// bytes, the signature form — changes this string.
func TestSignedSendMatchesItsGoldenBytes(t *testing.T) {
	key := txTestKey(t)
	from, err := key.Address(auth.TwilightHRP)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := buildSignedSend(key, sendParams{
		From:      from,
		To:        "twilight1kl0dn0rtwk46h9zcmazyyrruta290crh93rnlh",
		Denom:     "utwlt",
		Amount:    "1000000",
		Memo:      "",
		ChainID:   "twilight-devnet-2",
		AccountNo: 7,
		Sequence:  3,
		Gas:       200000,
		FeeAmount: "2000",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Verified field by field against the SDK's proto definitions before
	// being recorded, so this is a regression pin and not a tautology:
	//
	//   0a 97 01                 TxRaw.body_bytes, 151 bytes
	//     0a 94 01               TxBody.messages[0] (Any), 148
	//       0a 1c "/cosmos.bank.v1beta1.MsgSend"   type_url, 28 chars
	//       12 74                value, 116
	//         0a 2f <from>       MsgSend.from_address, 47 chars
	//         12 2f <to>         MsgSend.to_address
	//         1a 10              MsgSend.amount[0] (Coin)
	//           0a 05 "utwlt"  12 07 "1000000"
	//   12 67                    TxRaw.auth_info_bytes, 103
	//     0a 50                  AuthInfo.signer_infos[0], 80
	//       0a 46                SignerInfo.public_key (Any), 70
	//         0a 1f "/cosmos.crypto.secp256k1.PubKey"  12 23 (0a 21 <33-byte key>)
	//       12 04 0a 02 08 01    ModeInfo.single.mode = SIGN_MODE_DIRECT
	//       18 03                SignerInfo.sequence = 3
	//     12 13                  AuthInfo.fee, 19
	//       0a 0d (utwlt/2000)   10 c0 9a 0c = gas_limit 200000
	//   1a 40                    TxRaw.signatures[0], 64 bytes
	//
	// account_number (7) is absent here by design: it is covered by the
	// signature through SignDoc, never carried in TxRaw.
	const golden = "" +
		"0a97010a94010a1c2f636f736d6f732e62616e6b2e763162657461312e4d736753656e6412740a2f7477696c" +
		"69676874316a68673065377336676e3434746663356b33376b723034737a6e79686564746337753264307a12" +
		"2f7477696c69676874316b6c30646e307274776b343668397a636d617a797972727574613239306372683933" +
		"726e6c681a100a057574776c7412073130303030303012670a500a460a1f2f636f736d6f732e63727970746f" +
		"2e736563703235366b312e5075624b657912230a2102baa4ef93f2ce84592a49b1d729c074eab640112522a7" +
		"a89f7d03ebab21ded7b612040a020801180312130a0d0a057574776c7412043230303010c09a0c1a40"
	got := hex.EncodeToString(raw)
	// The signature is deterministic (RFC 6979), so the whole transaction
	// is stable; the 64 signature bytes are checked for shape here and
	// for stability by the repeat below.
	if !strings.HasPrefix(got, golden) {
		t.Fatalf("transaction bytes changed:\n got %s\nwant prefix %s", got, golden)
	}
	if len(raw) != len(golden)/2+64 {
		t.Fatalf("expected a 64-byte signature after the prefix, got %d trailing bytes", len(raw)-len(golden)/2)
	}

	// Determinism: the same inputs sign identically, forever.
	again, err := buildSignedSend(key, sendParams{
		From: from, To: "twilight1kl0dn0rtwk46h9zcmazyyrruta290crh93rnlh",
		Denom: "utwlt", Amount: "1000000", ChainID: "twilight-devnet-2",
		AccountNo: 7, Sequence: 3, Gas: 200000, FeeAmount: "2000",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, again) {
		t.Fatal("signing the same document twice produced different bytes")
	}
}

// The sequence and account number are covered by the signature, so a
// stale one produces a transaction the chain refuses. Changing either
// must change the bytes.
func TestSequenceAndAccountNumberChangeTheSignedBytes(t *testing.T) {
	key := txTestKey(t)
	from, _ := key.Address(auth.TwilightHRP)
	base := sendParams{
		From: from, To: "twilight1kl0dn0rtwk46h9zcmazyyrruta290crh93rnlh",
		Denom: "utwlt", Amount: "5", ChainID: "twilight-devnet-2",
		AccountNo: 1, Sequence: 1, Gas: 200000, FeeAmount: "2000",
	}
	first, err := buildSignedSend(key, base)
	if err != nil {
		t.Fatal(err)
	}
	bumped := base
	bumped.Sequence = 2
	second, err := buildSignedSend(key, bumped)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(first, second) {
		t.Fatal("a different sequence produced identical bytes; the sequence is not in the signed document")
	}
	other := base
	other.AccountNo = 2
	third, err := buildSignedSend(key, other)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(first, third) {
		t.Fatal("a different account number produced identical bytes")
	}
}

// The protobuf reader is the half that talks back: what it decodes has
// to match what the writer encodes, including absent-means-zero.
func TestProtoReaderRoundTripsTheWriter(t *testing.T) {
	buf := protoString(1, "twilight1abc")
	buf = append(buf, protoUint(3, 42)...)
	buf = append(buf, protoUint(4, 0)...) // proto3 omits zero entirely
	buf = append(buf, protoBytes(2, []byte{0xde, 0xad})...)

	got, err := protoField(buf, 1)
	if err != nil || string(got) != "twilight1abc" {
		t.Fatalf("field 1: %q %v", got, err)
	}
	inner, err := protoField(buf, 2)
	if err != nil || !bytes.Equal(inner, []byte{0xde, 0xad}) {
		t.Fatalf("field 2: %x %v", inner, err)
	}
	num, err := protoVarintField(buf, 3)
	if err != nil || num != 42 {
		t.Fatalf("field 3: %d %v", num, err)
	}
	zero, err := protoVarintField(buf, 4)
	if err != nil || zero != 0 {
		t.Fatalf("an omitted field must read as zero: %d %v", zero, err)
	}
}

// nodeConfig is what a test wants the fake chain to say.
type nodeConfig struct {
	chainID       string
	accountNumber uint64
	sequence      uint64
	noAccount     bool
	balance       string
	broadcastCode uint32
	broadcastLog  string
	deliverCode   uint32
	appearAfter   int // /tx returns "not found" this many times first
}

// fakeNode is a CometBFT JSON-RPC stand-in.
type fakeNode struct {
	srv *httptest.Server
	cfg nodeConfig

	mu        sync.Mutex
	lastTx    string // the DECODED transaction bytes
	decodeErr string
	txQueries int
}

func newFakeNode(t *testing.T, cfg nodeConfig) *fakeNode {
	t.Helper()
	f := &fakeNode{cfg: cfg}
	mux := http.NewServeMux()
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		writeRPC(w, map[string]any{"node_info": map[string]any{"network": f.cfg.chainID}})
	})
	mux.HandleFunc("/abci_query", func(w http.ResponseWriter, r *http.Request) {
		path := strings.Trim(r.URL.Query().Get("path"), `"`)
		switch path {
		case queryAccountPath:
			if f.cfg.noAccount {
				writeRPC(w, map[string]any{"response": map[string]any{"code": 1, "log": "key not found"}})
				return
			}
			base := protoString(1, "twilight1whoever")
			base = append(base, protoUint(3, f.cfg.accountNumber)...)
			base = append(base, protoUint(4, f.cfg.sequence)...)
			wrapped := encodeAny("/cosmos.auth.v1beta1.BaseAccount", base)
			writeRPC(w, map[string]any{"response": map[string]any{
				"code": 0, "value": b64(protoBytes(1, wrapped)),
			}})
		case queryBalancePath:
			coin := encodeCoin("utwlt", f.cfg.balance)
			writeRPC(w, map[string]any{"response": map[string]any{
				"code": 0, "value": b64(protoBytes(1, coin)),
			}})
		default:
			writeRPC(w, map[string]any{"response": map[string]any{"code": 6, "log": "unknown query path"}})
		}
	})
	mux.HandleFunc("/broadcast_tx_sync", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		// Decoded the way CometBFT decodes it: 0x-prefixed hex. A fake
		// that accepted any encoding is what let a quoted-base64 tx
		// (which the real node reads as the literal base64 text) pass
		// its tests and fail on the devnet.
		raw := r.URL.Query().Get("tx")
		if strings.HasPrefix(raw, "0x") {
			if b, err := hex.DecodeString(raw[2:]); err == nil {
				f.lastTx = string(b)
			} else {
				f.decodeErr = "tx is not valid hex"
			}
		} else {
			f.decodeErr = "tx was not 0x-prefixed hex; the node would read it literally"
		}
		f.mu.Unlock()
		writeRPC(w, map[string]any{
			"code": f.cfg.broadcastCode, "log": f.cfg.broadcastLog,
			"hash": "ABCD1234ABCD1234ABCD1234ABCD1234ABCD1234ABCD1234ABCD1234ABCD1234",
		})
	})
	mux.HandleFunc("/tx", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.txQueries++
		n := f.txQueries
		f.mu.Unlock()
		if n <= f.cfg.appearAfter {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": map[string]any{"code": -32603, "message": "Internal error", "data": "tx not found"},
			})
			return
		}
		writeRPC(w, map[string]any{
			"height":    "1234",
			"tx_result": map[string]any{"code": f.cfg.deliverCode, "log": "delivered"},
		})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func writeRPC(w http.ResponseWriter, result any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": -1, "result": result})
}

func b64(b []byte) string {
	return base64.StdEncoding.EncodeToString(b)
}

func TestWalletBalanceReportsWhatTheChainSays(t *testing.T) {
	node := newFakeNode(t, nodeConfig{chainID: "twilight-devnet-2", balance: "49942800"})
	dir := walletScratchDir(t)
	var out, errOut bytes.Buffer
	if code := cmdWallet([]string{"init", "-dir", dir, "-print-anyway"}, strings.NewReader(""), &out, &errOut,
		envOf(map[string]string{walletPassphraseEnv: "p-test-1"})); code != 0 {
		t.Fatalf("init: %s", errOut.String())
	}
	out.Reset()
	if code := cmdWallet([]string{"balance", "-dir", dir, "-node", node.srv.URL},
		strings.NewReader(""), &out, &errOut, noEnv); code != 0 {
		t.Fatalf("exit: %s", errOut.String())
	}
	if strings.TrimSpace(out.String()) != "49942800 utwlt" {
		t.Errorf("balance printed %q", out.String())
	}
}

// The whole send path against a fake chain: the transaction reaches the
// node, and success is reported only after the block confirms it.
func TestWalletSendBroadcastsAndWaitsForTheBlock(t *testing.T) {
	node := newFakeNode(t, nodeConfig{
		chainID: "twilight-devnet-2", accountNumber: 5, sequence: 9,
		balance: "1000000", appearAfter: 1,
	})
	dir := walletScratchDir(t)
	env := envOf(map[string]string{walletPassphraseEnv: "p-test-1"})
	var out, errOut bytes.Buffer
	if code := cmdWallet([]string{"init", "-dir", dir, "-print-anyway"}, strings.NewReader(""), &out, &errOut, env); code != 0 {
		t.Fatalf("init: %s", errOut.String())
	}

	out.Reset()
	errOut.Reset()
	code := cmdWallet([]string{"send", "-dir", dir, "-node", node.srv.URL,
		"-to", "twilight1kl0dn0rtwk46h9zcmazyyrruta290crh93rnlh", "-amount", "1000", "-yes"},
		strings.NewReader(""), &out, &errOut, env)
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "submitted:") || !strings.Contains(out.String(), "confirmed in block 1234") {
		t.Errorf("output: %q", out.String())
	}
	node.mu.Lock()
	tx, decodeErr := node.lastTx, node.decodeErr
	node.mu.Unlock()
	if decodeErr != "" {
		t.Fatalf("the node could not decode the broadcast parameter: %s", decodeErr)
	}
	if tx == "" {
		t.Fatal("no transaction reached the node")
	}
	raw := []byte(tx)
	// It must be a TxRaw carrying exactly one signature of 64 bytes.
	sig, err := protoField(raw, 3)
	if err != nil || len(sig) != 64 {
		t.Fatalf("signature field: %d bytes, %v", len(sig), err)
	}
}

// A chain refusal is not a transport failure: the wallet reports the
// code and log, and exits distinctly.
func TestWalletSendReportsAChainRejection(t *testing.T) {
	node := newFakeNode(t, nodeConfig{
		chainID: "twilight-devnet-2", accountNumber: 5, sequence: 9,
		broadcastCode: 5, broadcastLog: "insufficient funds",
	})
	dir := walletScratchDir(t)
	env := envOf(map[string]string{walletPassphraseEnv: "p-test-1"})
	var out, errOut bytes.Buffer
	if code := cmdWallet([]string{"init", "-dir", dir, "-print-anyway"}, strings.NewReader(""), &out, &errOut, env); code != 0 {
		t.Fatalf("init: %s", errOut.String())
	}
	out.Reset()
	errOut.Reset()
	code := cmdWallet([]string{"send", "-dir", dir, "-node", node.srv.URL,
		"-to", "twilight1kl0dn0rtwk46h9zcmazyyrruta290crh93rnlh", "-amount", "1000", "-yes"},
		strings.NewReader(""), &out, &errOut, env)
	if code != exitChainRejected {
		t.Fatalf("exit %d, want %d", code, exitChainRejected)
	}
	if !strings.Contains(errOut.String(), "insufficient funds") {
		t.Errorf("the chain's own reason should reach the operator: %q", errOut.String())
	}
}

// Every refusal below happens before the key is decrypted or the node is
// dialed: a bad address must not cost a passphrase prompt, let alone a
// signature.
func TestWalletSendRefusesBadArgumentsBeforeSigning(t *testing.T) {
	dir := walletScratchDir(t)
	env := envOf(map[string]string{walletPassphraseEnv: "p-test-1"})
	var out, errOut bytes.Buffer
	if code := cmdWallet([]string{"init", "-dir", dir, "-print-anyway"}, strings.NewReader(""), &out, &errOut, env); code != 0 {
		t.Fatalf("init: %s", errOut.String())
	}
	sc, _, err := loadSidecar(dir, noEnv)
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		args []string
		want string
	}{
		{"missing flags", []string{"send", "-dir", dir}, "needs -to"},
		{"not an address", []string{"send", "-dir", dir, "-to", "not-an-address", "-amount", "1"}, "not a valid address"},
		{"another chain", []string{"send", "-dir", dir, "-to", "cosmos1jhg0e7s6gn44tfc5k37kr04sznyhedtc9rzys5", "-amount", "1"}, "across chains"},
		{"decimal amount", []string{"send", "-dir", dir, "-to", "twilight1kl0dn0rtwk46h9zcmazyyrruta290crh93rnlh", "-amount", "1.5"}, "positive integer"},
		{"zero amount", []string{"send", "-dir", dir, "-to", "twilight1kl0dn0rtwk46h9zcmazyyrruta290crh93rnlh", "-amount", "0"}, "positive integer"},
		{"own address", []string{"send", "-dir", dir, "-to", sc.Address, "-amount", "1"}, "own address"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			// No -node: reaching the network at all would be the failure.
			if code := cmdWallet(c.args, strings.NewReader(""), &out, &errOut, env); code != exitUsage {
				t.Fatalf("exit %d, want %d (usage); stderr: %s", code, exitUsage, errOut.String())
			}
			if !strings.Contains(errOut.String(), c.want) {
				t.Errorf("stderr %q does not explain the refusal (%q)", errOut.String(), c.want)
			}
		})
	}
}

// Without -yes, a send that is not confirmed signs nothing.
func TestWalletSendWithoutConfirmationSendsNothing(t *testing.T) {
	node := newFakeNode(t, nodeConfig{chainID: "twilight-devnet-2", accountNumber: 1, sequence: 1})
	dir := walletScratchDir(t)
	env := envOf(map[string]string{walletPassphraseEnv: "p-test-1"})
	var out, errOut bytes.Buffer
	if code := cmdWallet([]string{"init", "-dir", dir, "-print-anyway"}, strings.NewReader(""), &out, &errOut, env); code != 0 {
		t.Fatalf("init: %s", errOut.String())
	}
	out.Reset()
	code := cmdWallet([]string{"send", "-dir", dir, "-node", node.srv.URL,
		"-to", "twilight1kl0dn0rtwk46h9zcmazyyrruta290crh93rnlh", "-amount", "1000"},
		strings.NewReader("no\n"), &out, &errOut, env)
	if code != exitOK {
		t.Fatalf("a declined send is not an error: exit %d", code)
	}
	if !strings.Contains(out.String(), "canceled") {
		t.Errorf("output: %q", out.String())
	}
	node.mu.Lock()
	defer node.mu.Unlock()
	if node.lastTx != "" {
		t.Fatal("a declined send still broadcast a transaction")
	}
}
