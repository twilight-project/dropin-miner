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
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/twilight-project/dropin-miner/pkg/auth"
	"github.com/twilight-project/dropin-miner/pkg/config"
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
	// Fast path only, unlocked: avoids prompting for a passphrase when
	// this will certainly refuse. createOrRecoverWallet's own check,
	// taken under wallet.lock, is what actually decides — a wallet
	// created by a racing caller between this check and the call below
	// is still caught there.
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

	passphrase, code := walletPassphrase(stdin, bufio.NewReader(stdin), stderr, getenv, true)
	if code != 0 {
		return code
	}

	address, mnemonic, outcome, err := createOrRecoverWallet(resolved, passphrase, true)
	if err != nil {
		fmt.Fprintln(stderr, "dropin-miner:", err)
		return exitTransport
	}
	// wallet init refuses to overwrite (a documented, unchanged fact):
	// reuse/repair inside createOrRecoverWallet only happens as a race
	// with a concurrent creator, and init's own answer to that race is
	// the SAME refusal the pre-lock check above gives the common case —
	// never a silent success reporting someone else's address.
	if outcome != walletCreated {
		fmt.Fprintf(stderr, "dropin-miner: a wallet already exists in %s; refusing to overwrite it.\n"+
			"If this address is registered as a payout destination, its key is the only way to spend what it receives.\n", resolved)
		return exitTransport
	}

	printMnemonic(stdout, resolved, address, mnemonic)
	fmt.Fprintln(stdout)
	fmt.Fprintln(stdout, "Next: register it as your payout destination:")
	fmt.Fprintln(stdout, "    dropin-miner wallet register -config <file>")
	return 0
}

// walletCreationOutcome tells a caller what createOrRecoverWallet
// actually did, so it can decide whether to print the mnemonic (only
// ever for a freshly generated key) and how to report the result.
type walletCreationOutcome int

const (
	walletCreated  walletCreationOutcome = iota // a fresh key was generated
	walletReused                                // an existing key + sidecar; nothing written
	walletRepaired                              // an existing key; the sidecar was (re)written
)

// errWalletRepairNeedsPassphrase is returned when a key exists with no
// usable sidecar and no passphrase was available to unlock it with —
// the scripted path, where nobody is there to type one.
var errWalletRepairNeedsPassphrase = errors.New("wallet key present, sidecar missing; run `wallet init` at a terminal to repair")

// afterWalletKeyWritten is generateWalletFiles' crash-injection seam
// (§5 test 5): called after wallet.key is durably on disk and before
// wallet.pub is written, so a test can simulate the process dying in
// exactly that window without a real crash. The default does nothing.
var afterWalletKeyWritten = func() error { return nil }

// createOrRecoverWallet is the ONE wallet-creation path after this PR
// (REL-13): wallet init and mining enable's address question both call
// this rather than writing a key directly. It takes wallet.lock for its
// entire critical section — the check, the crypto, and every write —
// so two concurrent callers for the same directory can never both
// generate: exactly one does, and every other caller discovers that
// result and returns the SAME address, never a second key silently
// replacing the first's.
//
// havePassphrase distinguishes "no passphrase is available to attempt
// a repair with" (a scripted path with none configured) from an empty
// string, which is already refused elsewhere as unsafe; repair without
// one refuses rather than ever generating a second key next to a key
// it could not read.
func createOrRecoverWallet(dir, passphrase string, havePassphrase bool) (address, mnemonic string, outcome walletCreationOutcome, err error) {
	release, err := lockWalletDir(dir, walletLockTimeout)
	if err != nil {
		return "", "", 0, err
	}
	defer release()

	var kf auth.WalletKeyfile
	haveKey := true
	if kerr := readWalletFile(dir, walletKeyFile, &kf); kerr != nil {
		if !errors.Is(kerr, fs.ErrNotExist) {
			return "", "", 0, fmt.Errorf("wallet: read existing key: %w", kerr)
		}
		haveKey = false
	}

	if !haveKey {
		address, mnemonic, err = generateWalletFiles(dir, passphrase)
		if err != nil {
			return "", "", 0, err
		}
		return address, mnemonic, walletCreated, nil
	}

	// A key is already on disk. What this call may still be missing is
	// the sidecar, not a second key.
	var sc sidecar
	sidecarOK := readWalletFile(dir, walletSidecarFile, &sc) == nil
	if sidecarOK {
		if _, _, derr := auth.DecodeBech32Address(sc.Address); derr != nil {
			sidecarOK = false
		}
	}
	if sidecarOK {
		return sc.Address, "", walletReused, nil
	}

	if !havePassphrase {
		return "", "", 0, errWalletRepairNeedsPassphrase
	}
	address, err = repairSidecar(dir, &kf, passphrase)
	if err != nil {
		return "", "", 0, err
	}
	return address, "", walletRepaired, nil
}

// generateWalletFiles is the fresh-key-only path: entropy -> mnemonic ->
// derive -> seal -> write the keyfile and sidecar into dir. Called only
// from inside createOrRecoverWallet's lock, only when no key exists.
func generateWalletFiles(dir, passphrase string) (address, mnemonic string, err error) {
	entropy := make([]byte, 32)
	if _, err := rand.Read(entropy); err != nil {
		return "", "", fmt.Errorf("entropy: %w", err)
	}
	mnemonic, err = auth.NewWalletMnemonic(entropy)
	if err != nil {
		return "", "", err
	}
	key, err := auth.DeriveWalletKey(mnemonic)
	if err != nil {
		return "", "", err
	}
	address, err = key.Address(auth.TwilightHRP)
	if err != nil {
		return "", "", err
	}
	kf, err := auth.SealWalletKey(key, passphrase)
	if err != nil {
		return "", "", err
	}
	if err := writeWalletFile(dir, walletKeyFile, kf); err != nil {
		return "", "", err
	}
	if err := afterWalletKeyWritten(); err != nil {
		return "", "", err
	}
	if err := writeWalletFile(dir, walletSidecarFile, &sidecar{
		Address: address,
		PubKey:  key.PubKeyHex(),
		Path:    auth.WalletHDPath,
	}); err != nil {
		return "", "", err
	}
	return address, mnemonic, nil
}

// repairSidecar rebuilds the public sidecar from the key already on
// disk: the sidecar's pubkey comes from the key, so repair needs the
// passphrase to unlock it, exactly the way a fresh creation does.
func repairSidecar(dir string, kf *auth.WalletKeyfile, passphrase string) (address string, err error) {
	key, err := auth.OpenWalletKey(kf, passphrase)
	if err != nil {
		return "", fmt.Errorf("wallet: repair: %w", err)
	}
	address, err = key.Address(auth.TwilightHRP)
	if err != nil {
		return "", err
	}
	if err := writeWalletFile(dir, walletSidecarFile, &sidecar{
		Address: address,
		PubKey:  key.PubKeyHex(),
		Path:    auth.WalletHDPath,
	}); err != nil {
		return "", err
	}
	return address, nil
}

// printMnemonic is the one and only appearance of a mnemonic, anywhere,
// shared by walletInit and the mining-enable flow (mining.go's
// askMiningQuestion) so the two paths that can create a wallet show
// identical words around it.
func printMnemonic(stdout io.Writer, dir, address, mnemonic string) {
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
	fmt.Fprintln(stdout, "The encrypted key lives in "+dir+"; the passphrase opens it for spending.")
	fmt.Fprintln(stdout)
	fmt.Fprintln(stdout, "address: "+address)
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

// readPassphraseLine reads one passphrase line. Echo is suppressed via
// term.ReadPassword when raw is a terminal — a raw fd read, bypassing br
// entirely. That is safe specifically because a terminal in canonical mode
// delivers one line per read: at the point this runs interactively, the
// user has not typed the passphrase yet, so br (used for a caller's own
// prior prompt, if any) never buffered past it.
//
// A pipe or test buffer is NOT a terminal, and MUST read the next line
// from br rather than raw directly. Reading raw fresh here would reopen
// the swallow bug this exists to close: bufio.Reader's first Read() on a
// pipe can consume far more than one line at once (unlike a TTY), so a
// second, independent reader over the same underlying stdin never sees
// what the first already buffered past its line.
//
// Unlike readSecret's key line, a passphrase MAY contain spaces, so only
// the trailing newline is trimmed here — the value itself is not
// validated the way secretFromLine validates a key.
func readPassphraseLine(raw io.Reader, br *bufio.Reader) (string, error) {
	if f, ok := raw.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		data, err := term.ReadPassword(int(f.Fd()))
		if err != nil {
			return "", err
		}
		return string(data), nil
	}
	line, err := br.ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// walletPassphrase resolves the keyfile passphrase: the env var when set,
// else read from stdin — twice on create, once otherwise. Echo is
// suppressed on a real terminal (readPassphraseLine, the same
// term.ReadPassword primitive readSecret uses for the sr- key); a pipe
// reads from br, which the caller must share with any prompt of its own
// that already read from the same stdin (walletSend's "type yes to send"
// confirmation) — two independent bufio.Readers over one stdin is the bug
// this signature exists to make impossible to reintroduce.
func walletPassphrase(stdin io.Reader, br *bufio.Reader, stderr io.Writer, getenv func(string) string, create bool) (string, int) {
	if p := getenv(walletPassphraseEnv); p != "" {
		return p, 0
	}
	fmt.Fprint(stderr, "keyfile passphrase: ")
	first, err := readPassphraseLine(stdin, br)
	fmt.Fprintln(stderr)
	if err != nil && first == "" {
		fmt.Fprintln(stderr, "dropin-miner: no passphrase provided (set "+walletPassphraseEnv+" for non-interactive use)")
		return "", exitUsage
	}
	if first == "" {
		fmt.Fprintln(stderr, "dropin-miner: an empty passphrase would store the key effectively unencrypted; refusing")
		return "", exitUsage
	}
	if !create {
		return first, 0
	}
	fmt.Fprint(stderr, "again: ")
	second, _ := readPassphraseLine(stdin, br)
	fmt.Fprintln(stderr)
	if second != first {
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
	cfgPath := fs2.String("config", "", "path to TOML config file (for the default RPC node's chain)")
	chainIDFlag := fs2.String("chain-id", "", "chain id, when no config names one")
	node := fs2.String("node", "", "CometBFT RPC endpoint (default: "+walletNodeEnv+", else the per-chain default)")
	insecure := fs2.Bool("insecure-node", false, "allow a plain http node that is not loopback")
	denom := fs2.String("denom", defaultWalletDenom, "denomination to report")
	address := fs2.String("address", "", "address to query (default: this wallet's own)")
	if err := fs2.Parse(args); err != nil {
		return exitUsage
	}

	resolvedDir, err := openWalletDir(*dir, getenv)
	if err != nil {
		fmt.Fprintln(stderr, "dropin-miner:", err)
		return exitTransport
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

	// Balance never signs, so an unresolved chain id falls back to the
	// one well-known default rather than refusing outright — the
	// wrong-chain guard §4.4 requires is a signing-safety rule, and
	// there is nothing here to misdirect.
	chainID := resolveChainID(*cfgPath, *chainIDFlag, getenv)
	if chainID == "" {
		chainID = config.DefaultChainID
	}
	nodeURL, err := walletNode(*node, getenv, chainID)
	if err != nil {
		fmt.Fprintln(stderr, "dropin-miner:", err)
		return exitUsage
	}
	if err := validateNodeURL(nodeURL, *insecure, stderr); err != nil {
		fmt.Fprintln(stderr, "dropin-miner:", err)
		return exitUsage
	}

	ctx, cancel := operatorContext(1 * time.Minute)
	defer cancel()
	c := newRPCClient(nodeURL)

	// §4.3: balance also gets a free look at an unresolved previous
	// send. Its outcome does not gate anything here — balance never
	// refuses to report — it is simply the other place a participant
	// is likely to notice and get the resolution for free.
	resolvePendingTx(ctx, c, resolvedDir, stdout, stderr)

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
	cfgPath := fs2.String("config", "", "path to TOML config file (for [mining] chain_id)")
	chainIDFlag := fs2.String("chain-id", "", "chain id, when no config names one")
	node := fs2.String("node", "", "CometBFT RPC endpoint (default: "+walletNodeEnv+", else the per-chain default)")
	insecure := fs2.Bool("insecure-node", false, "allow a plain http node that is not loopback")
	to := fs2.String("to", "", "recipient twilight1... address")
	amount := fs2.String("amount", "", "amount in the base denomination, e.g. 1000000")
	denom := fs2.String("denom", defaultWalletDenom, "denomination to send")
	memo := fs2.String("memo", "", "optional memo, recorded on chain in the clear")
	gas := fs2.Uint64("gas", defaultWalletGas, "gas limit")
	fee := fs2.String("fee", strconv.FormatUint(defaultWalletFee, 10), "fee amount in the same denomination")
	yes := fs2.Bool("yes", false, "skip the confirmation prompt (what an agent passes)")
	abandon := fs2.Bool("abandon-pending", false, "discard an unresolved previous send's journal without resolving it")
	if err := fs2.Parse(args); err != nil {
		return exitUsage
	}

	sc, resolved, err := loadSidecar(*dir, getenv)
	if err != nil {
		fmt.Fprintln(stderr, "dropin-miner:", err)
		return exitTransport
	}

	if *abandon {
		p, err := loadPendingTx(resolved)
		if err != nil {
			fmt.Fprintln(stderr, "dropin-miner:", err)
			return exitTransport
		}
		if p == nil {
			fmt.Fprintln(stdout, "no pending transaction to abandon")
			return exitOK
		}
		if err := removePendingTx(resolved); err != nil {
			fmt.Fprintln(stderr, "dropin-miner:", err)
			return exitTransport
		}
		fmt.Fprintf(stdout, "abandoned pending transaction %s; its outcome was never resolved\n", p.Hash)
		return exitOK
	}

	// Validate before anything is decrypted, resolved or dialed: a typo
	// in -to or -amount must not cost a chain-id lookup, let alone a
	// passphrase prompt or a network round trip.
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
	if sc.Address == *to {
		fmt.Fprintln(stderr, "dropin-miner: -to is this wallet's own address; nothing to do")
		return exitUsage
	}

	// REL-17: the expected chain id comes from configuration, never
	// from the node being dialed — a node is now evidence to check
	// AGAINST an expectation, not the source of the expectation itself.
	expectedChainID := resolveChainID(*cfgPath, *chainIDFlag, getenv)
	if expectedChainID == "" {
		fmt.Fprintln(stderr, "dropin-miner: no chain id to sign for — pass -chain-id, or -config naming a [mining] chain_id")
		return exitUsage
	}
	nodeURL, err := walletNode(*node, getenv, expectedChainID)
	if err != nil {
		fmt.Fprintln(stderr, "dropin-miner:", err)
		return exitUsage
	}
	if err := validateNodeURL(nodeURL, *insecure, stderr); err != nil {
		fmt.Fprintln(stderr, "dropin-miner:", err)
		return exitUsage
	}

	ctx, cancel := signalContext()
	defer cancel()
	c := newRPCClient(nodeURL)

	// No new payment is ever constructed while a journal is pending
	// (§4.3): resolve it first, before touching the node for anything
	// this specific send needs.
	switch resolvePendingTx(ctx, c, resolved, stdout, stderr) {
	case pendingResolved:
		fmt.Fprintln(stdout, "a previous send was confirmed; run again to send another")
		return exitOK
	case pendingUnresolved:
		return exitOutcomeUnknown
	case pendingCheckFailed:
		return exitTransport
	case pendingNone:
	}

	// The node's own report must agree with what this invocation expects
	// to sign for — a live node on a different chain is refused HERE,
	// before signing, not discovered after a silently-invalid signature.
	reportedChainID, err := c.chainID(ctx)
	if err != nil {
		fmt.Fprintln(stderr, "dropin-miner: cannot reach the node:", err)
		return exitTransport
	}
	if reportedChainID != expectedChainID {
		fmt.Fprintf(stderr, "dropin-miner: node %s reports chain %q, expected %q; refusing to sign\n", nodeURL, reportedChainID, expectedChainID)
		return exitUsage
	}
	chainID := expectedChainID

	acct, err := c.account(ctx, sc.Address)
	if err != nil {
		if errors.Is(err, errNoAccount) {
			fmt.Fprintf(stderr, "dropin-miner: %s has never received funds, so there is nothing to send\n", sc.Address)
			return exitTransport
		}
		fmt.Fprintln(stderr, "dropin-miner: account:", err)
		return exitTransport
	}

	// One reader, shared with walletPassphrase below: the confirmation and
	// the passphrase are two reads off the SAME stdin, and a pipe (a
	// script that cannot use a terminal) delivers both in one chunk. A
	// second, independent bufio.Reader here would silently lose whatever
	// this one buffered past the "yes\n" line — the exact bug T4 exists
	// to close.
	br := bufio.NewReader(stdin)
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
		line, _ := br.ReadString('\n')
		if strings.TrimSpace(line) != "yes" {
			fmt.Fprintln(stdout, "canceled; nothing was signed or sent")
			return exitOK
		}
	}

	passphrase, code := walletPassphrase(stdin, br, stderr, getenv, false)
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

	// REL-15: journaled BEFORE broadcast. A lost response after this
	// point is resolvable on the next invocation; a lost response
	// before it would mean the chain never saw this transaction at all,
	// which needs no journal to recover from.
	hash := txHash(txRaw)
	if err := writePendingTx(resolved, pendingTx{
		Hash:      hash,
		TxRawB64:  base64.StdEncoding.EncodeToString(txRaw),
		To:        *to,
		Amount:    *amount,
		Denom:     *denom,
		ChainID:   chainID,
		Sequence:  acct.Sequence,
		CreatedAt: nowRFC3339(),
	}); err != nil {
		fmt.Fprintln(stderr, "dropin-miner: could not journal this transaction before sending it:", err)
		return exitTransport
	}

	res, err := c.broadcast(ctx, txRaw)
	if err != nil {
		// REL-17: broadcast returning an error here means the response
		// did not prove the chain's answer was about THIS transaction —
		// including errBroadcastOutcomeUnknown, a mismatched/missing
		// hash. The journal stays; nothing is known.
		fmt.Fprintf(stderr, "dropin-miner: broadcast: %v\noutcome unknown: %s\n", err, hash)
		return exitOutcomeUnknown
	}
	if res.Code != 0 {
		// Rejected at the mempool: never executed, nothing moved. The
		// journal's job is done — there is nothing left to resolve.
		if rerr := removePendingTx(resolved); rerr != nil {
			fmt.Fprintln(stderr, "dropin-miner:", rerr)
		}
		fmt.Fprintf(stderr, "dropin-miner: the chain rejected this transaction (code %d): %s\n", res.Code, res.Log)
		return exitChainRejected
	}
	fmt.Fprintf(stdout, "submitted: %s\n", res.Hash)

	// Acceptance into the mempool is not execution. Waiting is the
	// difference between reporting a transfer and reporting an attempt.
	// Either way the journal's fate is decided here: confirmed (either
	// direction) removes it, a transport failure while waiting keeps it
	// — REL-15's "outcome unknown, resolve on the next invocation."
	waitCtx, waitCancel := context.WithTimeout(ctx, 60*time.Second)
	defer waitCancel()
	tx, err := c.waitForTx(waitCtx, res.Hash, 2*time.Second)
	if err != nil {
		fmt.Fprintf(stderr, "dropin-miner: submitted, but it has not appeared in a block yet: %v\n", err)
		fmt.Fprintf(stderr, "  outcome unknown: %s — check later: %s/tx?hash=0x%s\n", res.Hash, nodeURL, res.Hash)
		return exitOutcomeUnknown
	}
	if rerr := removePendingTx(resolved); rerr != nil {
		fmt.Fprintln(stderr, "dropin-miner:", rerr)
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

// walletNode resolves the CometBFT RPC endpoint, in order, stopping at
// the first hit: the -node flag, TOKENDROP_WALLET_NODE, then
// config.DefaultWalletNodes[chainID]. A chain with no row in that table
// gets no silent guess — the caller is told exactly what to pass rather
// than being handed a node for a chain it never asked for. There is no
// liveness check here: a dead node fails on the first real request, and
// a live node on the WRONG chain is caught by the chain-id comparison
// wallet send makes before it signs (chainIDMatches) — that comparison,
// not this function, is what makes a per-chain default safe to carry
// across networks.
func walletNode(flagValue string, getenv func(string) string, chainID string) (string, error) {
	if flagValue != "" {
		return flagValue, nil
	}
	if v := getenv(walletNodeEnv); v != "" {
		return v, nil
	}
	if node, ok := config.DefaultWalletNodes[chainID]; ok {
		return node, nil
	}
	return "", fmt.Errorf("no default RPC node is known for chain %q; pass -node or set %s", chainID, walletNodeEnv)
}

// isLoopbackHost mirrors pkg/config's and pkg/auth's own (unexported,
// so not reusable from here) copies — same simple logic, a third
// package's edge to state for itself rather than reach across a
// boundary for one predicate.
func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// resolveChainID applies REL-17's chain-id source rule: a loaded
// config's [mining] chain_id when a config is present, else the
// -chain-id flag. Empty means neither was available. A command that
// signs (wallet send) refuses on that rather than guessing; a
// read-only command (balance, earnings) may fall back to
// config.DefaultChainID — there is no signature to misdirect, only a
// report that would otherwise have nothing to ask the node-default
// table for.
func resolveChainID(cfgPath, chainIDFlag string, getenv func(string) string) string {
	if describeConfigSource(cfgPath, getenv) != "" {
		if cfg, _, err := loadConfig(cfgPath, getenv); err == nil && cfg.Mining.ChainID != "" {
			return cfg.Mining.ChainID
		}
	}
	return chainIDFlag
}

// validateNodeURL enforces the RPC endpoint's transport trust rule
// (REL-17): https, or http on a loopback host, or an operator who typed
// -insecure-node and accepted the warning. A plain http default is
// never permitted — every row in config.DefaultWalletNodes is https,
// enforced separately by TestDefaultsAreTestnet — this is what closes
// the door on an operator-supplied plain-http non-loopback URL too.
func validateNodeURL(raw string, insecure bool, stderr io.Writer) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("-node %q: %w", raw, err)
	}
	if u.Scheme == "https" {
		return nil
	}
	if u.Scheme == "http" && isLoopbackHost(u.Hostname()) {
		return nil
	}
	if insecure {
		fmt.Fprintf(stderr, "dropin-miner: -insecure-node: %s is not https and not loopback; sending the signed transaction and reading balances over it anyway\n", raw)
		return nil
	}
	return fmt.Errorf("node %q must be https, or http on loopback; pass -insecure-node to override and accept the risk", raw)
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
