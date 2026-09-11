// Package collector delivers spooled observations to the AS (contract
// §58, §63–§64). It runs entirely off the inference response path: the
// proxy never retries, waits, or fails a client request because of
// mining delivery.
//
// The removal rule is the whole safety story: a record leaves the spool
// ONLY on ACCEPTED or ALREADY_ACCEPTED. Every other outcome either
// retries with backoff or quarantines the record for inspection.
package collector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/twilight-project/dropin-miner/pkg/mining/spool"
	"github.com/twilight-project/dropin-miner/pkg/redact"
)

// Submitter delivers one record and reports the AS's answer. The real
// implementation lives in pkg/auth (it needs the capability and
// DPoP machinery); this seam keeps the collector free of auth imports
// and trivially testable.
type Submitter interface {
	// Submit returns satisfied=true when the AS durably accepted the
	// record (ACCEPTED or ALREADY_ACCEPTED). A permanent refusal
	// returns permanent=true, which quarantines rather than retries.
	Submit(ctx context.Context, rec *spool.Record) (satisfied bool, permanent bool, retryAfter time.Duration, err error)
}

// DefaultMaxAttempts is the historical CLI delivery limit. Direct Options zero is unlimited.
const DefaultMaxAttempts = 50

// Options tunes the delivery loop.
type Options struct {
	// Interval is the idle poll period; a wakeup shortcuts it.
	Interval time.Duration
	// BaseBackoff and MaxBackoff bound the exponential retry (§64).
	BaseBackoff time.Duration
	MaxBackoff  time.Duration
	// MaxAttempts quarantines a record that keeps failing, so one bad
	// record can never block the queue forever. Zero means unlimited.
	MaxAttempts int
	// Now is the retry/health clock. Nil uses the real wall clock.
	Now func() time.Time
}

func (o *Options) withDefaults() {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Interval == 0 {
		o.Interval = 30 * time.Second
	}
	if o.BaseBackoff == 0 {
		o.BaseBackoff = 2 * time.Second
	}
	if o.MaxBackoff == 0 {
		o.MaxBackoff = 5 * time.Minute
	}
}

// Collector drains the spool.
type Collector struct {
	spool     *spool.Spool
	submitter Submitter
	opts      Options

	wake chan struct{}
	// Narrow per-instance storage seams keep failure tests on the real spool.
	rewrite             func(*spool.Record) error
	quarantine          func(*spool.Record, string) error
	remove              func(*spool.Record) error
	passRemovalFailures int

	// health is what this collector knows about the AS, and it is the only
	// state here read from another goroutine — the admin surface and the
	// status command both ask for it while Run is going. Everything else
	// above is touched by Run alone.
	mu     sync.Mutex
	health Health
}

// Health is the live answer to "is the AS accepting our work", assembled from
// what the collector actually observed rather than from a probe.
//
// It replaces a startup check that could only ever report what was true at
// boot. This cannot go stale: every field is written by the delivery path as
// it happens, so a proxy that has been running for a day reports today.
type Health struct {
	RetryBacklog int
	// Queued is how many observations are waiting. It is the number that
	// matters to a participant, because "340 queued and none accepted for
	// 25 minutes" is the concrete form of being connected and earning
	// nothing.
	Queued int
	// LastSuccess is the last time the AS accepted a submission. Zero means
	// never in this process — which is a different fact from "not for a
	// while" and reads differently to whoever is debugging.
	LastSuccess time.Time
	// LastFailure and LastFailureNote describe the most recent refusal. The
	// note is redacted: it reaches an operator surface, and a submission
	// error can carry a URL or a credential.
	LastFailure     time.Time
	LastFailureNote string
	// ConsecutiveFailures distinguishes a blip from an outage. It resets on
	// any success, so it counts the current run of trouble rather than the
	// total.
	ConsecutiveFailures int
	// Delivered counts validated ACKs with completed durable local removal in
	// this pass; quarantine and unsuccessful local removal never increment it.
	Delivered            int
	SubmissionFailures   int
	TerminalFailures     int
	Quarantined          int
	LocalRemovalFailures int
	CleanupRecovered     int
	StorageError         string
}

// Health returns a snapshot. Safe to call from any goroutine.
func (c *Collector) Health() Health {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.health
}

func (c *Collector) recordSuccess() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.health.LastSuccess = c.opts.Now()
	c.health.Delivered++
	c.health.ConsecutiveFailures = 0
	c.health.LastFailureNote = ""
}

func (c *Collector) recordFailure(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.health.LastFailure = c.opts.Now()
	c.health.SubmissionFailures++
	c.health.ConsecutiveFailures++
	if err != nil {
		c.health.LastFailureNote = redact.Error(err).Error()
	}
}

func New(s *spool.Spool, sub Submitter, opts Options) *Collector {
	opts.withDefaults()
	c := &Collector{
		spool:     s,
		submitter: sub,
		opts:      opts,
		wake:      make(chan struct{}, 1),
		rewrite:   s.Rewrite, quarantine: s.Quarantine, remove: s.Remove,
	}
	c.refreshState()
	return c
}

// Wake is the best-effort nudge after a spool write (PX-15: the write
// is durable first; this only shortens latency and may be dropped).
func (c *Collector) Wake() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// Run drains until ctx is done. The first pass IS the restart scan:
// whatever the previous process left on disk gets picked up here.
func (c *Collector) Run(ctx context.Context) {
	ticker := time.NewTicker(c.opts.Interval)
	defer ticker.Stop()
	for {
		c.drain(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-c.wake:
		}
	}
}

// Drain runs one delivery pass (exported for tests and the sweep).
func (c *Collector) Drain(ctx context.Context) { c.drain(ctx) }

func (c *Collector) storageFailure(err error, removal bool) {
	if err == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.health.StorageError = redact.Error(err).Error()
	if removal {
		c.passRemovalFailures++
		c.health.LocalRemovalFailures = c.passRemovalFailures
	}
}

func (c *Collector) refreshState() {
	state, err := c.spool.State()
	if err != nil {
		c.storageFailure(err, false)
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.health.Queued = state.Queued
	c.health.RetryBacklog = state.RetryBacklog
	c.health.Quarantined = state.Quarantined
	if n := state.Terminal + state.Quarantined; n > c.health.TerminalFailures {
		c.health.TerminalFailures = n
	}
	c.health.LocalRemovalFailures = max(state.RemovalPending, c.passRemovalFailures)
}

func (c *Collector) drain(ctx context.Context) {
	c.mu.Lock()
	c.health.Delivered = 0
	c.health.SubmissionFailures = 0
	c.health.TerminalFailures = 0
	c.health.LocalRemovalFailures = 0
	c.passRemovalFailures = 0
	c.health.StorageError = ""
	c.mu.Unlock()
	c.refreshState()
	defer c.refreshState()
	records, err := c.spool.Pending()
	if err != nil {
		c.storageFailure(err, false)
		return
	}
	for _, rec := range records {
		if ctx.Err() != nil {
			return
		}
		// Terminal and acknowledged records are cleanup work, never submissions.
		if rec.TerminalReason != "" {
			c.storageFailure(c.quarantine(rec, rec.TerminalReason), false)
			continue
		}
		if rec.RemovalPending {
			c.finishRemoval(rec, false)
			continue
		}
		if c.opts.MaxAttempts > 0 && rec.Attempts >= c.opts.MaxAttempts {
			c.markTerminal(rec, "attempts_exhausted")
			continue
		}
		if c.opts.Now().Before(rec.NextAttemptAt) {
			continue
		}
		c.deliver(ctx, rec)
	}
}

func (c *Collector) finishRemoval(rec *spool.Record, acknowledged bool) {
	if err := c.remove(rec); err != nil {
		c.storageFailure(err, true)
		return
	}
	if !acknowledged {
		c.mu.Lock()
		c.health.CleanupRecovered++
		c.mu.Unlock()
		return
	}
	c.recordSuccess()
}

func (c *Collector) markTerminal(rec *spool.Record, reason string) {
	updated := *rec
	updated.TerminalReason = reason
	if err := c.rewrite(&updated); err != nil {
		c.storageFailure(err, false)
		return
	}
	*rec = updated
	c.mu.Lock()
	c.health.TerminalFailures++
	c.mu.Unlock()
	c.storageFailure(c.quarantine(rec, reason), false)
}

func (c *Collector) deliver(ctx context.Context, rec *spool.Record) {
	satisfied, permanent, retryAfter, err := c.submitter.Submit(ctx, rec)
	switch {
	case satisfied:
		updated := *rec
		updated.RemovalPending = true
		if err := c.rewrite(&updated); err != nil {
			c.storageFailure(err, true)
			return
		}
		*rec = updated
		c.finishRemoval(rec, true)
	case permanent:
		c.recordFailure(err)
		updated := *rec
		updated.Attempts++
		c.markTerminal(&updated, "permanent")
	default:
		c.recordFailure(err)
		c.storageFailure(c.scheduleRetry(rec, retryAfter), false)
	}
}

// scheduleRetry persists one selected deadline and incremented attempt count.
// No process-local backoff decision survives independently of this rewrite.
func (c *Collector) scheduleRetry(rec *spool.Record, retryAfter time.Duration) error {
	updated := *rec
	updated.Attempts++
	wait := retryAfter
	if wait <= 0 {
		wait = c.backoffFor(updated.Attempts)
	}
	updated.NextAttemptAt = c.opts.Now().Add(wait)
	if c.opts.MaxAttempts > 0 && updated.Attempts >= c.opts.MaxAttempts {
		updated.TerminalReason = "attempts_exhausted"
	}
	if err := c.rewrite(&updated); err != nil {
		return err
	}
	*rec = updated
	if rec.TerminalReason != "" {
		c.mu.Lock()
		c.health.TerminalFailures++
		c.mu.Unlock()
		return c.quarantine(rec, rec.TerminalReason)
	}
	return nil
}

func (c *Collector) backoffFor(attempts int) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	// 2^(n-1) * base, capped, then jittered by ±25% so a fleet of
	// proxies does not synchronize its retries.
	exp := math.Pow(2, float64(attempts-1))
	wait := time.Duration(exp * float64(c.opts.BaseBackoff))
	if wait > c.opts.MaxBackoff || wait <= 0 {
		wait = c.opts.MaxBackoff
	}
	jitter := 1 + (rand.Float64()-0.5)/2 //nolint:gosec // jitter, not a security decision
	return time.Duration(float64(wait) * jitter)
}

// SpoolWriter is the promotion-side entry point: it mints the stable
// record id, writes durably, then wakes the collector — in that order
// (PX-15). Callers run it on the mining plane, never on a
// request-reachable goroutine.
type SpoolWriter struct {
	Spool     *spool.Spool
	Collector *Collector
}

// Enqueue durably spools one promoted observation.
func (w *SpoolWriter) Enqueue(slotID, targetEpoch uint64, observation any) (string, error) {
	payload, err := json.Marshal(observation)
	if err != nil {
		return "", fmt.Errorf("collector: encode observation: %w", err)
	}
	var probe struct {
		ClientRecordID string `json:"client_record_id"`
	}
	if err := json.Unmarshal(payload, &probe); err != nil || probe.ClientRecordID == "" {
		return "", errors.New("collector: observation carries no client_record_id")
	}
	rec := &spool.Record{
		ClientRecordID: probe.ClientRecordID,
		SlotID:         slotID,
		TargetEpoch:    targetEpoch,
		Observation:    payload,
	}
	// Durable FIRST. Only then is the record safe to announce.
	if err := w.Spool.Enqueue(rec); err != nil {
		return "", err
	}
	if w.Collector != nil {
		w.Collector.Wake() // best-effort; losing it costs latency, not data
	}
	return rec.ClientRecordID, nil
}
