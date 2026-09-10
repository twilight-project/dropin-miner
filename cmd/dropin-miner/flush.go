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
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/twilight-project/dropin-miner/pkg/auth"
	"github.com/twilight-project/dropin-miner/pkg/config"
	"github.com/twilight-project/dropin-miner/pkg/mining/collector"
	"github.com/twilight-project/dropin-miner/pkg/mining/promote"
	"github.com/twilight-project/dropin-miner/pkg/mining/scope"
	"github.com/twilight-project/dropin-miner/pkg/mining/spool"
)

const flushDefaultTimeout = 90 * time.Second

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
			fmt.Fprintln(stderr, "dropin-miner flush: could not start:", err)
			return exitTransport
		}
		return exitOK
	}

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

	rep, code := runFlush(ctx, cfg, *cfgPath, *force, stdout, stderr)
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
	var rep flushReport
	m := cfg.Mining
	mn := cfg.Miner

	if err := os.MkdirAll(minerRoot(mn), 0o700); err != nil {
		fmt.Fprintln(stderr, "dropin-miner flush: state dir:", err)
		return rep, exitTransport
	}
	lock, held, err := tryLockFile(flushLockPath(mn))
	if err != nil {
		fmt.Fprintln(stderr, "dropin-miner flush: lock:", err)
		return rep, exitTransport
	}
	if !held {
		fmt.Fprintln(stdout, "flush: another flush is running; nothing to do")
		return rep, exitOK
	}
	defer func() { _ = unlockFile(lock) }()

	// The mining plane, built the way the operator commands build it: key
	// store, discovery, OAuth, mining client. miningClients prints its
	// own diagnosis when something is missing.
	oauthClient, mining, _, code := miningClients(ctx, []string{"-config", cfgPath}, "flush")
	if code != 0 {
		return rep, code
	}

	store, err := auth.OpenStore(m.StateDir)
	if err != nil {
		fmt.Fprintln(stderr, "dropin-miner flush: key store:", err)
		return rep, exitTransport
	}

	// [miner] enabled (checked above, in cmdFlush) says intake is
	// configured; it is not the mining decision. mining_decision.json
	// (miningActive) is, and this is one of the five places design item
	// 1 names it must gate: a disabled installation's flush retries a
	// pending AS-side revocation (item 2.3) if there is one, and
	// otherwise does none of the join/promote/submit work below — there
	// is nothing to join or declare for a family this installation chose
	// to stop, and "not enrolled; run enroll" below would be actively
	// misleading for one that used to be.
	if !miningActive(store) {
		retryPendingRevoke(ctx, store, oauthClient)
		fmt.Fprintln(stdout, "flush: mining is stopped here; nothing to do")
		return rep, exitOK
	}

	enrolled := func() bool {
		_, ok, err := store.LoadRefreshToken()
		return err == nil && ok
	}
	if !enrolled() {
		fmt.Fprintln(stderr, "dropin-miner flush: this installation is not enrolled; run: dropin-miner enroll")
		return rep, exitClientErr
	}

	holder := &scope.Holder{}
	caps := auth.NewCapabilityClient(mining, holder)
	logger := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	driver := newEpochDriver(mining, caps, m.TargetEpoch, logger, enrolled).withStore(store)

	// 1. target, join, capability — or the stamp's answer when fresh.
	stampPath := flushStampPath(mn)
	stamp := readFlushStamp(stampPath)
	now := time.Now()
	fresh := !force && stamp.TargetEpoch != 0 && stamp.SlotID == m.SlotID && now.Sub(stamp.LastAS) < mn.FlushInterval
	var epoch uint64
	switch {
	case fresh:
		epoch = stamp.TargetEpoch
	default:
		e, ok := driver.target(ctx)
		if !ok {
			// driver.target already said why, once. Intake stays for the
			// next flush; the spool may still drain if it holds records
			// for an epoch we joined earlier.
			stamp.LastFlush = now
			_ = writeFlushStamp(stampPath, stamp)
			return rep, deliverOnly(ctx, cfg, mining, caps, &rep, stderr)
		}
		epoch = e
		driver.joinIfNeeded(ctx, epoch)
		driver.ensure(ctx, epoch)
		rep.AskedAS = true
		stamp.SlotID, stamp.TargetEpoch, stamp.LastAS = m.SlotID, epoch, now
	}
	rep.Epoch = epoch
	stamp.LastFlush = now
	if err := writeFlushStamp(stampPath, stamp); err != nil {
		fmt.Fprintln(stderr, "dropin-miner flush: stamp:", err)
	}

	// WP4b (design aba1245 §2.3/§5.5): now that this run's target is
	// resolved, drop any previously-recorded conflicted epoch the AS has
	// moved past. State-based, not clock-based — see
	// dropConflictedObservationsPastTarget's own comment for why.
	dropConflictedObservationsPastTarget(store, m.SpoolDir, m.SlotID, epoch, stdout)

	// 2. promote intake into the spool.
	sp, err := spool.Open(m.SpoolDir)
	if err != nil {
		fmt.Fprintln(stderr, "dropin-miner flush: spool:", err)
		return rep, exitTransport
	}
	writer := &collector.SpoolWriter{Spool: sp}
	promoted, unreadable, err := promoteIntake(mn.IntakeDir, writer, m.SlotID, epoch)
	rep.Promoted, rep.Unreadable = promoted, unreadable
	if err != nil {
		fmt.Fprintln(stderr, "dropin-miner flush: intake:", err)
	}

	// 3. deliver once.
	submitter := auth.NewSubmitter(mining, caps)
	before, _ := sp.Count()
	coll := collector.New(sp, submitter, collector.Options{
		Interval:    time.Hour, // never fires: Drain is called once
		MaxAttempts: m.CollectorMaxAttempts,
	})
	coll.Drain(ctx)
	after, _ := sp.Count()
	rep.Pending = after
	if before > after {
		rep.Delivered = before - after
	}
	if h := coll.Health(); h.ConsecutiveFailures > 0 && h.LastFailureNote != "" {
		fmt.Fprintln(stderr, "flush: delivery:", h.LastFailureNote)
	}
	return rep, exitOK
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
func deliverOnly(ctx context.Context, cfg *config.Config, mining *auth.MiningClient, caps *auth.CapabilityClient, rep *flushReport, stderr io.Writer) int {
	sp, err := spool.Open(cfg.Mining.SpoolDir)
	if err != nil {
		fmt.Fprintln(stderr, "dropin-miner flush: spool:", err)
		return exitTransport
	}
	before, _ := sp.Count()
	coll := collector.New(sp, auth.NewSubmitter(mining, caps), collector.Options{Interval: time.Hour, MaxAttempts: cfg.Mining.CollectorMaxAttempts})
	coll.Drain(ctx)
	after, _ := sp.Count()
	rep.Pending = after
	if before > after {
		rep.Delivered = before - after
	}
	fmt.Fprintln(stderr, "flush: no target epoch this run; intake kept for the next flush")
	return exitOK
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
	records, bad, err := readIntake(dir)
	unreadable = len(bad)
	if err != nil {
		return 0, unreadable, err
	}
	var firstErr error
	for _, f := range records {
		obs := f.rec.observation()
		if !promote.Eligible(obs) {
			_ = os.Remove(f.path) // structurally never payable; keeping it earns nothing
			continue
		}
		id, err := spool.NewClientRecordID()
		if err != nil {
			firstErr = errors.Join(firstErr, err)
			continue
		}
		rec, err := promote.Build(obs, id)
		if err != nil {
			firstErr = errors.Join(firstErr, err)
			continue
		}
		if _, err := out.Enqueue(slotID, epoch, rec); err != nil {
			firstErr = errors.Join(firstErr, err)
			continue
		}
		_ = os.Remove(f.path)
		promoted++
	}
	return promoted, unreadable, firstErr
}
