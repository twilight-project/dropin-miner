package spool

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/twilight-project/dropin-miner/pkg/fsx"
)

func TestQuarantineCustodySurvivesRestart(t *testing.T) {
	s := newSpool(t)
	rec := record(t, 1)
	if err := s.Enqueue(rec); err != nil {
		t.Fatal(err)
	}
	rec.TerminalReason = "permanent"
	if err := s.Rewrite(rec); err != nil {
		t.Fatal(err)
	}
	if err := s.Quarantine(rec, "permanent"); err != nil {
		t.Fatal(err)
	}
	fresh, err := Open(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	replay := *rec
	replay.TargetEpoch++
	replay.TerminalReason = ""
	if err := fresh.Enqueue(&replay); err != nil {
		t.Fatal(err)
	}
	state, err := fresh.State()
	if err != nil || state.Queued != 0 || state.Quarantined != 1 {
		t.Fatalf("resurrected quarantine: %+v %v", state, err)
	}
	replay.Observation = json.RawMessage(`{"different":true}`)
	if err := fresh.Enqueue(&replay); !errors.Is(err, ErrEvidenceConflict) {
		t.Fatalf("conflict accepted: %v", err)
	}
	raw, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.dir, s.filename(rec)), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(s.dir); !errors.Is(err, ErrDuplicateIdentity) {
		t.Fatalf("active/quarantine duplicate hidden: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.dir, s.filename(rec))); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(s.dir, s.locations[rec.ClientRecordID])); err != nil {
		t.Fatal(err)
	}
}

func TestUncertainQuarantineUpdatesVisibleIndex(t *testing.T) {
	s := newSpool(t)
	rec := record(t, 1)
	if err := s.Enqueue(rec); err != nil {
		t.Fatal(err)
	}
	rec.TerminalReason = "permanent"
	if err := s.Rewrite(rec); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("directory sync failed")
	s.move = func(from, to string) error {
		if err := os.Rename(from, to); err != nil {
			return err
		}
		return &fsx.StageError{Stage: "move directory sync", Published: true, Err: failure}
	}
	if err := s.Quarantine(rec, "permanent"); !errors.Is(err, failure) {
		t.Fatalf("uncertainty lost: %v", err)
	}
	if !quarantined(s.locations[rec.ClientRecordID]) {
		t.Fatal("index still points to absent active file")
	}
	fresh, err := Open(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := fresh.Enqueue(rec); err != nil {
		t.Fatal(err)
	}
	state, err := fresh.State()
	if err != nil || state.Quarantined != 1 || state.Queued != 0 {
		t.Fatalf("custody lost: %+v %v", state, err)
	}
}

func TestUncertainRemovalRestoresStableAcknowledgedRecord(t *testing.T) {
	s := newSpool(t)
	rec := record(t, 1)
	if err := s.Enqueue(rec); err != nil {
		t.Fatal(err)
	}
	rec.RemovalPending = true
	if err := s.Rewrite(rec); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("remove sync failed")
	s.remove = func(path string) error {
		if err := os.Remove(path); err != nil {
			return err
		}
		return &fsx.StageError{Stage: "remove directory sync", Published: true, Err: failure}
	}
	if err := s.Remove(rec); !errors.Is(err, failure) {
		t.Fatalf("uncertainty hidden: %v", err)
	}
	fresh, err := Open(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	records, err := fresh.Pending()
	if err != nil || len(records) != 1 || records[0].ClientRecordID != rec.ClientRecordID || !records[0].RemovalPending {
		t.Fatalf("stable ACK record lost: %+v %v", records, err)
	}
}
