package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/twilight-project/dropin-miner/pkg/auth"
	"github.com/twilight-project/dropin-miner/pkg/fsx"
	"github.com/twilight-project/dropin-miner/pkg/mining/collector"
	"github.com/twilight-project/dropin-miner/pkg/mining/spool"
)

type deliveryAnswer func(*spool.Record) (bool, bool, time.Duration, error)

func (f deliveryAnswer) Submit(_ context.Context, r *spool.Record) (bool, bool, time.Duration, error) {
	return f(r)
}

func TestDurableQuarantineHealthCannotRecoverFromUnrelatedSuccess(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "spool")
	s, err := spool.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	authDir := filepath.Join(t.TempDir(), "auth")
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := auth.OpenStore(authDir)
	if err != nil {
		t.Fatal(err)
	}
	w := &collector.SpoolWriter{Spool: s}
	id, err := spool.NewClientRecordID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Enqueue(1, 1, map[string]string{"client_record_id": id}); err != nil {
		t.Fatal(err)
	}
	c := collector.New(s, deliveryAnswer(func(*spool.Record) (bool, bool, time.Duration, error) {
		return false, false, 0, errors.New("submission failed")
	}), collector.Options{MaxAttempts: 1})
	c.Drain(context.Background())
	h := c.Health()
	updateFlushDeliveryHealth(store, h, 1, 0, h.Delivered, io.Discard)
	if h.Delivered != 0 {
		t.Fatal("quarantine counted as delivery")
	}
	check := func() {
		t.Helper()
		rec, ok, err := store.LoadHealth(auth.HealthFlush)
		if err != nil || !ok || rec.Reason != auth.HealthSubmissionFailed {
			t.Fatalf("quarantine health lost: %+v %t %v", rec, ok, err)
		}
	}
	check()
	s, err = spool.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	w.Spool = s
	other, err := spool.NewClientRecordID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Enqueue(1, 1, map[string]string{"client_record_id": other}); err != nil {
		t.Fatal(err)
	}
	c = collector.New(s, deliveryAnswer(func(*spool.Record) (bool, bool, time.Duration, error) { return true, false, 0, nil }), collector.Options{})
	c.Drain(context.Background())
	h = c.Health()
	if h.Delivered != 1 || h.Quarantined != 1 {
		t.Fatalf("mixed outcome: %+v", h)
	}
	updateFlushDeliveryHealth(store, h, 1, 0, h.Delivered, io.Discard)
	check()
}

func TestAcknowledgedCleanupUsesBacklogHealth(t *testing.T) {
	authDir := filepath.Join(t.TempDir(), "auth")
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := auth.OpenStore(authDir)
	if err != nil {
		t.Fatal(err)
	}
	updateFlushDeliveryHealth(store, collector.Health{LocalRemovalFailures: 1, StorageError: "unlink failed"}, 1, 1, 0, io.Discard)
	rec, ok, err := store.LoadHealth(auth.HealthFlush)
	if err != nil || !ok || rec.Reason != auth.HealthSpoolBacklog {
		t.Fatalf("local ACK cleanup treated as AS failure: %+v %t %v", rec, ok, err)
	}
}

func TestBadSubmissionUsesSubmissionFailedHealth(t *testing.T) {
	authDir := filepath.Join(t.TempDir(), "auth")
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := auth.OpenStore(authDir)
	if err != nil {
		t.Fatal(err)
	}
	updateFlushDeliveryHealth(store, collector.Health{
		SubmissionFailures:  1,
		ConsecutiveFailures: 1,
		LastFailureNote:     "bad acknowledgement",
	}, 1, 1, 0, io.Discard)
	rec, ok, err := store.LoadHealth(auth.HealthFlush)
	if err != nil || !ok || rec.Reason != auth.HealthSubmissionFailed {
		t.Fatalf("bad acknowledgement health = %+v ok=%t err=%v", rec, ok, err)
	}
}

func TestSurvivingIntakeCannotResurrectQuarantine(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	if _, err := writeIntake(dir, intakeRecord{RequestID: "served", StatusCode: 200, StartedAt: now, FinishedAt: now}); err != nil {
		t.Fatal(err)
	}
	spoolDir := filepath.Join(t.TempDir(), "spool")
	s, err := spool.Open(spoolDir)
	if err != nil {
		t.Fatal(err)
	}
	w := &collector.SpoolWriter{Spool: s}
	if _, _, err := promoteIntakeWithOps(dir, w, 1, 1, fsx.WriteFileAtomic, func(string) error { return errors.New("unlink failed") }); err == nil {
		t.Fatal("missing unlink failure")
	}
	c := collector.New(s, deliveryAnswer(func(*spool.Record) (bool, bool, time.Duration, error) {
		return false, true, 0, errors.New("permanent refusal")
	}), collector.Options{})
	c.Drain(context.Background())
	s, err = spool.Open(spoolDir)
	if err != nil {
		t.Fatal(err)
	}
	w.Spool = s
	if _, _, err := promoteIntake(dir, w, 1, 2); err != nil {
		t.Fatal(err)
	}
	state, err := s.State()
	if err != nil || state.Queued != 0 || state.Quarantined != 1 {
		t.Fatalf("resurrected terminal evidence: %+v %v", state, err)
	}
	files, _, err := readIntake(dir)
	if err != nil || len(files) != 0 {
		t.Fatalf("intake cleanup incomplete: %+v %v", files, err)
	}
}
