package auth

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestMiningDecisionPreservesAllRuntimeStates(t *testing.T) {
	dir := privateTempDir(t)
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name    string
		content string
		want    MiningDecisionState
		present bool
		bad     bool
	}{
		{name: "undecided", want: MiningUndecided},
		{name: "enabled", content: `{"version":1,"enabled":true}`, want: MiningEnabled, present: true},
		{name: "disabled", content: `{"version":1,"enabled":false}`, want: MiningDisabled, present: true},
		{name: "legacy version one", content: `{"enabled":true}`, want: MiningEnabled, present: true},
		{name: "malformed", content: `{not json`, want: MiningDegraded, present: true, bad: true},
		{name: "unsupported version", content: `{"version":99,"enabled":true}`, want: MiningDegraded, present: true, bad: true},
		{name: "missing value", content: `{"version":1}`, want: MiningDegraded, present: true, bad: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, "mining_decision.json")
			if tc.content == "" {
				_ = os.Remove(path)
			} else if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
				t.Fatal(err)
			}
			got := store.ReadMiningDecision()
			if got.State != tc.want || got.Present != tc.present {
				t.Fatalf("decision = %+v, want state=%s present=%t", got, tc.want, tc.present)
			}
			if tc.bad != (got.Err != nil) {
				t.Fatalf("error = %v, want bad=%t", got.Err, tc.bad)
			}
			if got.State == MiningEnabled && !got.IsEnabled() {
				t.Fatal("enabled state did not authorize mining")
			}
			if got.State != MiningEnabled && got.IsEnabled() {
				t.Fatal("non-enabled state authorized mining")
			}
		})
	}

	if err := store.SaveMiningEnabled(true); err != nil {
		t.Fatal(err)
	}
	if got := store.ReadMiningDecision(); got.State != MiningEnabled {
		t.Fatalf("saved ON decision = %+v", got)
	}
	if err := store.SaveMiningEnabled(false); err != nil {
		t.Fatal(err)
	}
	if got := store.ReadMiningDecision(); got.State != MiningDisabled {
		t.Fatalf("saved OFF decision = %+v", got)
	}
}

func TestMiningDecisionInspectionDoesNotCreateState(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "missing")
	got := ReadMiningDecisionAt(dir)
	if got.State != MiningUndecided || got.Err != nil {
		t.Fatalf("missing state = %+v, want undecided", got)
	}
	if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("inspection created state directory: stat error=%v", err)
	}

	file := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := ReadMiningDecisionAt(file); got.State != MiningDegraded || got.Err == nil {
		t.Fatalf("unsafe state location = %+v, want degraded", got)
	}
}

func TestMiningDecisionUnsafeFilesAndStateLocationsAreDegraded(t *testing.T) {
	dir := privateTempDir(t)
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveMiningEnabled(true); err != nil {
		t.Fatal(err)
	}
	decisionPath := filepath.Join(dir, "mining_decision.json")
	if posixModes {
		if err := os.Chmod(decisionPath, 0o644); err != nil { // #nosec G302 -- deliberately manufacture unsafe permissions
			t.Fatal(err)
		}
		if got := store.ReadMiningDecision(); got.State != MiningDegraded || got.Err == nil {
			t.Fatalf("group/world-readable decision = %+v, want degraded", got)
		}
		if err := os.Chmod(decisionPath, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Remove(decisionPath); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "decision-target")
	if err := os.WriteFile(target, []byte(`{"version":1,"enabled":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, decisionPath); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if got := store.ReadMiningDecision(); got.State != MiningDegraded || got.Err == nil {
		t.Fatalf("symlinked decision = %+v, want degraded", got)
	}

	stateLink := filepath.Join(t.TempDir(), "state-link")
	if err := os.Symlink(dir, stateLink); err != nil {
		t.Skipf("state-dir symlink unavailable: %v", err)
	}
	if got := ReadMiningDecisionAt(stateLink); got.State != MiningDegraded || got.Err == nil {
		t.Fatalf("symlinked state dir = %+v, want degraded", got)
	}
}

func TestHealthIsBoundedRedactedAndComponentIndependent(t *testing.T) {
	store, err := OpenStore(privateTempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	secret := "sr-" + strings.Repeat("x", 32)
	if err := store.MarkHealth(HealthDecision, HealthDecisionUnreadable, secret+" "+strings.Repeat("detail ", 200)); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkHealth(HealthCapture, HealthSandboxRestricted, "sandbox denied"); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkHealth(HealthFlush, HealthSubmissionFailed, "submission failed"); err != nil {
		t.Fatal(err)
	}

	decision, ok, err := store.LoadHealth(HealthDecision)
	if err != nil || !ok {
		t.Fatalf("decision health = %+v ok=%t err=%v", decision, ok, err)
	}
	if len(decision.Detail) > healthDetailCap || strings.Contains(decision.Detail, secret) {
		t.Fatalf("health detail was not bounded/redacted: %q", decision.Detail)
	}
	if mode, err := os.Stat(filepath.Join(store.dir, healthDecision)); err != nil || mode.Mode().Perm()&0o077 != 0 {
		t.Fatalf("health file mode is not owner-only: mode=%v err=%v", mode, err)
	}
	if err := store.ClearHealth(HealthCapture); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.LoadHealth(HealthCapture); err != nil || ok {
		t.Fatalf("capture health survived its clear: ok=%t err=%v", ok, err)
	}
	if _, ok, err := store.LoadHealth(HealthDecision); err != nil || !ok {
		t.Fatalf("clearing capture erased decision health: ok=%t err=%v", ok, err)
	}
	if _, ok, err := store.LoadHealth(HealthFlush); err != nil || !ok {
		t.Fatalf("clearing capture erased flush health: ok=%t err=%v", ok, err)
	}
}

func TestRepairingDecisionClearsOnlyDecisionHealth(t *testing.T) {
	store, err := OpenStore(privateTempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkHealth(HealthDecision, HealthDecisionUnreadable, "decision broken"); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkHealth(HealthCapture, HealthIntakeUnwritable, "capture broken"); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveMiningEnabled(true); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.LoadHealth(HealthDecision); err != nil || ok {
		t.Fatalf("decision repair did not clear decision health: ok=%t err=%v", ok, err)
	}
	if _, ok, err := store.LoadHealth(HealthCapture); err != nil || !ok {
		t.Fatalf("decision repair erased capture health: ok=%t err=%v", ok, err)
	}
}

func TestHealthConcurrentComponentWritesDoNotLoseEachOther(t *testing.T) {
	store, err := OpenStore(privateTempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	components := []HealthComponent{HealthDecision, HealthCapture, HealthFlush}
	reasons := []HealthReason{HealthDecisionUnreadable, HealthSandboxRestricted, HealthSubmissionFailed}
	var wg sync.WaitGroup
	for i, component := range components {
		wg.Add(1)
		go func(i int, component HealthComponent) {
			defer wg.Done()
			for n := 0; n < 20; n++ {
				if err := store.MarkHealth(component, reasons[i], "concurrent"); err != nil {
					t.Errorf("mark %s: %v", component, err)
					return
				}
			}
		}(i, component)
	}
	wg.Wait()
	records, err := store.HealthRecords()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != len(components) {
		t.Fatalf("health records = %+v, want one per component", records)
	}
}

func TestHealthRejectsUnknownValuesAndCorruptRecords(t *testing.T) {
	store, err := OpenStore(privateTempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkHealth(HealthFlush, HealthReason("not-stable"), "x"); err == nil {
		t.Fatal("unknown reason was accepted")
	}
	if err := os.WriteFile(filepath.Join(store.dir, healthFlush), []byte(`{"version":99}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.LoadHealth(HealthFlush); err == nil {
		t.Fatal("corrupt health record was accepted")
	}
	if err := store.MarkHealth(HealthCapture, HealthIntakeUnwritable, "valid sibling"); err != nil {
		t.Fatal(err)
	}
	records, err := store.HealthRecords()
	if err == nil || len(records) != 1 || records[0].Component != HealthCapture {
		t.Fatalf("corrupt flush hid valid capture health: records=%+v err=%v", records, err)
	}

	var raw HealthRecord
	if err := json.Unmarshal([]byte(`{"version":1,"component":"flush","reason":"submission_failed"}`), &raw); err != nil {
		t.Fatal(err)
	}
}

func privateTempDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}
