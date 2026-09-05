package main

// The pieces search, flush and the hooks pass through the filesystem:
// intake records, lineage files, and promotion into the spool. All
// identifiers are synthetic.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/twilight-project/dropin-miner/pkg/wire"
)

func TestIntakeRoundTripsAndKeepsArrivalOrder(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "intake")
	base := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	for i, id := range []string{"req-a", "req-b", "req-c"} {
		if _, err := writeIntake(dir, intakeRecord{RequestID: id, StatusCode: 200, StartedAt: base, FinishedAt: base.Add(time.Duration(i) * time.Second)}); err != nil {
			t.Fatal(err)
		}
	}
	// A stray file that is not ours, and a half-written one.
	_ = os.WriteFile(filepath.Join(dir, "zzz-not-json.json"), []byte("{nope"), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("hi"), 0o600)

	recs, bad, err := readIntake(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 3 || recs[0].rec.RequestID != "req-a" || recs[2].rec.RequestID != "req-c" {
		t.Fatalf("records: %+v", recs)
	}
	if len(bad) != 1 {
		t.Errorf("unreadable: %v", bad)
	}
	if _, err := writeIntake(dir, intakeRecord{}); err == nil {
		t.Error("an intake record without a request id was accepted")
	}
}

func TestReadIntakeOnAMissingDirIsEmptyNotAnError(t *testing.T) {
	recs, bad, err := readIntake(filepath.Join(t.TempDir(), "never"))
	if err != nil || len(recs) != 0 || len(bad) != 0 {
		t.Fatalf("recs=%v bad=%v err=%v", recs, bad, err)
	}
}

type fakeEnqueuer struct {
	calls []struct {
		slot, epoch uint64
		rec         *wire.ProviderObservationV1
	}
	fail bool
}

func (f *fakeEnqueuer) Enqueue(slot, epoch uint64, obs any) (string, error) {
	if f.fail {
		return "", os.ErrPermission
	}
	rec := obs.(*wire.ProviderObservationV1)
	f.calls = append(f.calls, struct {
		slot, epoch uint64
		rec         *wire.ProviderObservationV1
	}{slot, epoch, rec})
	return rec.ClientRecordID, nil
}

func TestPromoteIntakeSpoolsUnderTheEpochAndRemovesOnlyWhatLanded(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "intake")
	now := time.Now()
	for i, id := range []string{"01a03e86-fictional-1", "01a03e86-fictional-2"} {
		if _, err := writeIntake(dir, intakeRecord{RequestID: id, StatusCode: 200, StartedAt: now, FinishedAt: now.Add(time.Duration(i) * time.Millisecond), ChosenProvider: "fictional"}); err != nil {
			t.Fatal(err)
		}
	}
	out := &fakeEnqueuer{}
	promoted, unreadable, err := promoteIntake(dir, out, 3, 147331)
	if err != nil || promoted != 2 || unreadable != 0 {
		t.Fatalf("promoted=%d unreadable=%d err=%v", promoted, unreadable, err)
	}
	if len(out.calls) != 2 || out.calls[0].slot != 3 || out.calls[0].epoch != 147331 {
		t.Fatalf("calls: %+v", out.calls)
	}
	rec := out.calls[0].rec
	if rec.SourceProfile != wire.SourceProfileSearchRouterV1 || rec.ProviderEventID != "01a03e86-fictional-1" || rec.ResolvedModel != "fictional" {
		t.Errorf("record: %+v", rec)
	}
	if rec.Outcome.Type != wire.OutcomeComplete || rec.StartedAt == "" || rec.FinishedAt == "" {
		t.Errorf("outcome/timestamps: %+v", rec)
	}
	if left, _, _ := readIntake(dir); len(left) != 0 {
		t.Errorf("intake not cleared after spooling: %d left", len(left))
	}

	// A spool that refuses leaves the intake in place for the next flush.
	if _, err := writeIntake(dir, intakeRecord{RequestID: "01a03e86-fictional-3", StatusCode: 200, StartedAt: now, FinishedAt: now}); err != nil {
		t.Fatal(err)
	}
	promoted, _, err = promoteIntake(dir, &fakeEnqueuer{fail: true}, 3, 147331)
	if err == nil || promoted != 0 {
		t.Fatalf("a failed enqueue was not reported: promoted=%d err=%v", promoted, err)
	}
	if left, _, _ := readIntake(dir); len(left) != 1 {
		t.Errorf("intake removed despite the spool refusing: %d left", len(left))
	}
}

func TestLineageFileRoundTripAndWalkUp(t *testing.T) {
	fs, ops := newFakeHookOps(nil)
	dir := "/sessions"
	root := "/home/u/project"
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	ops.now = func() time.Time { return now }

	if err := updateLineage(ops, lineagePath(dir, root), now, func(l *lineageFile) {
		l.Harness, l.SessionID, l.Window = "cursor", "abc", "none"
		l.History = []traceHistory{{Role: "assistant", Text: "looking it up"}}
	}); err != nil {
		t.Fatal(err)
	}
	// The file name reveals nothing about the path.
	for name := range fs.files {
		if filepath.Base(name) == "project.json" || len(filepath.Base(name)) != 32+len(".json") {
			t.Errorf("lineage file name leaks or is unhashed: %s", name)
		}
	}
	got, path := lineageForCwd(ops, dir, filepath.Join(root, "src", "deep"), now.Add(time.Minute))
	if got == nil || path != lineagePath(dir, root) || got.SessionID != "abc" {
		t.Fatalf("walk-up did not find the workspace lineage: %+v %q", got, path)
	}
	env := got.envelope()
	if env == nil || env.Harness != "cursor" || env.Window != "none" || len(env.History) != 1 {
		t.Errorf("envelope: %+v", env)
	}
	// Stale lineage is ignored rather than threading a new conversation
	// into an old one.
	if stale, _ := lineageForCwd(ops, dir, root, now.Add(lineageMaxAge+time.Second)); stale != nil {
		t.Error("a stale lineage file was used")
	}
	// A corrupt file is absent.
	fs.files[lineagePath(dir, root)] = []byte("{corrupt")
	if l, ok := loadLineage(ops, lineagePath(dir, root)); ok || l != nil {
		t.Error("a corrupt lineage file was not treated as absent")
	}
}

func TestLineageHistoryIsCappedOnSave(t *testing.T) {
	_, ops := newFakeHookOps(nil)
	now := time.Now()
	path := lineagePath("/s", "/w")
	big := make([]byte, traceHistoryCap*2)
	for i := range big {
		big[i] = 'x'
	}
	big[len(big)-1] = 'E'
	if err := saveLineage(ops, path, &lineageFile{SessionID: "s", History: []traceHistory{{Role: "assistant", Text: string(big)}}}, now); err != nil {
		t.Fatal(err)
	}
	l, ok := loadLineage(ops, path)
	if !ok || len(l.History[0].Text) != traceHistoryCap || l.History[0].Text[traceHistoryCap-1] != 'E' {
		t.Fatalf("history not capped to the tail: ok=%v len=%d", ok, len(l.History[0].Text))
	}
}

func TestFlushStampRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "flush.json")
	if got := readFlushStamp(path); got.TargetEpoch != 0 {
		t.Fatalf("missing stamp read as %+v", got)
	}
	want := flushStamp{SlotID: 3, TargetEpoch: 42, LastAS: time.Now().UTC().Truncate(time.Second)}
	if err := writeFlushStamp(path, want); err != nil {
		t.Fatal(err)
	}
	got := readFlushStamp(path)
	if got.SlotID != 3 || got.TargetEpoch != 42 || !got.LastAS.Equal(want.LastAS) {
		t.Fatalf("stamp: %+v", got)
	}
	var raw map[string]any
	b, _ := os.ReadFile(path)
	_ = json.Unmarshal(b, &raw)
	if raw["v"] != float64(1) {
		t.Errorf("stamp lacks a version: %v", raw)
	}
}
