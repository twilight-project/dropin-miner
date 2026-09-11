package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/twilight-project/dropin-miner/pkg/config"
	"github.com/twilight-project/dropin-miner/pkg/fsx"
	"github.com/twilight-project/dropin-miner/pkg/mining/collector"
	"github.com/twilight-project/dropin-miner/pkg/mining/spool"
)

func TestIntakeReplayAfterUnlinkFailure(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	path, err := writeIntake(dir, intakeRecord{RequestID: "served-search", StatusCode: 200, StartedAt: now, FinishedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	files, _, err := readIntake(dir)
	if err != nil || len(files) != 1 || files[0].rec.ClientRecordID == "" {
		t.Fatalf("durable identity missing: %+v %v", files, err)
	}
	id := files[0].rec.ClientRecordID
	spoolDir := filepath.Join(t.TempDir(), "spool")
	sp, err := spool.Open(spoolDir)
	if err != nil {
		t.Fatal(err)
	}
	failure := errors.New("unlink failed")
	_, _, err = promoteIntakeWithOps(dir, &collector.SpoolWriter{Spool: sp}, 7, 1, fsx.WriteFileAtomic, func(string) error { return failure })
	if !errors.Is(err, failure) {
		t.Fatalf("unlink error lost: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	reopened, err := spool.Open(spoolDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := promoteIntake(dir, &collector.SpoolWriter{Spool: reopened}, 7, 2); err != nil {
		t.Fatal(err)
	}
	pending, err := reopened.Pending()
	if err != nil || len(pending) != 1 || pending[0].ClientRecordID != id || pending[0].TargetEpoch != 1 {
		t.Fatalf("replay identity/target: %+v %v", pending, err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("intake not removed: %v", err)
	}
}

func TestLegacyIdentityDurableBeforeEnqueue(t *testing.T) {
	for _, failWrite := range []bool{true, false} {
		t.Run(map[bool]string{true: "write-failure", false: "crash-before-enqueue"}[failWrite], func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "legacy.json")
			now := time.Now()
			raw, err := json.Marshal(intakeRecord{V: intakeVersion, RequestID: "legacy-event", StatusCode: 200, StartedAt: now, FinishedAt: now})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			out := &fakeEnqueuer{fail: !failWrite}
			write := fsx.WriteFileAtomic
			if failWrite {
				write = func(string, string, []byte, os.FileMode) error { return os.ErrPermission }
			}
			if _, _, err := promoteIntakeWithOps(dir, out, 7, 1, write, os.Remove); err == nil {
				t.Fatal("expected failure")
			}
			if len(out.calls) != 0 {
				t.Fatal("enqueued before durable upgrade")
			}
			files, _, err := readIntake(dir)
			if err != nil || len(files) != 1 {
				t.Fatalf("legacy lost: %+v %v", files, err)
			}
			id := files[0].rec.ClientRecordID
			if failWrite && id != "" {
				t.Fatal("failed upgrade unexpectedly persisted")
			}
			if !failWrite && id == "" {
				t.Fatal("identity not persisted before enqueue failure")
			}
			out = &fakeEnqueuer{}
			if _, _, err := promoteIntake(dir, out, 7, 2); err != nil {
				t.Fatal(err)
			}
			if len(out.calls) != 1 || out.calls[0].rec.ClientRecordID == "" || (!failWrite && out.calls[0].rec.ClientRecordID != id) {
				t.Fatalf("legacy ID not reused: %+v", out.calls)
			}
		})
	}
}

func TestProductionMaxAttemptsCompatibility(t *testing.T) {
	for _, tc := range []struct {
		key  string
		want int
	}{
		{"", 50}, {"collector_max_attempts = 0", 50}, {"collector_max_attempts = 50", 50}, {"collector_max_attempts = 4", 4},
	} {
		path := filepath.Join(t.TempDir(), "config.toml")
		data := "[mining]\nas_url = \"https://as.example.com\"\nchain_id = \"twilight-1\"\nslot_id = 1\n" + tc.key + "\n"
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, _, err := config.Load([]string{"-config", path}, func(string) string { return "" })
		if err != nil {
			t.Fatal(err)
		}
		if got := productionMaxAttempts(cfg.Mining.CollectorMaxAttempts); got != tc.want {
			t.Fatalf("config %q: limit=%d want=%d", tc.key, got, tc.want)
		}
	}
}

func TestIntakePreservesSuppliedIdentity(t *testing.T) {
	dir := t.TempDir()
	id, err := spool.NewClientRecordID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writeIntake(dir, intakeRecord{ClientRecordID: id, RequestID: "served", StatusCode: 200, FinishedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	files, _, err := readIntake(dir)
	if err != nil || len(files) != 1 || files[0].rec.ClientRecordID != id {
		t.Fatalf("supplied identity changed: %+v %v", files, err)
	}
}
