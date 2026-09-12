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
	if err := s.Enqueue(rec); err != nil {
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
		if err := s.Enqueue(r); err != nil {
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
	if err := s.Enqueue(good); err != nil {
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
	if err := s.Enqueue(rec); err != nil {
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
		if err := s.Enqueue(rec); err != nil {
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

// CountQuarantined is the other half of the backlog question, and it
// exists because Count deliberately refuses to answer it: the number a
// participant is told about is what can still be delivered.
//
// A diagnosis asking "was anything ever recorded" needs the opposite
// number. A quarantined record got as far as the spool and only then
// failed to parse, which is about as strong as local evidence of recording
// gets — so a check that read Count alone would conclude "nothing was
// recorded" with the proof sitting on disk.
func TestCountQuarantinedSeesWhatCountDeliberatelyIgnores(t *testing.T) {
	s := newSpool(t)

	if n, err := s.CountQuarantined(); err != nil || n != 0 {
		t.Fatalf("empty quarantine = (%d, %v), want (0, nil)", n, err)
	}

	// Two deliverable records and one the collector quarantines.
	for _, epoch := range []uint64{1042, 1043} {
		if err := s.Enqueue(record(t, epoch)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(s.dir, "7-9-corrupt.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Pending is what quarantines it — the collector's own path, not a
	// file moved into place by hand.
	if _, err := s.Pending(); err != nil {
		t.Fatal(err)
	}

	active, err := s.Count()
	if err != nil {
		t.Fatal(err)
	}
	quarantined, err := s.CountQuarantined()
	if err != nil {
		t.Fatal(err)
	}
	if active != 2 {
		t.Errorf("Count = %d, want 2", active)
	}
	if quarantined != 1 {
		t.Errorf("CountQuarantined = %d, want 1 — Count cannot see it, which is the point", quarantined)
	}
}

// The same name filter as Count, and no mutation of what it counts.
func TestCountQuarantinedFiltersAndMovesNothing(t *testing.T) {
	s := newSpool(t)
	q := filepath.Join(s.dir, "quarantine")
	for name, content := range map[string]string{
		"a.json":      `{}`,
		"b.json":      `{}`,
		".tmp-c.json": `{}`, // a write in flight
		"notes.txt":   `x`,  // not a record
		"d.json.bak":  `{}`, // not a record either
	} {
		if err := os.WriteFile(filepath.Join(q, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(q, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}

	before, err := os.ReadDir(q)
	if err != nil {
		t.Fatal(err)
	}
	n, err := s.CountQuarantined()
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("CountQuarantined = %d, want 2 (.tmp- prefixes, non-.json and directories skipped)", n)
	}
	after, err := os.ReadDir(q)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != len(after) {
		t.Errorf("counting moved files: %d before, %d after", len(before), len(after))
	}
}

// OpenExisting does not create the quarantine directory, so a spool that
// has never quarantined anything must count zero rather than fail.
func TestCountQuarantinedOnASpoolWithNoQuarantineDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "spool")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	s, err := OpenExisting(dir)
	if err != nil {
		t.Fatal(err)
	}
	n, err := s.CountQuarantined()
	if err != nil {
		t.Fatalf("CountQuarantined on a spool with no quarantine dir: %v", err)
	}
	if n != 0 {
		t.Errorf("CountQuarantined = %d, want 0", n)
	}
	if _, err := os.Stat(filepath.Join(dir, "quarantine")); !os.IsNotExist(err) {
		t.Errorf("counting created the quarantine directory: %v", err)
	}
}
