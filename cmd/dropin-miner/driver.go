package main

// The epoch driver, lifted from the daemon: the one-tick logic a flush
// runs once (target, join, capability). The ticker that used to drive it
// lives nowhere now; `flush` calls target/joinIfNeeded/ensure directly.

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"golang.org/x/oauth2"

	"github.com/twilight-project/dropin-miner/pkg/auth"
	"github.com/twilight-project/dropin-miner/pkg/redact"
)

// driverTickTimeout bounds one tick's AS calls. A hung AS costs this
// loop one tick; it may never cost the loop itself.
const driverTickTimeout = 30 * time.Second

// joinedMemory is how many recently joined epochs the driver remembers,
// so that it does not re-ask to join the same target every minute.
// Fixed size on purpose: this process is meant to run for months, and a
// set that gains an entry per epoch forever is a leak. Forgetting is
// safe — the AS is asked before every join, so a forgotten epoch costs
// one extra status call, never a second enrollment.
const joinedMemory = 8

// miningPlane is the assembled mining side. It exists only to be handed
// to the observer and shut down; nothing on the inference path reads it.
// block, slow or fail a forwarded request.
type epochDriver struct {
	mining *auth.MiningClient
	caps   *auth.CapabilityClient
	// pinned is the operator's mining.target_epoch override: nil means
	// ask the AS, non-nil means use exactly this epoch and never query.
	pinned *uint64
	logger *slog.Logger

	// store persists what WP4b needs to survive across flush invocations
	// (which epochs conflicted) — see auth.Store.SaveEpochConflict. nil in
	// every existing driver-level test that predates this and does not
	// exercise it; every call site below guards on it being set.
	store *auth.Store

	// joined is the bounded ring of epochs already enrolled in this
	// process; next is the slot the ring overwrites once it is full.
	joined []uint64
	next   int

	// enrolled reports whether this installation holds a refresh
	// authorization. It decides the LEVEL a credential failure is logged
	// at, and it reads custody rather than this process's own history on
	// purpose: `joined` is per-process, so a daemon restarted after its
	// credential died would have joined nothing yet and would go quiet
	// again at exactly the moment it most needs to speak.
	enrolled func() bool

	// notes is the de-duplication state for the log. Keyed ONLY by the
	// note* constants below, so it is bounded by that closed set and
	// cannot grow with time, epochs, or distinct AS failures.
	notes map[string]note
	// repeat is how long the same complaint stays suppressed. A field
	// rather than a constant so a test can shorten it.
	repeat time.Duration

	// mu guards the published join state below, which the health lines
	// read from another goroutine. Nothing else here is shared.
	mu   sync.Mutex
	join JoinState
}

// note is one suppressed complaint: what was said, and when.
type note struct {
	line string
	at   time.Time
}

// noteRepeat is how long a persisting failure stays quiet before saying so
// again.
//
// The loop ticks every minute, so silence is the default and the only
// question is how long it lasts. Once-and-never-again was the old answer
// and it cost the Slot 3 run ninety minutes of a daemon that had stopped
// mining and said nothing. Four lines an hour is not a flood, and it is the
// difference between an operator finding this in the log and finding it in
// the payout register.
const noteRepeat = 15 * time.Minute

// JoinState is what the driver publishes about the join path, and the whole
// of it.
//
// It exists because the collector's health cannot answer this. Queued and
// ConsecutiveFailures are written by the DELIVERY path — a daemon that
// cannot join an epoch never queues an observation and never attempts a
// submission, so both stay at zero however long it has been failing, and
// the "connected and earning nothing" summary could not fire on this path
// by construction.
type JoinState struct {
	// Failures is consecutive join failures; it resets on a join.
	Failures int
	// Since is when the current run of failures began.
	Since time.Time
	// Note is the last failure, redacted.
	Note string
	// LastJoin and LastEpoch are the most recent success in this process.
	LastJoin  time.Time
	LastEpoch uint64
}

// JoinState returns a snapshot for the health lines.
func (d *epochDriver) JoinState() JoinState {
	if d == nil {
		return JoinState{}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.join
}

func (d *epochDriver) recordJoinFailure(err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.join.Failures == 0 {
		d.join.Since = time.Now()
	}
	d.join.Failures++
	if err != nil {
		d.join.Note = redact.Error(err).Error()
	}
}

func (d *epochDriver) recordJoinSuccess(epoch uint64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.join = JoinState{LastJoin: time.Now(), LastEpoch: epoch}
}

// credentialLevel is Warn once this installation is enrolled, and Debug
// before.
//
// The Debug this replaces carried its own justification — "before
// enrollment this fails on every tick by design, and an operator watching
// logs should not be told the proxy is broken when it is merely not
// joined" — which is right about the state the daemon STARTS in and wrong
// about the state it ends up in. A participant who has enrolled and is
// being refused is not "merely not joined"; they are earning nothing, and
// on 2026-08-27 that went unsaid for ninety minutes.
func (d *epochDriver) credentialLevel() slog.Level {
	if d.enrolled != nil && d.enrolled() {
		return slog.LevelWarn
	}
	return slog.LevelDebug
}

// The de-duplicated log sites.
const (
	noteTarget     = "target"
	noteJoin       = "join"
	noteCapability = "capability"
)

func newEpochDriver(mining *auth.MiningClient, caps *auth.CapabilityClient, pinned *uint64, logger *slog.Logger, enrolled func() bool) *epochDriver {
	return &epochDriver{
		mining:   mining,
		caps:     caps,
		pinned:   pinned,
		logger:   logger,
		enrolled: enrolled,
		joined:   make([]uint64, 0, joinedMemory),
		notes:    make(map[string]note, 3),
		repeat:   noteRepeat,
	}
}

// withStore attaches the store WP4b's persistence needs, after
// construction rather than as a newEpochDriver parameter — every existing
// call site (five in mining_test.go alone) constructs a driver with no
// store at all, and nil is the correct, already-guarded-for value for a
// test exercising the log-dedup logic rather than epoch participation.
func (d *epochDriver) withStore(store *auth.Store) *epochDriver {
	d.store = store
	return d
}

// tick is one pass: find the target, join it if it must, hold a
// capability for it. Each step is independent — a step that fails costs
// this tick, never the next one.
func (d *epochDriver) tick(ctx context.Context) {
	reqCtx, cancel := context.WithTimeout(ctx, driverTickTimeout)
	defer cancel()

	target := d.target(reqCtx)
	if target.state != targetResolved {
		return
	}
	d.joinIfNeeded(reqCtx, target.epoch)
	d.ensure(reqCtx, target.epoch)
}

type targetState uint8

const (
	targetResolved targetState = iota
	targetNoOpen
	targetEndpointAbsent
	targetResolutionFailed
)

// targetResult keeps the three current-target outcomes distinct for callers.
// queried says whether a live current-target request established the result;
// a pinned epoch is resolved locally and must not clear current-target health.
type targetResult struct {
	state       targetState
	epoch       uint64
	err         error
	authFailure bool
	queried     bool
}

// targetAuthFailure keeps the existing OAuth/credential classification and
// stops inferring it from message text: token-endpoint refusals are
// oauth2.RetrieveError values, a missing refresh authorization is a
// sentinel, and an AS refusal carries the status it answered with.
// Discovery, transport, and other current-target failures remain ordinary
// target-resolution failures.
//
// The strings this replaced were a contract nobody had agreed to. Whether
// a participant's health record said "your authorization needs attention"
// or "delivery failed" depended on five substrings matching the wording of
// errors built three packages away, so rephrasing a message silently
// reclassified a fault — and any unrelated error whose text happened to
// quote one of those phrases was classified as an authorization failure on
// the strength of its prose.
func targetAuthFailure(err error) bool {
	if err == nil {
		return false
	}
	var retrieveErr *oauth2.RetrieveError
	var refused *auth.ASRefusedError
	return errors.As(err, &retrieveErr) ||
		errors.Is(err, auth.ErrNoRefreshAuthorization) ||
		(errors.As(err, &refused) &&
			(refused.Status == http.StatusUnauthorized || refused.Status == http.StatusForbidden))
}

// target resolves the epoch this tick works on: the operator's pin when
// there is one, otherwise whatever the AS currently offers.
func (d *epochDriver) target(ctx context.Context) targetResult {
	if d.pinned != nil {
		// Pinned means pinned: no query, so this path also works
		// against an AS that cannot answer one.
		return targetResult{state: targetResolved, epoch: *d.pinned}
	}
	t, err := d.mining.CurrentTarget(ctx)
	switch {
	case errors.Is(err, auth.ErrNoCurrentTargetEndpoint):
		// Warn, unlike the failures below, and still only once: this is
		// not a state that clears on its own, and the operator has two
		// real fixes. Re-checked every tick anyway, because discovery
		// is refetched when its TTL expires — an upgraded AS is picked
		// up without restarting the proxy.
		d.once(ctx, slog.LevelWarn, noteTarget,
			"mining: this AS advertises no current-target endpoint; upgrade it, or pin mining.target_epoch", err)
		return targetResult{state: targetEndpointAbsent, err: err, queried: true}
	case err != nil:
		// credentialLevel, NOT Debug — and this is the site that matters
		// most for it.
		//
		// The Warn-once-enrolled escalation was written for the join and
		// capability sites below, after a daemon spent ninety minutes
		// saying nothing while it earned nothing. It was applied one
		// level too deep. This call is the FIRST thing a tick makes, so a
		// dead or suspect credential fails HERE and returns, and the two
		// sites that would have spoken are never reached. Every failure
		// of the authorization arrives at this line: a revoked family, a
		// conclusive refusal, a rotation whose outcome was never learned.
		// All of them were Debug, and the default level is Info.
		//
		// Debug's original justification survives inside credentialLevel
		// rather than being argued away: before enrollment this fails on
		// every tick by design, and nobody should be told the proxy is
		// broken when it is merely not joined yet.
		d.once(ctx, d.credentialLevel(), noteTarget, "mining: current target unavailable", err)
		return targetResult{state: targetResolutionFailed, err: err, authFailure: targetAuthFailure(err), queried: true}
	}
	d.clear(noteTarget)
	if t == nil {
		// 200 with nothing open is an answer, not a fault: the slot is
		// between targets. Nothing to do, and nothing to say about it —
		// this is the ordinary state for most of an epoch boundary.
		return targetResult{state: targetNoOpen, queried: true}
	}
	return targetResult{state: targetResolved, epoch: t.TargetEpoch, queried: true}
}

// joinIfNeeded enrolls for epoch, at most once.
//
// The AS is ASKED whether the target is joinable, never told. After a
// successful join it reports joinable=false with join_status ACCEPTED,
// which is the same shape as "enrollment closed and we are not in" — only
// join_status separates them, so inferring from joinable alone would
// either re-join forever or give up on a target we hold.
func (d *epochDriver) joinIfNeeded(ctx context.Context, epoch uint64) {
	if d.hasJoined(epoch) {
		return
	}
	st, err := d.mining.Status(ctx, epoch)
	if err != nil {
		// Same correction as target's, for the same reason: this is the
		// second authorized call a tick makes, and it is where a
		// credential failure lands whenever the epoch is pinned and
		// target() never queried the AS at all.
		d.once(ctx, d.credentialLevel(), noteJoin, "mining: epoch status unavailable", err,
			slog.Uint64("target_epoch", epoch))
		return
	}
	d.clear(noteJoin)
	if !st.Joinable {
		if auth.JoinHeld(st.JoinStatus) {
			d.remember(epoch)
		}
		// Not joinable and not held means this target closed without
		// us. There is nothing to retry, and the next target will come
		// from the AS like any other.
		return
	}
	res, err := d.mining.JoinEpoch(ctx, epoch)
	if err != nil {
		if errors.Is(err, auth.ErrEnrollmentConflict) {
			// WP4b (design f0ddb69 §2.3/§5.5): another installation of
			// this participant already holds this epoch. Not a failure —
			// recordJoinFailure/Warn-escalation is for the AS being
			// unreachable or this installation being unauthorized, neither
			// of which is true here — so this is deliberately NOT that
			// path. Remembered so the next tick does not re-attempt the
			// same epoch every minute, and persisted so `status` and a
			// later flush's drop-after-deadline check can see it.
			d.remember(epoch)
			if d.store != nil {
				if serr := d.store.SaveEpochConflict(d.mining.SlotID(), epoch); serr != nil {
					d.once(ctx, slog.LevelWarn, noteJoin, "mining: could not persist enrollment conflict", serr,
						slog.Uint64("target_epoch", epoch))
				}
			}
			d.clear(noteJoin)
			return
		}
		d.recordJoinFailure(err)
		d.once(ctx, d.credentialLevel(), noteJoin, "mining: join refused", err,
			slog.Uint64("target_epoch", epoch))
		return
	}
	d.recordJoinSuccess(epoch)
	d.remember(epoch)
	// Info, and exactly once per epoch: joining is the event an operator
	// actually wants in the log.
	d.logger.Info("mining: joined target epoch",
		slog.Uint64("target_epoch", epoch),
		slog.String("status", res.Status),
		slog.String("mode", st.DistributionMode))
}

// ensure publishes a capability for epoch into the scope holder.
func (d *epochDriver) ensure(ctx context.Context, epoch uint64) {
	_, err := d.caps.Ensure(ctx, epoch)
	if err != nil {
		if errors.Is(err, auth.ErrProxyBindingMismatch) {
			// WP4b, same fact as joinIfNeeded's ENROLLMENT_CONFLICT branch,
			// caught here instead when this installation's join appeared to
			// succeed locally before another installation's is the one the
			// AS actually bound (contract §30 check 4).
			if d.store != nil {
				if serr := d.store.SaveEpochConflict(d.mining.SlotID(), epoch); serr != nil {
					d.once(ctx, slog.LevelWarn, noteCapability, "mining: could not persist binding conflict", serr,
						slog.Uint64("target_epoch", epoch))
				}
			}
			d.clear(noteCapability)
			return
		}
		// Debug before enrollment — it fails on every tick by design and
		// an operator should not be told the proxy is broken when it is
		// merely not enrolled — and Warn after, where the same failure
		// means a participant who expects to be earning is not.
		d.once(ctx, d.credentialLevel(), noteCapability, "mining: no participation capability", err,
			slog.Uint64("target_epoch", epoch))
		return
	}
	d.clear(noteCapability)
}

// hasJoined / remember maintain the bounded joined ring. Linear scan
// over at most joinedMemory entries, once a minute.
func (d *epochDriver) hasJoined(epoch uint64) bool {
	for _, e := range d.joined {
		if e == epoch {
			return true
		}
	}
	return false
}

func (d *epochDriver) remember(epoch uint64) {
	if d.hasJoined(epoch) {
		return
	}
	if len(d.joined) < joinedMemory {
		d.joined = append(d.joined, epoch)
		return
	}
	d.joined[d.next] = epoch
	d.next = (d.next + 1) % joinedMemory
}

// once logs a degradation the first time it is seen at that site and
// stays quiet while the same one persists. This loop runs every minute
// forever: an AS that is down for a day must cost one line, not 1440.
// A different failure at the same site still speaks, and so does the
// same failure again after the site has recovered (clear resets it).
func (d *epochDriver) once(ctx context.Context, level slog.Level, site, msg string, err error, attrs ...any) {
	if errors.Is(err, context.Canceled) {
		return // shutdown is not a degradation
	}
	line := msg + ": " + err.Error()
	now := time.Now()
	if n, seen := d.notes[site]; seen && n.line == line && now.Sub(n.at) < d.repeat {
		return
	}
	d.notes[site] = note{line: line, at: now}
	d.logger.Log(ctx, level, msg, append([]any{slog.Any("err", redact.Error(err))}, attrs...)...)
}

// clear forgets a site's last complaint, so that a recurrence after a
// recovery is reported rather than swallowed.
func (d *epochDriver) clear(site string) { delete(d.notes, site) }
