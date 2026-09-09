package main

// mining: the one command that asks, later, the question connect asks
// at first run (agent onboarding design §5.5: "mining enable"). Both
// call askMiningQuestion — one shared decision tree, one wallet model,
// however it gets triggered.
//
// Structural invariant 11 (with connect.go): no os/exec import here.

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/twilight-project/dropin-miner/pkg/auth"
	"github.com/twilight-project/dropin-miner/pkg/config"
	"github.com/twilight-project/dropin-miner/pkg/platform"
)

func cmdMining(args []string, stdin io.Reader, stdout, stderr io.Writer, getenv func(string) string) int {
	if len(args) == 0 || args[0] != "enable" {
		fmt.Fprintln(stderr, "dropin-miner: mining needs a subcommand: enable")
		return exitUsage
	}
	fs := newFlagSet("mining enable", stderr)
	cfgPath := fs.String("config", "", "path to TOML config file")
	if err := fs.Parse(args[1:]); err != nil {
		return exitUsage
	}

	cfg, cfgSource, err := loadConfig(*cfgPath, getenv)
	if err != nil {
		fmt.Fprintf(stderr, "dropin-miner: config (%s): %v\n", orDefaults(cfgSource), err)
		return exitTransport
	}
	store, err := auth.OpenStore(cfg.Mining.StateDir)
	if err != nil {
		fmt.Fprintln(stderr, "dropin-miner:", err)
		return exitTransport
	}
	reg, ok, err := store.LoadAgentRegistration()
	if err != nil {
		fmt.Fprintln(stderr, "dropin-miner:", err)
		return exitTransport
	}
	if !ok {
		fmt.Fprintln(stderr, "dropin-miner: no agent registration found; run `dropin-miner connect` first")
		return exitTransport
	}

	br := bufio.NewReader(stdin)
	outcome, code := askMiningQuestion(stdin, br, stdout, stderr, getenv, cfg, store, isInteractive(stdin, stdout))
	if code != exitOK {
		return code
	}
	if !outcome.enabled {
		return exitOK
	}

	// If the key doesn't yet have the mining scope, this is §2.2's
	// re-approval case: print the claim URL again rather than trying to
	// enroll against a scope that isn't there. connect's own resume
	// picks up the enrollment automatically once the human re-approves —
	// no second flag, the granted scope is the instruction (decision 3).
	if !hasScope(reg.Scopes, "mining") {
		fmt.Fprintln(stdout, "\nmining is not yet granted for this agent. Re-approve it at:")
		fmt.Fprintln(stdout, "  "+reg.ClaimURL)
		fmt.Fprintln(stdout, "\nOnce approved, this resolves automatically the next time `search` runs, or run `dropin-miner connect` to check now.")
		return exitOK
	}

	// Already granted: act now rather than waiting for the next search.
	ctx, cancel := operatorContext(30 * time.Second)
	defer cancel()
	key, _, err := resolveAPIKey(getenv, cfg.Miner)
	if err != nil || key == "" {
		fmt.Fprintln(stderr, "dropin-miner: no api key resolved; run `dropin-miner connect` first")
		return exitTransport
	}
	client := platform.New(cfg.Platform.BaseURL)
	_, code = pollOnce(ctx, stdout, stderr, client, store, cfg, &reg, key)
	return code
}

// miningEnableOutcome is what askMiningQuestion decided.
type miningEnableOutcome struct {
	enabled       bool   // the participant chose (or config said) to enable mining
	payoutAddress string // set only when enabled and an address exists; "" means "enabled, no wallet/address yet"
}

// askMiningQuestion is the one place "enable mining rewards?" is
// decided, shared by connect's first run and `mining enable` later
// (agent onboarding design §5.5, "the model does not change" decision):
//
//   - an interactive terminal, and [mining] enabled not already
//     explicit in the config (cfg.MiningEnabledExplicit false): ask.
//     Empty address answer -> wallet init's own core (createWallet),
//     passphrase and mnemonic exactly as `wallet init` does today.
//   - otherwise (non-interactive, or the file already decided): trust
//     cfg.Mining.Enabled / cfg.Mining.PayoutAddress silently — this is
//     what makes a scripted install possible. Enabled with no address
//     and no terminal is not an error: no wallet is created, and the
//     caller (connect/mining enable, and status) says so in plain text.
//
// It never enrolls or declares anything — only decides whether mining
// is on and, if so, persists the resulting address via
// store.SavePayoutAddress so a later detached resume has one thing to
// read before declaring it unattended.
//
// interactive is the caller's own isInteractive(stdin, stdout) — passed
// in rather than computed here so a test can force the interactive
// branch without a real terminal, the same way isTerminal's non-terminal
// branch is already what every automated test of it exercises (a real
// *os.File character device is not something a unit test can fake
// portably; forcing the boolean is the injection point instead).
func askMiningQuestion(stdin io.Reader, br *bufio.Reader, stdout, stderr io.Writer, getenv func(string) string, cfg *config.Config, store *auth.Store, interactive bool) (miningEnableOutcome, int) {
	if cfg.MiningEnabledExplicit || !interactive {
		if !cfg.Mining.Enabled {
			return miningEnableOutcome{}, exitOK
		}
		if cfg.Mining.PayoutAddress == "" {
			fmt.Fprintln(stdout, "mining is enabled in the config, but no payout_address was given and no terminal is "+
				"available to create a wallet; no wallet was created. Set mining.payout_address, or run "+
				"`dropin-miner mining enable` at a terminal.")
			return miningEnableOutcome{enabled: true}, exitOK
		}
		if err := store.SavePayoutAddress(cfg.Mining.PayoutAddress); err != nil {
			fmt.Fprintln(stderr, "dropin-miner:", err)
			return miningEnableOutcome{}, exitTransport
		}
		return miningEnableOutcome{enabled: true, payoutAddress: cfg.Mining.PayoutAddress}, exitOK
	}

	fmt.Fprint(stderr, "Enable mining rewards? [y/N] ")
	line, _ := br.ReadString('\n')
	if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(line)), "y") {
		return miningEnableOutcome{}, exitOK
	}

	fmt.Fprint(stderr, "Payout address (leave empty to create a wallet here): ")
	addrLine, _ := br.ReadString('\n')
	address := strings.TrimSpace(addrLine)

	if address == "" {
		dir, err := openWalletDir("", getenv)
		if err != nil {
			fmt.Fprintln(stderr, "dropin-miner:", err)
			return miningEnableOutcome{}, exitTransport
		}
		passphrase, code := walletPassphrase(stdin, br, stderr, getenv, true)
		if code != 0 {
			return miningEnableOutcome{}, code
		}
		addr, mnemonic, err := createWallet(dir, passphrase)
		if err != nil {
			fmt.Fprintln(stderr, "dropin-miner:", err)
			return miningEnableOutcome{}, exitTransport
		}
		printMnemonic(stdout, dir, addr, mnemonic)
		fmt.Fprintln(stdout)
		address = addr
	}

	if err := store.SavePayoutAddress(address); err != nil {
		fmt.Fprintln(stderr, "dropin-miner:", err)
		return miningEnableOutcome{}, exitTransport
	}
	return miningEnableOutcome{enabled: true, payoutAddress: address}, exitOK
}

// isInteractive reports whether both ends of the terminal are real: a
// character device to read the answer from, and one to print the
// question and (if a wallet gets created) the mnemonic to. Either end
// being a pipe means this is a script, and askMiningQuestion must not
// block on it.
func isInteractive(stdin io.Reader, stdout io.Writer) bool {
	if !isTerminal(stdout) {
		return false
	}
	f, ok := stdin.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}
