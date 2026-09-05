package main

// The wallet commands: the smallest thing that lets an agent-operated
// installation hold a reward address and move funds when the user asks.
//
// wallet init      generate the key, print the mnemonic ONCE, seal the key
// wallet address   print the twilight1... address (no passphrase needed)
// wallet register  declare that address as the payout destination
// wallet balance   what the chain says this address holds
// wallet send      move funds to another twilight address
//
// The mnemonic is the recovery instrument and exists only on the console
// at init; the keyfile is the operating instrument and never leaves the
// wallet dir. An agent drives these as plain subprocesses and is never
// shown the mnemonic, the passphrase, or the keyfile.

import (
	"bufio"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/twilight-project/dropin-miner/pkg/auth"
)

// walletPassphraseEnv lets non-interactive callers (setup scripts, agents
// the user has delegated to) supply the keyfile passphrase without a
// prompt. Never a flag: argv is visible to every process on the machine.
const walletPassphraseEnv = "TOKENDROP_WALLET_PASSPHRASE"

func cmdWallet(args []string, stdin io.Reader, stdout, stderr io.Writer, getenv func(string) string) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "dropin-miner: wallet needs a subcommand: init, address, register, balance or send")
		return exitUsage
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "init":
		return walletInit(rest, stdin, stdout, stderr, getenv)
	case "address":
		return walletAddress(rest, stdout, stderr, getenv)
	case "register":
		return walletRegister(rest, stdout, stderr, getenv)
	case "balance":
		return walletBalance(rest, stdout, stderr, getenv)
	case "send":
		return walletSend(rest, stdin, stdout, stderr, getenv)
	default:
		fmt.Fprintf(stderr, "dropin-miner: unknown wallet subcommand %q; want init, address, register, balance or send\n", sub)
		return exitUsage
	}
}

func walletInit(args []string, stdin io.Reader, stdout, stderr io.Writer, getenv func(string) string) int {
	fs2 := newFlagSet("wallet init", stderr)
	dir := fs2.String("dir", "", "wallet directory (default: $TOKENDROP_WALLET_DIR, else the user config dir)")
	printAnyway := fs2.Bool("print-anyway", false,
		"print the mnemonic even when stdout is not a terminal (it will land in whatever captures the output)")
	if err := fs2.Parse(args); err != nil {
		return exitUsage
	}

	resolved, err := openWalletDir(*dir, getenv)
	if err != nil {
		fmt.Fprintln(stderr, "dropin-miner:", err)
		return exitTransport
	}
	// Refuse to overwrite: a second init would orphan whatever the first
	// address has accumulated or been registered for. There is no force
	// flag — recovering a wallet is `init` into a FRESH directory with the
	// mnemonic, not overwriting the old one in place.
	if _, err := os.Lstat(filepath.Join(resolved, walletKeyFile)); err == nil {
		fmt.Fprintf(stderr, "dropin-miner: a wallet already exists in %s; refusing to overwrite it.\n"+
			"If this address is registered as a payout destination, its key is the only way to spend what it receives.\n", resolved)
		return exitTransport
	} else if !errors.Is(err, fs.ErrNotExist) {
		fmt.Fprintln(stderr, "dropin-miner:", err)
		return exitTransport
	}

	// The mnemonic goes to a human's eyes, not a pipe, unless the caller
	// says otherwise out loud.
	if !*printAnyway && !isTerminal(stdout) {
		fmt.Fprintln(stderr, "dropin-miner: stdout is not a terminal; the mnemonic would be printed into a capture.\n"+
			"Run wallet init in an interactive terminal, or pass -print-anyway if you really mean this.")
		return exitUsage
	}

	passphrase, code := walletPassphrase(stdin, stderr, getenv, true)
	if code != 0 {
		return code
	}

	entropy := make([]byte, 32)
	if _, err := rand.Read(entropy); err != nil {
		fmt.Fprintln(stderr, "dropin-miner: entropy:", err)
		return exitTransport
	}
	mnemonic, err := auth.NewWalletMnemonic(entropy)
	if err != nil {
		fmt.Fprintln(stderr, "dropin-miner:", err)
		return exitTransport
	}
	key, err := auth.DeriveWalletKey(mnemonic)
	if err != nil {
		fmt.Fprintln(stderr, "dropin-miner:", err)
		return exitTransport
	}
	address, err := key.Address(auth.TwilightHRP)
	if err != nil {
		fmt.Fprintln(stderr, "dropin-miner:", err)
		return exitTransport
	}

	kf, err := auth.SealWalletKey(key, passphrase)
	if err != nil {
		fmt.Fprintln(stderr, "dropin-miner:", err)
		return exitTransport
	}
	if err := writeWalletFile(resolved, walletKeyFile, kf); err != nil {
		fmt.Fprintln(stderr, "dropin-miner:", err)
		return exitTransport
	}
	if err := writeWalletFile(resolved, walletSidecarFile, &sidecar{
		Address: address,
		PubKey:  key.PubKeyHex(),
		Path:    auth.WalletHDPath,
	}); err != nil {
		fmt.Fprintln(stderr, "dropin-miner:", err)
		return exitTransport
	}

	// The one and only appearance of the mnemonic, anywhere.
	fmt.Fprintln(stdout, "recovery phrase (shown ONCE, stored NOWHERE — written down or lost):")
	fmt.Fprintln(stdout)
	words := strings.Fields(mnemonic)
	for i := 0; i < len(words); i += 6 {
		end := i + 6
		if end > len(words) {
			end = len(words)
		}
		fmt.Fprintf(stdout, "    %s\n", strings.Join(words[i:end], " "))
	}
	fmt.Fprintln(stdout)
	fmt.Fprintln(stdout, "Anyone with these words controls every token this wallet ever receives.")
	fmt.Fprintln(stdout, "The encrypted key lives in "+resolved+"; the passphrase opens it for spending.")
	fmt.Fprintln(stdout)
	fmt.Fprintln(stdout, "address: "+address)
	fmt.Fprintln(stdout)
	fmt.Fprintln(stdout, "Next: register it as your payout destination:")
	fmt.Fprintln(stdout, "    dropin-miner wallet register -config <file>")
	return 0
}

func walletAddress(args []string, stdout, stderr io.Writer, getenv func(string) string) int {
	fs2 := newFlagSet("wallet address", stderr)
	dir := fs2.String("dir", "", "wallet directory (default: $TOKENDROP_WALLET_DIR, else the user config dir)")
	if err := fs2.Parse(args); err != nil {
		return exitUsage
	}
	sc, _, err := loadSidecar(*dir, getenv)
	if err != nil {
		fmt.Fprintln(stderr, "dropin-miner:", err)
		return exitTransport
	}
	fmt.Fprintln(stdout, sc.Address)
	return 0
}

// walletRegister declares this wallet's address as the payout
// destination, so rewards settle somewhere this installation can spend
// from.
//
// It is deliberately the same act as `payout set <address>` — same
// client, same authorization, same AS route — with the address taken
// from the wallet instead of retyped. That is the whole point: the
// address a person copies between two windows is the address a person
// can copy wrong, and until WALLET_SIGNATURE_V1 exists nothing on the
// server side would catch it.
func walletRegister(args []string, stdout, stderr io.Writer, getenv func(string) string) int {
	fs2 := newFlagSet("wallet register", stderr)
	dir := fs2.String("dir", "", "wallet directory (default: $TOKENDROP_WALLET_DIR, else the user config dir)")
	cfgPath := fs2.String("config", "", "path to TOML config file")
	if err := fs2.Parse(args); err != nil {
		return exitUsage
	}
	sc, _, err := loadSidecar(*dir, getenv)
	if err != nil {
		fmt.Fprintln(stderr, "dropin-miner:", err)
		return exitTransport
	}

	// Two minutes, matching cmdPayout: this is one authenticated round
	// trip, and the refresh-token lock may hold it briefly behind a live
	// daemon.
	ctx, cancel := operatorContext(2 * time.Minute)
	defer cancel()

	_, mining, _, code := miningClients(ctx, []string{"-config", *cfgPath}, "wallet register")
	if code != 0 {
		return code
	}
	doc, err := mining.DeclarePayoutAddress(ctx, sc.Address)
	if err != nil {
		fmt.Fprintln(stderr, "dropin-miner: payout declaration failed:", err)
		return exitTransport
	}
	// The same reporting as `payout set`, including the canonical
	// read-back and what a held declaration still needs.
	printDeclaration(doc)
	return 0
}

// walletPassphrase resolves the keyfile passphrase: the env var when set,
// else read from stdin — twice on create, once otherwise. Terminal echo is
// not suppressed (that needs a terminal-control dependency this repo does
// not carry); the provider command set the precedent of reading secrets
// plainly from stdin, and the env var is the non-interactive path.
func walletPassphrase(stdin io.Reader, stderr io.Writer, getenv func(string) string, create bool) (string, int) {
	if p := getenv(walletPassphraseEnv); p != "" {
		return p, 0
	}
	r := bufio.NewReader(stdin)
	fmt.Fprint(stderr, "keyfile passphrase: ")
	first, err := r.ReadString('\n')
	if err != nil && first == "" {
		fmt.Fprintln(stderr, "\ndropin-miner: no passphrase provided (set "+walletPassphraseEnv+" for non-interactive use)")
		return "", exitUsage
	}
	first = strings.TrimRight(first, "\r\n")
	if first == "" {
		fmt.Fprintln(stderr, "dropin-miner: an empty passphrase would store the key effectively unencrypted; refusing")
		return "", exitUsage
	}
	if !create {
		return first, 0
	}
	fmt.Fprint(stderr, "again: ")
	second, _ := r.ReadString('\n')
	if strings.TrimRight(second, "\r\n") != first {
		fmt.Fprintln(stderr, "dropin-miner: passphrases do not match")
		return "", exitUsage
	}
	return first, 0
}

// isTerminal reports whether w is a character device, without a terminal
// library: buffers and pipes are not, consoles are.
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// walletBalance reports what the chain says this address holds.
func walletBalance(args []string, stdout, stderr io.Writer, getenv func(string) string) int {
	fs2 := newFlagSet("wallet balance", stderr)
	dir := fs2.String("dir", "", "wallet directory (default: $TOKENDROP_WALLET_DIR, else the user config dir)")
	node := fs2.String("node", "", "CometBFT RPC endpoint (default: "+walletNodeEnv+" or the devnet)")
	denom := fs2.String("denom", defaultWalletDenom, "denomination to report")
	address := fs2.String("address", "", "address to query (default: this wallet's own)")
	if err := fs2.Parse(args); err != nil {
		return exitUsage
	}

	target := *address
	if target == "" {
		sc, _, err := loadSidecar(*dir, getenv)
		if err != nil {
			fmt.Fprintln(stderr, "dropin-miner:", err)
			return exitTransport
		}
		target = sc.Address
	}
	if _, _, err := auth.DecodeBech32Address(target); err != nil {
		fmt.Fprintf(stderr, "dropin-miner: %q is not a valid address: %v\n", target, err)
		return exitUsage
	}

	ctx, cancel := operatorContext(1 * time.Minute)
	defer cancel()
	c := newRPCClient(walletNode(*node, getenv))
	amount, err := c.balance(ctx, target, *denom)
	if err != nil {
		fmt.Fprintln(stderr, "dropin-miner: balance:", err)
		return exitTransport
	}
	fmt.Fprintf(stdout, "%s %s\n", amount, *denom)
	return 0
}

// walletSend moves funds to another twilight address.
//
// The confirmation is interactive by default and skipped with -yes. That
// is the seam an agent uses: a person delegating "send 5 TWLT to X" to
// an agent has already confirmed it, and a prompt no one can answer
// would just hang. Everything a mistake would need to get past — the
// address decoding under the right prefix, the amount being an integer
// of the base denomination, the chain id coming from the node rather
// than a guess — is checked whether or not anyone is watching.
func walletSend(args []string, stdin io.Reader, stdout, stderr io.Writer, getenv func(string) string) int {
	fs2 := newFlagSet("wallet send", stderr)
	dir := fs2.String("dir", "", "wallet directory (default: $TOKENDROP_WALLET_DIR, else the user config dir)")
	node := fs2.String("node", "", "CometBFT RPC endpoint (default: "+walletNodeEnv+" or the devnet)")
	to := fs2.String("to", "", "recipient twilight1... address")
	amount := fs2.String("amount", "", "amount in the base denomination, e.g. 1000000")
	denom := fs2.String("denom", defaultWalletDenom, "denomination to send")
	memo := fs2.String("memo", "", "optional memo, recorded on chain in the clear")
	gas := fs2.Uint64("gas", defaultWalletGas, "gas limit")
	fee := fs2.String("fee", strconv.FormatUint(defaultWalletFee, 10), "fee amount in the same denomination")
	yes := fs2.Bool("yes", false, "skip the confirmation prompt (what an agent passes)")
	if err := fs2.Parse(args); err != nil {
		return exitUsage
	}

	// Validate before anything is decrypted or dialed.
	if *to == "" || *amount == "" {
		fmt.Fprintln(stderr, "dropin-miner: wallet send needs -to <address> and -amount <integer>")
		return exitUsage
	}
	hrp, _, err := auth.DecodeBech32Address(*to)
	if err != nil {
		fmt.Fprintf(stderr, "dropin-miner: -to %q is not a valid address: %v\n", *to, err)
		return exitUsage
	}
	// A well-formed address for ANOTHER chain is the expensive mistake
	// here: funds sent to a prefix this chain does not recognize are gone.
	if hrp != auth.TwilightHRP {
		fmt.Fprintf(stderr, "dropin-miner: -to is a %q address, not %q; refusing to send across chains\n", hrp, auth.TwilightHRP)
		return exitUsage
	}
	if !isPositiveInteger(*amount) {
		fmt.Fprintf(stderr, "dropin-miner: -amount %q must be a positive integer of %s (the base denomination, not a decimal)\n", *amount, *denom)
		return exitUsage
	}
	if !isPositiveInteger(*fee) {
		fmt.Fprintf(stderr, "dropin-miner: -fee %q must be a positive integer\n", *fee)
		return exitUsage
	}

	sc, resolved, err := loadSidecar(*dir, getenv)
	if err != nil {
		fmt.Fprintln(stderr, "dropin-miner:", err)
		return exitTransport
	}
	if sc.Address == *to {
		fmt.Fprintln(stderr, "dropin-miner: -to is this wallet's own address; nothing to do")
		return exitUsage
	}

	ctx, cancel := signalContext()
	defer cancel()
	c := newRPCClient(walletNode(*node, getenv))

	// The chain id comes from the node: signing for the wrong chain
	// produces a signature that is silently invalid, and a hardcoded
	// string is how that happens.
	chainID, err := c.chainID(ctx)
	if err != nil {
		fmt.Fprintln(stderr, "dropin-miner: cannot reach the node:", err)
		return exitTransport
	}
	acct, err := c.account(ctx, sc.Address)
	if err != nil {
		if errors.Is(err, errNoAccount) {
			fmt.Fprintf(stderr, "dropin-miner: %s has never received funds, so there is nothing to send\n", sc.Address)
			return exitTransport
		}
		fmt.Fprintln(stderr, "dropin-miner: account:", err)
		return exitTransport
	}

	if !*yes {
		fmt.Fprintf(stdout, "send %s %s\n", *amount, *denom)
		fmt.Fprintf(stdout, "  from:  %s\n", sc.Address)
		fmt.Fprintf(stdout, "  to:    %s\n", *to)
		fmt.Fprintf(stdout, "  fee:   %s %s (gas %d)\n", *fee, *denom, *gas)
		fmt.Fprintf(stdout, "  chain: %s\n", chainID)
		if *memo != "" {
			fmt.Fprintf(stdout, "  memo:  %s (public, on chain forever)\n", *memo)
		}
		fmt.Fprint(stdout, "\nThis cannot be undone. Type yes to send: ")
		line, _ := bufio.NewReader(stdin).ReadString('\n')
		if strings.TrimSpace(line) != "yes" {
			fmt.Fprintln(stdout, "canceled; nothing was signed or sent")
			return exitOK
		}
	}

	passphrase, code := walletPassphrase(stdin, stderr, getenv, false)
	if code != 0 {
		return code
	}
	var kf auth.WalletKeyfile
	if err := readWalletFile(resolved, walletKeyFile, &kf); err != nil {
		fmt.Fprintln(stderr, "dropin-miner: keyfile:", err)
		return exitTransport
	}
	key, err := auth.OpenWalletKey(&kf, passphrase)
	if err != nil {
		fmt.Fprintln(stderr, "dropin-miner:", err)
		return exitTransport
	}

	txRaw, err := buildSignedSend(key, sendParams{
		From:      sc.Address,
		To:        *to,
		Denom:     *denom,
		Amount:    *amount,
		Memo:      *memo,
		ChainID:   chainID,
		AccountNo: acct.AccountNumber,
		Sequence:  acct.Sequence,
		Gas:       *gas,
		FeeAmount: *fee,
	})
	if err != nil {
		fmt.Fprintln(stderr, "dropin-miner: build transaction:", err)
		return exitTransport
	}

	res, err := c.broadcast(ctx, txRaw)
	if err != nil {
		fmt.Fprintln(stderr, "dropin-miner: broadcast:", err)
		return exitTransport
	}
	if res.Code != 0 {
		// Rejected at the mempool: never executed, nothing moved.
		fmt.Fprintf(stderr, "dropin-miner: the chain rejected this transaction (code %d): %s\n", res.Code, res.Log)
		return exitChainRejected
	}
	fmt.Fprintf(stdout, "submitted: %s\n", res.Hash)

	// Acceptance into the mempool is not execution. Waiting is the
	// difference between reporting a transfer and reporting an attempt.
	waitCtx, waitCancel := context.WithTimeout(ctx, 60*time.Second)
	defer waitCancel()
	tx, err := c.waitForTx(waitCtx, res.Hash, 2*time.Second)
	if err != nil {
		fmt.Fprintf(stderr, "dropin-miner: submitted, but it has not appeared in a block yet: %v\n", err)
		fmt.Fprintf(stderr, "  check later: %s/tx?hash=0x%s\n", walletNode(*node, getenv), res.Hash)
		return exitTransport
	}
	if tx.TxResult.Code != 0 {
		fmt.Fprintf(stderr, "dropin-miner: included at height %s but FAILED (code %d): %s\n",
			tx.Height, tx.TxResult.Code, tx.TxResult.Log)
		return exitChainRejected
	}
	fmt.Fprintf(stdout, "confirmed in block %s\n", tx.Height)
	return exitOK
}

// walletNodeEnv names the node endpoint without a flag, for agents and
// scripts that should not carry it in every invocation.
const walletNodeEnv = "TOKENDROP_WALLET_NODE"

func walletNode(flagValue string, getenv func(string) string) string {
	if flagValue != "" {
		return flagValue
	}
	if v := getenv(walletNodeEnv); v != "" {
		return v
	}
	return defaultWalletNodeURL
}

// isPositiveInteger accepts only base-denomination integers: "1.5" is
// the mistake this catches, since the chain has no decimals and would
// read a truncated or rejected value.
func isPositiveInteger(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return strings.Trim(s, "0") != ""
}
