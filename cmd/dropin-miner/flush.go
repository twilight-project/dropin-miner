package main

// The flush command: the mining plane as one pass instead of a ticker.
//
//	dropin-miner flush [-config file] [-force] [-timeout 90s]
//	dropin-miner flush -detach ...     start it and return immediately
//
// A flush does, in order, exactly what the daemon's driver and collector
// did between two ticks:
//
//	1. ask the AS which epoch is open, join it if not joined, hold a
//	   participation capability for it — skipped when the last flush did
//	   this less than miner.flush_interval ago (-force overrides);
//	2. promote every intake record `search` left behind into a spooled
//	   ProviderObservationV1 under that (slot, epoch);
//	3. deliver the spool once, honoring the collector's removal rule: a
//	   record leaves only on ACCEPTED or ALREADY_ACCEPTED.
//
// Then it exits. `search` starts one after every served request; the
// session hooks start one at session start and end. Two flushes at once
// queue on a lock; the second finds nothing left and exits.
//
// Every failure is reported and none is fatal to anything but this run:
// intake stays on disk, the spool stays on disk, and the next flush
// picks both up. The one thing a flush never does is touch the router —
// it talks to the AS only.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/twilight-project/dropin-miner/pkg/fsx"

	"github.com/twilight-project/dropin-miner/pkg/auth"
	"github.com/twilight-project/dropin-miner/pkg/config"
	"github.com/twilight-project/dropin-miner/pkg/mining/collector"
	"github.com/twilight-project/dropin-miner/pkg/mining/promote"
	"github.com/twilight-project/dropin-miner/pkg/mining/scope"
	"github.com/twilight-project/dropin-miner/pkg/mining/spool"
	"github.com/twilight-project/dropin-miner/pkg/redact"
)

const flushDefaultTimeout = 90 * time.Second

const currentTargetHealthPrefix = "current-target resolution failed: "

func markCurrentTargetHealth(store *auth.Store, target targetResult) {
	if store == nil || target.state != targetResolutionFailed || target.err == nil {
		return
	}
	reason := auth.HealthSubmissionFailed
	if target.authFailure {
		reason = auth.HealthAuthUnavailable
	}
	detail := currentTargetHealthPrefix + redact.Error(target.err).Error()
	_ = store.MarkHealth(auth.HealthFlush, reason, detail)
}

// clearCurrentTargetHealth clears only the flush record produced by a
// current-target resolution failure. Delivery failures and lifecycle faults
// use the same public reason vocabulary but are not proved recovered by a
// successful target lookup.
func clearCurrentTargetHealth(store *auth.Store) {
	if store == nil {
		return
	}
	record, ok, err := store.LoadHealth(auth.HealthFlush)
	if err != nil || !ok || !strings.HasPrefix(record.Detail, currentTargetHealthPrefix) {
		return
	}
	if record.Reason == auth.HealthAuthUnavailable || record.Reason == auth.HealthSubmissionFailed {
		_ = store.ClearHealth(auth.HealthFlush)
	}
}

// clearAuthUnavailableHealth clears a stale flush health record left by an
// earlier run that had no refresh authorization at all (#62), once THIS
// flush has proven authorization is available: it holds a live
// participation capability for the target epoch, which requires exactly
// the refresh authorization the earlier failure was missing. Reached only
// after a join attempt (a fresh join, or finding the epoch already held)
// — a flush with no target open never calls this. The delivery-only
// clearing rule in updateFlushDeliveryHealth stays for submission_failed
// and spool_backlog, whose recovery is a delivery, not authorization; this
// is scoped to auth_state_unavailable alone so it never touches either.
func clearAuthUnavailableHealth(store *auth.Store) {
	if store == nil {
		return
	}
	if record, ok, err := store.LoadHealth(auth.HealthFlush); err == nil && ok && record.Reason == auth.HealthAuthUnavailable {
		_ = store.ClearHealth(auth.HealthFlush)
	}
}

const (
	flushLockHealthPrefix  = "flush lock: "
	flushStampHealthPrefix = "flush stamp: "
)

// markFlushStateHealth records that a flush could not take its lock, best
// effort: when the state directory is what cannot be written, no record can
// be guaranteed.
func markFlushStateHealth(stateDir, detail string) {
	store, err := auth.OpenStoreExisting(stateDir)
	if err != nil {
		return
	}
	_ = store.MarkHealth(auth.HealthFlush, auth.HealthFlushStateUnavailable, detail)
}

// clearFlushStateHealth clears a flush_state_unavailable record whose detail
// names the part (lock or stamp) this run has just shown to work.
func clearFlushStateHealth(store *auth.Store, prefix string) {
	if store == nil {
		return
	}
	record, ok, err := store.LoadHealth(auth.HealthFlush)
	if err == nil && ok && record.Reason == auth.HealthFlushStateUnavailable && strings.HasPrefix(record.Detail, prefix) {
		_ = store.ClearHealth(auth.HealthFlush)
	}
}

// saveFlushStamp is the one way a flush writes its stamp. The stamp is a
// cache: a failure is reported and recorded best-effort, and the caller goes
// on to promote and deliver.
func saveFlushStamp(store *auth.Store, path string, st flushStamp, stderr io.Writer) error {
	if err := writeFlushStamp(path, st); err != nil {
		fmt.Fprintln(stderr, "dropin-miner flush: stamp:", err)
		if store != nil {
			_ = store.MarkHealth(auth.HealthFlush, auth.HealthFlushStateUnavailable, flushStampHealthPrefix+err.Error())
		}
		return err
	}
	clearFlushStateHealth(store, flushStampHealthPrefix)
	return nil
}

// keepFlushStampHealth restores this run's stamp failure when delivery left
// no flush record: an accepted delivery clears the record, but it does not
// make the stamp writable.
func keepFlushStampHealth(store *auth.Store, stampErr error) {
	if store == nil || stampErr == nil {
		return
	}
	if _, ok, err := store.LoadHealth(auth.HealthFlush); err == nil && !ok {
		_ = store.MarkHealth(auth.HealthFlush, auth.HealthFlushStateUnavailable, flushStampHealthPrefix+stampErr.Error())
	}
}

func markFlushHealthFromConfig(cfgPath string, getenv func(string) string, reason auth.HealthReason, detail string) {
	cfg, _, err := loadConfig(cfgPath, getenv)
	if err != nil {
		return
	}
	store, err := auth.OpenStoreExisting(cfg.Mining.StateDir)
	if err != nil {
		return
	}
	_ = store.MarkHealth(auth.HealthFlush, reason, detail)
}

// flushReport is what one pass did, for the summary line and for tests.
type flushReport struct {
	Epoch      uint64
	AskedAS    bool
	Promoted   int
	Unreadable int
	Pending    int // spool records still waiting after delivery
	Delivered  int
}

func cmdFlush(args []string, stdout, stderr io.Writer, getenv func(string) string) int {
	fs := newFlagSet("flush", stderr)
	cfgPath := fs.String("config", "", "path to TOML config file")
	detach := fs.Bool("detach", false, "start the flush in the background and return at once")
	force := fs.Bool("force", false, "ask the AS about the target epoch even if the last flush just did")
	timeout := fs.Duration("timeout", flushDefaultTimeout, "bound on the whole pass")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *detach {
		if err := startFlush(*cfgPath); err != nil {
			markFlushHealthFromConfig(*cfgPath, getenv, auth.HealthFlushSpawnFailed, err.Error())
			fmt.Fprintln(stderr, "dropin-miner flush: could not start:", err)
			return exitTransport
		}
		return exitOK
	}

	// Lifecycle admission, before the config is loaded or any directory
	// made. A flush a search or hook started never waits and, when an
	// uninstall or upgrade holds the gate, leaves quietly with no health
	// record: its intake stays on disk for the next flush. A flush a person
	// ran waits briefly, then says why it did nothing.
	mode := admitForeground
	if detachedChild(getenv) {
		mode = admitDetached
	}
	gatePath, err := configGatePath(*cfgPath, getenv)
	if err != nil {
		fmt.Fprintln(stderr, "dropin-miner flush:", err)
		return exitTransport
	}
	gate, err := admitOrdinary(gatePath, mode)
	if err != nil {
		if mode == admitDetached {
			fmt.Fprintf(stderr, "flush: %v; not flushing now\n", err)
			return exitOK
		}
		fmt.Fprintf(stderr, "dropin-miner flush: %v (%s); nothing was flushed, run it again shortly\n", err, gatePath)
		return exitTransport
	}
	defer gate.release()

	cfg, src, err := loadConfig(*cfgPath, getenv)
	if err != nil {
		fmt.Fprintf(stderr, "dropin-miner flush: config (%s): %v\n", orDefaults(src), err)
		return exitTransport
	}
	if !cfg.Miner.Enabled {
		fmt.Fprintf(stderr, "dropin-miner flush: [miner] is not enabled in %s; the daemon flushes for itself\n", orDefaults(src))
		return exitUsage
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	sctx, stop := signalContext()
	defer stop()
	go func() {
		<-sctx.Done()
		cancel()
	}()

	rep, code := runFlushAdmitted(ctx, cfg, *cfgPath, *force, stdout, stderr, gate.release)
	if code == exitOK {
		fmt.Fprintf(stdout, "flush: epoch %d  promoted %d  delivered %d  pending %d\n",
			rep.Epoch, rep.Promoted, rep.Delivered, rep.Pending)
		if rep.Unreadable > 0 {
			fmt.Fprintf(stderr, "flush: %d intake file(s) could not be read and were left in place\n", rep.Unreadable)
		}
	}
	return code
}

// runFlush is the pass itself, split from the flag parsing so a test can
// drive it against a fake AS and a temp dir.
func runFlush(ctx context.Context, cfg *config.Config, cfgPath string, force bool, stdout, stderr io.Writer) (flushReport, int) {
	return runFlushAdmitted(ctx, cfg, cfgPath, force, stdout, stderr, nil)
}

// runFlushAdmitted is runFlush for a caller that passed the lifecycle gate:
// endAdmission releases it the moment flush.lock has been tried, so the gate
// is never held through the pass itself.
func runFlushAdmitted(ctx context.Context, cfg *config.Config, cfgPath string, force bool, stdout, stderr io.Writer, endAdmission func()) (flushReport, int) {
	var rep flushReport
	m := cfg.Mining
	mn := cfg.Miner

	// MkdirAll succeeds on an existing directory it cannot write, which is
	// the miner root under a sandbox.
	if err := os.MkdirAll(minerRoot(mn), 0o700); err != nil { // #nosec G703 -- the configured intake directory's parent
		if endAdmission != nil {
			endAdmission()
		}
		markFlushStateHealth(m.StateDir, flushLockHealthPrefix+err.Error())
		fmt.Fprintln(stderr, "dropin-miner flush: state dir:", err)
		return rep, exitTransport
	}
	lock, held, lockMode, err := tryFlushLock(flushLockPath(mn))
	if endAdmission != nil {
		endAdmission()
	}
	if err != nil {
		markFlushStateHealth(m.StateDir, flushLockHealthPrefix+err.Error())
		fmt.Fprintf(stderr, "dropin-miner flush: lock %s: %v\n", flushLockPath(mn), err)
		return rep, exitTransport
	}
	if !held {
		flushEvent(ctx, "busy "+lockMode.String())
		fmt.Fprintln(stdout, "flush: another flush is running; nothing to do")
		return rep, exitOK
	}
	defer func() { _ = unlockFile(lock) }()
	lifecycleEvent("operation locked")
	flushEvent(ctx, "locked "+lockMode.String())

	// Read the persisted authority before constructing any OAuth/DPoP client.
	// OFF, undecided, and degraded states must not create mining credentials or
	// promote intake. A missing directory is a normal undecided first-run
	// state; unsafe inspection is degraded and remains fail-closed.
	decision, store := inspectMiningState(m.StateDir)
	clearFlushStateHealth(store, flushLockHealthPrefix)
	if store == nil && decision.State == auth.MiningDegraded {
		fmt.Fprintf(stderr, "dropin-miner flush: mining state cannot be safely trusted%s\n", miningDecisionDetail(decision))
		return rep, exitTransport
	}
	if decision.State == auth.MiningDegraded {
		_ = store.MarkHealth(auth.HealthDecision, auth.HealthDecisionUnreadable, miningDecisionDetail(decision))
		fmt.Fprintf(stderr, "dropin-miner flush: mining state cannot be safely trusted%s\n", miningDecisionDetail(decision))
		return rep, exitOK
	}
	if decision.State != auth.MiningEnabled {
		if decision.State == auth.MiningDisabled {
			if pending, perr := store.LoadRevokePending(); perr == nil && pending && miningASConfigured(m) {
				if oauthClient, _, berr := buildMiningClient(ctx, m); berr == nil {
					retryPendingRevoke(ctx, store, oauthClient)
				} else {
					_ = store.MarkHealth(auth.HealthFlush, auth.HealthAuthUnavailable, berr.Error())
				}
			}
			if !miningASConfigured(m) {
				fmt.Fprintln(stdout, "flush: no authorization server configured; mining is stopped here; nothing to do")
				return rep, exitOK
			}
			fmt.Fprintln(stdout, "flush: mining is stopped here; nothing to do")
		} else {
			fmt.Fprintln(stdout, "flush: mining is not decided here; nothing to do")
		}
		return rep, exitOK
	}

	// The mining plane, built only after persisted ON is proven. This is the
	// operator path's established constructor; missing auth material is a
	// flush failure, never a reason to infer consent.
	_, mining, _, code := miningClients(ctx, []string{"-config", cfgPath}, "flush")
	if code != 0 {
		_ = store.MarkHealth(auth.HealthFlush, auth.HealthAuthUnavailable, "mining authorization state is unavailable")
		return rep, code
	}

	enrolled := func() bool {
		_, ok, err := store.LoadRefreshToken()
		return err == nil && ok
	}
	if !enrolled() {
		_ = store.MarkHealth(auth.HealthFlush, auth.HealthAuthUnavailable, "no refresh authorization is available")
		fmt.Fprintln(stderr, "dropin-miner flush: this installation is not enrolled; run: dropin-miner enroll")
		return rep, exitClientErr
	}

	holder := &scope.Holder{}
	caps := auth.NewCapabilityClient(mining, holder)
	logger := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	driver := newEpochDriver(mining, caps, m.TargetEpoch, logger, enrolled).withStore(store)

	// 1. target, join, capability — or the stamp's answer when fresh.
	stampPath := flushStampPath(m)
	stamp := loadFlushStamp(stampPath, legacyFlushStampPath(mn))
	now := time.Now()
	fresh := !force && stamp.TargetEpoch != 0 && stamp.SlotID == m.SlotID && now.Sub(stamp.LastAS) < mn.FlushInterval
	var epoch uint64
	switch {
	case fresh:
		epoch = stamp.TargetEpoch
	default:
		target := driver.target(ctx)
		if target.state != targetResolved {
			markCurrentTargetHealth(store, target)
			if target.queried && target.state == targetNoOpen {
				clearCurrentTargetHealth(store)
			}
			// driver.target already said why, once. Intake stays for the
			// next flush; the spool may still drain if it holds records
			// for an epoch we joined earlier.
			stamp.LastFlush = now
			stampErr := saveFlushStamp(store, stampPath, stamp, stderr)
			code := deliverOnly(ctx, cfg, mining, caps, store, &rep, stderr)
			keepFlushStampHealth(store, stampErr)
			return rep, code
		}
		if target.queried {
			clearCurrentTargetHealth(store)
		}
		epoch = target.epoch
		driver.joinIfNeeded(ctx, epoch)
		driver.ensure(ctx, epoch)
		if holder.Snapshot() != nil {
			clearAuthUnavailableHealth(store)
		}
		rep.AskedAS = true
		stamp.SlotID, stamp.TargetEpoch, stamp.LastAS = m.SlotID, epoch, now
	}
	rep.Epoch = epoch
	stamp.LastFlush = now
	stampErr := saveFlushStamp(store, stampPath, stamp, stderr)
	defer keepFlushStampHealth(store, stampErr)

	// WP4b (design aba1245 §2.3/§5.5): now that this run's target is
	// resolved, drop any previously-recorded conflicted epoch the AS has
	// moved past. State-based, not clock-based — see
	// dropConflictedObservationsPastTarget's own comment for why.
	dropConflictedObservationsPastTarget(store, m.SpoolDir, m.SlotID, epoch, stdout)

	// 2. promote intake into the spool.
	sp, err := spool.Open(m.SpoolDir)
	if err != nil {
		_ = store.MarkHealth(auth.HealthFlush, auth.HealthSpoolBacklog, err.Error())
		fmt.Fprintln(stderr, "dropin-miner flush: spool:", err)
		return rep, exitTransport
	}
	writer := &collector.SpoolWriter{Spool: sp}
	var out intakeEnqueuer = writer
	if flushTestHook != nil {
		out = &firstEnqueueHook{next: writer, fire: func() { flushEvent(ctx, "intake read") }}
	}
	promoted, unreadable, err := promoteIntake(mn.IntakeDir, out, m.SlotID, epoch)
	rep.Promoted, rep.Unreadable = promoted, unreadable
	promotionErr := err
	if err != nil {
		fmt.Fprintln(stderr, "dropin-miner flush: intake:", err)
	}

	// 3. deliver once.
	submitter := auth.NewSubmitter(mining, caps)
	before, _ := sp.Count()
	coll := collector.New(sp, submitter, collector.Options{
		Interval:    time.Hour, // never fires: Drain is called once
		MaxAttempts: productionMaxAttempts(m.CollectorMaxAttempts),
	})
	coll.Drain(ctx)
	after, _ := sp.Count()
	rep.Pending = after
	rep.Delivered = coll.Health().Delivered
	updateFlushDeliveryHealth(store, coll.Health(), before, after, rep.Delivered, stderr)
	if promotionErr != nil && coll.Health().TerminalFailures == 0 {
		_ = store.MarkHealth(auth.HealthFlush, auth.HealthSpoolBacklog, promotionErr.Error())
	}
	return rep, exitOK
}

func updateFlushDeliveryHealth(store *auth.Store, health collector.Health, before, after, delivered int, stderr io.Writer) {
	if health.TerminalFailures > 0 {
		_ = store.MarkHealth(auth.HealthFlush, auth.HealthSubmissionFailed, "terminal or quarantined evidence remains unresolved")
		if health.StorageError != "" {
			fmt.Fprintln(stderr, "flush: storage:", health.StorageError)
		}
		return
	}
	if health.LocalRemovalFailures > 0 {
		_ = store.MarkHealth(auth.HealthFlush, auth.HealthSpoolBacklog, "accepted evidence awaits durable local removal: "+health.StorageError)
		return
	}
	if health.StorageError != "" && health.SubmissionFailures == 0 {
		_ = store.MarkHealth(auth.HealthFlush, auth.HealthSpoolBacklog, health.StorageError)
		return
	}
	if health.ConsecutiveFailures > 0 || health.SubmissionFailures > 0 {
		detail := health.LastFailureNote
		if health.StorageError != "" {
			detail += "; retry persistence: " + health.StorageError
		}
		if detail == "" {
			detail = "the authorization server did not accept a queued observation"
		}
		reason := auth.HealthSubmissionFailed
		if previous, ok, _ := store.LoadHealth(auth.HealthFlush); ok && after > 0 &&
			(previous.Reason == auth.HealthSubmissionFailed || previous.Reason == auth.HealthSpoolBacklog) {
			reason = auth.HealthSpoolBacklog
			detail = fmt.Sprintf("%d queued observation(s) remain after failed delivery: %s", after, detail)
		}
		_ = store.MarkHealth(auth.HealthFlush, reason, detail)
		fmt.Fprintln(stderr, "flush: delivery:", detail)
		return
	}

	if health.RetryBacklog > 0 {
		_ = store.MarkHealth(auth.HealthFlush, auth.HealthSpoolBacklog, fmt.Sprintf("%d queued observation(s) await retry", health.RetryBacklog))
		return
	}
	if health.CleanupRecovered > 0 {
		_ = store.ClearHealth(auth.HealthFlush)
		return
	}
	// A fresh collector can legitimately skip records that are still under
	// persisted backoff. That is not a new submission failure, but if a prior
	// failed delivery is already on record and no queued item progressed, the
	// unresolved condition is now accurately described as spool backlog.
	if before > 0 && after == before {
		if previous, ok, _ := store.LoadHealth(auth.HealthFlush); ok &&
			(previous.Reason == auth.HealthSubmissionFailed || previous.Reason == auth.HealthSpoolBacklog) {
			_ = store.MarkHealth(auth.HealthFlush, auth.HealthSpoolBacklog,
				fmt.Sprintf("%d queued observation(s) remain after failed progress", after))
		}
		return
	}
	if delivered > 0 {
		// Only an actual accepted delivery proves that a previous flush
		// problem has recovered. Empty queues and no-target runs do not.
		_ = store.ClearHealth(auth.HealthFlush)
	}
}

// dropConflictedObservationsPastTarget is WP4b's other half (design
// aba1245 §2.3/§5.5). The rule is state-based, not clock-based: this
// installation never obtains a capability for an epoch another
// installation of the same participant holds (ErrEnrollmentConflict on
// join, or ErrProxyBindingMismatch on exchange — driver.go's
// joinIfNeeded/ensure), so it has no deadline to wait out. What proves the
// observations it queued for that epoch are worthless is simpler: the AS's
// current target has moved past it. So every conflicted epoch below
// currentTarget is dropped, quietly — no error-level output, because this
// is a bounded, expected property of several installations sharing one
// participant, not a bad record (Quarantine's "kept for inspection because
// something is wrong" semantics do not apply).
//
// An earlier version of this gated the drop on a capability deadline
// instead, but the non-holder case (ErrEnrollmentConflict — never held the
// epoch at all) never has one, so the drop never fired for the common
// case; only the rarer "held it, then lost it to ErrProxyBindingMismatch"
// case had a deadline to compare against. The current-target comparison
// covers both.
func dropConflictedObservationsPastTarget(store *auth.Store, spoolDir string, slotID, currentTarget uint64, stdout io.Writer) {
	// WP2-adversarial-review finding 18: an entry for a DIFFERENT slot
	// than the one currently configured can never reach the
	// currentTarget comparison below (it is filtered out of `due` by the
	// SlotID match) — a slot_id reconfiguration would otherwise strand it
	// in the set forever. Pruned here, once per flush, before that filter
	// ever runs.
	_ = store.PruneEpochConflictsForOtherSlots(slotID)

	conflicts, err := store.EpochConflicts()
	if err != nil || len(conflicts) == 0 {
		return
	}
	var due []auth.ConflictedEpoch
	for _, c := range conflicts {
		if c.SlotID == slotID && c.TargetEpoch < currentTarget {
			due = append(due, c)
		}
	}
	if len(due) == 0 {
		return
	}
	sp, err := spool.Open(spoolDir)
	if err != nil {
		return // transient; the next flush tries again, `due` stays on file
	}
	pending, err := sp.Pending()
	if err != nil {
		return
	}
	dropped := 0
	for _, prec := range pending {
		for _, c := range due {
			if prec.SlotID == c.SlotID && prec.TargetEpoch == c.TargetEpoch {
				if err := sp.Remove(prec); err == nil {
					dropped++
				}
				break
			}
		}
	}
	if dropped > 0 {
		fmt.Fprintf(stdout, "flush: dropped %d observation(s) across %d conflicted epoch(s) for slot %d — another installation of this participant held them\n",
			dropped, len(due), slotID)
	}
	// Removed once processed, whether or not anything was actually left to
	// drop (already delivered, or never spooled at all) — the pair has
	// been acted on either way, and the AS will never re-open it.
	if err := store.RemoveEpochConflicts(due); err != nil {
		fmt.Fprintln(stdout, "flush:", err)
	}
}

// deliverOnly drains the spool when no target could be resolved this run:
// records spooled under an epoch joined earlier can still land.
func deliverOnly(ctx context.Context, cfg *config.Config, mining *auth.MiningClient, caps *auth.CapabilityClient, store *auth.Store, rep *flushReport, stderr io.Writer) int {
	sp, err := spool.Open(cfg.Mining.SpoolDir)
	if err != nil {
		_ = store.MarkHealth(auth.HealthFlush, auth.HealthSpoolBacklog, err.Error())
		fmt.Fprintln(stderr, "dropin-miner flush: spool:", err)
		return exitTransport
	}
	before, _ := sp.Count()
	coll := collector.New(sp, auth.NewSubmitter(mining, caps), collector.Options{Interval: time.Hour, MaxAttempts: productionMaxAttempts(cfg.Mining.CollectorMaxAttempts)})
	coll.Drain(ctx)
	after, _ := sp.Count()
	rep.Pending = after
	rep.Delivered = coll.Health().Delivered
	updateFlushDeliveryHealth(store, coll.Health(), before, after, rep.Delivered, stderr)
	fmt.Fprintln(stderr, "flush: no target epoch this run; intake kept for the next flush")
	return exitOK
}

// flushTestHook is inert in production: nothing outside a _test.go file ever
// sets it, so every flushEvent is a nil check. A test sets it to observe or
// pause a pass at a named point: "locked read-write" or "locked read-only"
// once the flush lock is held, "busy read-write" or "busy read-only" when
// another flush holds it, and "intake read" after intake has been read and
// before the first record is spooled.
var flushTestHook func(ctx context.Context, event string)

func flushEvent(ctx context.Context, event string) {
	if flushTestHook != nil {
		flushTestHook(ctx, event)
	}
}

// firstEnqueueHook fires once, after intake has been read and before the
// first record is spooled. It is installed only while flushTestHook is set.
type firstEnqueueHook struct {
	next  intakeEnqueuer
	fire  func()
	fired bool
}

func (h *firstEnqueueHook) Enqueue(slotID, targetEpoch uint64, observation any) (string, error) {
	if !h.fired {
		h.fired = true
		h.fire()
	}
	return h.next.Enqueue(slotID, targetEpoch, observation)
}

// intakeEnqueuer is the one method promoteIntake needs from the spool,
// so a test can count what would have been spooled.
type intakeEnqueuer interface {
	Enqueue(slotID, targetEpoch uint64, observation any) (string, error)
}

// promoteIntake turns intake records into spooled observations under
// (slot, epoch) and removes each intake file only after its record is
// durably spooled. Files it cannot read are counted and left alone.
func promoteIntake(dir string, out intakeEnqueuer, slotID, epoch uint64) (promoted, unreadable int, err error) {
	return promoteIntakeWithOps(dir, out, slotID, epoch, fsx.WriteFileAtomic, os.Remove)
}

func promoteIntakeWithOps(dir string, out intakeEnqueuer, slotID, epoch uint64, write func(string, string, []byte, os.FileMode) error, remove func(string) error) (promoted, unreadable int, err error) {
	records, bad, err := readIntake(dir)
	unreadable = len(bad)
	if err != nil {
		return 0, unreadable, err
	}
	var firstErr error
	for _, f := range records {
		obs := f.rec.observation()
		if !promote.Eligible(obs) {
			if err := remove(f.path); err != nil {
				firstErr = errors.Join(firstErr, err)
			} // structurally ineligible
			continue
		}
		if f.rec.ClientRecordID == "" {
			id, mintErr := spool.NewClientRecordID()
			if mintErr != nil {
				firstErr = errors.Join(firstErr, mintErr)
				continue
			}
			f.rec.ClientRecordID = id
			data, encodeErr := json.Marshal(f.rec)
			if encodeErr != nil {
				firstErr = errors.Join(firstErr, encodeErr)
				continue
			}
			if writeErr := write(filepath.Dir(f.path), filepath.Base(f.path), data, 0o600); writeErr != nil {
				firstErr = errors.Join(firstErr, writeErr)
				continue
			}
		}
		// A previous upgrade may have published the ID but failed its directory
		// sync. Confirm that publication before allowing destructive promotion.
		if syncErr := fsx.SyncDirectory(dir); syncErr != nil && !errors.Is(syncErr, fsx.ErrDirectorySyncUnsupported) {
			firstErr = errors.Join(firstErr, syncErr)
			continue
		}
		rec, err := promote.Build(obs, f.rec.ClientRecordID)
		if err != nil {
			firstErr = errors.Join(firstErr, err)
			continue
		}
		if _, err := out.Enqueue(slotID, epoch, rec); err != nil {
			firstErr = errors.Join(firstErr, err)
			continue
		}
		if err := remove(f.path); err != nil {
			firstErr = errors.Join(firstErr, err)
			continue
		}
		promoted++
	}
	return promoted, unreadable, firstErr
}

// productionMaxAttempts preserves omitted and explicit-zero configuration behavior.
func productionMaxAttempts(configured int) int {
	if configured == 0 {
		return collector.DefaultMaxAttempts
	}
	return configured
}
