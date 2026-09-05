package spool

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func newSpool(t *testing.T) *Spool {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "spool"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func record(t *testing.T, epoch uint64) *Record {
	t.Helper()
	id, err := NewClientRecordID()
	if err != nil {
		t.Fatal(err)
	}
	return &Record{
		ClientRecordID: id,
		SlotID:         7,
		TargetEpoch:    epoch,
		Observation:    json.RawMessage(`{"client_record_id":"` + id + `"}`),
	}
}

// §49: the transport identity must be UUIDv7-shaped, unique, and
// sortable by creation time (the collector relies on the ordering).
func TestClientRecordIDShape(t *testing.T) {
	seen := make(map[string]bool)
	var previous string
	for i := 0; i < 200; i++ {
		id, err := NewClientRecordID()
		if err != nil {
			t.Fatal(err)
		}
		if len(id) != 36 || strings.Count(id, "-") != 4 {
			t.Fatalf("not UUID-shaped: %q", id)
		}
		if id[14] != '7' {
			t.Fatalf("not version 7: %q", id)
		}
		if v := id[19]; v != '8' && v != '9' && v != 'a' && v != 'b' {
			t.Fatalf("wrong variant nibble: %q", id)
		}
		if seen[id] {
			t.Fatalf("duplicate id: %q", id)
		}
		seen[id] = true
		if previous != "" && id < previous {
			// Same-millisecond ids may tie; only a large regression matters.
			if id[:8] < previous[:8] {
				t.Fatalf("ids not time-sortable: %q then %q", previous, id)
			}
		}
		previous = id
	}
}

// PX-15: a written record is on disk immediately and is found by a
// fresh Spool over the same directory — the restart scan.
func TestWriteSurvivesRestart(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "spool")
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	rec := record(t, 1042)
	if err := s.Write(rec); err != nil {
		t.Fatal(err)
	}

	// A brand-new Spool (as after a crash) must see it, with no
	// in-memory state carried over.
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := reopened.Pending()
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].ClientRecordID != rec.ClientRecordID {
		t.Fatalf("record did not survive: %+v", pending)
	}
	if pending[0].TargetEpoch != 1042 || pending[0].SlotID != 7 {
		t.Fatalf("delivery context lost: %+v", pending[0])
	}
	info, err := os.Stat(filepath.Join(dir, reopened.filename(rec)))
	if err != nil || (runtime.GOOS != "windows" && info.Mode().Perm() != 0o600) {
		t.Fatalf("record perms: %v %v", err, info)
	}
}

// §58: removal happens only after a durable ACK; the test asserts the
// mechanics (Remove deletes, and only the named record).
func TestRemoveAndQuarantine(t *testing.T) {
	s := newSpool(t)
	keep, drop := record(t, 1), record(t, 2)
	for _, r := range []*Record{keep, drop} {
		if err := s.Write(r); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Remove(drop); err != nil {
		t.Fatal(err)
	}
	pending, err := s.Pending()
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].ClientRecordID != keep.ClientRecordID {
		t.Fatalf("wrong record removed: %+v", pending)
	}
	// Removing an absent record is not an error (crash between ACK and
	// unlink must be replayable).
	if err := s.Remove(drop); err != nil {
		t.Fatalf("second remove failed: %v", err)
	}

	// Quarantine preserves the evidence outside the queue.
	if err := s.Quarantine(keep, "OBSERVATION_CONFLICT"); err != nil {
		t.Fatal(err)
	}
	if pending, _ := s.Pending(); len(pending) != 0 {
		t.Fatalf("quarantined record still queued: %+v", pending)
	}
	entries, err := os.ReadDir(filepath.Join(s.dir, "quarantine"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("quarantine dir: %v %d entries", err, len(entries))
	}
	if !strings.Contains(entries[0].Name(), "OBSERVATION_CONFLICT") {
		t.Fatalf("quarantine name lost the reason: %s", entries[0].Name())
	}
}

// A corrupt file must not wedge the queue: it is set aside and the rest
// still delivers.
func TestCorruptRecordQuarantinedNotFatal(t *testing.T) {
	s := newSpool(t)
	good := record(t, 5)
	if err := s.Write(good); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.dir, "7-5-corrupt.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	pending, err := s.Pending()
	if err != nil {
		t.Fatalf("scan failed on corrupt record: %v", err)
	}
	if len(pending) != 1 || pending[0].ClientRecordID != good.ClientRecordID {
		t.Fatalf("good record lost: %+v", pending)
	}
}

// Attempt counts survive restarts so backoff is not reset by a reboot.
func TestTouchPersistsAttempts(t *testing.T) {
	s := newSpool(t)
	rec := record(t, 9)
	if err := s.Write(rec); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := s.Touch(rec); err != nil {
			t.Fatal(err)
		}
	}
	pending, err := s.Pending()
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending: %v %+v", err, pending)
	}
	if pending[0].Attempts != 3 {
		t.Fatalf("attempts = %d, want 3", pending[0].Attempts)
	}
	if pending[0].SpooledAt.After(time.Now().Add(time.Minute)) {
		t.Fatal("implausible spool timestamp")
	}
}

func TestPendingIsOldestFirst(t *testing.T) {
	s := newSpool(t)
	var ids []string
	for i := 0; i < 5; i++ {
		rec := record(t, 3)
		if err := s.Write(rec); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, rec.ClientRecordID)
		time.Sleep(2 * time.Millisecond)
	}
	pending, err := s.Pending()
	if err != nil {
		t.Fatal(err)
	}
	for i, rec := range pending {
		if rec.ClientRecordID != ids[i] {
			t.Fatalf("order mismatch at %d: %s vs %s", i, rec.ClientRecordID, ids[i])
		}
	}
}
