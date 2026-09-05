package main

// Transaction construction and chain I/O for the CLI.
//
// The wallet's send path is the biggest thing here, but it is not the
// only caller: `earnings` reads the same node through the same client,
// and deliberately reuses this file's protobuf reader and writer rather
// than growing a second one.
//
// The Cosmos SDK module tree is banned repo-wide (AGENTS.md;
// TestNoChainImportsAnywhere), so the six messages a bank send needs are
// encoded here by hand. Protobuf's wire format is small and stable:
// every field below is a length-delimited or varint field with a fixed
// number, and the shapes are frozen in cosmos-sdk's .proto files. A
// golden-bytes test pins the whole assembly, which is possible because
// the signature is deterministic (RFC 6979).
//
// Chain I/O is CometBFT's JSON-RPC over plain HTTP rather than gRPC:
// stdlib only, and the same endpoint an operator curls.

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/twilight-project/dropin-miner/pkg/auth"
)

// Type URLs, frozen by the SDK's proto registration.
const (
	typeURLMsgSend       = "/cosmos.bank.v1beta1.MsgSend"
	typeURLSecp256k1Pub  = "/cosmos.crypto.secp256k1.PubKey"
	signModeDirect       = 1 // SIGN_MODE_DIRECT
	queryAccountPath     = "/cosmos.auth.v1beta1.Query/Account"
	queryBalancePath     = "/cosmos.bank.v1beta1.Query/Balance"
	queryModuleAcctPath  = "/cosmos.auth.v1beta1.Query/ModuleAccountByName"
	defaultWalletNodeURL = "http://54.179.101.3:26657"
	defaultWalletDenom   = "utwlt"
	defaultWalletGas     = 200000
	defaultWalletFee     = 2000 // utwlt; devnet gas price is nominal
)

// ---- minimal protobuf writer ----

func protoTag(field int, wireType byte) []byte {
	if field < 1 || field > 0x1fffffff {
		panic("wallet: proto field number out of range") // ours, never input
	}
	return protoVarint(uint64(field)<<3 | uint64(wireType))
}

func protoVarint(v uint64) []byte {
	var out []byte
	for v >= 0x80 {
		out = append(out, byte(v)|0x80)
		v >>= 7
	}
	return append(out, byte(v))
}

// protoBytes writes a length-delimited field (wire type 2).
func protoBytes(field int, val []byte) []byte {
	if len(val) == 0 {
		return nil
	}
	out := protoTag(field, 2)
	out = append(out, protoVarint(uint64(len(val)))...)
	return append(out, val...)
}

func protoString(field int, val string) []byte {
	return protoBytes(field, []byte(val))
}

// protoUint writes a varint field (wire type 0), omitting zero the way
// proto3 does — the encoding the chain's own encoder produces.
func protoUint(field int, val uint64) []byte {
	if val == 0 {
		return nil
	}
	out := protoTag(field, 0)
	return append(out, protoVarint(val)...)
}

// ---- messages ----

// coin is Coin{denom=1, amount=2}; amount is a decimal string.
func encodeCoin(denom, amount string) []byte {
	out := protoString(1, denom)
	return append(out, protoString(2, amount)...)
}

// encodeMsgSend is MsgSend{from_address=1, to_address=2, amount=3}.
func encodeMsgSend(from, to, denom, amount string) []byte {
	out := protoString(1, from)
	out = append(out, protoString(2, to)...)
	return append(out, protoBytes(3, encodeCoin(denom, amount))...)
}

// encodeAny is Any{type_url=1, value=2}.
func encodeAny(typeURL string, value []byte) []byte {
	out := protoString(1, typeURL)
	return append(out, protoBytes(2, value)...)
}

// encodeTxBody is TxBody{messages=1, memo=2}.
func encodeTxBody(msgs [][]byte, memo string) []byte {
	var out []byte
	for _, m := range msgs {
		out = append(out, protoBytes(1, m)...)
	}
	return append(out, protoString(2, memo)...)
}

// encodeSignerInfo is SignerInfo{public_key=1, mode_info=2, sequence=3}
// with ModeInfo{single=1{mode=1}}.
func encodeSignerInfo(pubKey []byte, sequence uint64) []byte {
	pk := encodeAny(typeURLSecp256k1Pub, protoBytes(1, pubKey))
	single := protoUint(1, signModeDirect)
	modeInfo := protoBytes(1, single)

	out := protoBytes(1, pk)
	out = append(out, protoBytes(2, modeInfo)...)
	return append(out, protoUint(3, sequence)...)
}

// encodeFee is Fee{amount=1, gas_limit=2}.
func encodeFee(denom, amount string, gas uint64) []byte {
	out := protoBytes(1, encodeCoin(denom, amount))
	return append(out, protoUint(2, gas)...)
}

// encodeAuthInfo is AuthInfo{signer_infos=1, fee=2}.
func encodeAuthInfo(signerInfo, fee []byte) []byte {
	out := protoBytes(1, signerInfo)
	return append(out, protoBytes(2, fee)...)
}

// encodeSignDoc is SignDoc{body_bytes=1, auth_info_bytes=2, chain_id=3,
// account_number=4} — the bytes the signature covers.
func encodeSignDoc(body, authInfo []byte, chainID string, accountNumber uint64) []byte {
	out := protoBytes(1, body)
	out = append(out, protoBytes(2, authInfo)...)
	out = append(out, protoString(3, chainID)...)
	return append(out, protoUint(4, accountNumber)...)
}

// encodeTxRaw is TxRaw{body_bytes=1, auth_info_bytes=2, signatures=3}.
func encodeTxRaw(body, authInfo, signature []byte) []byte {
	out := protoBytes(1, body)
	out = append(out, protoBytes(2, authInfo)...)
	return append(out, protoBytes(3, signature)...)
}

// sendParams is everything a transfer needs that is not the key.
type sendParams struct {
	From      string
	To        string
	Denom     string
	Amount    string
	Memo      string
	ChainID   string
	AccountNo uint64
	Sequence  uint64
	Gas       uint64
	FeeAmount string
}

// buildSignedSend assembles and signs one bank send, returning the
// broadcastable TxRaw bytes.
func buildSignedSend(key *auth.WalletKey, p sendParams) ([]byte, error) {
	msg := encodeAny(typeURLMsgSend, encodeMsgSend(p.From, p.To, p.Denom, p.Amount))
	body := encodeTxBody([][]byte{msg}, p.Memo)
	authInfo := encodeAuthInfo(
		encodeSignerInfo(key.PubKeyCompressed(), p.Sequence),
		encodeFee(p.Denom, p.FeeAmount, p.Gas),
	)
	signDoc := encodeSignDoc(body, authInfo, p.ChainID, p.AccountNo)
	digest := sha256Sum(signDoc)
	sig, err := key.SignDigest(digest)
	if err != nil {
		return nil, err
	}
	return encodeTxRaw(body, authInfo, sig), nil
}

// ---- CometBFT JSON-RPC ----

type rpcClient struct {
	base string
	http *http.Client
}

func newRPCClient(base string) *rpcClient {
	return &rpcClient{
		base: strings.TrimRight(base, "/"),
		// Never consult HTTP_PROXY for a node endpoint the operator named.
		http: &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 30 * time.Second},
	}
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    string `json:"data"`
}

func (c *rpcClient) get(ctx context.Context, path string, params url.Values, out any) error {
	return c.getLimited(ctx, path, params, out, defaultRPCReadLimit)
}

// getLimited is get with an explicit response ceiling. Everything except
// /tx_search answers in kilobytes; a page of a hundred settlement
// transactions is megabytes, and silently truncating one at the default
// limit surfaces as "unparseable JSON" rather than as the size problem it
// is.
func (c *rpcClient) getLimited(ctx context.Context, path string, params url.Values, out any, limit int64) error {
	u := c.base + path
	if len(params) > 0 {
		u += "?" + params.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	// One byte past the ceiling, so a response AT the ceiling is
	// distinguishable from one that was cut off.
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("node returned HTTP %s", resp.Status)
	}
	if int64(len(body)) > limit {
		return fmt.Errorf("node response for %s exceeds %d bytes; ask for fewer results", path, limit)
	}
	var envelope struct {
		Result json.RawMessage `json:"result"`
		Error  *rpcError       `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return fmt.Errorf("node returned unparseable JSON: %w", err)
	}
	if envelope.Error != nil {
		return fmt.Errorf("node error %d: %s %s", envelope.Error.Code, envelope.Error.Message, envelope.Error.Data)
	}
	return json.Unmarshal(envelope.Result, out)
}

// chainID asks the node what chain it is, rather than trusting a
// hardcoded string: signing for the wrong chain produces a signature the
// chain silently rejects.
func (c *rpcClient) chainID(ctx context.Context) (string, error) {
	var status struct {
		NodeInfo struct {
			Network string `json:"network"`
		} `json:"node_info"`
	}
	if err := c.get(ctx, "/status", nil, &status); err != nil {
		return "", err
	}
	if status.NodeInfo.Network == "" {
		return "", errors.New("node did not report a chain id")
	}
	return status.NodeInfo.Network, nil
}

// abciQuery runs one ABCI query and returns the raw response value.
func (c *rpcClient) abciQuery(ctx context.Context, path string, data []byte) ([]byte, error) {
	var res struct {
		Response struct {
			Code  uint32 `json:"code"`
			Log   string `json:"log"`
			Value string `json:"value"`
		} `json:"response"`
	}
	params := url.Values{}
	params.Set("path", strconv.Quote(path))
	// CometBFT's URI encoding reads a QUOTED value as a raw string and an
	// 0x-prefixed one as hex. Quoting the hex here sent the ASCII of the
	// hex digits as the request body, which the node reported as
	// "illegal wireType 7" — a live-devnet failure no fake could produce,
	// since a fake decodes whatever the client happens to send.
	params.Set("data", "0x"+hex.EncodeToString(data))
	if err := c.get(ctx, "/abci_query", params, &res); err != nil {
		return nil, err
	}
	if res.Response.Code != 0 {
		return nil, fmt.Errorf("query %s: code %d: %s", path, res.Response.Code, res.Response.Log)
	}
	if res.Response.Value == "" {
		return nil, nil
	}
	return base64.StdEncoding.DecodeString(res.Response.Value)
}

// accountState is what signing needs from the chain.
type accountState struct {
	AccountNumber uint64
	Sequence      uint64
}

// account reads account_number and sequence. A never-funded address has
// no account at all, which is not an error here — it is a balance
// problem the send path reports plainly.
func (c *rpcClient) account(ctx context.Context, address string) (*accountState, error) {
	value, err := c.abciQuery(ctx, queryAccountPath, protoString(1, address))
	if err != nil {
		if strings.Contains(err.Error(), "not found") || strings.Contains(err.Error(), "key not found") {
			return nil, errNoAccount
		}
		return nil, err
	}
	if len(value) == 0 {
		return nil, errNoAccount
	}
	// QueryAccountResponse{account=1 Any}; Any{type_url=1, value=2};
	// BaseAccount{address=1, pub_key=2, account_number=3, sequence=4}.
	anyBytes, err := protoField(value, 1)
	if err != nil {
		return nil, fmt.Errorf("decode account response: %w", err)
	}
	baseAccount, err := protoField(anyBytes, 2)
	if err != nil {
		return nil, fmt.Errorf("decode account: %w", err)
	}
	num, err := protoVarintField(baseAccount, 3)
	if err != nil {
		return nil, fmt.Errorf("decode account number: %w", err)
	}
	seq, err := protoVarintField(baseAccount, 4)
	if err != nil {
		return nil, fmt.Errorf("decode sequence: %w", err)
	}
	return &accountState{AccountNumber: num, Sequence: seq}, nil
}

var errNoAccount = errors.New("this address has never received funds, so the chain has no account for it")

// balance reads one denom's balance.
func (c *rpcClient) balance(ctx context.Context, address, denom string) (string, error) {
	req := append(protoString(1, address), protoString(2, denom)...)
	value, err := c.abciQuery(ctx, queryBalancePath, req)
	if err != nil {
		return "", err
	}
	if len(value) == 0 {
		return "0", nil
	}
	// QueryBalanceResponse{balance=1 Coin}; Coin{denom=1, amount=2}.
	coin, err := protoField(value, 1)
	if err != nil || len(coin) == 0 {
		return "0", nil // #nosec G104 -- an absent balance is zero, not an error
	}
	amount, err := protoField(coin, 2)
	if err != nil || len(amount) == 0 {
		return "0", nil
	}
	return string(amount), nil
}

// broadcastResult is what broadcast_tx_sync reports: acceptance into the
// mempool, not inclusion in a block.
type broadcastResult struct {
	Code uint32 `json:"code"`
	Log  string `json:"log"`
	Hash string `json:"hash"`
}

func (c *rpcClient) broadcast(ctx context.Context, txRaw []byte) (*broadcastResult, error) {
	params := url.Values{}
	// 0x-prefixed hex, for the same reason as the query above: a quoted
	// value is taken literally, so quoting base64 broadcast the ASCII of
	// the base64 TEXT as the transaction. The node accepted it as far as
	// the tx decoder and rejected it there — proven by the broadcast hash
	// matching SHA-256 of the base64 text rather than of the transaction.
	params.Set("tx", "0x"+hex.EncodeToString(txRaw))
	var res broadcastResult
	if err := c.get(ctx, "/broadcast_tx_sync", params, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// txResult is the delivered outcome, once the transaction is in a block.
type txResult struct {
	Height   string `json:"height"`
	TxResult struct {
		Code uint32 `json:"code"`
		Log  string `json:"log"`
	} `json:"tx_result"`
}

// waitForTx polls until the transaction appears in a block or the
// context expires. broadcast_tx_sync only proves the mempool accepted
// it; execution can still fail, and a wallet that stopped at "accepted"
// would report a failed transfer as a success.
func (c *rpcClient) waitForTx(ctx context.Context, hash string, interval time.Duration) (*txResult, error) {
	params := url.Values{}
	params.Set("hash", "0x"+strings.TrimPrefix(strings.ToUpper(hash), "0X"))
	for {
		var res txResult
		err := c.get(ctx, "/tx", params, &res)
		if err == nil {
			return &res, nil
		}
		// "not found" is the normal answer until it lands.
		if !strings.Contains(strings.ToLower(err.Error()), "not found") {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(interval):
		}
	}
}

// ---- minimal protobuf reader (only what the two queries need) ----

// protoField returns the bytes of the first length-delimited field with
// the given number.
func protoField(buf []byte, field int) ([]byte, error) {
	for len(buf) > 0 {
		tag, n := readVarint(buf)
		if n == 0 {
			return nil, errors.New("truncated tag")
		}
		buf = buf[n:]
		num, wire := int(tag>>3), byte(tag&7)
		switch wire {
		case 2:
			l, n := readVarint(buf)
			// The length is compared as uint64 before any conversion:
			// a huge varint must be refused, not wrapped.
			if n == 0 || l > uint64(len(buf[n:])) {
				return nil, errors.New("truncated length-delimited field")
			}
			end := n + int(l) // #nosec G115 -- l <= len(buf[n:]), checked above
			val := buf[n:end]
			if num == field {
				return val, nil
			}
			buf = buf[end:]
		case 0:
			_, n := readVarint(buf)
			if n == 0 {
				return nil, errors.New("truncated varint")
			}
			buf = buf[n:]
		case 5:
			if len(buf) < 4 {
				return nil, errors.New("truncated fixed32")
			}
			buf = buf[4:]
		case 1:
			if len(buf) < 8 {
				return nil, errors.New("truncated fixed64")
			}
			buf = buf[8:]
		default:
			return nil, fmt.Errorf("unsupported wire type %d", wire)
		}
	}
	return nil, nil // absent, which proto3 renders as the zero value
}

// protoVarintField returns a varint field's value; absent means zero.
func protoVarintField(buf []byte, field int) (uint64, error) {
	for len(buf) > 0 {
		tag, n := readVarint(buf)
		if n == 0 {
			return 0, errors.New("truncated tag")
		}
		buf = buf[n:]
		num, wire := int(tag>>3), byte(tag&7)
		switch wire {
		case 0:
			v, n := readVarint(buf)
			if n == 0 {
				return 0, errors.New("truncated varint")
			}
			if num == field {
				return v, nil
			}
			buf = buf[n:]
		case 2:
			l, n := readVarint(buf)
			if n == 0 || l > uint64(len(buf[n:])) {
				return 0, errors.New("truncated length-delimited field")
			}
			buf = buf[n+int(l):] // #nosec G115 -- l <= len(buf[n:]), checked above
		case 5:
			if len(buf) < 4 {
				return 0, errors.New("truncated fixed32")
			}
			buf = buf[4:]
		case 1:
			if len(buf) < 8 {
				return 0, errors.New("truncated fixed64")
			}
			buf = buf[8:]
		default:
			return 0, fmt.Errorf("unsupported wire type %d", wire)
		}
	}
	return 0, nil
}

func readVarint(buf []byte) (uint64, int) {
	var v uint64
	for i := 0; i < len(buf) && i < 10; i++ {
		v |= uint64(buf[i]&0x7f) << (7 * uint(i))
		if buf[i] < 0x80 {
			return v, i + 1
		}
	}
	return 0, 0
}

func sha256Sum(b []byte) []byte {
	sum := sha256.Sum256(b)
	return sum[:]
}

// ---- reads `earnings` needs, and nothing else does ----

const (
	// defaultRPCReadLimit bounds every node answer except a search page.
	defaultRPCReadLimit = 4 << 20
	// txSearchReadLimit bounds one search page. A hundred settlement
	// transactions, each carrying its full event set and its raw bytes,
	// runs to a few hundred kilobytes on the devnet; the headroom is for
	// a chunk that pays many participants at once.
	txSearchReadLimit = 32 << 20

	// txSearchPageSize is what one /tx_search call asks for. CometBFT
	// caps per_page at 100 and silently returns 100 for anything larger
	// (observed: per_page="200" answered 100 of 217), so asking for more
	// would just look like the last page and stop the walk early.
	txSearchPageSize = 100
	// txSearchMaxPages stops the walk somewhere. An address with more
	// receipts than this gets an explicitly partial answer rather than an
	// unbounded one; the caller says so out loud rather than presenting a
	// floor as a total.
	txSearchMaxPages = 100
)

// txSearchPage is the /tx_search envelope.
//
// total_count is a decimal STRING, like every other 64-bit quantity on
// this wire — not the number a hand-written fixture would have carried.
type txSearchPage struct {
	Txs        []txSearchResult `json:"txs"`
	TotalCount string           `json:"total_count"`
}

type txSearchResult struct {
	Hash     string `json:"hash"`
	Height   string `json:"height"`
	TxResult struct {
		Code   uint32      `json:"code"`
		Events []abciEvent `json:"events"`
	} `json:"tx_result"`
}

type abciEvent struct {
	Type       string `json:"type"`
	Attributes []struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	} `json:"attributes"`
}

// attr returns one attribute's value, empty when absent.
func (e abciEvent) attr(key string) string {
	for _, a := range e.Attributes {
		if a.Key == key {
			return a.Value
		}
	}
	return ""
}

// txSearch runs one page of a CometBFT event search.
//
// It only works on a node with tx_index.indexer = "kv"; an unindexed node
// refuses the method outright rather than answering emptily, which is the
// distinction that made this command buildable at all.
//
// EVERY parameter is a QUOTED JSON value. CometBFT's URI layer decodes
// each argument as JSON before the handler sees it, so an unquoted query
// comes back as `-32602 Invalid params: invalid character 'a' in literal
// true` — the node reading `transfer.recipient=…` as a JSON literal. That
// is the same trap abciQuery documents for `path`, and no fake would have
// shown it, because a fake decodes whatever the client happens to send.
func (c *rpcClient) txSearch(ctx context.Context, query string, page, perPage int) (*txSearchPage, error) {
	params := url.Values{}
	params.Set("query", strconv.Quote(query))
	params.Set("page", strconv.Quote(strconv.Itoa(page)))
	params.Set("per_page", strconv.Quote(strconv.Itoa(perPage)))
	// Newest first, so a truncated walk truncates the OLD end. A partial
	// answer that dropped the most recent receipts would be the one a
	// participant is most likely to be looking for.
	params.Set("order_by", strconv.Quote("desc"))
	var out txSearchPage
	if err := c.getLimited(ctx, "/tx_search", params, &out, txSearchReadLimit); err != nil {
		return nil, err
	}
	return &out, nil
}

// blockTime reads one block's timestamp, so a receipt can carry a date
// rather than only a height.
func (c *rpcClient) blockTime(ctx context.Context, height int64) (time.Time, error) {
	params := url.Values{}
	// Unquoted: /header takes an int64, and a quoted value would arrive
	// as a JSON string the node cannot decode into one. The opposite rule
	// from txSearch above, which is why both are stated rather than
	// assumed.
	params.Set("height", strconv.FormatInt(height, 10))
	var res struct {
		Header struct {
			Time string `json:"time"`
		} `json:"header"`
	}
	if err := c.get(ctx, "/header", params, &res); err != nil {
		return time.Time{}, err
	}
	if res.Header.Time == "" {
		return time.Time{}, fmt.Errorf("node reported no time for height %d", height)
	}
	t, err := time.Parse(time.RFC3339Nano, res.Header.Time)
	if err != nil {
		return time.Time{}, fmt.Errorf("node reported an unparseable block time %q: %w", res.Header.Time, err)
	}
	return t, nil
}

// moduleAccountAddress resolves a module account's address by name.
//
// It is the same shape as balance(): a protobuf request in, protoField
// out, through abciQuery. QueryModuleAccountByNameResponse{account=1 Any};
// Any{type_url=1, value=2}; ModuleAccount{base_account=1, name=2};
// BaseAccount{address=1}.
func (c *rpcClient) moduleAccountAddress(ctx context.Context, name string) (string, error) {
	value, err := c.abciQuery(ctx, queryModuleAcctPath, protoString(1, name))
	if err != nil {
		return "", err
	}
	if len(value) == 0 {
		return "", fmt.Errorf("this chain has no module account named %q", name)
	}
	anyBytes, err := protoField(value, 1)
	if err != nil {
		return "", fmt.Errorf("decode module account response: %w", err)
	}
	moduleAccount, err := protoField(anyBytes, 2)
	if err != nil {
		return "", fmt.Errorf("decode module account: %w", err)
	}
	baseAccount, err := protoField(moduleAccount, 1)
	if err != nil {
		return "", fmt.Errorf("decode module base account: %w", err)
	}
	address, err := protoField(baseAccount, 1)
	if err != nil {
		return "", fmt.Errorf("decode module account address: %w", err)
	}
	if len(address) == 0 {
		return "", fmt.Errorf("the chain returned a module account named %q with no address", name)
	}
	return string(address), nil
}

// ---- transfer events ----

// chainReceipt is one incoming transfer credited to one address.
//
// Amount is a big.Int because a Cosmos amount is a 256-bit decimal
// string. Nothing on this chain comes close to overflowing a uint64
// today, but a total that wrapped would be a wrong number presented with
// the same confidence as a right one, and the fix costs a stdlib import.
type chainReceipt struct {
	Height int64
	TxHash string
	Sender string
	Denom  string
	Amount *big.Int
}

// receiptsCreditedTo extracts the transfer events in one search page that
// credit `address` in `denom`.
//
// TWO filters, and both are load-bearing:
//
//   - PER EVENT, never per transaction. A transaction matches the search
//     when ANY of its transfer events names the address, and a settlement
//     chunk or a multi-recipient bank send carries one event per
//     recipient. Crediting a matched transaction's whole transfer set
//     hands this participant other people's money — the devnet fixture in
//     testdata/fixtures/chain is exactly that transaction, three transfer
//     events of which one is ours.
//   - Delivered transactions only. A failed transaction moved nothing,
//     and its ante-handler events are still indexed and still returned by
//     the search.
func receiptsCreditedTo(page *txSearchPage, address, denom string) []chainReceipt {
	var out []chainReceipt
	for _, tx := range page.Txs {
		if tx.TxResult.Code != 0 {
			continue
		}
		height, err := strconv.ParseInt(tx.Height, 10, 64)
		if err != nil {
			// A result whose height does not parse is not usable
			// evidence of anything; dropping it is safer than
			// reporting an amount at height zero.
			continue
		}
		for _, ev := range tx.TxResult.Events {
			if ev.Type != "transfer" || ev.attr("recipient") != address {
				continue
			}
			amount := coinAmount(ev.attr("amount"), denom)
			if amount == nil || amount.Sign() <= 0 {
				continue
			}
			out = append(out, chainReceipt{
				Height: height,
				TxHash: tx.Hash,
				Sender: ev.attr("sender"),
				Denom:  denom,
				Amount: amount,
			})
		}
	}
	return out
}

// coinAmount reads one denomination out of a Cosmos coin string
// ("24971400utwlt", or "100utwlt,5ibc/ABCD" for a multi-denom transfer),
// returning nil when the denomination is absent.
//
// The split point is the first non-digit: an SDK denomination must begin
// with a letter, so the leading digits are always the whole amount and
// never part of the name.
func coinAmount(coins, denom string) *big.Int {
	for _, part := range strings.Split(coins, ",") {
		part = strings.TrimSpace(part)
		i := 0
		for i < len(part) && part[i] >= '0' && part[i] <= '9' {
			i++
		}
		if i == 0 || i == len(part) || part[i:] != denom {
			continue
		}
		if v, ok := new(big.Int).SetString(part[:i], 10); ok {
			return v
		}
	}
	return nil
}
