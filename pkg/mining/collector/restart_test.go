package collector

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/twilight-project/dropin-miner/pkg/mining/spool"
)

func openQueue(t *testing.T, dir string) *spool.Spool {
	t.Helper()
	s, err := spool.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestRetryDeadlineSurvivesFreshCollector(t *testing.T) {
	for _, retryAfter := range []time.Duration{time.Hour, 0} {
		t.Run(retryAfter.String(), func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "spool")
			now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
			opts := Options{Now: func() time.Time { return now }, BaseBackoff: time.Minute}
			sp := openQueue(t, dir)
			sub := newScripted()
			c := New(sp, sub, opts)
			id := enqueue(t, sp, c, 1)
			sub.answers[id] = func(int) (bool, bool, time.Duration, error) { return false, false, retryAfter, errors.New("busy") }
			c.Drain(context.Background())
			sp = openQueue(t, dir)
			records, err := sp.Pending()
			if err != nil || len(records) != 1 {
				t.Fatalf("retry record: %+v %v", records, err)
			}
			deadline := records[0].NextAttemptAt
			if records[0].Attempts != 1 {
				t.Fatalf("attempts=%d", records[0].Attempts)
			}
			if retryAfter > 0 && !deadline.Equal(now.Add(time.Hour)) {
				t.Fatalf("one-hour deadline=%v", deadline)
			}
			if retryAfter == 0 && (deadline.Before(now.Add(45*time.Second)) || deadline.After(now.Add(75*time.Second))) {
				t.Fatalf("jitter outside bounds: %v", deadline)
			}
			now = deadline.Add(-time.Nanosecond)
			freshSub := newScripted()
			fresh := New(sp, freshSub, opts)
			fresh.Drain(context.Background())
			if freshSub.count(id) != 0 {
				t.Fatal("fresh process submitted before persisted deadline")
			}
			records, err = sp.Pending()
			if err != nil || len(records) != 1 || !records[0].NextAttemptAt.Equal(deadline) || records[0].Attempts != 1 {
				t.Fatalf("restart recomputed state: %+v %v", records, err)
			}
			now = deadline
			fresh.Drain(context.Background())
			if freshSub.count(id) != 1 || fresh.Health().Delivered != 1 {
				t.Fatalf("due record not delivered: %+v", fresh.Health())
			}
		})
	}
}

func TestFailedQuarantineRestartNeverSubmitsTerminalEvidence(t *testing.T) {
	for _, permanent := range []bool{true, false} {
		t.Run(map[bool]string{true: "permanent", false: "exhausted"}[permanent], func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "spool")
			sp := openQueue(t, dir)
			sub := newScripted()
			c := New(sp, sub, Options{MaxAttempts: 1})
			id := enqueue(t, sp, c, 1)
			sub.answers[id] = func(int) (bool, bool, time.Duration, error) { return false, permanent, 0, errors.New("refused") }
			c.quarantine = func(*spool.Record, string) error { return errors.New("move failed") }
			c.Drain(context.Background())
			if c.Health().Delivered != 0 || c.Health().StorageError == "" {
				t.Fatalf("quarantine failure lost: %+v", c.Health())
			}
			// Unlimited on restart ensures only the persisted terminal marker,
			// not rediscovery of the numeric limit, prevents another submission.
			freshSub := newScripted()
			freshSp := openQueue(t, dir)
			fresh := New(freshSp, freshSub, Options{})
			fresh.Drain(context.Background())
			if freshSub.count(id) != 0 {
				t.Fatal("terminal evidence resubmitted after failed quarantine")
			}
			state, err := freshSp.State()
			if err != nil || state.Queued != 0 || state.Quarantined != 1 || fresh.Health().Delivered != 0 {
				t.Fatalf("quarantine recovery: %+v %+v %v", state, fresh.Health(), err)
			}
			last := New(openQueue(t, dir), newScripted(), Options{})
			if last.Health().TerminalFailures != 1 || last.Health().Quarantined != 1 {
				t.Fatalf("quarantine hidden on restart: %+v", last.Health())
			}
		})
	}
}

func TestRetryRewriteFailureIsReported(t *testing.T) {
	sp, sub, c := testEnv(t)
	id := enqueue(t, sp, c, 1)
	sub.answers[id] = func(int) (bool, bool, time.Duration, error) { return false, false, time.Hour, errors.New("busy") }
	c.rewrite = func(*spool.Record) error { return errors.New("disk failed") }
	c.Drain(context.Background())
	records, err := sp.Pending()
	if err != nil || len(records) != 1 || records[0].Attempts != 0 || !records[0].NextAttemptAt.IsZero() {
		t.Fatalf("failed rewrite reported persisted: %+v %v", records, err)
	}
	if c.Health().StorageError == "" || c.Health().SubmissionFailures != 1 {
		t.Fatalf("failure not visible: %+v", c.Health())
	}
}

func TestAcknowledgedRemovalFailureSurvivesRestart(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "spool")
	sp := openQueue(t, dir)
	sub := newScripted()
	c := New(sp, sub, Options{})
	id := enqueue(t, sp, c, 1)
	c.remove = func(*spool.Record) error { return errors.New("unlink failed") }
	c.Drain(context.Background())
	h := c.Health()
	if h.Delivered != 0 || h.LocalRemovalFailures == 0 || h.SubmissionFailures != 0 || h.ConsecutiveFailures != 0 {
		t.Fatalf("ACK cleanup misclassified: %+v", h)
	}
	freshSp := openQueue(t, dir)
	records, err := freshSp.Pending()
	if err != nil || len(records) != 1 || records[0].ClientRecordID != id || !records[0].RemovalPending {
		t.Fatalf("ACK identity lost: %+v %v", records, err)
	}
	freshSub := newScripted()
	fresh := New(freshSp, freshSub, Options{})
	if fresh.Health().LocalRemovalFailures != 1 {
		t.Fatalf("cleanup hidden: %+v", fresh.Health())
	}
	fresh.Drain(context.Background())
	if freshSub.count(id) != 0 || fresh.Health().Delivered != 0 || fresh.Health().CleanupRecovered != 1 || fresh.Health().LocalRemovalFailures != 0 {
		t.Fatalf("cleanup retry: submissions=%d health=%+v", freshSub.count(id), fresh.Health())
	}
}

func TestLegacyAttemptsAtLimitNeverResubmit(t *testing.T) {
	sp, sub, c := testEnv(t)
	id := enqueue(t, sp, c, 1)
	records, err := sp.Pending()
	if err != nil {
		t.Fatal(err)
	}
	records[0].Attempts = 4
	if err := sp.Rewrite(records[0]); err != nil {
		t.Fatal(err)
	}
	c.Drain(context.Background())
	if sub.count(id) != 0 || c.Health().Quarantined != 1 {
		t.Fatalf("limit rediscovered by submitting: %+v", c.Health())
	}
}
