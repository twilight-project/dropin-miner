package collector

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/twilight-project/dropin-miner/pkg/mining/spool"
)

// scriptedSubmitter answers per client_record_id, recording attempts.
type scriptedSubmitter struct {
	mu       sync.Mutex
	answers  map[string]func(attempt int) (bool, bool, time.Duration, error)
	attempts map[string]int
}

func newScripted() *scriptedSubmitter {
	return &scriptedSubmitter{
		answers:  map[string]func(int) (bool, bool, time.Duration, error){},
		attempts: map[string]int{},
	}
}

func (s *scriptedSubmitter) Submit(_ context.Context, rec *spool.Record) (bool, bool, time.Duration, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.attempts[rec.ClientRecordID]++
	if fn, ok := s.answers[rec.ClientRecordID]; ok {
		return fn(s.attempts[rec.ClientRecordID])
	}
	return true, false, 0, nil // default: accepted
}

func (s *scriptedSubmitter) count(id string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attempts[id]
}

func testEnv(t *testing.T) (*spool.Spool, *scriptedSubmitter, *Collector) {
	t.Helper()
	sp, err := spool.Open(filepath.Join(t.TempDir(), "spool"))
	if err != nil {
		t.Fatal(err)
	}
	sub := newScripted()
	c := New(sp, sub, Options{BaseBackoff: time.Millisecond, MaxBackoff: 2 * time.Millisecond, MaxAttempts: 4})
	return sp, sub, c
}

func enqueue(t *testing.T, sp *spool.Spool, c *Collector, epoch uint64) string {
	t.Helper()
	id, err := spool.NewClientRecordID()
	if err != nil {
		t.Fatal(err)
	}
	w := &SpoolWriter{Spool: sp, Collector: c}
	got, err := w.Enqueue(7, epoch, map[string]string{"client_record_id": id})
	if err != nil {
		t.Fatal(err)
	}
	if got != id {
		t.Fatalf("Enqueue returned %q, want %q", got, id)
	}
	return id
}

// §58: a durable ACK — and only that — removes the record.
func TestAcceptedRemovesRecord(t *testing.T) {
	sp, _, c := testEnv(t)
	id := enqueue(t, sp, c, 1042)
	c.Drain(context.Background())

	pending, err := sp.Pending()
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("accepted record still spooled: %+v", pending)
	}
	_ = id
}

// A failure must NOT remove the record: evidence outlives the attempt.
func TestFailureKeepsRecordSpooled(t *testing.T) {
	sp, sub, c := testEnv(t)
	id := enqueue(t, sp, c, 1042)
	sub.answers[id] = func(int) (bool, bool, time.Duration, error) {
		return false, false, 0, errors.New("network down")
	}
	c.Drain(context.Background())

	pending, err := sp.Pending()
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("failed delivery lost the record: %+v", pending)
	}
	if pending[0].Attempts != 1 {
		t.Fatalf("attempt count not persisted: %d", pending[0].Attempts)
	}
}

// A permanent refusal (evidence conflict) quarantines rather than
// deleting or retrying forever.
func TestPermanentRefusalQuarantines(t *testing.T) {
	sp, sub, c := testEnv(t)
	id := enqueue(t, sp, c, 1042)
	sub.answers[id] = func(int) (bool, bool, time.Duration, error) {
		return false, true, 0, errors.New("OBSERVATION_CONFLICT")
	}
	c.Drain(context.Background())

	pending, _ := sp.Pending()
	if len(pending) != 0 {
		t.Fatalf("conflicting record still queued: %+v", pending)
	}
	if n := sub.count(id); n != 1 {
		t.Fatalf("permanent refusal retried %d times", n)
	}
}

// Backoff must actually delay the next attempt, and a record that keeps
// failing is eventually quarantined so it cannot block the queue.
func TestBackoffAndAttemptExhaustion(t *testing.T) {
	sp, sub, c := testEnv(t)
	id := enqueue(t, sp, c, 1042)
	sub.answers[id] = func(int) (bool, bool, time.Duration, error) {
		return false, false, 0, errors.New("temporary")
	}
	// Immediately re-draining must not retry (backoff in effect).
	c.Drain(context.Background())
	c.Drain(context.Background())
	if n := sub.count(id); n != 1 {
		t.Fatalf("backoff ignored: %d attempts", n)
	}
	// After the (tiny) backoff elapses, attempts resume and eventually
	// exhaust into quarantine.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		pending, _ := sp.Pending()
		if len(pending) == 0 {
			break
		}
		time.Sleep(3 * time.Millisecond)
		c.Drain(context.Background())
	}
	if pending, _ := sp.Pending(); len(pending) != 0 {
		t.Fatalf("record never exhausted: %+v", pending)
	}
	if n := sub.count(id); n < 4 {
		t.Fatalf("expected repeated attempts before quarantine, got %d", n)
	}
}

// Retry-After from the AS is honored over local backoff (§64).
func TestRetryAfterHonored(t *testing.T) {
	sp, sub, c := testEnv(t)
	id := enqueue(t, sp, c, 1042)
	sub.answers[id] = func(int) (bool, bool, time.Duration, error) {
		return false, false, time.Hour, errors.New("busy")
	}
	c.Drain(context.Background())
	time.Sleep(10 * time.Millisecond)
	c.Drain(context.Background())
	if n := sub.count(id); n != 1 {
		t.Fatalf("Retry-After ignored: %d attempts", n)
	}
}

// The first pass over an existing directory IS the restart scan: a
// collector that never saw the write still delivers it.
func TestRestartScanDeliversOrphanedRecords(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "spool")
	sp, err := spool.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a crashed process: records written, never delivered.
	writer := &SpoolWriter{Spool: sp}
	var ids []string
	for i := 0; i < 3; i++ {
		id, err := spool.NewClientRecordID()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Enqueue(7, 1042, map[string]string{"client_record_id": id}); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}

	// Fresh process: new spool handle, new collector, no shared state.
	reopened, err := spool.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	sub := newScripted()
	c := New(reopened, sub, Options{BaseBackoff: time.Millisecond})
	c.Drain(context.Background())

	for _, id := range ids {
		if sub.count(id) != 1 {
			t.Fatalf("record %s not delivered after restart", id)
		}
	}
	if pending, _ := reopened.Pending(); len(pending) != 0 {
		t.Fatalf("records remained after successful delivery: %+v", pending)
	}
}

// Run must stop promptly on context cancellation (shutdown path).
func TestRunStopsOnContextCancel(t *testing.T) {
	sp, _, c := testEnv(t)
	enqueue(t, sp, c, 1042)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop on cancel")
	}
}
