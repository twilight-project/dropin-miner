package main

// mining: the one command that asks, later, the question connect asks
// at first run (agent onboarding design §5.5: "mining enable"). Both
// call askMiningQuestion — one shared decision tree, one wallet model,
// however it gets triggered.
//
// Structural invariant 11 (with connect.go): no os/exec import here.

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
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

	// Polled BEFORE the question, unlike connect's first-run ask, which
	// cannot: this agent is already claimed by construction (LoadAgentRegistration
	// found one), so the platform has a participant to answer
	// "does this participant already have another mining agent" about —
	// the signal a pre-claim registration structurally cannot have (WP4b,
	// design f0ddb69 §5.5). A poll failure degrades to "unknown" rather
	// than blocking the local decision: this is a nice-to-have prompt
	// note, not a safety check.
	ctx, cancel := operatorContext(30 * time.Second)
	defer cancel()
	// WP2-adversarial-review finding 3: the platform bearer, from
	// credentials.json only — never resolveAPIKey's env fallbacks.
	key, err := platformKey(cfg.Miner)
	if err != nil {
		fmt.Fprintln(stderr, "dropin-miner:", err)
		return exitTransport
	}
	client := platform.New(cfg.Platform.AgentsAPIURL, cfg.Platform.BaseURL)
	participantHasOtherAgent := false
	consoleURL := ""
	if st, serr := client.Status(ctx, reg.AgentID, key); serr == nil {
		reg.Status, reg.Scopes, reg.ClaimExpiresAt = st.Status, st.Scopes, st.ClaimExpiresAt
		_ = store.SaveAgentRegistration(reg)
		participantHasOtherAgent = st.ParticipantHasOtherMiningAgent
		consoleURL = st.ConsoleURL
	} else {
		fmt.Fprintln(stderr, "dropin-miner: could not reach the platform to check status; proceeding on what was last known:", serr)
	}

	br := bufio.NewReader(stdin)
	outcome, code := askMiningQuestion(stdin, br, stdout, stderr, getenv, cfg, store, isInteractive(stdin, stdout), participantHasOtherAgent)
	if code != exitOK {
		return code
	}
	if !outcome.enabled {
		return exitOK
	}

	// If the key doesn't yet have the mining scope, this is §2.2's
	// re-approval case — but only when the agent is actually CLAIMED
	// already. Three statuses reach here, each needing a different
	// destination:
	//   - unclaimed: never claimed at all, so reg.ClaimURL is still the
	//     original, valid, UNCONSUMED link — claiming it grants mining
	//     as part of the same visit, nothing separate to "re-approve"
	//     yet. Printing the console/generic fallback here would be
	//     actively worse: a human would land on a claim form with no
	//     code to type, when they already have a working one.
	//   - expired: no valid path forward at all; a fresh registration is
	//     the only option (matches pollOnce's identical wording).
	//   - claimed (the actual re-approval case): reg.ClaimURL IS already
	//     consumed by the first claim — confirmed live that resubmitting
	//     it 404s even for the original owner (handleAgentClaim refuses
	//     any claimed registration outright). The real mechanism is a
	//     portal-console grant (POST /internal/v1/agents/{id}/scopes,
	//     owner-only, never the sr- key — search-router commit d20a6ac).
	//     Prefer the direct console_url this same poll just returned
	//     (search-router added it specifically to close this gap — live
	//     testing found the generic claim address routes a human through
	//     submitting a doomed code first, only discovering the real
	//     console from the resulting error page); fall back to the
	//     generic address when it's absent (an older platform, or this
	//     poll call itself failed above).
	// connect's own resume still picks up the enrollment automatically
	// once granted — no second flag, no new registration needed, the
	// granted scope is the instruction (decision 3).
	if !hasScope(reg.Scopes, "mining") {
		switch reg.Status {
		case "unclaimed":
			fmt.Fprintln(stdout, "\nthis agent has not been claimed yet. Claim it (and grant mining) at:")
			fmt.Fprintln(stdout, "  "+reg.ClaimURL)
			fmt.Fprintln(stdout, "\nOnce claimed, this resolves automatically the next time `search` runs, or run `dropin-miner connect` to check now.")
		case "expired":
			fmt.Fprintln(stdout, "\nthis registration expired before being claimed; run `dropin-miner connect` again for a new one")
		default:
			dest := consoleURL
			if dest == "" {
				dest = strings.TrimRight(cfg.Platform.BaseURL, "/") + "/claim"
			}
			fmt.Fprintln(stdout, "\nmining is not yet granted for this agent. Sign in and grant it at:")
			fmt.Fprintln(stdout, "  "+dest)
			fmt.Fprintln(stdout, "\nOnce granted, this resolves automatically the next time `search` runs, or run `dropin-miner connect` to check now.")
		}
		return exitOK
	}

	// Already granted: act now rather than waiting for the next search.
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
//
// participantHasOtherAgent is WP4b's "defaults to no on every agent
// after the first" (design f0ddb69 §5.5) — an explanatory line before
// the prompt, not a mechanical change: the question was already visually
// defaulted to "no" (a bare Enter answers N), so the only thing WP4b
// adds is telling the human WHY, when the platform can say so. It can
// only ever be true post-claim (only the platform sees a participant's
// other agents, and there is no participant to compare against before
// one exists) — cmdConnect's pre-claim first-run call always passes
// false; cmdMining's call polls status first specifically to have a real
// answer here.
func askMiningQuestion(stdin io.Reader, br *bufio.Reader, stdout, stderr io.Writer, getenv func(string) string, cfg *config.Config, store *auth.Store, interactive, participantHasOtherAgent bool) (miningEnableOutcome, int) {
	if !interactive {
		// No terminal to ask anything of: the file is the only voice
		// here, trusted silently — the scripted-install path.
		if err := store.SaveMiningEnabled(cfg.Mining.Enabled); err != nil {
			fmt.Fprintln(stderr, "dropin-miner:", err)
			return miningEnableOutcome{}, exitTransport
		}
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

	// Interactive from here on. WP2-adversarial-review finding 4: the
	// terminal answer is the decision (design rule 5), not the config
	// file — even when the file already wrote enabled = true (what
	// setup.sh writes), the human at the terminal right now is who this
	// question is actually for. An EXPLICIT false, though, is a
	// deliberate operator opt-out and is not re-litigated by asking
	// again — that half of the old gate stays.
	if cfg.MiningEnabledExplicit && !cfg.Mining.Enabled {
		if err := store.SaveMiningEnabled(false); err != nil {
			fmt.Fprintln(stderr, "dropin-miner:", err)
			return miningEnableOutcome{}, exitTransport
		}
		return miningEnableOutcome{}, exitOK
	}

	enabled := cfg.MiningEnabledExplicit && cfg.Mining.Enabled // file already answered this half
	if !cfg.MiningEnabledExplicit {
		if participantHasOtherAgent {
			fmt.Fprintln(stderr, "Note: you already have mining enabled on another agent. Several installations of one "+
				"participant draw one share, so enabling it here earns nothing extra and mostly adds conflict noise "+
				"during epochs where both are live.")
		}
		fmt.Fprint(stderr, "Enable mining rewards? [y/N] ")
		line, _ := br.ReadString('\n')
		enabled = strings.HasPrefix(strings.ToLower(strings.TrimSpace(line)), "y")
	}
	if err := store.SaveMiningEnabled(enabled); err != nil {
		fmt.Fprintln(stderr, "dropin-miner:", err)
		return miningEnableOutcome{}, exitTransport
	}
	if !enabled {
		return miningEnableOutcome{}, exitOK
	}

	// The address question is asked at a terminal whenever nothing is on
	// file — even when enabled = true was already written (finding 4's
	// concrete failure: this used to be unreachable in exactly that case).
	address := cfg.Mining.PayoutAddress
	if address == "" {
		fmt.Fprint(stderr, "Payout address (leave empty to create a wallet here): ")
		addrLine, _ := br.ReadString('\n')
		address = strings.TrimSpace(addrLine)
	}

	if address == "" {
		dir, err := openWalletDir("", getenv)
		if err != nil {
			fmt.Fprintln(stderr, "dropin-miner:", err)
			return miningEnableOutcome{}, exitTransport
		}
		// WP2-adversarial-review finding 2: an empty answer means "handle
		// the wallet for me," not "create one unconditionally" — a wallet
		// already at dir (from a prior `wallet init`, or a previous run of
		// this very question) already answers that. createWallet has no
		// existence check of its own by design (wallet.go: callers own
		// every human-facing decision, including this one); a funded
		// wallet silently overwritten in place is unrecoverable.
		if _, err := os.Lstat(filepath.Join(dir, walletKeyFile)); err == nil {
			sc, _, err := loadSidecar(dir, getenv)
			if err != nil {
				fmt.Fprintln(stderr, "dropin-miner: a wallet already exists in "+dir+" but its address could not be read:", err)
				return miningEnableOutcome{}, exitTransport
			}
			fmt.Fprintln(stdout, "a wallet already exists at "+dir+"; reusing its address: "+sc.Address)
			address = sc.Address
		} else if !errors.Is(err, fs.ErrNotExist) {
			fmt.Fprintln(stderr, "dropin-miner:", err)
			return miningEnableOutcome{}, exitTransport
		} else {
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
