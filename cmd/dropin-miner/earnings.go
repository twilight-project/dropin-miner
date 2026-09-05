package main

// `earnings` — what the chain has actually paid.
//
// Chain-first, and deliberately so. Everything else a participant can run
// asks the AS, which can only report what it BELIEVES: that an observation
// verified, that an epoch closed, that an allocation was planned. None of
// that is money. The only authority on money is the chain, and this command
// reads it directly.
//
// THE ADDRESS IS THE REGISTERED PAYOUT ADDRESS, read from the AS, and not
// the wallet's. A participant who registered an external address — a Keplr
// account, an exchange deposit, a colleague's wallet — has no local wallet at
// all, and is exactly the person who most needs to see whether they have been
// paid. So nothing here touches the wallet, and `earnings` runs with no
// wallet present. `-address` names one directly for the case where the AS is
// unreachable and a participant already knows their address.
//
// LABELING. A balance is not earnings: a balance rises because somebody sent
// you tokens, and calling that a mining payment would be a lie a participant
// repeats back to us. Only a transfer RELEASED BY THE CHAIN'S REWARD ESCROW
// is a mining payment. That escrow is the x/rewards module account —
// SendCoinsFromModuleToAccount is how every participant release and every
// operator remainder leaves it, and no key exists that can spend from it
// otherwise. Its address is read from the chain by module name; when that
// read fails the receipts are shown UNLABELED rather than guessed at.
//
// It is NOT the Slot's settlement_address. That address signs
// MsgSubmitSettlementChunk and pays its fee, so it shows up on a payout
// transaction as message.sender and fee_payer — never as the transfer's
// sender. Labeling on it would classify every real payment as an ordinary
// transfer, and would let anyone holding that ordinary account mark a plain
// send as a mining payment.
//
// WHAT IS NOT HERE. No epoch column: recovering which epoch a receipt settled
// needs MsgSubmitSettlementChunk decoded out of TxBody → messages → Any, and
// the transfer event does not carry it. No --watch and no notification.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/big"
	"strings"
	"time"

	"github.com/twilight-project/dropin-miner/pkg/auth"
	"github.com/twilight-project/dropin-miner/pkg/config"
	"github.com/twilight-project/dropin-miner/pkg/redact"
)

const (
	// rewardEscrowModule is the chain module that owns the reward escrow.
	// Every participant release and every operator remainder leaves it
	// through SendCoinsFromModuleToAccount, so it is the sender on every
	// mining payment and on nothing else.
	rewardEscrowModule = "rewards"

	// escrowAddressEnv names the escrow address without a flag, for a
	// deployment that would rather pin it than have it read per invocation.
	escrowAddressEnv = "TOKENDROP_REWARD_ESCROW"

	// defaultEarningsLimit is how many receipts are listed by default.
	// Every receipt found is counted in the totals; this only bounds the
	// listing, and the block-time lookups that go with it.
	defaultEarningsLimit = 20
)

// receiptLabel is the whole classification vocabulary, and it has three
// values because "we could not tell" must not collapse into either verdict.
type receiptLabel int

const (
	// labelMining: released by the reward escrow. A mining payment.
	labelMining receiptLabel = iota
	// labelOther: an ordinary incoming transfer. Somebody sent you tokens.
	labelOther
	// labelUnlabeled: the escrow address was not established, so nothing
	// here is known to be a mining payment or known not to be.
	labelUnlabeled
)

// labelReceipt classifies one receipt.
//
// The only way to reach labelMining is an exact match against an escrow
// address the caller established. Empty escrow — the read failed, and no
// override was given — reaches labelUnlabeled for every input, which is why
// an unknown escrow cannot silently mark everything as mining. A sender that
// merely resembles the escrow address is labelOther, like any other sender.
func labelReceipt(r chainReceipt, escrow string) receiptLabel {
	if escrow == "" {
		return labelUnlabeled
	}
	if r.Sender == escrow {
		return labelMining
	}
	return labelOther
}

// earningsReport is everything the printer needs, with no I/O left in it.
type earningsReport struct {
	Address string
	Denom   string

	// Escrow is empty when it could not be established; EscrowErr says why.
	Escrow    string
	EscrowErr error

	Mining      []chainReceipt
	Other       []chainReceipt
	Unlabeled   []chainReceipt
	MiningTotal *big.Int
	OtherTotal  *big.Int
	AllTotal    *big.Int

	// Truncated means the search walk hit its page ceiling, so every total
	// above is a floor. Nothing may present a floor as a total.
	Truncated  bool
	TotalCount uint64

	Balance    string
	BalanceErr error

	// The AS half. Epoch is meaningful only when EpochKnown.
	Epoch       uint64
	EpochKnown  bool
	Activity    *auth.EpochActivity
	ActivityErr error

	// Limit is how many receipts of each kind are listed; 0 lists all.
	Limit int
	// Times carries block timestamps by height for the listed receipts
	// only; an absent entry prints as a height with no date.
	Times map[int64]time.Time
}

// buildEarningsReport sorts and totals the receipts. Pure.
func buildEarningsReport(receipts []chainReceipt, escrow string, escrowErr error) *earningsReport {
	r := &earningsReport{
		Escrow:      escrow,
		EscrowErr:   escrowErr,
		MiningTotal: new(big.Int),
		OtherTotal:  new(big.Int),
		AllTotal:    new(big.Int),
	}
	for _, rec := range receipts {
		r.AllTotal.Add(r.AllTotal, rec.Amount)
		switch labelReceipt(rec, escrow) {
		case labelMining:
			r.Mining = append(r.Mining, rec)
			r.MiningTotal.Add(r.MiningTotal, rec.Amount)
		case labelOther:
			r.Other = append(r.Other, rec)
			r.OtherTotal.Add(r.OtherTotal, rec.Amount)
		default:
			r.Unlabeled = append(r.Unlabeled, rec)
		}
	}
	return r
}

// errNoASConfigured is the activity line's reason when the command ran
// with an explicit -address and no config: nothing was wrong, there was
// simply nobody to ask.
var errNoASConfigured = errors.New("this invocation named an address directly and has no AS configured")

func cmdEarnings(args []string, stdout, stderr io.Writer, getenv func(string) string) int {
	fs := newFlagSet("earnings", stderr)
	cfgPath := fs.String("config", "", "path to TOML config file")
	node := fs.String("node", "", "CometBFT RPC endpoint (default: "+walletNodeEnv+" or the devnet)")
	address := fs.String("address", "", "the address to read (default: the payout address the AS holds for you)")
	denom := fs.String("denom", defaultWalletDenom, "denomination to report")
	escrow := fs.String("escrow", "", "the chain account mining payments are released from "+
		"(default: the "+rewardEscrowModule+" module account, read from the chain; or "+escrowAddressEnv+")")
	limit := fs.Int("limit", defaultEarningsLimit, "how many receipts to list; 0 lists every one found")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *limit < 0 {
		fmt.Fprintln(stderr, "dropin-miner: -limit cannot be negative; 0 lists every receipt")
		return exitUsage
	}

	ctx, cancel := operatorContext(5 * time.Minute)
	defer cancel()

	// The AS is consulted for two things, and an explicit -address makes
	// the first of them unnecessary: which address to read, and this
	// epoch's activity line. With an address already in hand and no config
	// to be found, the AS is skipped rather than made to fail noisily —
	// which is what lets this run on a machine that holds nothing but the
	// binary and an address on a piece of paper.
	var (
		as     asClient
		m      config.Mining
		haveAS bool
	)
	target := strings.TrimSpace(*address)
	if target == "" || describeConfigSource(*cfgPath, getenv) != "" {
		_, mc, mcfg, code := miningClients(ctx, []string{"-config", *cfgPath}, "earnings")
		switch {
		case code == exitOK:
			as, m, haveAS = mc, mcfg, true
		case target == "":
			// miningClients has already said which config problem this is.
			fmt.Fprintln(stderr, "dropin-miner: with no config there is no AS to ask where you are paid.")
			fmt.Fprintln(stderr, "  Name the address yourself: dropin-miner earnings -address <twilight1...>")
			return code
		}
	}

	if target == "" {
		var code int
		target, code = earningsAddressFromAS(ctx, as, stdout, stderr)
		if code != exitOK || target == "" {
			// An empty address with exitOK is the "nothing is paid to you
			// and here is why" answer, already printed in full.
			return code
		}
	}
	if _, _, err := auth.DecodeBech32Address(target); err != nil {
		// Validated before it is interpolated into a CometBFT query, so a
		// value that is not an address can never reach the query language.
		fmt.Fprintf(stderr, "dropin-miner: %q is not a valid address: %v\n", target, err)
		return exitUsage
	}

	c := newRPCClient(walletNode(*node, getenv))

	escrowAddr, escrowErr := resolveEscrow(ctx, c, strings.TrimSpace(*escrow), getenv)

	receipts, total, truncated, err := collectReceipts(ctx, c, target, *denom, txSearchPageSize, txSearchMaxPages)
	if err != nil {
		fmt.Fprintln(stderr, "dropin-miner: cannot read this address's transactions:", redact.Error(err))
		fmt.Fprintln(stderr, "  This needs a node with tx_index.indexer = \"kv\"; -node names a different one.")
		return exitTransport
	}

	rep := buildEarningsReport(receipts, escrowAddr, escrowErr)
	rep.Address, rep.Denom, rep.Limit = target, *denom, *limit
	rep.Truncated, rep.TotalCount = truncated, total
	rep.Balance, rep.BalanceErr = c.balance(ctx, target, *denom)
	rep.Times = resolveReceiptTimes(ctx, c, rep)

	if haveAS {
		epoch, known, epochErr := epochFor(ctx, as, m.TargetEpoch)
		rep.Epoch, rep.EpochKnown = epoch, known
		switch {
		case epochErr != nil:
			rep.ActivityErr = epochErr
		case known:
			rep.Activity, rep.ActivityErr = as.EpochActivity(ctx, epoch)
		}
	} else {
		rep.ActivityErr = errNoASConfigured
	}

	printEarnings(stdout, rep)
	return exitOK
}

// earningsAddressFromAS reads the address in force. A proposal that is not
// in force is not an address anything is paid to, and reporting receipts
// against it would tell a participant their setup works.
func earningsAddressFromAS(ctx context.Context, as asClient, stdout, stderr io.Writer) (string, int) {
	if as == nil {
		fmt.Fprintln(stderr, "dropin-miner: no AS to ask where you are paid.")
		fmt.Fprintln(stderr, "  Name the address yourself: dropin-miner earnings -address <twilight1...>")
		return "", exitTransport
	}
	standing, err := as.PayoutStanding(ctx)
	if err != nil {
		fmt.Fprintln(stderr, "dropin-miner: could not ask the AS where you are paid:", redact.Error(err))
		fmt.Fprintln(stderr, "  It works offline if you name the address: dropin-miner earnings -address <twilight1...>")
		return "", exitTransport
	}
	if standing.Active != nil {
		return payoutShown(standing.Active), exitOK
	}
	// Not an error: "nothing has been paid to you, and here is why" is the
	// true answer, and it is the answer this command exists to give.
	if standing.Pending != nil {
		fmt.Fprintln(stdout, "  paid            nothing — no payout address is in force yet.")
		fmt.Fprintf(stdout, "                  %s is proposed and NOT active; nothing is paid to it.\n",
			payoutShown(standing.Pending))
		if standing.Pending.HeldFor == auth.HeldAddressInUse {
			fmt.Fprintln(stdout, "                  It is registered to another participant and will not be activated.")
		} else {
			fmt.Fprintln(stdout, "                  A Slot operator must activate it. Ask them to; no command here can.")
		}
		fmt.Fprintln(stdout, "\n  To read that address on the chain anyway: earnings -address "+payoutShown(standing.Pending))
		return "", exitOK
	}
	fmt.Fprintln(stdout, "  paid            nothing — you have not registered a payout address,")
	fmt.Fprintln(stdout, "                  so there is nowhere for a mining payment to go.")
	fmt.Fprintln(stdout, "\n  Register one:   dropin-miner payout set <twilight1...>")
	fmt.Fprintln(stdout, "                  dropin-miner wallet register   (if this machine holds the wallet)")
	return "", exitOK
}

// resolveEscrow establishes the address mining payments are released from:
// the flag, then the environment, then the chain.
//
// A failed chain read is returned, never swallowed: the caller degrades to
// showing every incoming transfer unlabeled, which is honest, where a
// guessed escrow would silently mislabel.
func resolveEscrow(ctx context.Context, c *rpcClient, flagValue string, getenv func(string) string) (string, error) {
	if flagValue != "" {
		if _, _, err := auth.DecodeBech32Address(flagValue); err != nil {
			return "", fmt.Errorf("-escrow %q is not a valid address: %w", flagValue, err)
		}
		return flagValue, nil
	}
	if v := strings.TrimSpace(getenv(escrowAddressEnv)); v != "" {
		if _, _, err := auth.DecodeBech32Address(v); err != nil {
			return "", fmt.Errorf("%s=%q is not a valid address: %w", escrowAddressEnv, v, err)
		}
		return v, nil
	}
	addr, err := c.moduleAccountAddress(ctx, rewardEscrowModule)
	if err != nil {
		return "", err
	}
	if _, _, err := auth.DecodeBech32Address(addr); err != nil {
		return "", fmt.Errorf("the chain returned an unreadable %s module address: %w", rewardEscrowModule, err)
	}
	return addr, nil
}

// collectReceipts walks every page of the recipient search.
//
// truncated=true means the walk hit its ceiling and the caller holds a
// floor, not a total.
// perPage and maxPages are parameters rather than the constants they are
// called with, so the walk can be driven against the captured devnet pages
// at the page size they were actually captured with — a re-paginating fake
// would be a fixture again.
func collectReceipts(ctx context.Context, c *rpcClient, address, denom string, perPage, maxPages int) (
	receipts []chainReceipt, totalCount uint64, truncated bool, err error) {
	query := fmt.Sprintf("transfer.recipient='%s'", address)
	seen := 0
	for page := 1; page <= maxPages; page++ {
		res, err := c.txSearch(ctx, query, page, perPage)
		if err != nil {
			return nil, 0, false, err
		}
		if page == 1 {
			totalCount = parseCount(res.TotalCount)
		}
		receipts = append(receipts, receiptsCreditedTo(res, address, denom)...)
		seen += len(res.Txs)
		// Two independent stops, because either alone has a failure mode:
		// a short page is the last page, and the reported total guards
		// against a node that keeps answering full pages.
		if len(res.Txs) < perPage || uint64(seen) >= totalCount {
			return receipts, totalCount, false, nil
		}
	}
	return receipts, totalCount, true, nil
}

func parseCount(s string) uint64 {
	var n uint64
	for _, ch := range s {
		if ch < '0' || ch > '9' {
			return 0
		}
		n = n*10 + uint64(ch-'0')
	}
	return n
}

// resolveReceiptTimes fetches block times for the receipts that will be
// LISTED, and only those: a header call per height is the expensive part,
// and an address with two hundred receipts does not need two hundred of
// them to print a total. A failed lookup leaves the height without a date
// rather than failing the command.
func resolveReceiptTimes(ctx context.Context, c *rpcClient, r *earningsReport) map[int64]time.Time {
	times := map[int64]time.Time{}
	for _, group := range [][]chainReceipt{r.Mining, r.Other, r.Unlabeled} {
		for _, rec := range listed(group, r.Limit) {
			if _, ok := times[rec.Height]; ok {
				continue
			}
			if t, err := c.blockTime(ctx, rec.Height); err == nil {
				times[rec.Height] = t
			}
		}
	}
	return times
}

func listed(recs []chainReceipt, limit int) []chainReceipt {
	if limit <= 0 || len(recs) <= limit {
		return recs
	}
	return recs[:limit]
}

// ---- presentation ----

func printEarnings(w io.Writer, r *earningsReport) {
	if r.Escrow == "" {
		printEarningsUnlabeled(w, r)
	} else {
		printEarningsLabeled(w, r)
	}
	printEarningsBalance(w, r)
	printEarningsEpoch(w, r)
	printEarningsFooter(w, r)
}

func printEarningsLabeled(w io.Writer, r *earningsReport) {
	fmt.Fprintf(w, "  %-15s %s\n", "paid", countAndTotal(len(r.Mining), r.MiningTotal, r.Denom, r.Truncated))
	printReceipts(w, r, r.Mining, false)
	if len(r.Other) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintf(w, "  %-15s %s   — ordinary incoming transfers, NOT mining payments\n",
			"also received", countAndTotal(len(r.Other), r.OtherTotal, r.Denom, r.Truncated))
		printReceipts(w, r, r.Other, true)
	}
}

func printEarningsUnlabeled(w io.Writer, r *earningsReport) {
	fmt.Fprintf(w, "  %-15s %s\n", "received", countAndTotal(len(r.Unlabeled), r.AllTotal, r.Denom, r.Truncated))
	printReceipts(w, r, r.Unlabeled, true)
	fmt.Fprintln(w)
	fmt.Fprintf(w, "  These are UNLABELED: the reward escrow address could not be read (%v),\n", redact.Error(r.EscrowErr))
	fmt.Fprintln(w, "  so none of them is known to be a mining payment. Some may be ordinary")
	fmt.Fprintln(w, "  transfers from anyone at all. Name the escrow to label them:")
	fmt.Fprintln(w, "      dropin-miner earnings -escrow <twilight1...>")
}

// countAndTotal renders "3 receipts   74,914,200 utwlt", or the same with
// "at least" in front of it when the walk was truncated. A floor is never
// printed as a total.
func countAndTotal(n int, total *big.Int, denom string, truncated bool) string {
	noun := "receipts"
	if n == 1 {
		noun = "receipt"
	}
	s := fmt.Sprintf("%d %-11s%s %s", n, noun, group(total.String()), denom)
	if truncated {
		return "at least " + s
	}
	return s
}

func printReceipts(w io.Writer, r *earningsReport, recs []chainReceipt, withSender bool) {
	shown := listed(recs, r.Limit)
	width := 0
	for _, rec := range shown {
		if n := len(group(rec.Amount.String())); n > width {
			width = n
		}
	}
	for _, rec := range shown {
		when := "                "
		if t, ok := r.Times[rec.Height]; ok {
			when = t.UTC().Format("2006-01-02 15:04")
		}
		line := fmt.Sprintf("    %s   %*s   height %d", when, width, group(rec.Amount.String()), rec.Height)
		if withSender && rec.Sender != "" {
			line += "   from " + rec.Sender
		}
		fmt.Fprintln(w, line)
	}
	if len(recs) > len(shown) {
		fmt.Fprintf(w, "    … %d more not listed; every one of them is in the total above (-limit 0 lists all)\n",
			len(recs)-len(shown))
	}
}

func printEarningsBalance(w io.Writer, r *earningsReport) {
	fmt.Fprintln(w)
	if r.BalanceErr != nil {
		fmt.Fprintf(w, "  %-15s unknown (%v)\n", "balance", redact.Error(r.BalanceErr))
	} else {
		fmt.Fprintf(w, "  %-15s %s %s\n", "balance", group(r.Balance), r.Denom)
	}
	// Printed in full, never elided. This is the address a participant has
	// to be able to recognize character by character — it is the one thing
	// standing between a mistyped declaration and money that never arrives
	// — and an ellipsis in the middle is exactly where a transcription
	// error hides.
	fmt.Fprintf(w, "  %-15s %s\n", "address", r.Address)
}

func printEarningsEpoch(w io.Writer, r *earningsReport) {
	fmt.Fprintln(w)
	switch {
	case r.ActivityErr != nil:
		if errors.Is(r.ActivityErr, errNoASConfigured) {
			fmt.Fprintf(w, "  %-15s not asked — this invocation named an address and has no AS configured\n", "this epoch")
			return
		}
		fmt.Fprintf(w, "  %-15s unknown — AS unreachable (%v)\n", "this epoch", redact.Error(r.ActivityErr))
		return
	case !r.EpochKnown:
		fmt.Fprintf(w, "  %-15s none open right now\n", "this epoch")
		return
	case r.Activity == nil:
		fmt.Fprintf(w, "  %-15s unknown — the AS reported no activity for epoch %d\n", "this epoch", r.Epoch)
		return
	}
	verdict := "not eligible"
	if r.Activity.Eligible() {
		verdict = "eligible"
	}
	fmt.Fprintf(w, "  %-15s %d — %s, %d verified (%d needed), %d rejected\n", "this epoch",
		r.Epoch, verdict,
		r.Activity.VerifiedObservationCount, auth.MinVerifiedObservations,
		r.Activity.RejectedObservationCount)
	if n := r.Activity.PendingObservationCount; n > 0 {
		fmt.Fprintf(w, "  %-15s %d not yet verified\n", "", n)
	}
}

func printEarningsFooter(w io.Writer, r *earningsReport) {
	fmt.Fprintln(w)
	if r.Escrow != "" {
		fmt.Fprintln(w, "  A mining payment is a transfer released by the chain's reward escrow")
		fmt.Fprintln(w, "  "+r.Escrow+".")
		fmt.Fprintln(w, "  Anything else that arrives is somebody sending you tokens, not earnings.")
	}
	fmt.Fprintln(w, "  An epoch's reward is an equal split among its eligible participants: one verified")
	fmt.Fprintln(w, "  observation qualifies, more do not earn more, and no amount belongs to any single")
	fmt.Fprintln(w, "  request. Times are UTC, from the block each receipt landed in.")
	if r.Truncated {
		fmt.Fprintf(w, "\n  The search stopped after %d pages, so every count and total above is a FLOOR,\n", txSearchMaxPages)
		fmt.Fprintln(w, "  not a total. The oldest receipts are the ones missing.")
	}
}

// group inserts thousands separators into a decimal string. It works on the
// string rather than an integer type because a Cosmos amount is a 256-bit
// decimal and must not be narrowed to be printed.
func group(s string) string {
	if s == "" {
		return "0"
	}
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	for _, ch := range s {
		if ch < '0' || ch > '9' {
			return s // not a plain decimal; print whatever the chain said
		}
	}
	var b strings.Builder
	for i, ch := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(ch)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}
