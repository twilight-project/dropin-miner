package spool

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReplayRetainsOriginalLocationAndRetryState(t *testing.T) {
	s := newSpool(t)
	rec := record(t, 1)
	if err := s.Enqueue(rec); err != nil {
		t.Fatal(err)
	}
	rec.NextAttemptAt = time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := s.Touch(rec); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	replay := *rec
	replay.TargetEpoch = 2
	replay.SlotID = 8
	replay.Attempts = 0
	replay.NextAttemptAt = time.Time{}
	if err := reopened.Enqueue(&replay); err != nil {
		t.Fatal(err)
	}
	pending, err := reopened.Pending()
	if err != nil || len(pending) != 1 {
		t.Fatalf("logical records: %v %v", pending, err)
	}
	got := pending[0]
	if got.TargetEpoch != 1 || got.SlotID != 7 || got.Attempts != 1 || !got.NextAttemptAt.Equal(rec.NextAttemptAt) {
		t.Fatalf("replay rewrote original: %+v", got)
	}
	got.NextAttemptAt = got.NextAttemptAt.Add(time.Hour)
	if err := reopened.Touch(got); err != nil {
		t.Fatal(err)
	}
	final, err := Open(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	records, err := final.Pending()
	if err != nil || len(records) != 1 || records[0].Attempts != 2 || !records[0].NextAttemptAt.Equal(got.NextAttemptAt) {
		t.Fatalf("retry rewrite lost: %+v %v", records, err)
	}
	if _, err := os.Stat(filepath.Join(s.dir, s.filename(rec))); err != nil {
		t.Fatal(err)
	}
}

func TestConflictsPreserveEvidence(t *testing.T) {
	s := newSpool(t)
	rec := record(t, 1)
	if err := s.Enqueue(rec); err != nil {
		t.Fatal(err)
	}
	conflict := *rec
	conflict.Observation = json.RawMessage(`{"changed":true}`)
	if err := s.Enqueue(&conflict); !errors.Is(err, ErrEvidenceConflict) {
		t.Fatalf("enqueue conflict: %v", err)
	}
	if err := s.Rewrite(&conflict); !errors.Is(err, ErrEvidenceConflict) {
		t.Fatalf("rewrite conflict: %v", err)
	}
	for _, slot := range []bool{false, true} {
		moved := *rec
		if slot {
			moved.SlotID++
		} else {
			moved.TargetEpoch++
		}
		if err := s.Rewrite(&moved); !errors.Is(err, ErrEvidenceConflict) {
			t.Fatalf("migration allowed: %v", err)
		}
	}
	missing := record(t, 2)
	if err := s.Rewrite(missing); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("created absent record: %v", err)
	}
	got, err := s.read(s.filename(rec))
	if err != nil || !sameEvidence(got.Observation, rec.Observation) {
		t.Fatalf("evidence changed: %+v %v", got, err)
	}
}

func TestStartupDuplicateIdentityPreservesBothFiles(t *testing.T) {
	s := newSpool(t)
	rec := record(t, 1)
	if err := s.Enqueue(rec); err != nil {
		t.Fatal(err)
	}
	rec.TargetEpoch++
	raw, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.dir, s.filename(rec)), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(s.dir); !errors.Is(err, ErrDuplicateIdentity) {
		t.Fatalf("duplicate startup: %v", err)
	}
	if n, err := s.Count(); err != nil || n != 2 {
		t.Fatalf("evidence lost: %d %v", n, err)
	}
}
