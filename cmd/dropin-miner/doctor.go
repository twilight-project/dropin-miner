package main

// `doctor` — the participant's health check.
//
// /statusz already answers the operator's question: which listeners are up,
// which routes are meterable, how much of the ring budget is left. `status`
// answers a different one — it prints what the AS said, flatly, in the AS's
// own vocabulary. Neither answers the question a participant actually has,
// which is "am I set up, and if not, which part is wrong".
//
// So this command is a VERDICT, not a dump. Five lines, each of which is
// either true or not, and each of which names the next command when it is
// not. `status` stays: reading the AS's raw answer is what you want when you
// are debugging the AS, and a verdict hides exactly the detail you would
// need for that.
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

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/twilight-project/dropin-miner/pkg/auth"
	"github.com/twilight-project/dropin-miner/pkg/config"
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
	HasRefresh    bool
	LocalErr      error
	HasEnrollment bool
	LocalSlot     uint64
	LocalEpoch    uint64
}

// assembleDoctor turns the gathered facts into the five verdicts.
//
// It is pure, and it is where every wording rule lives.
func assembleDoctor(f doctorFacts) []doctorCheck {
	return []doctorCheck{
		doctorASCheck(f),
		doctorEnrolledCheck(f),
		doctorJoinedCheck(f),
		doctorPayoutCheck(f),
		doctorEarningCheck(f),
	}
}

func doctorASCheck(f doctorFacts) doctorCheck {
	c := doctorCheck{Name: "authorization server"}
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
		c.Fix = "dropin-miner enroll -config <file>"
	case f.StatusErr != nil && f.DocErr == nil && f.EpochKnown:
		// The AS is up and refused an authorization we hold. That is worth
		// separating from "the AS is down": one is waited out, the other is
		// re-enrolled.
		c.Verdict = verdictNo
		c.Detail = fmt.Sprintf("an authorization is stored here and the AS did not accept it — %v", redact.Error(f.StatusErr))
		c.Fix = "if this does not clear on its own, re-run: dropin-miner enroll -config <file>"
	case f.DocErr != nil:
		c.Verdict = verdictOK
		c.Detail = "an authorization is stored here (read locally; the AS was not reachable to confirm it)"
	default:
		c.Verdict = verdictOK
		c.Detail = "the AS accepted this installation's authorization"
	}
	return c
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

// doctorLocalEnrollmentNote adds what the disk knows when the AS cannot be
// asked. It is the difference between "we have no idea" and "you joined
// epoch 1042 at some point and we could not confirm it today".
func doctorLocalEnrollmentNote(f doctorFacts) string {
	if !f.HasEnrollment || f.LocalSlot != f.SlotID {
		return ""
	}
	return fmt.Sprintf(" (this installation last joined epoch %d, recorded locally)", f.LocalEpoch)
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
		c.Fix = "the daemon has to be running and agent traffic has to go through it: dropin-miner start"
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
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	ctx, cancel := operatorContext(2 * time.Minute)
	defer cancel()

	_, mining, m, code := miningClients(ctx, []string{"-config", *cfgPath}, "doctor")
	if code != 0 {
		return code
	}

	f := gatherDoctorFacts(ctx, mining, m)
	checks := assembleDoctor(f)
	printDoctor(stdout, checks, f)

	// The exit status reports whether the DIAGNOSIS succeeded, not whether
	// the news is good.
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
	for _, c := range checks {
		if c.Verdict != verdictUnknown {
			return exitOK
		}
	}
	return exitTransport
}

// gatherDoctorFacts asks each source once, keeping failures rather than
// returning on the first one — a degraded AS must not blank the report.
func gatherDoctorFacts(ctx context.Context, as asClient, m config.Mining) doctorFacts {
	f := doctorFacts{
		ASBaseURL: m.ASBaseURL,
		ChainID:   m.ChainID,
		SlotID:    m.SlotID,
		SpoolDir:  m.SpoolDir,
	}

	// Local first, and unconditionally: it is the half that still answers
	// when nothing else does.
	if store, err := auth.OpenStore(m.StateDir); err != nil {
		f.LocalErr = err
	} else {
		_, ok, err := store.LoadRefreshToken()
		if err != nil {
			f.LocalErr = err
		}
		f.HasRefresh = ok
		if slotID, epoch, ok, err := store.LoadEnrollment(); err == nil {
			f.HasEnrollment, f.LocalSlot, f.LocalEpoch = ok, slotID, epoch
		}
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
	sp, err := spool.Open(spoolDir)
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
