package main

// `earnings`. The node double here is NOT hand-written: every response it
// serves is a verbatim capture from the public devnet, recorded in
// testdata/fixtures/chain with the exact request that produced it. Two
// details that an invented fixture had wrong, and that only the real node
// showed, are asserted on the way in — /tx_search takes QUOTED JSON
// arguments, and total_count is a decimal string.
//
// The address behind those captures has the exact mix this command has to
// tell apart: two releases from the reward escrow, and one ordinary transfer
// that arrives inside a transaction carrying three transfer events, only one
// of which is ours.

import (
	"bytes"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The three addresses in the captured fixtures.
const (
	fxParticipant = "twilight1xm9xxt24meqqpzt3lc5725t3d764j0j2w6jl6c"
	fxEscrow      = "twilight1245yut9zht8q4hz39sd0lzqtzkuw5us5pd3c3u"
	fxFaucet      = "twilight1kkxs9mjpq5waz7uxkugntzsp4dj67fgy4dwmk5"
	// fxSettlement is Slot 4's settlement_address on the devnet: the account
	// that SIGNS MsgSubmitSettlementChunk and pays its fee. It is never the
	// sender of a payout transfer, which is why labeling on it would be
	// wrong, and it is in the table below for exactly that reason.
	fxSettlement = "twilight18mehtzjgn9vp4wx6xkaw78p3mr5jc2y38yw8j2"
	// fxOtherParticipant shares a settlement transaction with fxParticipant
	// in the wider corpus; here it stands for "somebody else's line".
	fxOtherParticipant = "twilight1n06lvwdqwgema46l4504h6962t9x7slapwdue3"
)

// chainFixtureDir is resolved at package load, BEFORE any test chdirs into
// a scratch directory. A relative path here would break the moment a command
// test moved the working directory out from under the node double.
var chainFixtureDir = func() string {
	wd, err := os.Getwd()
	if err != nil {
		panic(err)
	}
	return filepath.Join(wd, "..", "..", "testdata", "fixtures", "chain")
}()

func chainFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(chainFixtureDir, name)) // #nosec G304 -- a fixed test-owned directory
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// ---- the node double ----

type earningsNodeConfig struct {
	// searchErr, when set, is returned as a CometBFT JSON-RPC error for
	// /tx_search — the shape an unindexed node answers with.
	searchErr string
	// moduleErr makes the module-account query fail, which is the
	// degrade-to-unlabeled path.
	moduleErr bool
	// headerErr makes every /header call fail, so a receipt has to print
	// without a date rather than the command failing.
	headerErr bool
	// balanceErr makes the balance query fail.
	balanceErr bool
}

type earningsNode struct {
	srv *httptest.Server
	cfg earningsNodeConfig
	t   *testing.T

	searchCalls int
	headerCalls int
}

func newEarningsNode(t *testing.T, cfg earningsNodeConfig) *earningsNode {
	t.Helper()
	n := &earningsNode{cfg: cfg, t: t}
	mux := http.NewServeMux()

	mux.HandleFunc("/tx_search", func(w http.ResponseWriter, r *http.Request) {
		n.searchCalls++
		q := r.URL.Query()
		// Every /tx_search argument is decoded as JSON by CometBFT's URI
		// layer, so all four have to arrive quoted. An unquoted query is
		// a -32602 from the real node; a fake that accepted either is how
		// that would have shipped.
		for _, key := range []string{"query", "page", "per_page", "order_by"} {
			v := q.Get(key)
			if len(v) < 2 || v[0] != '"' || v[len(v)-1] != '"' {
				t.Errorf("/tx_search %s=%q is not a quoted JSON value; the node answers -32602 for that", key, v)
			}
		}
		if got := unquote(q.Get("order_by")); got != "desc" {
			t.Errorf("/tx_search order_by=%q, want desc: a truncated walk must drop the OLDEST receipts", got)
		}
		if n.cfg.searchErr != "" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":-1,"error":{"code":-32603,` +
				`"message":"Internal error","data":` + strconv.Quote(n.cfg.searchErr) + `}}`))
			return
		}
		page := unquote(q.Get("page"))
		// At the page size the captures were taken with, the two files go
		// back byte for byte. At any other size the node would have paged
		// differently, so the envelope is rebuilt around the SAME verbatim
		// transaction objects rather than a captured envelope being served
		// under a page size that did not produce it.
		if unquote(q.Get("per_page")) == "2" {
			switch page {
			case "1":
				_, _ = w.Write(chainFixture(t, "tx_search_recipient_page1.json"))
			case "2":
				_, _ = w.Write(chainFixture(t, "tx_search_recipient_page2.json"))
			default:
				// What the real node answers past the end of the result set.
				// The requested page is deliberately NOT echoed back: this
				// is a fixture, not a mirror.
				_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":-1,"error":{"code":-32603,` +
					`"message":"Internal error","data":"page should be within [1, 2] range"}}`))
			}
			return
		}
		if page != "1" {
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":-1,"error":{"code":-32603,` +
				`"message":"Internal error","data":"page should be within [1, 1] range"}}`))
			return
		}
		_, _ = w.Write(mergedSearchPage(t))
	})

	mux.HandleFunc("/header", func(w http.ResponseWriter, r *http.Request) {
		n.headerCalls++
		if n.cfg.headerErr {
			http.Error(w, "no", http.StatusInternalServerError)
			return
		}
		h := r.URL.Query().Get("height")
		// /header takes a BARE int64, the opposite of /tx_search. Quoting
		// it is a decode failure at the node.
		if strings.Contains(h, `"`) {
			t.Errorf("/header height=%q is quoted; the node wants a bare integer", h)
		}
		// The three heights the captures cover, resolved through a literal
		// table rather than by pasting the query parameter into a filename.
		name, ok := map[string]string{
			"161479": "header_161479.json",
			"159663": "header_159663.json",
			"119227": "header_119227.json",
		}[h]
		if !ok {
			http.Error(w, "no such height", http.StatusInternalServerError)
			return
		}
		_, _ = w.Write(chainFixture(t, name))
	})

	mux.HandleFunc("/abci_query", func(w http.ResponseWriter, r *http.Request) {
		switch strings.Trim(r.URL.Query().Get("path"), `"`) {
		case queryModuleAcctPath:
			if n.cfg.moduleErr {
				writeRPC(w, map[string]any{"response": map[string]any{
					"code": 22, "log": "rpc error: code = NotFound desc = account rewards not found: key not found",
				}})
				return
			}
			_, _ = w.Write(chainFixture(t, "abci_module_account_rewards.json"))
		case queryBalancePath:
			if n.cfg.balanceErr {
				writeRPC(w, map[string]any{"response": map[string]any{"code": 1, "log": "unavailable"}})
				return
			}
			_, _ = w.Write(chainFixture(t, "abci_balance.json"))
		default:
			writeRPC(w, map[string]any{"response": map[string]any{"code": 6, "log": "unknown query path"}})
		}
	})

	n.srv = httptest.NewServer(mux)
	t.Cleanup(n.srv.Close)
	return n
}

// mergedSearchPage is the two captured pages under one envelope, for the
// page sizes the real node would have answered in one. Every transaction
// object inside it is the node's own bytes; only the envelope is rebuilt.
func mergedSearchPage(t *testing.T) []byte {
	t.Helper()
	type envelope struct {
		JSONRPC string `json:"jsonrpc"`
		ID      int    `json:"id"`
		Result  struct {
			Txs        []json.RawMessage `json:"txs"`
			TotalCount string            `json:"total_count"`
		} `json:"result"`
	}
	var merged envelope
	for _, name := range []string{"tx_search_recipient_page1.json", "tx_search_recipient_page2.json"} {
		var e envelope
		if err := json.Unmarshal(chainFixture(t, name), &e); err != nil {
			t.Fatal(err)
		}
		merged.JSONRPC, merged.ID = e.JSONRPC, e.ID
		merged.Result.TotalCount = e.Result.TotalCount
		merged.Result.Txs = append(merged.Result.Txs, e.Result.Txs...)
	}
	raw, err := json.Marshal(merged)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func unquote(s string) string {
	if v, err := strconv.Unquote(s); err == nil {
		return v
	}
	return s
}

// ---- labeling ----

// The whole sender domain, and only one member of it produces a mining
// label. This is the guard that keeps "somebody sent you tokens" from being
// reported as pay.
func TestOnlyTheRewardEscrowProducesAMiningLabel(t *testing.T) {
	for _, tc := range []struct {
		name   string
		sender string
		want   receiptLabel
	}{
		{"the reward escrow itself", fxEscrow, labelMining},
		{"the Slot's settlement address", fxSettlement, labelOther},
		{"a faucet", fxFaucet, labelOther},
		{"another participant", fxOtherParticipant, labelOther},
		{"the recipient's own address", fxParticipant, labelOther},
		{"no sender at all", "", labelOther},
		{"the escrow in upper case", strings.ToUpper(fxEscrow), labelOther},
		{"the escrow with a trailing space", fxEscrow + " ", labelOther},
		{"the escrow with a leading space", " " + fxEscrow, labelOther},
		{"a prefix of the escrow", fxEscrow[:len(fxEscrow)-1], labelOther},
		{"the escrow with one more character", fxEscrow + "q", labelOther},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := labelReceipt(chainReceipt{Sender: tc.sender}, fxEscrow)
			if got != tc.want {
				t.Fatalf("sender %q labeled %v, want %v", tc.sender, got, tc.want)
			}
		})
	}
}

// With no escrow established, NOTHING is a mining payment — including a
// receipt from the address that would have been the escrow. An unknown
// escrow must degrade to "unlabeled", never to "all of it counts".
func TestAnUnknownEscrowLabelsNothingAsMining(t *testing.T) {
	for _, sender := range []string{fxEscrow, fxFaucet, fxSettlement, ""} {
		if got := labelReceipt(chainReceipt{Sender: sender}, ""); got != labelUnlabeled {
			t.Fatalf("with no escrow, sender %q labeled %v, want labelUnlabeled", sender, got)
		}
	}
}

// ---- receipt extraction ----

func decodePage(t *testing.T, name string) *txSearchPage {
	t.Helper()
	var env struct {
		Result txSearchPage `json:"result"`
	}
	if err := json.Unmarshal(chainFixture(t, name), &env); err != nil {
		t.Fatal(err)
	}
	return &env.Result
}

// The captured page-2 transaction is a bank multi-send carrying THREE
// transfer events. The search matched it because one of them names our
// address; the other two name other people. Crediting the transaction
// instead of the event triples the amount and spends two strangers' lines.
func TestReceiptsAreCreditedPerEventNotPerTransaction(t *testing.T) {
	page := decodePage(t, "tx_search_recipient_page2.json")

	events := 0
	for _, tx := range page.Txs {
		for _, ev := range tx.TxResult.Events {
			if ev.Type == "transfer" {
				events++
			}
		}
	}
	if events != 3 {
		t.Fatalf("the fixture is meant to carry three transfer events, has %d — this test proves nothing now", events)
	}

	got := receiptsCreditedTo(page, fxParticipant, "utwlt")
	if len(got) != 1 {
		t.Fatalf("got %d receipts from a transaction with three transfer events, want 1", len(got))
	}
	if got[0].Amount.String() != "1000000" {
		t.Fatalf("amount %s, want 1000000 — the other two events belong to other addresses", got[0].Amount)
	}
	if got[0].Sender != fxFaucet {
		t.Fatalf("sender %q, want %q", got[0].Sender, fxFaucet)
	}
	if got[0].Height != 119227 {
		t.Fatalf("height %d, want 119227", got[0].Height)
	}

	// And the mirror: each of the other recipients gets exactly their own
	// line out of the same transaction.
	for _, other := range []string{fxSettlement, "twilight1nruncknhlvzh8rc6qupgkywkqkgfkghfkmysae"} {
		theirs := receiptsCreditedTo(page, other, "utwlt")
		if len(theirs) != 1 || theirs[0].Amount.String() != "1000000" {
			t.Fatalf("%s got %d receipts out of the shared transaction", other, len(theirs))
		}
	}
}

// A failed transaction moved nothing. Its ante-handler events are still
// indexed and still come back from the search, so the code check is the only
// thing between a rejected transaction and a reported payment.
func TestAFailedTransactionIsNeverAReceipt(t *testing.T) {
	page := decodePage(t, "tx_search_recipient_page1.json")
	if n := len(receiptsCreditedTo(page, fxParticipant, "utwlt")); n != 2 {
		t.Fatalf("the delivered fixture yields %d receipts, want 2", n)
	}
	for i := range page.Txs {
		page.Txs[i].TxResult.Code = 32 // the SDK's "unauthorized" code
	}
	if got := receiptsCreditedTo(page, fxParticipant, "utwlt"); len(got) != 0 {
		t.Fatalf("a failed transaction produced %d receipts totalling %s", len(got), got[0].Amount)
	}
}

// A different denomination is not this denomination, and an amount that is
// not a positive integer is not an amount.
func TestReceiptsIgnoreOtherDenominationsAndEmptyAmounts(t *testing.T) {
	page := decodePage(t, "tx_search_recipient_page1.json")
	if n := len(receiptsCreditedTo(page, fxParticipant, "uatom")); n != 0 {
		t.Fatalf("%d utwlt receipts were reported as uatom", n)
	}
	if n := len(receiptsCreditedTo(page, "twilight1nobody", "utwlt")); n != 0 {
		t.Fatalf("%d receipts were credited to an address that appears nowhere in the page", n)
	}
}

func TestCoinAmountReadsOneDenominationOutOfACoinString(t *testing.T) {
	huge := "115792089237316195423570985008687907853269984665640564039457584007913129639935"
	for _, tc := range []struct {
		coins string
		denom string
		want  string // "" means nil
	}{
		{"24971400utwlt", "utwlt", "24971400"},
		{"1000000utwlt", "utwlt", "1000000"},
		{"0utwlt", "utwlt", "0"},
		{"100utwlt,5uatom", "uatom", "5"},
		{"100utwlt,5uatom", "utwlt", "100"},
		{"100utwlt,5uatom", "uosmo", ""},
		{"5ibc/27394FB092D2ECCD56123C74F36E4C1F926001CEADA9CA97EA622B25F41E5EB2", "ibc/27394FB092D2ECCD56123C74F36E4C1F926001CEADA9CA97EA622B25F41E5EB2", "5"},
		{"utwlt", "utwlt", ""},    // a denom with no amount
		{"1000", "utwlt", ""},     // an amount with no denom
		{"", "utwlt", ""},         // nothing at all
		{"12utwltx", "utwlt", ""}, // a denom that merely starts the same way
		{"12xutwlt", "utwlt", ""}, // and one that merely ends the same way
		{"-5utwlt", "utwlt", ""},  // a sign is not a digit; the chain never emits one
		{huge + "utwlt", "utwlt", huge},
	} {
		t.Run(tc.coins+"/"+tc.denom, func(t *testing.T) {
			got := coinAmount(tc.coins, tc.denom)
			switch {
			case tc.want == "" && got != nil:
				t.Fatalf("got %s, want nil", got)
			case tc.want != "" && got == nil:
				t.Fatalf("got nil, want %s", tc.want)
			case tc.want != "" && got.String() != tc.want:
				t.Fatalf("got %s, want %s", got, tc.want)
			}
		})
	}
}

// A total is a sum of 256-bit chain amounts. Narrowing it to a machine word
// would produce a wrong number with exactly the same confidence as a right
// one, which is the worst thing a money report can do.
func TestTotalsDoNotWrapAtSixtyFourBits(t *testing.T) {
	big1, _ := new(big.Int).SetString("18446744073709551615", 10) // 2^64 - 1
	big2, _ := new(big.Int).SetString("18446744073709551615", 10)
	r := buildEarningsReport([]chainReceipt{
		{Sender: fxEscrow, Amount: big1},
		{Sender: fxEscrow, Amount: big2},
	}, fxEscrow, nil)
	if got, want := r.MiningTotal.String(), "36893488147419103230"; got != want {
		t.Fatalf("total %s, want %s", got, want)
	}
}

func TestGroupInsertsThousandsSeparators(t *testing.T) {
	for _, tc := range [][2]string{
		{"0", "0"}, {"1", "1"}, {"999", "999"}, {"1000", "1,000"},
		{"24971400", "24,971,400"}, {"75914200", "75,914,200"},
		{"1000000000000", "1,000,000,000,000"},
		{"", "0"},
		{"not-a-number", "not-a-number"},
	} {
		if got := group(tc[0]); got != tc[1] {
			t.Errorf("group(%q) = %q, want %q", tc[0], got, tc[1])
		}
	}
}

// The walk has to cross a page boundary, or an address with more receipts
// than one page reports a fraction of what it was paid. Driven at the page
// size the captures were taken with, so both files go back verbatim.
func TestTheWalkCrossesThePageBoundary(t *testing.T) {
	node := newEarningsNode(t, earningsNodeConfig{})
	c := newRPCClient(node.srv.URL)
	got, total, truncated, err := collectReceipts(t.Context(), c, fxParticipant, "utwlt", 2, 10)
	if err != nil {
		t.Fatal(err)
	}
	if node.searchCalls != 2 {
		t.Fatalf("searched %d page(s) for a 3-result set at 2 per page, want 2", node.searchCalls)
	}
	if len(got) != 3 {
		t.Fatalf("collected %d receipts across two pages, want 3", len(got))
	}
	if total != 3 {
		t.Fatalf("total_count decoded as %d, want 3 — it is a decimal STRING on this wire", total)
	}
	if truncated {
		t.Fatal("a complete two-page walk reported itself truncated")
	}
	sum := new(big.Int)
	for _, r := range got {
		sum.Add(sum, r.Amount)
	}
	if sum.String() != "75914200" {
		t.Fatalf("the walk totals %s; the chain says this address holds 75914200", sum)
	}
}

// The page ceiling is what stops an unbounded walk, and hitting it must be
// SAID rather than silently producing a smaller total.
func TestHittingThePageCeilingIsReportedAsAFloor(t *testing.T) {
	node := newEarningsNode(t, earningsNodeConfig{})
	c := newRPCClient(node.srv.URL)
	_, _, truncated, err := collectReceipts(t.Context(), c, fxParticipant, "utwlt", 2, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !truncated {
		t.Fatal("a walk stopped by its page ceiling did not report itself truncated")
	}
	r := buildEarningsReport(nil, fxEscrow, nil)
	r.Truncated, r.Denom = true, "utwlt"
	var b bytes.Buffer
	printEarnings(&b, r)
	if !strings.Contains(b.String(), "at least") || !strings.Contains(b.String(), "FLOOR") {
		t.Fatalf("a truncated report presented its total as a total:\n%s", b.String())
	}
}

// ---- the whole command ----

// earningsRun runs the command in an empty working directory, so nothing on
// the developer's machine — a stray tokendrop.toml, a wallet — can be found.
func earningsRun(t *testing.T, args ...string) (string, string, int) {
	t.Helper()
	t.Chdir(t.TempDir())
	t.Setenv("TOKENDROP_CONFIG", "")
	t.Setenv("TOKENDROP_WALLET_DIR", "")
	var out, errOut bytes.Buffer
	code := cmdEarnings(args, &out, &errOut, noEnv)
	return out.String(), errOut.String(), code
}

// The load-bearing case: no wallet, no config, no AS — a participant who
// registered an external address, holds no key on this machine, and wants to
// know whether they were paid.
func TestEarningsWorksWithNoWalletAndNoAS(t *testing.T) {
	node := newEarningsNode(t, earningsNodeConfig{})
	out, errOut, code := earningsRun(t, "-address", fxParticipant, "-node", node.srv.URL)
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, errOut)
	}

	if node.searchCalls != 1 {
		t.Errorf("searched %d page(s), want 1 at the production page size", node.searchCalls)
	}
	for _, want := range []string{
		"paid            2 receipts   74,914,200 utwlt",
		"37,457,100   height 161479",
		"37,457,100   height 159663",
		"2026-08-30 13:24",
		"balance         75,914,200 utwlt",
		"address         " + fxParticipant,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not contain %q:\n%s", want, out)
		}
	}
	// The ordinary transfer is present, is NOT under "paid", and says so.
	paid, rest, ok := strings.Cut(out, "also received")
	if !ok {
		t.Fatalf("the ordinary transfer was not reported at all:\n%s", out)
	}
	if strings.Contains(paid, "1,000,000") {
		t.Errorf("an ordinary transfer was counted under 'paid':\n%s", paid)
	}
	if !strings.Contains(rest, "NOT mining payments") || !strings.Contains(rest, "from "+fxFaucet) {
		t.Errorf("the ordinary transfer is not marked as one:\n%s", rest)
	}
	// The full address, never elided: it is the thing a participant has to
	// be able to check character by character.
	if strings.Contains(out, "…") {
		t.Errorf("an address or amount was elided; nothing here may be:\n%s", out)
	}
}

// The totals must agree with the chain's own balance. The captured fixtures
// were chosen so that they do: 37457100 x 2 + 1000000 = 75914200. A drift in
// either direction is a defect in the walk, not in the fixture.
func TestTheReportedTotalsAccountForTheWholeBalance(t *testing.T) {
	node := newEarningsNode(t, earningsNodeConfig{})
	out, _, code := earningsRun(t, "-address", fxParticipant, "-node", node.srv.URL)
	if code != exitOK {
		t.Fatal(out)
	}
	if !strings.Contains(out, "74,914,200") || !strings.Contains(out, "1,000,000") ||
		!strings.Contains(out, "75,914,200") {
		t.Fatalf("the two totals do not add up to the balance:\n%s", out)
	}
}

// The escrow read failing must not turn every incoming transfer into a
// mining payment. It degrades to unlabeled, and says out loud that it did.
func TestEarningsDegradesToUnlabeledWhenTheEscrowCannotBeRead(t *testing.T) {
	node := newEarningsNode(t, earningsNodeConfig{moduleErr: true})
	out, errOut, code := earningsRun(t, "-address", fxParticipant, "-node", node.srv.URL)
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if strings.Contains(out, "paid") {
		t.Errorf("an unlabeled report used the word 'paid':\n%s", out)
	}
	for _, want := range []string{"received", "UNLABELED", "-escrow"} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not contain %q:\n%s", want, out)
		}
	}
	// All three receipts are still shown, and the total is all of them.
	if !strings.Contains(out, "3 receipts") || !strings.Contains(out, "75,914,200") {
		t.Errorf("the unlabeled report lost receipts:\n%s", out)
	}
}

// -escrow names it when the chain read cannot, and labels resume.
func TestEscrowFlagRestoresLabeling(t *testing.T) {
	node := newEarningsNode(t, earningsNodeConfig{moduleErr: true})
	out, _, code := earningsRun(t, "-address", fxParticipant, "-node", node.srv.URL, "-escrow", fxEscrow)
	if code != exitOK {
		t.Fatal(out)
	}
	if !strings.Contains(out, "paid            2 receipts   74,914,200 utwlt") {
		t.Fatalf("-escrow did not restore labeling:\n%s", out)
	}
}

// An -escrow that is not an address must not silently become "no escrow",
// which would quietly relabel a working report as unlabeled.
func TestABadEscrowIsReportedRatherThanIgnored(t *testing.T) {
	node := newEarningsNode(t, earningsNodeConfig{})
	out, _, code := earningsRun(t, "-address", fxParticipant, "-node", node.srv.URL, "-escrow", "not-an-address")
	if code != exitOK {
		t.Fatal(out)
	}
	if !strings.Contains(out, "UNLABELED") || !strings.Contains(out, "not a valid address") {
		t.Fatalf("a malformed -escrow was not explained:\n%s", out)
	}
}

// A node without tx_index cannot answer this at all, and the message has to
// name the reason — it is a node configuration problem, not a bad address.
func TestEarningsExplainsANodeThatCannotSearch(t *testing.T) {
	node := newEarningsNode(t, earningsNodeConfig{
		searchErr: "transaction indexing is disabled",
	})
	_, errOut, code := earningsRun(t, "-address", fxParticipant, "-node", node.srv.URL)
	if code != exitTransport {
		t.Fatalf("exit %d, want %d", code, exitTransport)
	}
	if !strings.Contains(errOut, "tx_index") || !strings.Contains(errOut, "indexing is disabled") {
		t.Fatalf("the failure should name the node's own reason and the setting: %q", errOut)
	}
}

// A node that is not there at all.
func TestEarningsReportsAnUnreachableNode(t *testing.T) {
	_, errOut, code := earningsRun(t, "-address", fxParticipant, "-node", "http://127.0.0.1:1")
	if code != exitTransport {
		t.Fatalf("exit %d, want %d", code, exitTransport)
	}
	if !strings.Contains(errOut, "cannot read this address's transactions") {
		t.Fatalf("unclear failure: %q", errOut)
	}
}

// A block-time lookup failing costs the date and nothing else. The amounts
// and the total are the answer; the date is decoration.
func TestAFailedBlockTimeLookupStillReportsTheReceipt(t *testing.T) {
	node := newEarningsNode(t, earningsNodeConfig{headerErr: true})
	out, _, code := earningsRun(t, "-address", fxParticipant, "-node", node.srv.URL)
	if code != exitOK {
		t.Fatal(out)
	}
	if !strings.Contains(out, "74,914,200") || !strings.Contains(out, "height 161479") {
		t.Fatalf("a missing block time lost the receipt:\n%s", out)
	}
	if strings.Contains(out, "2026-") {
		t.Fatalf("a date was printed although every header lookup failed:\n%s", out)
	}
}

// A balance that cannot be read is reported as unknown, not as zero. Zero is
// a claim; "unknown" is the truth.
func TestAnUnreadableBalanceIsNotReportedAsZero(t *testing.T) {
	node := newEarningsNode(t, earningsNodeConfig{balanceErr: true})
	out, _, code := earningsRun(t, "-address", fxParticipant, "-node", node.srv.URL)
	if code != exitOK {
		t.Fatal(out)
	}
	if !strings.Contains(out, "balance         unknown") {
		t.Fatalf("an unreadable balance was not reported as unknown:\n%s", out)
	}
	if strings.Contains(out, "balance         0 utwlt") {
		t.Fatalf("an unreadable balance was reported as zero:\n%s", out)
	}
}

// The AS line when there is no AS to ask: "not asked", never a number.
func TestTheEpochLineSaysWhenTheASWasNotAsked(t *testing.T) {
	node := newEarningsNode(t, earningsNodeConfig{})
	out, _, _ := earningsRun(t, "-address", fxParticipant, "-node", node.srv.URL)
	if !strings.Contains(out, "this epoch      not asked") {
		t.Fatalf("the epoch line did not say the AS was not asked:\n%s", out)
	}
}

// Only bech32 reaches the query language. The address is interpolated into a
// CometBFT search expression, so anything that is not an address is refused
// before it gets there.
func TestOnlyAValidAddressReachesTheQuery(t *testing.T) {
	node := newEarningsNode(t, earningsNodeConfig{})
	for _, bad := range []string{
		"twilight1notavalidchecksum",
		"' OR tx.height > 0 AND transfer.recipient='",
		"cosmos1abcdefghijklmnopqrstuvwxyz0123456789xyz",
		"..",
	} {
		_, errOut, code := earningsRun(t, "-address", bad, "-node", node.srv.URL)
		if code != exitUsage {
			t.Errorf("%q was accepted (exit %d)", bad, code)
		}
		if !strings.Contains(errOut, "not a valid address") {
			t.Errorf("%q refused without saying why: %q", bad, errOut)
		}
	}
	if node.searchCalls != 0 {
		t.Fatalf("%d search(es) were made with an address that never validated", node.searchCalls)
	}
}

// -limit bounds the listing, never the totals, and says how many it held
// back. A count that shrank with the listing would misreport what was paid.
func TestLimitBoundsTheListingAndNotTheTotal(t *testing.T) {
	node := newEarningsNode(t, earningsNodeConfig{})
	out, _, code := earningsRun(t, "-address", fxParticipant, "-node", node.srv.URL, "-limit", "1")
	if code != exitOK {
		t.Fatal(out)
	}
	if !strings.Contains(out, "paid            2 receipts   74,914,200 utwlt") {
		t.Fatalf("-limit changed the total:\n%s", out)
	}
	if !strings.Contains(out, "1 more not listed") {
		t.Fatalf("-limit did not say what it held back:\n%s", out)
	}
	if strings.Contains(out, "height 159663") {
		t.Fatalf("-limit 1 listed two receipts:\n%s", out)
	}
	// Only the listed receipt's block time is fetched.
	if node.headerCalls > 2 {
		t.Errorf("%d header lookups for one listed receipt", node.headerCalls)
	}
}

func TestANegativeLimitIsRefused(t *testing.T) {
	if _, _, code := earningsRun(t, "-address", fxParticipant, "-limit", "-1"); code != exitUsage {
		t.Fatalf("exit %d, want %d", code, exitUsage)
	}
}

// Nothing in the report may call an incoming transfer earnings, and nothing
// may attribute an amount to a request.
func TestTheReportNeverCallsABalanceEarningsOrPricesARequest(t *testing.T) {
	node := newEarningsNode(t, earningsNodeConfig{})
	out, _, _ := earningsRun(t, "-address", fxParticipant, "-node", node.srv.URL)
	lower := strings.ToLower(out)
	for _, forbidden := range []string{
		"per request", "per-request", "per search", "per observation",
		"you earned", "earned so far", "rate", "price",
	} {
		if strings.Contains(lower, forbidden) {
			t.Errorf("the report contains %q, which prices something it must not:\n%s", forbidden, out)
		}
	}
	for _, want := range []string{
		"equal split among its eligible participants",
		"no amount belongs to any single",
		"not earnings",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the report does not say %q:\n%s", want, out)
		}
	}
}

// The escrow is named in the report, so a participant can check the claim
// rather than take it.
func TestTheReportNamesTheEscrowItLabeledWith(t *testing.T) {
	node := newEarningsNode(t, earningsNodeConfig{})
	out, _, _ := earningsRun(t, "-address", fxParticipant, "-node", node.srv.URL)
	if !strings.Contains(out, fxEscrow) {
		t.Fatalf("the report does not name the escrow it labeled with:\n%s", out)
	}
}
