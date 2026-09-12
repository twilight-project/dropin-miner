package main

// `doctor` — the participant's health check.
//
// /statusz already answers the operator's question: which listeners are up,
// which routes are meterable, how much of the ring budget is left. `status`
// answers a different one — it prints what the AS said, flatly, in the AS's
// own vocabulary. Neither answers the question a participant actually has,
// which is "am I set up, and if not, which part is wrong".
//
// So this command is a VERDICT, not a dump. One line per question, each of
// which is either true or not, and each of which names the next command
// when it is not. `status` stays: reading the AS's raw answer is what you
// want when you are debugging the AS, and a verdict hides exactly the
// detail you would need for that.
//
// Two rules about what it may say, both of which the wording is built
// around. It never calls anything earnings — `doctor` reports setup, and
// what has actually been paid is a chain question `earnings` answers. And it
// never attributes anything to a single request: the epoch's budget is an
// equal split among eligible participants, so the verified-observation count
// is a THRESHOLD (one qualifies) and never a quantity.
//
// A degraded AS produces a partial answer, never a blank one. Everything the
// spool, the config and the local custody state can establish is reported
// whether or not the AS answers, and the checks that could not run say so by
// name rather than reading as failures.
//
// It opens only existing state and spool paths and creates no state
// directory, DPoP key, wallet or enrollment to diagnose one. There is one
// bounded local probe operation, and only when `intake writable` is active:
// it may create the intake directory, publishes at most one inert probe
// file there — named so a flush can never mistake it for a record, since
// it does not end in .json — and then attempts cleanup, reporting the
// pathname if the removal failed. "One write" would be the wrong claim:
// the mkdir, the atomic publication and the removal are separate
// filesystem operations, and each can fail on its own and is reported on
// its own. The intake directory is created only when its parent already
// exists and it does not, because that is what the first search would
// create anyway; nothing above it ever is.

import (
	"context"
	"encoding/json"
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
	"github.com/twilight-project/dropin-miner/pkg/fsx"
	"github.com/twilight-project/dropin-miner/pkg/mining/spool"
	"github.com/twilight-project/dropin-miner/pkg/redact"
	"github.com/twilight-project/dropin-miner/pkg/wire"
)

// asClient is the AS surface the two participant-facing commands read.
//
// It exists so the degraded cases — AS down, unauthorized, no open target,
// no payout declaration — are ordinary table-driven tests rather than a
// second OAuth/DPoP server built in the test binary. *auth.MiningClient is
// the only production implementation; internal/auth's own tests cover it
// against a fake AS at the wire level.
type asClient interface {
	ServiceDocument(context.Context) (*wire.DiscoveryDocument, error)
	CurrentTarget(context.Context) (*auth.MiningTarget, error)
	Status(context.Context, uint64) (*auth.EpochStatus, error)
	PayoutStanding(context.Context) (*auth.PayoutStanding, error)
	EpochActivity(context.Context, uint64) (*auth.EpochActivity, error)
}

// doctorVerdict has exactly three values, and the third is not a polite
// spelling of the second. NO is a fact — the thing is not true, and there is
// usually something to do about it. UNKNOWN is the absence of a fact, which
// calls for fixing the dependency rather than the setup.
type doctorVerdict string

const (
	verdictOK      doctorVerdict = "OK"
	verdictNo      doctorVerdict = "NO"
	verdictUnknown doctorVerdict = "UNKNOWN"
)

// asDidNotAnswer is what a check says when the AS-reachability line above
// it already carries the reason.
const asDidNotAnswer = "the AS did not answer, so this could not be checked — see the first line"

type doctorCheck struct {
	Name    string
	Verdict doctorVerdict
	Detail  string
	// Fix is the next command or action, and is empty when there is none
	// to offer — a closed enrollment window is nobody's mistake.
	Fix string
}

// doctorFacts is everything the checks are computed from: the raw answers
// and the raw failures, with no interpretation applied yet. Gathering and
// judging are separate so the judging is testable without a network.
type doctorFacts struct {
	ASBaseURL string
	ChainID   string
	SlotID    uint64
	SpoolDir  string

	// The AS, in the order the checks consume it. A nil value with a nil
	// error means the AS answered and the answer was "nothing".
	Doc    *wire.DiscoveryDocument
	DocErr error

	Epoch       uint64
	EpochKnown  bool
	EpochPinned bool
	// EpochErr is a failure to ASK; an open target simply not existing is
	// EpochKnown=false with EpochErr=nil.
	EpochErr error

	Status    *auth.EpochStatus
	StatusErr error

	Standing    *auth.PayoutStanding
	StandingErr error

	Activity    *auth.EpochActivity
	ActivityErr error

	// The local half, which answers whether or not the AS does.
	HasRefresh bool
	LocalErr   error
	// HasRegistration/RegistrationSlot/RegistrationAt come from the
	// agent-onboarding registration (agent.json), the one durable local
	// record of a past mining enrollment succeeding — see
	// gatherDoctorFacts's comment on why this replaced enrollment.json.
	HasRegistration  bool
	RegistrationSlot string
	RegistrationAt   string

	MiningDecision    auth.MiningDecision
	LocalStateKnown   bool
	ASConfigured      bool
	ASConfigKnown     bool
	Health            []auth.HealthRecord
	HealthErr         error
	AuthIncomplete    bool
	AuthIncompleteErr error

	// The miner half, for `intake writable` and `recording`.
	MinerEnabled bool
	IntakeDir    string
	IntakeProbe  intakeProbeResult

	// Recording inputs. Each carries its own error rather than folding a
	// read failure into an absent value: the two mean different things and
	// the check reports them differently.
	Stamp           flushStamp
	StampPresent    bool
	StampErr        error
	IntakeCount     int
	IntakeErr       error
	SpoolCount      int
	QuarantineCount int
	SpoolErr        error

	// Now is sampled once, by the gatherer. Nothing downstream calls
	// time.Now(), so a judgment over these facts is reproducible.
	Now time.Time
}

// assembleDoctor turns the gathered facts into the verdicts.
//
// It is pure — including of the clock, which arrives as f.Now — and it is
// where every wording rule lives.
func assembleDoctor(f doctorFacts) []doctorCheck {
	return []doctorCheck{
		doctorASCheck(f),
		doctorEnrolledCheck(f),
		doctorJoinedCheck(f),
		doctorPayoutCheck(f),
		doctorEarningCheck(f),
		doctorIntakeCheck(f),
		doctorRecordingCheck(f),
	}
}

func doctorASCheck(f doctorFacts) doctorCheck {
	c := doctorCheck{Name: "authorization server"}
	if f.ASConfigKnown && !f.ASConfigured {
		c.Verdict = verdictNo
		c.Detail = "no authorization server configured"
		c.Fix = "set mining.as_url, chain_id, and slot_id when this participant should mine"
		return c
	}
	if f.DocErr != nil {
		c.Verdict = verdictNo
		c.Detail = fmt.Sprintf("cannot reach %s — %v", f.ASBaseURL, redact.Error(f.DocErr))
		c.Fix = "check that mining.as_url is right and that this machine can reach it; " +
			"everything below that needs the AS is unchecked until it answers"
		return c
	}
	c.Verdict = verdictOK
	c.Detail = fmt.Sprintf("%s — slot %d on %s", f.ASBaseURL, f.SlotID, f.ChainID)
	return c
}

func doctorEnrolledCheck(f doctorFacts) doctorCheck {
	c := doctorCheck{Name: "enrolled"}
	switch {
	case f.LocalErr != nil:
		c.Verdict = verdictUnknown
		c.Detail = fmt.Sprintf("cannot read this installation's state directory — %v", redact.Error(f.LocalErr))
		c.Fix = "check mining.state_dir exists and is owner-only"
	case !f.HasRefresh:
		c.Verdict = verdictNo
		c.Detail = "this installation holds no authorization, so it cannot talk to the AS at all"
		c.Fix = doctorEnrollFix(f, "claim the URL `dropin-miner connect` printed, then run `dropin-miner connect` "+
			"again (or just search — it resumes automatically once the claim goes through)")
	case f.AuthIncomplete:
		c.Verdict = verdictNo
		c.Detail = "local authorization is incomplete; no DPoP key is available for authenticated AS checks"
		c.Fix = doctorEnrollFix(f, "run `dropin-miner connect` again to complete local authorization")
	case f.StatusErr != nil && f.DocErr == nil && f.EpochKnown:
		// The AS is up and refused an authorization we hold. That is worth
		// separating from "the AS is down": one is waited out, the other is
		// re-enrolled.
		c.Verdict = verdictNo
		c.Detail = fmt.Sprintf("an authorization is stored here and the AS did not accept it — %v", redact.Error(f.StatusErr))
		c.Fix = doctorEnrollFix(f, "if this does not clear on its own: dropin-miner mining disable, then "+
			"dropin-miner mining enable (or just `connect` again) to get a fresh authorization")
	case f.DocErr != nil:
		c.Verdict = verdictOK
		c.Detail = "an authorization is stored here (read locally; the AS was not reachable to confirm it)"
	default:
		c.Verdict = verdictOK
		c.Detail = "the AS accepted this installation's authorization"
	}
	return c
}

// doctorEnrollFix picks the right remediation for a bad "enrolled" check:
// connectFix for an installation that has ever run connect (f.HasRegistration
// — agent.json exists), since there is no enrollment token to redeem by
// hand there; the old manual `enroll -assertion` path otherwise, for an
// installation that enrolled without ever registering with the search
// platform (setup.sh/install.ps1 before they ran connect, or the portal's
// still-supported manual flow).
func doctorEnrollFix(f doctorFacts, connectFix string) string {
	if f.HasRegistration {
		return connectFix
	}
	return "dropin-miner enroll -config <file>"
}

func doctorJoinedCheck(f doctorFacts) doctorCheck {
	c := doctorCheck{Name: "joined this epoch"}
	switch {
	case f.DocErr != nil:
		// The first line already carries the reason in full. Repeating a
		// DNS failure four times buries the three checks that DID run.
		c.Verdict = verdictUnknown
		c.Detail = asDidNotAnswer + doctorLocalEnrollmentNote(f)
	case f.EpochErr != nil:
		c.Verdict = verdictUnknown
		c.Detail = fmt.Sprintf("could not ask the AS which epoch is open — %v", redact.Error(f.EpochErr))
		c.Detail += doctorLocalEnrollmentNote(f)
	case !f.EpochKnown:
		// The AS answered, and the answer is that nothing is joinable. Not
		// a fault and not a fix: the Slot operator opens the next one.
		c.Verdict = verdictNo
		c.Detail = "the AS has no open target for this slot right now, so there is nothing to join yet"
		c.Detail += doctorLocalEnrollmentNote(f)
	case f.StatusErr != nil:
		c.Verdict = verdictUnknown
		c.Detail = fmt.Sprintf("epoch %d — the AS did not answer for it: %v", f.Epoch, redact.Error(f.StatusErr))
	case auth.JoinHeld(f.Status.JoinStatus):
		c.Verdict = verdictOK
		c.Detail = fmt.Sprintf("epoch %d%s — %s", f.Epoch, doctorEpochOrigin(f), strings.ToLower(f.Status.JoinStatus))
	case f.Status.Joinable:
		c.Verdict = verdictNo
		c.Detail = fmt.Sprintf("epoch %d%s is open and this installation is not in it", f.Epoch, doctorEpochOrigin(f))
		c.Fix = "dropin-miner join -config <file>"
	default:
		c.Verdict = verdictNo
		c.Detail = fmt.Sprintf("epoch %d%s is not joinable (phase %s) and this installation is not in it",
			f.Epoch, doctorEpochOrigin(f), f.Status.Phase)
		c.Fix = "wait for the operator to open the next target, then: dropin-miner join -config <file>"
	}
	return c
}

// doctorLocalEnrollmentNote adds what the disk knows when the AS cannot
// be asked. It used to name a specific epoch ("you joined epoch 1042 at
// some point"), read from enrollment.json via SaveEnrollment/
// LoadEnrollment — but nothing has ever called SaveEnrollment (the
// driver's own epoch-join bookkeeping, JoinState, is in-memory only), so
// that record was always empty and this note never fired, on any
// installation, ever. It says the weaker thing this installation's own
// files can actually prove instead: that an AS authorization is stored,
// and — when the agent-onboarding registration recorded one — when and
// for which platform slot it was obtained. Never a specific epoch: there
// is no durable local record of that to read.
func doctorLocalEnrollmentNote(f doctorFacts) string {
	if !f.HasRefresh {
		return ""
	}
	if f.HasRegistration {
		return fmt.Sprintf(" (this installation holds a stored authorization, enrolled for slot %q at %s)",
			f.RegistrationSlot, f.RegistrationAt)
	}
	return " (this installation holds a stored authorization, recorded locally)"
}

func doctorEpochOrigin(f doctorFacts) string {
	if f.EpochPinned {
		return " (pinned by mining.target_epoch)"
	}
	return ""
}

func doctorPayoutCheck(f doctorFacts) doctorCheck {
	c := doctorCheck{Name: "payout address"}
	switch {
	case f.DocErr != nil:
		c.Verdict = verdictUnknown
		c.Detail = asDidNotAnswer
	case f.StandingErr != nil:
		c.Verdict = verdictUnknown
		c.Detail = fmt.Sprintf("could not ask the AS where you are paid — %v", redact.Error(f.StandingErr))
	case f.Standing == nil || (f.Standing.Active == nil && f.Standing.Pending == nil):
		c.Verdict = verdictNo
		c.Detail = "no address is in force, so nothing can be paid to you"
		c.Fix = "dropin-miner payout set <twilight1...>   (or: dropin-miner wallet register)"
	case f.Standing.Active == nil:
		c.Verdict = verdictNo
		c.Detail = fmt.Sprintf("%s is proposed but NOT in force; nothing is paid to it yet",
			payoutShown(f.Standing.Pending))
		if f.Standing.Pending.HeldFor == auth.HeldAddressInUse {
			c.Detail += " — it is registered to another participant"
			c.Fix = "set a different address, or talk to your Slot operator; waiting will not activate this one"
		} else {
			c.Fix = "a Slot operator must activate it. Ask them to; no command here can"
		}
	case f.Standing.Pending != nil:
		c.Verdict = verdictOK
		c.Detail = fmt.Sprintf("%s is in force; a change to %s is waiting for an operator",
			payoutShown(f.Standing.Active), payoutShown(f.Standing.Pending))
	default:
		c.Verdict = verdictOK
		c.Detail = fmt.Sprintf("%s is in force", payoutShown(f.Standing.Active))
	}
	return c
}

// payoutShown prefers the canonical rendering the chain gave back: it is
// what an operator reads, and an address that round-trips to something a
// participant does not recognize is the transcription error showing itself.
func payoutShown(d *auth.PayoutDeclaration) string {
	if d == nil {
		return ""
	}
	if d.CanonicalAddress != "" {
		return d.CanonicalAddress
	}
	return d.Address
}

func doctorEarningCheck(f doctorFacts) doctorCheck {
	c := doctorCheck{Name: "earning"}
	switch {
	case f.DocErr != nil:
		c.Verdict = verdictUnknown
		c.Detail = asDidNotAnswer
		return c
	case !f.EpochKnown && f.EpochErr == nil:
		c.Verdict = verdictNo
		c.Detail = "no epoch is open, so nothing is being credited right now"
		return c
	case f.EpochErr != nil:
		c.Verdict = verdictUnknown
		c.Detail = fmt.Sprintf("could not ask the AS which epoch is open — %v", redact.Error(f.EpochErr))
		return c
	case f.ActivityErr != nil:
		c.Verdict = verdictUnknown
		c.Detail = fmt.Sprintf("could not ask the AS what it has credited — %v", redact.Error(f.ActivityErr))
		return c
	case f.Activity == nil:
		c.Verdict = verdictUnknown
		c.Detail = "the AS did not report this epoch's activity"
		return c
	}
	// The count is a threshold and never a weight (§23: one qualifying
	// VERIFIED observation). Saying so on the line itself is what stops it
	// being read as an amount.
	if f.Activity.Eligible() {
		c.Verdict = verdictOK
		c.Detail = fmt.Sprintf("epoch %d — %s verified, which qualifies (%d is the threshold, more does not earn more)",
			f.Epoch, plural(f.Activity.VerifiedObservationCount, "observation"), auth.MinVerifiedObservations)
	} else {
		c.Verdict = verdictNo
		c.Detail = fmt.Sprintf("epoch %d — %s verified; %d qualifies",
			f.Epoch, plural(f.Activity.VerifiedObservationCount, "observation"), auth.MinVerifiedObservations)
		c.Fix = "searches have to go through it: run one from an agent, or by hand: dropin-miner search -format model \"<query>\"  (then: dropin-miner flush)"
	}
	if n := f.Activity.PendingObservationCount; n > 0 {
		c.Detail += fmt.Sprintf("; %s not yet verified", plural(n, "observation"))
	}
	if n := f.Activity.RejectedObservationCount; n > 0 {
		c.Detail += fmt.Sprintf("; %s rejected", plural(n, "observation"))
	}
	return c
}

func plural(n uint64, word string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, word)
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// cmdDoctor gathers the facts and prints the verdicts.
func cmdDoctor(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("doctor", stderr)
	cfgPath := fs.String("config", "", "path to TOML config file")
	asJSON := fs.Bool("json", false, "report as one JSON object instead of text")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	ctx, cancel := operatorContext(2 * time.Minute)
	defer cancel()

	cfg, src, err := loadConfig(*cfgPath, os.Getenv)
	if err != nil {
		if *asJSON {
			emitMachine(stdout, commandEnvelope{
				machineHeader: newMachineHeader("doctor", exitTransport, "config_unreadable", false, actionFixInput),
				Error:         clientMessage(err),
			})
			return exitTransport
		}
		fmt.Fprintf(stderr, "dropin-miner: config (%s): %v\n", orDefaults(src), err)
		return exitTransport
	}
	mining := doctorASClient(ctx, cfg.Mining)

	// Gathered once, rendered either way: the JSON report makes no call
	// the text report does not make, and neither is produced from the
	// other's output.
	f := gatherDoctorFactsFor(ctx, mining, cfg.Mining, cfg.Miner, realIntakeProbeOps())
	f.ASConfigured = miningASConfigured(cfg.Mining)
	f.ASConfigKnown = true
	checks := assembleDoctor(f)
	if *asJSON {
		code := doctorExit(checks)
		emitMachine(stdout, doctorEnvelope(f, checks, code))
		return code
	}
	printDoctor(stdout, checks, f)
	return doctorExit(checks)
}

// doctorExit decides the process status.
//
// The exit status reports whether the DIAGNOSIS succeeded, not whether the
// news is good, and it is the same decision whichever renderer ran.
//
// The tempting design is the opposite — non-zero whenever any check is
// not OK — and it is wrong for this command in a way that only shows up
// in front of a new participant. The first thing anyone runs `doctor`
// for is a proxy that has just been installed and not yet enrolled,
// which is the state it exists to explain. Exiting 1 there tells a
// person, and every script wrapping this, that the command failed, when
// what actually happened is that it worked perfectly and the answer is
// "not enrolled yet". A diagnostic that reports a correct diagnosis as
// its own failure teaches people to stop reading it.
//
// So `NO` is a successful run: the check ran and the answer is no. Only
// a report that could not be produced at all is a failure, and the way
// that shows is every check coming back UNKNOWN — nothing was reachable,
// so nothing was learned.
func doctorExit(checks []doctorCheck) int {
	for _, c := range checks {
		if c.Verdict != verdictUnknown {
			return exitOK
		}
	}
	return exitTransport
}

type doctorUnauthenticatedAS struct {
	disc *auth.Discoverer
	err  error
}

func (a doctorUnauthenticatedAS) ServiceDocument(ctx context.Context) (*wire.DiscoveryDocument, error) {
	if a.disc == nil {
		return nil, a.err
	}
	return a.disc.Document(ctx)
}
func (a doctorUnauthenticatedAS) CurrentTarget(context.Context) (*auth.MiningTarget, error) {
	return nil, a.err
}
func (a doctorUnauthenticatedAS) Status(context.Context, uint64) (*auth.EpochStatus, error) {
	return nil, a.err
}
func (a doctorUnauthenticatedAS) PayoutStanding(context.Context) (*auth.PayoutStanding, error) {
	return nil, a.err
}
func (a doctorUnauthenticatedAS) EpochActivity(context.Context, uint64) (*auth.EpochActivity, error) {
	return nil, a.err
}

func doctorASClient(_ context.Context, m config.Mining) asClient {
	if !miningASConfigured(m) {
		return doctorUnauthenticatedAS{err: errors.New("no authorization server configured")}
	}
	disc, err := auth.NewDiscoverer(auth.DiscoveryConfig{
		BaseURL: m.ASBaseURL, ChainID: m.ChainID, SlotID: m.SlotID, TTL: m.MetadataTTL,
	})
	if err != nil {
		return doctorUnauthenticatedAS{err: err}
	}
	store, err := auth.OpenStoreExisting(m.StateDir)
	if err != nil {
		return doctorUnauthenticatedAS{disc: disc, err: errors.New("local authorization state is unavailable")}
	}
	if _, ok, err := store.LoadRefreshToken(); err != nil || !ok {
		return doctorUnauthenticatedAS{disc: disc, err: errors.New("local authorization is not enrolled")}
	}
	oauthClient, err := auth.NewReadOnlyOAuthClient(context.Background(), disc, store)
	if err != nil {
		return doctorUnauthenticatedAS{disc: disc, err: errors.New("local authorization is incomplete")}
	}
	return auth.NewMiningClient(disc, oauthClient, store)
}

// gatherDoctorFacts asks each source once, keeping failures rather than
// returning on the first one — a degraded AS must not blank the report.
// gatherDoctorFacts is the AS-and-store half, for callers with no [miner]
// block to speak of. gatherDoctorFactsFor is the whole of it.
func gatherDoctorFacts(ctx context.Context, as asClient, m config.Mining) doctorFacts {
	return gatherDoctorFactsFor(ctx, as, m, config.Miner{}, realIntakeProbeOps())
}

func gatherDoctorFactsFor(ctx context.Context, as asClient, m config.Mining, miner config.Miner, probe intakeProbeOps) doctorFacts {
	f := doctorFacts{
		ASBaseURL: m.ASBaseURL,
		ChainID:   m.ChainID,
		SlotID:    m.SlotID,
		SpoolDir:  m.SpoolDir,
		// Sampled once, here, so every window comparison downstream is
		// against the same instant.
		Now:          time.Now(),
		MinerEnabled: miner.Enabled,
		IntakeDir:    miner.IntakeDir,
	}

	// Local first, and unconditionally: it is the half that still answers
	// when nothing else does.
	if store, err := auth.OpenStoreExisting(m.StateDir); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			f.LocalStateKnown = true
			f.MiningDecision = auth.MiningDecision{State: auth.MiningUndecided}
		} else {
			f.LocalErr = err
			f.MiningDecision = auth.MiningDecision{State: auth.MiningDegraded, Present: true, Err: err}
		}
	} else {
		f.LocalStateKnown = true
		f.MiningDecision = store.ReadMiningDecision()
		_, ok, err := store.LoadRefreshToken()
		if err != nil {
			f.LocalErr = err
		}
		f.HasRefresh = ok
		if f.HasRefresh {
			if _, err := store.DPoPKeyExisting(); err != nil {
				f.AuthIncomplete, f.AuthIncompleteErr = true, err
			}
		}
		if reg, ok, err := store.LoadAgentRegistration(); err == nil && ok && reg.LastEnrollmentSlot != "" {
			f.HasRegistration, f.RegistrationSlot, f.RegistrationAt = true, reg.LastEnrollmentSlot, reg.LastEnrollmentAt
		}
		f.Health, f.HealthErr = store.HealthRecords()
	}

	// The miner half. The probe is the one write doctor performs, and it
	// is skipped entirely unless intake is both configured and active —
	// there is nothing to diagnose about a directory no search will use.
	if f.MinerEnabled && f.MiningDecision.State == auth.MiningEnabled {
		f.IntakeProbe = probeIntakeWritable(probe, miner.IntakeDir)
		f.IntakeCount, f.IntakeErr = countIntakeJSON(miner.IntakeDir)
		f.Stamp, f.StampPresent, f.StampErr = readFlushStampForDoctor(flushStampPath(miner))
		f.SpoolCount, f.QuarantineCount, f.SpoolErr = countSpool(m.SpoolDir)
	}

	f.Doc, f.DocErr = as.ServiceDocument(ctx)
	f.EpochPinned = m.TargetEpoch != nil
	if f.DocErr != nil {
		// Discovery failing is the one failure that decides every AS-backed
		// check at once, so it is recorded against each of them rather than
		// tried four more times. The alternative — leaving them nil — is
		// what made "no payout address is in force" appear on a run that
		// never asked, which is a claim about a participant's setup made
		// from no evidence at all.
		f.EpochErr, f.StatusErr, f.StandingErr, f.ActivityErr = f.DocErr, f.DocErr, f.DocErr, f.DocErr
		return f
	}

	// Where you are paid does not depend on which epoch is open, so it is
	// asked even when no target is.
	f.Standing, f.StandingErr = as.PayoutStanding(ctx)

	f.Epoch, f.EpochKnown, f.EpochErr = epochFor(ctx, as, m.TargetEpoch)
	if !f.EpochKnown {
		return f
	}
	f.Status, f.StatusErr = as.Status(ctx, f.Epoch)
	f.Activity, f.ActivityErr = as.EpochActivity(ctx, f.Epoch)
	return f
}

// epochFor resolves the target epoch without printing anything, which is
// what separates it from resolveEpoch: a report has to carry the failure
// into a line rather than emit it as a side effect.
//
// The three answers are kept apart because they call for different lines:
//
//	(e, true, nil)    this is the epoch
//	(0, false, nil)   the AS answered, and nothing is open
//	(0, false, err)   the AS could not be asked
func epochFor(ctx context.Context, as asClient, pinned *uint64) (uint64, bool, error) {
	if pinned != nil {
		return *pinned, true, nil
	}
	t, err := as.CurrentTarget(ctx)
	if err != nil {
		return 0, false, err
	}
	if t == nil {
		return 0, false, nil
	}
	return t.TargetEpoch, true, nil
}

// The two column widths every line in the report shares.
const (
	doctorNameWidth    = 21
	doctorVerdictWidth = 7
)

func printDoctor(w io.Writer, checks []doctorCheck, f doctorFacts) {
	if f.LocalStateKnown {
		detail := "persisted runtime decision"
		switch f.MiningDecision.State {
		case auth.MiningUndecided:
			detail = "no persisted runtime decision; mining remains inactive"
		case auth.MiningDegraded:
			detail = "mining is stopped for safety until the decision can be trusted"
		}
		fmt.Fprintf(w, "  mining                 %-7s  %s%s\n", miningDecisionText(f.MiningDecision), detail, miningDecisionDetail(f.MiningDecision))
	}
	if f.ASConfigKnown {
		if f.ASConfigured {
			fmt.Fprintf(w, "  authorization config   %-7s  %s\n", "SET", f.ASBaseURL)
		} else {
			fmt.Fprintf(w, "  authorization config   %-7s  no authorization server configured\n", "UNSET")
		}
	}
	for _, record := range f.Health {
		label := "health"
		if f.MiningDecision.State == auth.MiningDisabled {
			label = "health (previous unresolved)"
		}
		if record.Detail == "" {
			fmt.Fprintf(w, "  %s %-14s %-7s  %s\n", label, record.Component, "OPEN", record.Reason)
		} else {
			fmt.Fprintf(w, "  %s %-14s %-7s  %s — %s\n", label, record.Component, "OPEN", record.Reason, record.Detail)
		}
	}
	if f.HealthErr != nil {
		fmt.Fprintf(w, "  health               UNKNOWN  %v\n", redact.Error(f.HealthErr))
	}
	var unknown []string
	for _, c := range checks {
		fmt.Fprintf(w, "  %-*s %-*s  %s\n", doctorNameWidth, c.Name, doctorVerdictWidth, c.Verdict, c.Detail)
		if c.Fix != "" {
			fmt.Fprintf(w, "  %-*s %-*s  → %s\n", doctorNameWidth, "", doctorVerdictWidth, "", c.Fix)
		}
		if c.Verdict == verdictUnknown {
			unknown = append(unknown, c.Name)
		}
	}

	fmt.Fprintln(w)
	printQueueTo(w, f.SpoolDir)

	if len(unknown) > 0 {
		fmt.Fprintf(w, "\n  %d check(s) could not run: %s.\n", len(unknown), strings.Join(unknown, ", "))
		fmt.Fprintln(w, "  Everything above them was answered; nothing was assumed.")
	}

	// The two things a participant most often reads into a health check
	// that are not true. Stated once, at the end, where they are read.
	fmt.Fprintln(w)
	fmt.Fprintln(w, "  An epoch's reward is an equal split among its eligible participants. One verified")
	fmt.Fprintln(w, "  observation qualifies; more do not earn more, and no amount belongs to any single")
	fmt.Fprintln(w, "  request. This command reports setup — for what the chain has actually paid, run:")
	fmt.Fprintln(w, "      dropin-miner earnings")
}

// printQueueTo is printQueue against an explicit writer. The local backlog
// is the one line that answers whether an unreachable AS is losing work
// (it is not) or the daemon is idle.
func printQueueTo(w io.Writer, spoolDir string) {
	line := func(text string) {
		fmt.Fprintf(w, "  %-*s %-*s  %s\n", doctorNameWidth, "queued", doctorVerdictWidth, "", text)
	}
	cont := func(text string) {
		fmt.Fprintf(w, "  %-*s %-*s  %s\n", doctorNameWidth, "", doctorVerdictWidth, "", text)
	}
	sp, err := spool.OpenExisting(spoolDir)
	if err != nil {
		line(fmt.Sprintf("unknown (%v)", redact.Error(err)))
		return
	}
	// Count, not Len: Len goes through Pending, which quarantines records
	// it cannot parse, and a read-only report must not move a running
	// daemon's files.
	n, err := sp.Count()
	if err != nil {
		line(fmt.Sprintf("unknown (%v)", redact.Error(err)))
		return
	}
	line(fmt.Sprintf("%d observation(s) in %s", n, spoolDir))
	if n > 0 {
		cont("held locally until the AS accepts them; nothing is lost")
	}
}

// The production implementation, asserted at compile time so a signature
// change in internal/auth breaks here rather than at the call site.
var _ asClient = (*auth.MiningClient)(nil)

// ── intake writability, and whether anything is being recorded ──────────
//
// These two checks answer #21: "searches are running but nothing is being
// recorded" had no line of its own, because every other check reads the AS
// or the store and neither can see the one thing that breaks — the intake
// intake directory the `search` command writes into not being the one the
// flush reads from. (The record is written by search itself, through
// writeIntake; the hooks maintain lineage and start flushes.)

// intakeProbeOps is the seam for the one write doctor performs.
//
// It is exactly writeIntake's own sequence plus removal, because a probe
// that modeled an approximation of the real writer would answer a question
// nobody asked. writeIntake does os.MkdirAll(dir, 0o700) then
// fsx.WriteFileAtomic(dir, name, data, 0o600); so does this.
type intakeProbeOps struct {
	mkdirAll    func(string, os.FileMode) error
	writeAtomic func(dir, name string, data []byte, mode os.FileMode) error
	remove      func(string) error
}

func realIntakeProbeOps() intakeProbeOps {
	return intakeProbeOps{
		mkdirAll:    os.MkdirAll,
		writeAtomic: fsx.WriteFileAtomic,
		remove:      os.Remove,
	}
}

// Probe stage names, as the detail prints them.
const (
	probeStageMkdir  = "creating the intake directory"
	probeStageWrite  = "writing a probe file"
	probeStageRemove = "removing the probe file"
)

// intakeProbeResult is what the probe established, with no wording applied
// yet — doctorIntakeCheck turns it into a verdict.
type intakeProbeResult struct {
	// Ran is false when the probe deliberately did nothing: no [miner],
	// mining not active, or the intake directory's parent is missing.
	Ran bool
	Dir string
	// ParentMissing means the probe declined to create a tree. README
	// promises doctor does not create a state directory merely to
	// diagnose it, and that promise stops being true the moment this
	// walks up.
	ParentMissing bool
	// Created means mkdirAll created IntakeDir itself. It is left in
	// place: it is what the first search creates anyway.
	Created bool
	// Stage and Err describe the first failure. Empty Stage is success.
	Stage string
	Err   error
	// Leftover is the probe pathname when cleanup could not remove it.
	// Set independently of Stage, because cleanup is attempted even after
	// a write failure.
	Leftover string
}

// ok means the probe ran AND every stage succeeded.
//
// Ran is part of it because a probe that was deliberately skipped has
// established nothing. Reading "no failure recorded" as success is how a
// check reports OK for a directory it never touched, and how `recording`
// would go on to call a state suspicious on the strength of writability it
// never tested.
func (p intakeProbeResult) ok() bool { return p.Ran && p.Stage == "" && p.Leftover == "" }

// probeIntakeWritable publishes one file in the intake directory and
// attempts to remove it. Not "writes and removes": the removal is an
// attempt, which is what Leftover exists to report, and the paragraph
// below already says so — the summary line said otherwise.
//
// The name deliberately does not end in .json. readIntake considers only
// .json files, so a probe that somehow outlives this process — a crash
// between write and remove, a cleanup failure reported below — can never
// be promoted into the mining pipeline. That is the property worth having;
// "never leaves a file behind" is not one this can honestly promise.
func probeIntakeWritable(ops intakeProbeOps, dir string) intakeProbeResult {
	res := intakeProbeResult{Dir: dir}
	if dir == "" {
		res.ParentMissing = true
		return res
	}
	// Bounded creation: at most the intake directory itself, and only when
	// its parent is already there.
	_, statErr := os.Stat(dir)
	absent := errors.Is(statErr, fs.ErrNotExist)
	if absent {
		if _, perr := os.Stat(filepath.Dir(dir)); perr != nil {
			res.ParentMissing = true
			return res
		}
	}

	res.Ran = true
	if err := ops.mkdirAll(dir, 0o700); err != nil {
		res.Stage, res.Err = probeStageMkdir, err
		return res
	}
	res.Created = absent

	name := fmt.Sprintf(".doctor-probe-%d-%s.tmp", os.Getpid(), randomSuffix())
	path := filepath.Join(dir, name)
	werr := ops.writeAtomic(dir, name, []byte("dropin-miner doctor probe\n"), 0o600)

	// Cleanup runs on every exit path, including after a write failure:
	// some failure shapes leave the final name in place. A cleanup failure
	// is itself a probe failure — a leftover cannot enter the pipeline,
	// but a participant is still owed the pathname.
	if rerr := ops.remove(path); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
		res.Leftover = path
		if werr == nil {
			res.Stage, res.Err = probeStageRemove, rerr
		}
	}
	if werr != nil {
		res.Stage, res.Err = probeStageWrite, werr
	}
	return res
}

// sandboxDenied reports the permission shapes an agent sandbox produces —
// EACCES and EPERM on Linux under Landlock, EROFS on macOS under Seatbelt.
// The EROFS case is matched by string so this stays correct on every GOOS
// without a syscall import, exactly as intakeWriteBlocked does.
func sandboxDenied(err error) bool {
	return err != nil &&
		(errors.Is(err, fs.ErrPermission) || strings.Contains(err.Error(), "read-only file system"))
}

// doctorIntakeCheck: can this process write where a served search records
// its observation?
//
// The wording is careful about what a success proves. The probe runs as
// whoever ran doctor, which is usually a person at a terminal; a search
// runs inside the agent's sandbox. Those are different subjects, so the
// detail says "from this process" and the fix only SUGGESTS the sandbox.
// The search path's own "inside this agent's sandbox" sentence is entitled
// to be definite because that write actually happened inside one.
func doctorIntakeCheck(f doctorFacts) doctorCheck {
	c := doctorCheck{Name: "intake writable"}
	if !f.MinerEnabled {
		c.Verdict, c.Detail = verdictOK, "not configured"
		return c
	}
	if f.MiningDecision.State != auth.MiningEnabled {
		c.Verdict, c.Detail = verdictOK, "mining not active"
		return c
	}
	p := f.IntakeProbe
	if p.ParentMissing {
		// Nothing was tried, so nothing is known. The bounded-creation
		// rule is what stopped it — doctor will not build a directory
		// tree to diagnose one — and saying OK here would report a
		// writable intake directory on the strength of never having
		// looked.
		c.Verdict = verdictUnknown
		c.Detail = fmt.Sprintf("could not determine — %s and its parent do not exist, "+
			"and doctor does not create the parent tree merely to test it; "+
			"the first search creates the directory", p.Dir)
		return c
	}
	if p.ok() {
		c.Verdict = verdictOK
		c.Detail = "writable from this process"
		if p.Created {
			c.Detail = "writable from this process (created " + p.Dir + ")"
		}
		return c
	}

	c.Verdict = verdictNo
	switch {
	case p.Stage != "" && p.Leftover != "":
		c.Detail = fmt.Sprintf("%s failed for %s — %v; and the probe file %s could not be removed",
			p.Stage, p.Dir, redact.Error(p.Err), p.Leftover)
	case p.Stage != "":
		c.Detail = fmt.Sprintf("%s failed for %s — %v", p.Stage, p.Dir, redact.Error(p.Err))
	default:
		c.Detail = fmt.Sprintf("the probe file %s could not be removed", p.Leftover)
	}
	if sandboxDenied(p.Err) {
		c.Fix = fmt.Sprintf("if searches run under Codex, re-run `dropin-miner agents install` so the sandbox allows %s", p.Dir)
	}
	return c
}

// readFlushStampForDoctor is readFlushStamp's opposite number.
//
// readFlushStamp turns every failure — unreadable file, malformed JSON, a
// version it does not know — into a zero stamp, which is right for the
// flush (an unreadable stamp must not stop mining) and useless here: a
// diagnosis that cannot tell "no flush has ever run" from "the stamp is
// unreadable" will report the first when it means the second. That is the
// whole bug class this check exists to avoid, so it reads the file again
// rather than reusing a function designed to lose the distinction.
//
// readFlushStamp itself is untouched.
func readFlushStampForDoctor(path string) (flushStamp, bool, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- our own state dir
	if errors.Is(err, fs.ErrNotExist) {
		return flushStamp{}, false, nil
	}
	if err != nil {
		return flushStamp{}, false, err
	}
	var st flushStamp
	if jerr := json.Unmarshal(data, &st); jerr != nil {
		return flushStamp{}, false, fmt.Errorf("flush stamp %s is not valid JSON: %w", path, jerr)
	}
	if st.V != 1 {
		return flushStamp{}, false, fmt.Errorf("flush stamp %s has version %d, not 1", path, st.V)
	}
	return st, true, nil
}

// countIntakeJSON counts what a flush would promote, and nothing else.
//
// readIntake is not used: it parses every record and reports the ones it
// could not, and a malformed record is still evidence that something was
// recorded. Counting names answers the question being asked.
func countIntakeJSON(dir string) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, nil // never created: known empty, not unknown
		}
		return 0, err
	}
	n := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		n++
	}
	return n, nil
}

// recordingWindow is how far back a flush stamp still counts as recent.
const recordingWindow = 24 * time.Hour

// doctorRecordingCheck is a heuristic and says so in its own wording.
//
// It never returns NO. Every input it reads is circumstantial — a flush
// stamp proves the mining plane ran, not that a search did; an empty spool
// proves nothing is queued, not that nothing was ever queued — and a
// verdict of NO would assert a fault this evidence cannot establish. The
// suspicious combination gets UNKNOWN with advice, which is the honest
// shape: something here does not add up, here is the one thing to check.
//
// Every input distinguishes absent from unreadable, and an unreadable one
// produces "could not determine — <reason>" rather than being folded into
// the absent case. Silently treating unreadable as empty is how a
// diagnosis tells a participant their setup is fine when it has not
// looked.
func doctorRecordingCheck(f doctorFacts) doctorCheck {
	c := doctorCheck{Name: "recording"}
	undetermined := func(reason string) doctorCheck {
		c.Verdict, c.Detail = verdictUnknown, "could not determine — "+reason
		return c
	}

	if !f.MinerEnabled {
		c.Verdict, c.Detail = verdictOK, "not configured"
		return c
	}
	if f.MiningDecision.State != auth.MiningEnabled {
		c.Verdict, c.Detail = verdictOK, "mining not active"
		return c
	}

	// Anything unreadable stops the heuristic before it can conclude.
	switch {
	case f.HealthErr != nil:
		return undetermined(fmt.Sprintf("component health could not be read: %v", redact.Error(f.HealthErr)))
	case f.StampErr != nil:
		return undetermined(fmt.Sprintf("the flush stamp could not be read: %v", redact.Error(f.StampErr)))
	case f.IntakeErr != nil:
		return undetermined(fmt.Sprintf("the intake directory could not be read: %v", redact.Error(f.IntakeErr)))
	case f.SpoolErr != nil:
		return undetermined(fmt.Sprintf("the spool could not be read: %v", redact.Error(f.SpoolErr)))
	case f.StampPresent && f.Stamp.LastFlush.After(f.Now):
		// Never "recent": a clock that disagrees with the stamp makes
		// every window comparison below meaningless.
		return undetermined("the flush stamp is in the future")
	case f.IntakeProbe.ParentMissing:
		return undetermined("intake writability was not tested; see intake writable")
	case !f.IntakeProbe.ok():
		return undetermined("the intake probe failed; see intake writable")
	}

	// Anything actually recorded settles it.
	if f.IntakeCount > 0 {
		c.Verdict = verdictOK
		c.Detail = fmt.Sprintf("%d observation(s) waiting in %s", f.IntakeCount, f.IntakeDir)
		return c
	}
	if f.SpoolCount > 0 || f.QuarantineCount > 0 {
		c.Verdict = verdictOK
		c.Detail = fmt.Sprintf("%d observation(s) queued in the spool", f.SpoolCount+f.QuarantineCount)
		if f.QuarantineCount > 0 {
			c.Detail += fmt.Sprintf(" (%d quarantined)", f.QuarantineCount)
		}
		return c
	}
	if rec, ok := captureHealth(f.Health); ok {
		// The client already knows capture failed and has said so on its
		// own line. Repeating it as a mystery here would be worse than
		// saying nothing.
		c.Verdict = verdictOK
		c.Detail = fmt.Sprintf("a capture failure is already recorded (%s); see the health line above", rec.Reason)
		return c
	}

	// With nothing recorded, the question is whether anything ran.
	if !f.StampPresent || f.Stamp.LastFlush.Before(f.Now.Add(-recordingWindow)) {
		c.Verdict = verdictOK
		c.Detail = "no recent activity"
		return c
	}

	// Something ran, and nothing local shows for it. The AS is the last
	// place a recorded observation could be.
	if f.ActivityErr != nil || f.Activity == nil {
		return undetermined("the AS did not report this epoch's activity")
	}
	a := f.Activity
	if a.VerifiedActivity && a.VerifiedObservationCount == 0 &&
		a.PendingObservationCount == 0 && a.RejectedObservationCount == 0 {
		// The AS owns the eligibility verdict and also reports the count
		// it was derived from; pkg/auth keeps both rather than
		// recomputing one, precisely so a disagreement stays visible.
		// Concluding "nothing reached the AS" from an answer that says
		// there was verified activity would resolve that contradiction in
		// the participant's disfavor, so it is reported instead.
		return undetermined("the AS reports verified activity for this epoch but a verified count of zero; the two disagree")
	}
	if a.VerifiedObservationCount > 0 || a.PendingObservationCount > 0 || a.RejectedObservationCount > 0 {
		c.Verdict = verdictOK
		c.Detail = fmt.Sprintf("the AS has %d verified, %d pending and %d rejected for this epoch",
			a.VerifiedObservationCount, a.PendingObservationCount, a.RejectedObservationCount)
		return c
	}

	c.Verdict = verdictUnknown
	c.Detail = fmt.Sprintf("recent miner activity, but nothing is queued locally or verified at the AS; "+
		"if searches have been running, check that %s is the mining intake directory used by "+
		"the agent's `dropin-miner search` command", f.IntakeDir)
	c.Fix = "dropin-miner agents status; re-run `dropin-miner agents install` if the agent is using another config or lacks sandbox access"
	return c
}

func captureHealth(records []auth.HealthRecord) (auth.HealthRecord, bool) {
	for _, r := range records {
		if r.Component == auth.HealthCapture {
			return r, true
		}
	}
	return auth.HealthRecord{}, false
}

// countSpool reads both halves of the queue without moving anything.
//
// A missing spool directory is known-empty rather than an error: the
// collector creates it, and a participant who has never flushed has none.
func countSpool(dir string) (active, quarantined int, err error) {
	sp, oerr := spool.OpenExisting(dir)
	if oerr != nil {
		if errors.Is(oerr, fs.ErrNotExist) {
			return 0, 0, nil
		}
		return 0, 0, oerr
	}
	if active, err = sp.Count(); err != nil {
		return 0, 0, err
	}
	quarantined, err = sp.CountQuarantined()
	if err != nil {
		return 0, 0, err
	}
	return active, quarantined, nil
}
