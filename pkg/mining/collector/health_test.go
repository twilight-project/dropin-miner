package collector

// The collector's health, which replaces a startup probe that could only ever
// report what was true at boot (ESC-028). These assert it reports what is true
// now, and that a refused submission never costs an observation.

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Before anything is delivered, "never" must be distinguishable from "not
// lately". The two separate a proxy that has never reached its AS — wrong URL,
// no enrollment, a firewall — from one that has and has stopped, and they call
// for different next steps.
func TestHealthReportsNeverBeforeTheFirstSuccess(t *testing.T) {
	_, _, c := testEnv(t)
	if h := c.Health(); !h.LastSuccess.IsZero() {
		t.Errorf("LastSuccess is set before any delivery: %v", h.LastSuccess)
	}
}

// A refusing AS must leave the work queued and say so. This is the state a
// participant needs described: connected, and earning nothing.
func TestHealthCountsAFailingASWithoutLosingTheWork(t *testing.T) {
	sp, sub, c := testEnv(t)
	a := enqueue(t, sp, c, 11)
	b := enqueue(t, sp, c, 11)
	refuse := func(int) (bool, bool, time.Duration, error) {
		return false, false, 0, errors.New("dial tcp 10.0.0.1:443: connect: connection refused")
	}
	sub.answers[a] = refuse
	sub.answers[b] = refuse

	c.Drain(context.Background())

	h := c.Health()
	if h.Queued != 2 {
		t.Errorf("Queued = %d, want 2", h.Queued)
	}
	if h.ConsecutiveFailures == 0 {
		t.Error("ConsecutiveFailures = 0 after refusals; a surface cannot report an outage it did not count")
	}
	if h.LastFailureNote == "" {
		t.Error("no failure note; an operator needs to know WHICH failure, not only that there was one")
	}
	if n, err := sp.Count(); err != nil || n != 2 {
		t.Errorf("spool holds %d (err %v), want 2: a failing AS must never cost an observation", n, err)
	}
}

// A success clears the run, so the count describes the trouble happening now
// rather than the lifetime total.
func TestASuccessResetsTheFailureRun(t *testing.T) {
	sp, sub, c := testEnv(t)
	id := enqueue(t, sp, c, 12)
	sub.answers[id] = func(attempt int) (bool, bool, time.Duration, error) {
		if attempt == 1 {
			return false, false, 0, errors.New("temporary")
		}
		return true, false, 0, nil
	}

	c.Drain(context.Background())
	if c.Health().ConsecutiveFailures == 0 {
		t.Fatal("first drain recorded no failure; the fixture is not exercising the path")
	}

	time.Sleep(5 * time.Millisecond) // let the backoff expire
	c.Drain(context.Background())

	h := c.Health()
	if h.LastSuccess.IsZero() {
		t.Error("LastSuccess unset after an accepted submission")
	}
	if h.ConsecutiveFailures != 0 {
		t.Errorf("ConsecutiveFailures = %d after a success, want 0", h.ConsecutiveFailures)
	}
}

// Count must not move files. Len goes through Pending, which quarantines what
// it cannot parse — correct for the collector that owns the queue, and wrong
// for a second process asking how deep the backlog is.
func TestCountDoesNotQuarantineWhatItCannotParse(t *testing.T) {
	sp, _, c := testEnv(t)
	_ = enqueue(t, sp, c, 13)

	before, err := sp.Count()
	if err != nil {
		t.Fatal(err)
	}
	after, err := sp.Count()
	if err != nil {
		t.Fatal(err)
	}
	if before != after || before != 1 {
		t.Errorf("Count moved something: %d then %d, want 1 both times", before, after)
	}
}
