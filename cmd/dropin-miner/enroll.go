package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/twilight-project/dropin-miner/pkg/auth"
	"github.com/twilight-project/dropin-miner/pkg/config"
	"github.com/twilight-project/dropin-miner/pkg/mining/spool"
	"github.com/twilight-project/dropin-miner/pkg/redact"
)

// The enrollment commands.
//
// These exist because two steps of the mining lifecycle cannot be
// automated and had no entry point: a human must approve the OAuth grant,
// and a human must supply the provider verification key. Without somewhere
// to do that, a proxy could never obtain a refresh token, so the daemon's
// capability driver would fail on every tick forever.
//
// They are deliberately separate from `serve`: enrollment is a one-time
// operator act that blocks on a person, and the daemon must never block on
// a person. Each command does one step and prints what the next one is.
//
// The agent commands are the other family: they assume the daemon is
// already running and get an agent's traffic into it.
// miningClients builds just enough of the mining plane for an operator
// command: the key store, discovery, the OAuth client and the mining
// client. No spool, no collector, no sink — nothing that runs.
func miningClients(ctx context.Context, args []string, cmd string) (*auth.OAuthClient, *auth.MiningClient, config.Mining, int) {
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	cfgPath := fs.String("config", "", "path to TOML config file")
	if err := fs.Parse(args); err != nil {
		return nil, nil, config.Mining{}, 2
	}
	// No -config is not an error here: config.Load resolves the flag, then
	// TOKENDROP_CONFIG, then ./tokendrop.toml, then defaults — the same
	// order the daemon and the agent commands use. Refusing early made
	// these six commands the only ones that ignored the environment.
	cfg, _, err := config.Load([]string{"-config", *cfgPath}, os.Getenv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "dropin-miner:", err)
		return nil, nil, config.Mining{}, 1
	}
	m := cfg.Mining
	if !m.Enabled {
		// Which of the two failures this is matters: a config that exists
		// and has no [mining] block is a different fix from no config at
		// all, and the old wording ("in this config") described the first
		// while usually meaning the second.
		if src := describeConfigSource(*cfgPath, os.Getenv); src != "" {
			fmt.Fprintf(os.Stderr, "dropin-miner: no [mining] block in %s; there is nothing to enroll\n", src)
		} else {
			fmt.Fprintln(os.Stderr, "dropin-miner: no config file found — looked at $TOKENDROP_CONFIG and ./tokendrop.toml.")
			fmt.Fprintln(os.Stderr, "  Pass -config <file>, or set TOKENDROP_CONFIG.")
		}
		return nil, nil, config.Mining{}, 1
	}

	store, err := auth.OpenStore(m.StateDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "dropin-miner: key store:", err)
		return nil, nil, config.Mining{}, 1
	}
	disc, err := auth.NewDiscoverer(auth.DiscoveryConfig{
		BaseURL: m.ASBaseURL, ChainID: m.ChainID, SlotID: m.SlotID, TTL: m.MetadataTTL,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "dropin-miner: discovery:", err)
		return nil, nil, config.Mining{}, 1
	}
	oauthClient, err := auth.NewOAuthClient(ctx, disc, store)
	if err != nil {
		fmt.Fprintln(os.Stderr, "dropin-miner: authorization server:", err)
		return nil, nil, config.Mining{}, 1
	}
	return oauthClient, auth.NewMiningClient(disc, oauthClient, store), m, 0
}

// resolveEpoch answers which target an operator command acts on: the
// configured pin, or whatever the AS currently offers. The daemon
// resolves it exactly this way on every tick, and a command that
// disagreed with the daemon about which epoch is "the" epoch would be
// worse than no command at all.
//
// ok=false means there is nothing to act on; the reason is already
// printed.
func resolveEpoch(ctx context.Context, mining *auth.MiningClient, m config.Mining) (uint64, bool) {
	if m.TargetEpoch != nil {
		return *m.TargetEpoch, true
	}
	t, err := mining.CurrentTarget(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "dropin-miner: current target:", err)
		if errors.Is(err, auth.ErrNoCurrentTargetEndpoint) {
			fmt.Fprintln(os.Stderr, "  set mining.target_epoch in the config to name the epoch yourself")
		}
		return 0, false
	}
	if t == nil {
		fmt.Fprintln(os.Stderr, "dropin-miner: the AS reports no open target for this slot right now")
		return 0, false
	}
	return t.TargetEpoch, true
}

// epochOrigin labels a resolved epoch with where it came from, so an
// operator can see whether a stale pin is in force.
func epochOrigin(pinned *uint64) string {
	if pinned != nil {
		return " (pinned by mining.target_epoch)"
	}
	return " (from the AS)"
}

// operatorContext cancels on SIGINT/SIGTERM so a person waiting at a
// device prompt can abandon it without leaving a listener open.
func operatorContext(d time.Duration) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	sigCtx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	return sigCtx, func() { stop(); cancel() }
}

func cmdEnroll(args []string) int {
	fs := flag.NewFlagSet("enroll", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "path to TOML config file")
	browser := fs.Bool("browser", false, "use the loopback authorization-code flow instead of the device flow")
	assertion := fs.Bool("assertion", false, "redeem a provider-issued enrollment token read from stdin, with no browser")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *browser && *assertion {
		fmt.Fprintln(os.Stderr, "dropin-miner: -browser and -assertion are different enrollment doors; choose one")
		return 2
	}

	ctx, cancel := operatorContext(15 * time.Minute)
	defer cancel()

	oauthClient, _, _, code := miningClients(ctx, []string{"-config", *cfgPath}, "enroll")
	if code != 0 {
		return code
	}

	switch {
	case *assertion:
		// stdin, never a flag, for the reason `provider` reads its key that
		// way: a token in argv is visible to every other process through ps
		// and lands in shell history. This one is worth more than a key —
		// it is the credential that makes this machine the enrolled one.
		fmt.Fprintln(os.Stderr, "paste the enrollment token from the provider's page, then press Enter:")
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && strings.TrimSpace(line) == "" {
			fmt.Fprintln(os.Stderr, "dropin-miner: no enrollment token on stdin")
			return 2
		}
		if _, err := oauthClient.RedeemEnrollmentAssertion(ctx, strings.TrimSpace(line)); err != nil {
			fmt.Fprintln(os.Stderr, "dropin-miner: enrollment failed:", err)
			// The token is single-use at the AS, and a failure after the
			// request left this machine may have spent it. Saying so is
			// what stops an operator retrying the same string for ten
			// minutes against a server that will never accept it again.
			fmt.Fprintln(os.Stderr, "\nenrollment tokens are single-use and short-lived; obtain a new one rather than retrying this one.")
			return 1
		}
	case *browser:
		pending, err := oauthClient.StartAuthorization(ctx)
		if err != nil {
			fmt.Fprintln(os.Stderr, "dropin-miner: could not start authorization:", err)
			return 1
		}
		defer pending.Close()
		fmt.Fprintln(os.Stdout, "open this URL to approve:")
		fmt.Fprintln(os.Stdout, "  "+pending.URL)
		fmt.Fprintln(os.Stdout, "\nwaiting for the callback on "+pending.RedirectURI+" …")
		if _, err := pending.Wait(ctx); err != nil {
			fmt.Fprintln(os.Stderr, "dropin-miner: authorization failed:", err)
			return 1
		}
	default:
		da, err := oauthClient.StartDeviceAuthorization(ctx)
		if err != nil {
			fmt.Fprintln(os.Stderr, "dropin-miner: could not start the device flow:", err)
			return 1
		}
		fmt.Fprintln(os.Stdout, "to approve this installation, open:")
		fmt.Fprintln(os.Stdout, "  "+da.VerificationURI)
		fmt.Fprintln(os.Stdout, "and enter code:")
		fmt.Fprintln(os.Stdout, "  "+da.UserCode)
		if da.VerificationURIComplete != "" {
			fmt.Fprintln(os.Stdout, "\nor open this, which carries the code:")
			fmt.Fprintln(os.Stdout, "  "+da.VerificationURIComplete)
		}
		fmt.Fprintln(os.Stdout, "\nwaiting for approval …")
		if _, err := oauthClient.WaitForDeviceApproval(ctx, da); err != nil {
			fmt.Fprintln(os.Stderr, "dropin-miner: approval failed:", err)
			return 1
		}
	}

	// The refresh token is persisted by the flow itself; saying so is
	// what tells an operator this machine is now the enrolled one.
	fmt.Fprintln(os.Stdout, "\nauthorized. the refresh authorization is stored in the state directory.")
	fmt.Fprintln(os.Stdout, "next: dropin-miner join -config <file>")
	return 0
}

func cmdJoin(args []string) int {
	ctx, cancel := operatorContext(2 * time.Minute)
	defer cancel()

	_, mining, m, code := miningClients(ctx, args, "join")
	if code != 0 {
		return code
	}

	epoch, ok := resolveEpoch(ctx, mining, m)
	if !ok {
		return 1
	}

	// Joinable is asked for, never inferred from Phase: the AS decides.
	st, err := mining.Status(ctx, epoch)
	if err != nil {
		fmt.Fprintln(os.Stderr, "dropin-miner: epoch status:", err)
		return 1
	}
	fmt.Fprintf(os.Stdout, "slot %s epoch %s: phase=%s mode=%s joinable=%t join_status=%s\n",
		st.SlotID, st.TargetEpoch, st.Phase, st.DistributionMode, st.Joinable, st.JoinStatus)
	// joinable=false means two different things, and only join_status tells
	// them apart: already in (a reinstall, a second run — success, nothing
	// to send) or enrollment closed without us (a failure flush retries).
	if auth.JoinHeld(st.JoinStatus) {
		fmt.Fprintf(os.Stdout, "already joined epoch %s (%s); nothing to send\n", st.TargetEpoch, st.JoinStatus)
		return 0
	}
	if !st.Joinable {
		fmt.Fprintln(os.Stderr, "dropin-miner: the AS reports this target is not joinable; nothing was sent")
		return 1
	}

	res, err := mining.JoinEpoch(ctx, epoch)
	if err != nil {
		fmt.Fprintln(os.Stderr, "dropin-miner: join:", err)
		return 1
	}
	fmt.Fprintf(os.Stdout, "join %s  draw_id=%s  receipt stored (%d bytes, verified)\n",
		res.Status, res.DrawID, len(res.Receipt))
	// The next step depends on the profile, so ASK rather than assert. Under
	// SEARCH_ROUTER_V1 there is no provider step at all — the AS verifies
	// with its own operator credential and §35.1 gives that profile no
	// participant key — so naming it unconditionally sent a live deployment
	// down a path that ends in a refusal.
	if accepts, err := mining.AcceptsOpenRouterProfile(ctx); err == nil && !accepts {
		fmt.Fprintln(os.Stdout, "next: nothing. This Slot holds no participant provider credential;")
		fmt.Fprintln(os.Stdout, "      verification runs on the operator's own. Searches from your agents do the rest.")
		return 0
	}
	fmt.Fprintln(os.Stdout, "next: dropin-miner provider -config <file>   (reads the key from stdin)")
	return 0
}

func cmdProvider(args []string) int {
	fs := flag.NewFlagSet("provider", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "path to TOML config file")
	show := fs.Bool("status", false, "report the current binding instead of registering")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	ctx, cancel := operatorContext(3 * time.Minute)
	defer cancel()

	_, mining, _, code := miningClients(ctx, []string{"-config", *cfgPath}, "provider")
	if code != 0 {
		return code
	}

	// Scoped to OPENROUTER_V1 (integration plan §10 step 6). Under
	// SEARCH_ROUTER_V1 the participant holds no provider credential at all —
	// the AS verifies with its own operator credential and §35.1 gives this
	// profile no key to ask about — so on a Slot serving only that profile
	// there is no binding for this command to establish. Asked here rather
	// than relayed from the AS's refusal, because "this Slot does not use
	// participant provider keys" is an answer a person can act on and
	// PROVIDER_UNAVAILABLE is not.
	accepts, err := mining.AcceptsOpenRouterProfile(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "dropin-miner: could not read the AS service document:", err)
		return 1
	}
	if !accepts {
		fmt.Fprintln(os.Stderr, "dropin-miner: this Slot does not accept OPENROUTER_V1 observations, "+
			"so it holds no participant provider credential and there is nothing for `provider` to register.")
		fmt.Fprintln(os.Stderr, "Verification there runs on the Slot operator's own credential; you supply nothing.")
		return 2
	}

	if *show {
		b, err := mining.ProviderStatus(ctx)
		if err != nil {
			fmt.Fprintln(os.Stderr, "dropin-miner: provider status:", err)
			return 1
		}
		printBinding(b)
		return 0
	}

	// stdin, never a flag: an API key in argv is visible to every other
	// process on the machine via ps, and lands in shell history.
	fmt.Fprintln(os.Stderr, "paste the zero-spend provider key, then press Enter (it is not echoed to stdout, stored locally, or logged):")
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && strings.TrimSpace(line) == "" {
		fmt.Fprintln(os.Stderr, "dropin-miner: no key on stdin")
		return 2
	}
	key := strings.TrimSpace(line)
	if key == "" {
		fmt.Fprintln(os.Stderr, "dropin-miner: empty key")
		return 2
	}

	b, err := mining.RegisterProviderCredential(ctx, key)
	if err != nil {
		fmt.Fprintln(os.Stderr, "dropin-miner: register provider credential:", err)
		return 1
	}
	printBinding(b)
	fmt.Fprintln(os.Stdout, "\nenrollment complete. start the daemon and it will acquire a capability within a minute.")
	return 0
}

func cmdStatus(args []string) int {
	ctx, cancel := operatorContext(time.Minute)
	defer cancel()

	_, mining, m, code := miningClients(ctx, args, "status")
	if code != 0 {
		return code
	}

	fmt.Fprintf(os.Stdout, "as:     %s\nchain:  %s\nslot:   %d\n",
		m.ASBaseURL, m.ChainID, m.SlotID)

	epoch, ok := resolveEpoch(ctx, mining, m)
	if !ok {
		// A report, not a failure: "no target right now" is a state
		// this command exists to show.
		fmt.Fprintln(os.Stdout, "epoch:  none")
		return 0
	}
	fmt.Fprintf(os.Stdout, "epoch:  %d%s\n", epoch, epochOrigin(m.TargetEpoch))

	st, err := mining.Status(ctx, epoch)
	if err != nil {
		// Reaching here almost always means "not authorized yet", which
		// is a state to report, not a failure to shout about.
		fmt.Fprintf(os.Stdout, "epoch:  unavailable (%v)\n", err)
		fmt.Fprintln(os.Stdout, "\nnext: dropin-miner enroll -config <file>")
		return 0
	}
	fmt.Fprintf(os.Stdout, "phase=%s mode=%s joinable=%t join_status=%s participation=%s capability_available=%t\n",
		st.Phase, st.DistributionMode, st.Joinable, st.JoinStatus, st.ParticipationStatus, st.CapabilityAvailable)

	if b, err := mining.ProviderStatus(ctx); err == nil {
		printBinding(b)
	} else {
		fmt.Fprintf(os.Stdout, "provider: none (%v)\n", err)
	}
	printQueue(m)
	return 0
}

// printQueue reports the local backlog.
//
// Everything above this line asks the AS. This asks the disk, and it is the
// half a participant needs when the answers above are bad: whether the work
// is being held or thrown away. An unreachable AS with a queue is a proxy
// doing its job — the observations are safe and will drain — and saying so is
// the difference between "wait" and "something is broken, restart it".
//
// It uses Count rather than Len deliberately: Len goes through Pending, which
// quarantines records it cannot parse, and this command must not move files
// belonging to a running daemon.
func printQueue(m config.Mining) {
	sp, err := spool.Open(m.SpoolDir)
	if err != nil {
		fmt.Fprintf(os.Stdout, "queued: unknown (%v)\n", redact.Error(err))
		return
	}
	n, err := sp.Count()
	if err != nil {
		fmt.Fprintf(os.Stdout, "queued: unknown (%v)\n", redact.Error(err))
		return
	}
	fmt.Fprintf(os.Stdout, "queued: %d observation(s) in %s\n", n, m.SpoolDir)
	if n > 0 {
		fmt.Fprintln(os.Stdout,
			"        held locally until the AS accepts them; nothing is lost while it is unreachable")
	}
}

func printBinding(b *auth.ProviderBinding) {
	fmt.Fprintf(os.Stdout, "provider: %s  status=%s  profile=%s  fingerprint=%s\n",
		b.Provider, b.Status, b.SourceProfile, b.KeyFingerprint)
	if b.LastSuccessfulVerificationAt != "" {
		fmt.Fprintf(os.Stdout, "          last verified %s\n", b.LastSuccessfulVerificationAt)
	}
}

// cmdPayout proposes a payout destination, or shows the open proposal.
//
// `set` is not `register`, and the difference is the whole command. The AS
// records what this sends as inert and an operator activates it separately —
// so the wording here says "proposed", never "set up", and the command prints
// what still has to happen. A participant who reads this output as "done" and
// stops watching is a participant who is not paid, and the one thing a CLI can
// do about that is refuse to imply completion.
func cmdPayout(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "dropin-miner: payout needs a subcommand: set <address>, or show")
		return 2
	}
	sub, rest := args[0], args[1:]

	fs := flag.NewFlagSet("payout "+sub, flag.ContinueOnError)
	cfgPath := fs.String("config", "", "path to TOML config file")

	var address string
	switch sub {
	case "set":
		// The address IS in argv, unlike the credentials the other commands
		// read from stdin. It is public — it is where money goes, not a
		// thing that authorizes anyone — and having it in shell history is
		// useful rather than dangerous.
		if len(rest) == 0 || strings.HasPrefix(rest[0], "-") {
			fmt.Fprintln(os.Stderr, "dropin-miner: payout set needs an address: dropin-miner payout set <address> [-config file]")
			return 2
		}
		address, rest = rest[0], rest[1:]
	case "show":
	default:
		fmt.Fprintf(os.Stderr, "dropin-miner: unknown payout subcommand %q; want set or show\n", sub)
		return 2
	}
	if err := fs.Parse(rest); err != nil {
		return 2
	}

	ctx, cancel := operatorContext(2 * time.Minute)
	defer cancel()

	_, mining, _, code := miningClients(ctx, []string{"-config", *cfgPath}, "payout")
	if code != 0 {
		return code
	}

	if sub == "show" {
		standing, err := mining.PayoutStanding(ctx)
		if err != nil {
			fmt.Fprintln(os.Stderr, "dropin-miner: payout status:", err)
			return 1
		}
		printStanding(standing)
		return 0
	}

	doc, err := mining.DeclarePayoutAddress(ctx, address)
	if err != nil {
		fmt.Fprintln(os.Stderr, "dropin-miner: payout declaration failed:", err)
		return 1
	}
	printDeclaration(doc)
	return 0
}

// printDeclaration reports what the AS recorded, and — when it did not take
// effect — what the participant has to do about it.
//
// The three outcomes get three different closing paragraphs on purpose. A
// single "ask your operator" covered a case where waiting works, one where
// waiting works and the old address keeps paying meanwhile, and one where
// waiting never works because the address belongs to somebody else.
func printDeclaration(doc *auth.PayoutDeclaration) {
	if doc.Effective {
		fmt.Fprintln(os.Stdout, "payout address set:")
	} else {
		fmt.Fprintln(os.Stdout, "payout address proposed:")
	}
	fmt.Fprintln(os.Stdout, "  address:    "+doc.Address)
	// The canonical rendering is what the chain gave back, so it is what a
	// participant should check against — an address that round-trips to
	// something they do not recognize is the typo, caught here. It is the
	// only transcription check that exists until WALLET_SIGNATURE_V1, and
	// it is theirs to perform, not an operator's (ESC-031).
	if doc.CanonicalAddress != "" && doc.CanonicalAddress != doc.Address {
		fmt.Fprintln(os.Stdout, "  as chain:   "+doc.CanonicalAddress)
	}
	fmt.Fprintln(os.Stdout, "  status:     "+doc.Status)
	if doc.Effective {
		fmt.Fprintln(os.Stdout, "  in force:   YES")
		fmt.Fprintln(os.Stdout, "\nYou are set up to be paid. Check the address above is the one you")
		fmt.Fprintln(os.Stdout, "meant — nothing else verifies that, and payments are irreversible.")
		return
	}
	fmt.Fprintln(os.Stdout, "  in force:   no")
	switch doc.HeldFor {
	case auth.HeldAddressInUse:
		fmt.Fprintln(os.Stdout, "\nThis address is already registered to another participant, so it")
		fmt.Fprintln(os.Stdout, "will NOT be activated by waiting. Either you mistyped it, or it is")
		fmt.Fprintln(os.Stdout, "in use — set a different address, or talk to your Slot operator.")
		fmt.Fprintln(os.Stdout, "You keep being paid at whatever was already in force.")
	case auth.HeldReplacesActive:
		fmt.Fprintln(os.Stdout, "\nThis REPLACES the address currently in force, so a Slot operator")
		fmt.Fprintln(os.Stdout, "has to approve it. Until they do you are still paid at the old one —")
		fmt.Fprintln(os.Stdout, "run 'payout show' to see both. That step exists because a change of")
		fmt.Fprintln(os.Stdout, "address is what a stolen credential would do.")
	default:
		fmt.Fprintln(os.Stdout, "\nThis is a PROPOSAL. A Slot operator must activate it before anything")
		fmt.Fprintln(os.Stdout, "is paid to it. Ask them to; no command here can.")
	}
}

// printStanding answers the only question a participant has — am I set up to
// be paid — in the three states it can actually have.
//
// The previous output reported the operator's approval QUEUE, so a
// participant who was active and earning read the same "nothing pending" as
// one who had never declared. A success state that reads as a failure, and
// the Slot 3 run of 2026-08-26 found it would send a correctly configured
// person back to re-declare.
func printStanding(s *auth.PayoutStanding) {
	switch {
	case s.Active == nil && s.Pending == nil:
		fmt.Fprintln(os.Stdout, "payout: none. You have not proposed an address, and none is in force.")
		fmt.Fprintln(os.Stdout, "  propose one: dropin-miner payout set <twilight1...>")
		return

	case s.Active != nil && s.Pending == nil:
		fmt.Fprintln(os.Stdout, "payout: ACTIVE — you are set up to be paid.")
		printPayoutAddress(s.Active)
		return

	case s.Active == nil && s.Pending != nil:
		fmt.Fprintln(os.Stdout, "payout: PENDING — proposed, NOT in force. Nothing is paid to it yet.")
		printPayoutAddress(s.Pending)
		if s.Pending.HeldFor == auth.HeldAddressInUse {
			// Waiting does not fix this one, and telling this
			// participant to wait is telling them to earn nothing
			// indefinitely.
			fmt.Fprintln(os.Stdout, "  This address is registered to another participant. It will not be")
			fmt.Fprintln(os.Stdout, "  activated — set a different one, or talk to your Slot operator.")
			return
		}
		fmt.Fprintln(os.Stdout, "  A Slot operator must activate it. Ask them to; no command here can.")
		return

	default:
		fmt.Fprintln(os.Stdout, "payout: ACTIVE, with a change awaiting approval.")
		fmt.Fprintln(os.Stdout, " in force now:")
		printPayoutAddress(s.Active)
		fmt.Fprintln(os.Stdout, " proposed, not yet in force:")
		printPayoutAddress(s.Pending)
		if s.Pending.HeldFor == auth.HeldAddressInUse {
			fmt.Fprintln(os.Stdout, "  The proposed address is registered to another participant and will")
			fmt.Fprintln(os.Stdout, "  not be activated. You stay on the address in force.")
			return
		}
		fmt.Fprintln(os.Stdout, "  Until an operator activates the change you are paid at the first.")
	}
}

func printPayoutAddress(d *auth.PayoutDeclaration) {
	fmt.Fprintln(os.Stdout, "  address:  "+d.Address)
	// The canonical rendering is what an operator reads back, so it is what a
	// participant should check against — an address that round-trips to
	// something they do not recognize is the typo, caught here.
	if d.CanonicalAddress != "" && d.CanonicalAddress != d.Address {
		fmt.Fprintln(os.Stdout, "  as chain: "+d.CanonicalAddress)
	}
}
