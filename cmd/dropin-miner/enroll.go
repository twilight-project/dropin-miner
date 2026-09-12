package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
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
	if !miningASConfigured(m) {
		// AS presence is independent of the participant's persisted mining
		// decision. A config can explicitly say OFF and still name the AS;
		// it must remain inspectable and must not be mistaken for absent
		// configuration.
		if src := describeConfigSource(*cfgPath, os.Getenv); src != "" {
			fmt.Fprintf(os.Stderr, "dropin-miner: no authorization server configured in %s; there is nothing to enroll\n", src)
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

func cmdStatus(args []string) int { return statusMain(args, os.Stdout, os.Stderr, os.Getenv) }

// statusASFacts is the AS-facing half of `status`, gathered without
// printing. Every failure is kept rather than returned, because each of
// them is a state this command exists to report.
type statusASFacts struct {
	Mining config.Mining

	ClientErr error // local authorization setup is incomplete

	Epoch       uint64
	EpochKnown  bool
	EpochPinned bool
	EpochErr    error

	Status    *auth.EpochStatus
	StatusErr error

	Binding    *auth.ProviderBinding
	BindingErr error

	Queue queueState
}

func gatherStatusAS(ctx context.Context, m config.Mining) statusASFacts {
	f := statusASFacts{Mining: m, EpochPinned: m.TargetEpoch != nil}
	mining, err := readOnlyMiningClient(ctx, m)
	if err != nil {
		f.ClientErr = err
		return f
	}
	// epochFor is resolveEpoch without the side effects: a report has to
	// carry the failure into a line rather than emit it as it goes.
	f.Epoch, f.EpochKnown, f.EpochErr = epochFor(ctx, mining, m.TargetEpoch)
	if !f.EpochKnown {
		return f
	}
	f.Status, f.StatusErr = mining.Status(ctx, f.Epoch)
	if f.StatusErr != nil {
		return f
	}
	f.Binding, f.BindingErr = mining.ProviderStatus(ctx)
	f.Queue = spoolQueueState(m.SpoolDir)
	return f
}

func statusMain(args []string, stdout, stderr io.Writer, getenv func(string) string) int {
	ctx, cancel := operatorContext(time.Minute)
	defer cancel()
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", "", "path to TOML config file")
	asJSON := fs.Bool("json", false, "report as one JSON object instead of text")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	cfg, src, err := loadConfig(*cfgPath, getenv)
	if err != nil {
		if *asJSON {
			emitMachine(stdout, commandEnvelope{
				machineHeader: newMachineHeader("status", exitTransport, "config_unreadable", false, actionFixInput),
				Error:         clientMessage(err),
			})
			return exitTransport
		}
		fmt.Fprintf(stderr, "dropin-miner: config (%s): %v\n", orDefaults(src), err)
		return exitTransport
	}

	local, ok := gatherAgentIdentity(args, getenv)
	if !ok {
		local = agentIdentityFacts{Mining: cfg.Mining, StoreMissing: true,
			Decision: auth.MiningDecision{State: auth.MiningUndecided}}
	}

	// WP2-adversarial-review finding 16: an unclaimed or search-only
	// agent (no AS configuration, by design) is an ordinary state, not a
	// failure — authenticated AS checks require mining.as_url and existing
	// auth material and would otherwise make `status` fail for the two most
	// common states an agent-onboarding participant is actually in.
	// needsMining is false for exactly those two; the AS-facing report is
	// skipped entirely rather than attempted and its failure suppressed,
	// since there is nothing for it to say when there is no [mining] block
	// to ask about.
	var as *statusASFacts
	if local.needsMining() {
		f := gatherStatusAS(ctx, cfg.Mining)
		as = &f
	}

	if *asJSON {
		emitMachine(stdout, statusEnvelope(local, as))
		return exitOK
	}
	renderAgentIdentity(local, stdout, stderr)
	if as != nil {
		renderStatusAS(*as, stdout, stderr)
	}
	return exitOK
}

func renderStatusAS(f statusASFacts, stdout, stderr io.Writer) {
	if f.ClientErr != nil {
		fmt.Fprintf(stdout, "auth:    local authorization setup is incomplete (%v); authenticated AS checks skipped\n", redact.Error(f.ClientErr))
		return
	}
	m := f.Mining
	fmt.Fprintf(stdout, "as:     %s\nchain:  %s\nslot:   %d\n", m.ASBaseURL, m.ChainID, m.SlotID)

	if !f.EpochKnown {
		if f.EpochErr != nil {
			fmt.Fprintln(stderr, "dropin-miner: current target:", f.EpochErr)
			if errors.Is(f.EpochErr, auth.ErrNoCurrentTargetEndpoint) {
				fmt.Fprintln(stderr, "  set mining.target_epoch in the config to name the epoch yourself")
			}
		} else {
			fmt.Fprintln(stderr, "dropin-miner: the AS reports no open target for this slot right now")
		}
		// A report, not a failure: "no target right now" is a state this
		// command exists to show.
		fmt.Fprintln(stdout, "epoch:  none")
		return
	}
	fmt.Fprintf(stdout, "epoch:  %d%s\n", f.Epoch, epochOrigin(m.TargetEpoch))

	if f.StatusErr != nil {
		// Reaching here almost always means "not authorized yet", which
		// is a state to report, not a failure to shout about.
		fmt.Fprintf(stdout, "epoch:  unavailable (%v)\n", f.StatusErr)
		fmt.Fprintln(stdout, "\nnext: dropin-miner enroll -config <file>")
		return
	}
	st := f.Status
	fmt.Fprintf(stdout, "phase=%s mode=%s joinable=%t join_status=%s participation=%s capability_available=%t\n",
		st.Phase, st.DistributionMode, st.Joinable, st.JoinStatus, st.ParticipationStatus, st.CapabilityAvailable)

	if f.BindingErr == nil {
		printBindingTo(stdout, f.Binding)
	} else {
		fmt.Fprintf(stdout, "provider: none (%v)\n", f.BindingErr)
	}
	printQueueState(stdout, f.Queue)
}

func readOnlyMiningClient(ctx context.Context, m config.Mining) (*auth.MiningClient, error) {
	if !miningASConfigured(m) {
		return nil, errors.New("no authorization server configured")
	}
	store, err := auth.OpenStoreExisting(m.StateDir)
	if err != nil {
		return nil, err
	}
	disc, err := auth.NewDiscoverer(auth.DiscoveryConfig{
		BaseURL: m.ASBaseURL, ChainID: m.ChainID, SlotID: m.SlotID, TTL: m.MetadataTTL,
	})
	if err != nil {
		return nil, err
	}
	oauthClient, err := auth.NewReadOnlyOAuthClient(ctx, disc, store)
	if err != nil {
		return nil, err
	}
	return auth.NewMiningClient(disc, oauthClient, store), nil
}

// printAgentIdentityStatus reports the agent-onboarding identity this
// installation has, if any (agent onboarding design §5.5: "status —
// unclaimed | claimed (scopes) | enrolled (slot, payout address)").
//
// Deliberately separate from the miningClients() call below it: that
// one requires [mining] to be enabled and configured, which a
// search-only unclaimed participant has no reason to have. This prints
// first and unconditionally, then whatever follows is the existing
// AS-facing report, unchanged.
//
// The returned bool tells cmdStatus whether the AS-facing report below
// is worth attempting at all (WP2-adversarial-review finding 16):
// false for an unclaimed/expired registration, or a claimed one with no
// mining scope — authenticated AS checks require an AS to be configured, which
// none of those three ordinary states has any reason to have, and
// `status` used to exit 1 for all three purely because of that. true
// when there is a real mining story (scope granted, whether or not
// enrolled yet) or when there is no agent-onboarding registration at
// all — a legacy, pre-connect installation, where the AS-facing report
// is the WHOLE of what `status` has ever done and must run unchanged.
func printAgentIdentityStatus(args []string, stdout, stderr io.Writer, getenv func(string) string) bool {
	f, ok := gatherAgentIdentity(args, getenv)
	if !ok {
		// The real parse, in cmdStatus below, reports the usage or config
		// error; there is nothing this half can say about it.
		return true
	}
	return renderAgentIdentity(f, stdout, stderr)
}

// agentIdentityFacts is everything the local half of `status` reads, with
// no interpretation applied yet.
//
// Gathering and rendering are separate so the JSON report is built from
// the same values the human report prints, rather than from the human
// report itself. A renderer that parsed its own prose back into fields
// would make every wording change a silent data change.
//
// It stays disk-only by design (see printQueue's comment on the same
// point): nothing here asks the AS or the platform.
type agentIdentityFacts struct {
	Mining config.Mining

	// StoreMissing is the ordinary "nothing has decided anything here"
	// state; StoreErr is a state directory that exists and cannot be read.
	StoreMissing bool
	StoreErr     error

	Decision  auth.MiningDecision
	Health    []auth.HealthRecord
	HealthErr error

	Registration    auth.AgentRegistration
	HasRegistration bool
	RegistrationErr error

	PayoutAddress    string
	HasPayoutAddress bool
	PayoutAddressErr error

	Held    auth.PayoutBindingHeld
	HasHeld bool

	Conflicts []auth.ConflictedEpoch
}

func gatherAgentIdentity(args []string, getenv func(string) string) (agentIdentityFacts, bool) {
	flags := flag.NewFlagSet("status", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	cfgPath := flags.String("config", "", "path to TOML config file")
	flags.Bool("json", false, "")
	if err := flags.Parse(args); err != nil {
		return agentIdentityFacts{}, false
	}
	cfg, _, err := loadConfig(*cfgPath, getenv)
	if err != nil {
		return agentIdentityFacts{}, false
	}
	f := agentIdentityFacts{Mining: cfg.Mining}
	store, err := auth.OpenStoreExisting(cfg.Mining.StateDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			f.StoreMissing = true
			f.Decision = auth.MiningDecision{State: auth.MiningUndecided}
		} else {
			f.StoreErr = err
			f.Decision = auth.MiningDecision{State: auth.MiningDegraded, Present: true, Err: err}
		}
		return f, true
	}
	f.Decision = store.ReadMiningDecision()
	f.Health, f.HealthErr = store.HealthRecords()
	f.Registration, f.HasRegistration, f.RegistrationErr = store.LoadAgentRegistration()
	if f.RegistrationErr != nil {
		return f, true
	}
	if f.HasRegistration {
		f.PayoutAddress, f.HasPayoutAddress, f.PayoutAddressErr = store.LoadPayoutAddress()
	}
	// WP4b (design f0ddb69 §5.5): both of these are read-before-declare /
	// conflict bookkeeping the store already has, no AS round trip needed.
	if held, ok, herr := store.LoadPayoutBindingHeld(); herr == nil && ok {
		f.Held, f.HasHeld = held, true
	}
	if conflicts, cerr := store.EpochConflicts(); cerr == nil {
		f.Conflicts = conflicts
	}
	return f, true
}

// needsMining reports whether cmdStatus's AS-facing report is worth
// attempting at all (WP2-adversarial-review finding 16). See
// printAgentIdentityStatus's doc comment for the three states it is false
// for.
func (f agentIdentityFacts) needsMining() bool {
	if f.StoreMissing || f.StoreErr != nil || f.RegistrationErr != nil {
		return false
	}
	if !f.HasRegistration {
		return true // never ran connect: the legacy AS-facing report is the whole of status
	}
	if f.Registration.Status != "claimed" || !hasScope(f.Registration.Scopes, "mining") {
		return false
	}
	return f.Decision.State == auth.MiningEnabled && miningASConfigured(f.Mining)
}

func renderAgentIdentity(f agentIdentityFacts, stdout, stderr io.Writer) bool {
	if f.StoreMissing {
		fmt.Fprintln(stdout, "mining: NOT DECIDED")
		printMiningASConfiguration(stdout, f.Mining)
		return false
	}
	if f.StoreErr != nil {
		fmt.Fprintf(stdout, "mining: DEGRADED — stored decision could not be read%s\n", miningDecisionDetail(f.Decision))
		printMiningASConfiguration(stdout, f.Mining)
		return false
	}
	if f.Decision.State == auth.MiningDegraded {
		fmt.Fprintf(stdout, "mining:  DEGRADED — stored decision could not be read%s\n", miningDecisionDetail(f.Decision))
	} else {
		fmt.Fprintf(stdout, "mining:  %s%s\n", miningDecisionText(f.Decision), miningDecisionDetail(f.Decision))
	}
	printMiningASConfiguration(stdout, f.Mining)
	for _, record := range f.Health {
		prefix := "health:"
		if f.Decision.State == auth.MiningDisabled {
			prefix = "health (previous unresolved):"
		}
		if record.Detail == "" {
			fmt.Fprintf(stdout, "%-26s %s/%s\n", prefix, record.Component, record.Reason)
		} else {
			fmt.Fprintf(stdout, "%-26s %s/%s — %s\n", prefix, record.Component, record.Reason, record.Detail)
		}
	}
	if f.HealthErr != nil {
		fmt.Fprintf(stderr, "health: could not read persistent component health: %v\n", redact.Error(f.HealthErr))
	}
	if f.RegistrationErr != nil {
		// WP2-adversarial-review finding 17: an undecodable agent.json
		// used to make this function print nothing at all, identical to
		// "never ran connect" — status is exactly where a participant
		// would go looking to understand why connect started refusing.
		fmt.Fprintf(stderr, "agent:  registration on file could not be read: %v\n", f.RegistrationErr)
		return false
	}
	if !f.HasRegistration {
		return true // never ran connect: nothing to report here, legacy report proceeds
	}

	reg := f.Registration
	switch reg.Status {
	case "unclaimed":
		fmt.Fprintf(stdout, "agent:  unclaimed — claim at %s\n", reg.ClaimURL)
	case "expired":
		fmt.Fprintln(stdout, "agent:  expired — run `dropin-miner connect` again for a new registration")
	case "claimed":
		fmt.Fprintf(stdout, "agent:  claimed (scopes: %s)\n", strings.Join(reg.Scopes, ", "))
		if reg.LastEnrollmentSlot != "" {
			if f.PayoutAddressErr == nil && f.HasPayoutAddress {
				fmt.Fprintf(stdout, "        enrolled on %s, payout address %s\n", reg.LastEnrollmentSlot, f.PayoutAddress)
			} else {
				fmt.Fprintf(stdout, "        enrolled on %s, no payout address on file yet\n", reg.LastEnrollmentSlot)
			}
		} else if hasScope(reg.Scopes, "mining") {
			switch f.Decision.State {
			case auth.MiningDisabled:
				printMiningStoppedPair(stdout)
			case auth.MiningUndecided:
				fmt.Fprintln(stdout, "        mining is not decided here")
			case auth.MiningDegraded:
				fmt.Fprintln(stdout, "        mining is stopped for safety until the local decision can be trusted")
			case auth.MiningEnabled:
				if reg.SlotRefusal != "" {
					// finding 15: name the refusal explicitly rather than let
					// a participant discover it only from a resume's silent
					// no-op.
					fmt.Fprintln(stdout, "        mining scope granted, but not enrolled: "+reg.SlotRefusal)
				} else if !f.HasPayoutAddress {
					fmt.Fprintln(stdout, "        mining enabled, no wallet yet (no terminal was available at setup) — "+
						"run `dropin-miner mining enable` at a terminal, or set mining.payout_address")
				} else {
					fmt.Fprintln(stdout, "        mining scope granted; enrollment pending the next `search` or `connect`")
				}
			}
		}
	default:
		fmt.Fprintf(stderr, "agent:  unrecognized status %q from a previous poll\n", reg.Status)
	}

	if f.HasHeld {
		reason := f.Held.HeldFor
		if reason == "" {
			reason = "REPLACES_ACTIVE" // this client's own read-before-declare pre-check, not an AS-returned reason
		}
		fmt.Fprintf(stdout, "payout: HELD (%s) — the AS has %s active for this participant; this installation "+
			"would declare %s. Changing the active binding is an operator-activated change.\n",
			reason, f.Held.Active, f.Held.Local)
	}
	for _, c := range f.Conflicts {
		fmt.Fprintf(stdout, "mining: another installation of this participant holds slot %d epoch %d; "+
			"this installation's observations for it are queued until the AS's target moves past it, then dropped\n",
			c.SlotID, c.TargetEpoch)
	}
	return f.needsMining()
}

func printMiningASConfiguration(w io.Writer, m config.Mining) {
	if miningASConfigured(m) {
		fmt.Fprintln(w, "as:      configured")
		return
	}
	fmt.Fprintln(w, "as:      no authorization server configured")
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
// queueState is the local backlog as a value, so the text report and the
// JSON report read the same count rather than each opening the spool.
type queueState struct {
	Dir   string `json:"dir"`
	Known bool   `json:"known"`
	Count int    `json:"count"`
	Error string `json:"error,omitempty"`
}

// spoolQueueState uses Count rather than Len deliberately: Len goes through
// Pending, which quarantines records it cannot parse, and a read-only
// report must not move files belonging to a running daemon.
func spoolQueueState(spoolDir string) queueState {
	q := queueState{Dir: spoolDir}
	sp, err := spool.OpenExisting(spoolDir)
	if err != nil {
		q.Error = redact.Error(err).Error()
		return q
	}
	n, err := sp.Count()
	if err != nil {
		q.Error = redact.Error(err).Error()
		return q
	}
	q.Known, q.Count = true, n
	return q
}

func printQueueState(w io.Writer, q queueState) {
	if !q.Known {
		fmt.Fprintf(w, "queued: unknown (%s)\n", q.Error)
		return
	}
	fmt.Fprintf(w, "queued: %d observation(s) in %s\n", q.Count, q.Dir)
	if q.Count > 0 {
		fmt.Fprintln(w,
			"        held locally until the AS accepts them; nothing is lost while it is unreachable")
	}
}

func printBinding(b *auth.ProviderBinding) { printBindingTo(os.Stdout, b) }

func printBindingTo(w io.Writer, b *auth.ProviderBinding) {
	fmt.Fprintf(w, "provider: %s  status=%s  profile=%s  fingerprint=%s\n",
		b.Provider, b.Status, b.SourceProfile, b.KeyFingerprint)
	if b.LastSuccessfulVerificationAt != "" {
		fmt.Fprintf(w, "          last verified %s\n", b.LastSuccessfulVerificationAt)
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
