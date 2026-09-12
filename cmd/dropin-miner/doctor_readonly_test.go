package main

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/twilight-project/dropin-miner/pkg/auth"
	"github.com/twilight-project/dropin-miner/pkg/config"
	"github.com/twilight-project/dropin-miner/pkg/mining/spool"
)

func doctorConfig(t *testing.T, asURL, stateDir, spoolDir string) string {
	t.Helper()
	body := "[mining]\n" +
		"as_url = " + quoteTOML(asURL) + "\n" +
		"chain_id = \"twilight-1\"\nslot_id = 7\n" +
		"state_dir = " + quoteTOML(stateDir) + "\n" +
		"spool_dir = " + quoteTOML(spoolDir) + "\n"
	return writeTOML(t, body)
}

func quoteTOML(value string) string {
	return strconv.Quote(value)
}

func TestDoctorMissingStateDoesNotCreateMiningStateOrDPoP(t *testing.T) {
	as := newFakeAS(t)
	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	spoolDir := filepath.Join(root, "spool")
	cfgPath := doctorConfig(t, as.srv.URL, stateDir, spoolDir)

	var out, stderr bytes.Buffer
	_ = cmdDoctor([]string{"-config", cfgPath}, &out, &stderr)
	if _, err := os.Stat(stateDir); !os.IsNotExist(err) {
		t.Fatalf("doctor created missing state dir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "dpop.key")); !os.IsNotExist(err) {
		t.Fatalf("doctor created DPoP state: %v", err)
	}
	if _, err := os.Stat(spoolDir); !os.IsNotExist(err) {
		t.Fatalf("doctor created missing spool dir: %v", err)
	}
	if !strings.Contains(out.String(), "NOT DECIDED") || !strings.Contains(out.String(), "holds no authorization") {
		t.Fatalf("doctor did not report missing local state clearly: %q", out.String())
	}
}

func TestDoctorMissingAuthDoesNotCreateDPoPOrEnrollmentState(t *testing.T) {
	as := newFakeAS(t)
	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	if err := os.Mkdir(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	spoolDir := filepath.Join(root, "spool")
	cfgPath := doctorConfig(t, as.srv.URL, stateDir, spoolDir)

	var out, stderr bytes.Buffer
	_ = cmdDoctor([]string{"-config", cfgPath}, &out, &stderr)
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("doctor created local auth state without credentials: %v", entries)
	}
	if !strings.Contains(out.String(), "holds no authorization") {
		t.Fatalf("doctor did not report incomplete local auth: %q", out.String())
	}
}

func TestDoctorHealthAndSpoolInspectionIsNonMutating(t *testing.T) {
	as := newFakeAS(t)
	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	spoolDir := filepath.Join(root, "spool")
	store, err := auth.OpenStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkHealth(auth.HealthCapture, auth.HealthIntakeUnwritable, "still unavailable"); err != nil {
		t.Fatal(err)
	}
	seedSpoolRecord(t, spoolDir, 7, 1042)

	beforeHealth, beforeOK, err := store.LoadHealth(auth.HealthCapture)
	if err != nil || !beforeOK {
		t.Fatalf("health setup = %+v ok=%t err=%v", beforeHealth, beforeOK, err)
	}
	sp, err := spool.OpenExisting(spoolDir)
	if err != nil {
		t.Fatal(err)
	}
	beforeCount, err := sp.Count()
	if err != nil {
		t.Fatal(err)
	}
	beforeFiles, err := os.ReadDir(spoolDir)
	if err != nil {
		t.Fatal(err)
	}
	beforeSnapshot := snapshotFiles(t, stateDir, spoolDir)

	cfgPath := doctorConfig(t, as.srv.URL, stateDir, spoolDir)
	var out, stderr bytes.Buffer
	_ = cmdDoctor([]string{"-config", cfgPath}, &out, &stderr)
	if !strings.Contains(out.String(), string(auth.HealthIntakeUnwritable)) {
		t.Fatalf("doctor did not render persistent health: %q", out.String())
	}
	afterHealth, afterOK, err := store.LoadHealth(auth.HealthCapture)
	if err != nil || !afterOK || afterHealth != beforeHealth {
		t.Fatalf("health changed during doctor: before=%+v/%t after=%+v/%t err=%v", beforeHealth, beforeOK, afterHealth, afterOK, err)
	}
	afterCount, err := sp.Count()
	if err != nil || afterCount != beforeCount {
		t.Fatalf("spool count changed during doctor: before=%d after=%d err=%v", beforeCount, afterCount, err)
	}
	afterFiles, err := os.ReadDir(spoolDir)
	if err != nil || len(afterFiles) != len(beforeFiles) {
		t.Fatalf("doctor changed spool files: before=%v after=%v err=%v", beforeFiles, afterFiles, err)
	}
	if afterSnapshot := snapshotFiles(t, stateDir, spoolDir); !reflect.DeepEqual(afterSnapshot, beforeSnapshot) {
		t.Fatalf("doctor rewrote local state or spool files:\nbefore=%+v\nafter=%+v", beforeSnapshot, afterSnapshot)
	}
}

type localFileSnapshot struct {
	Mode    os.FileMode
	ModTime time.Time
	Data    string
}

func snapshotFiles(t *testing.T, roots ...string) map[string]localFileSnapshot {
	t.Helper()
	result := make(map[string]localFileSnapshot)
	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				return nil
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			raw, err := os.ReadFile(path) // #nosec G304 G122 -- WalkDir traverses only test-owned roots with no concurrent writers
			if err != nil {
				return err
			}
			result[path] = localFileSnapshot{Mode: info.Mode(), ModTime: info.ModTime(), Data: string(raw)}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return result
}

func TestDoctorDoesNotBypassTheRefreshLock(t *testing.T) {
	as := newFakeAS(t)
	stateDir := filepath.Join(t.TempDir(), "state")
	store, err := auth.OpenStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveRefreshToken("rt-doctor"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DPoPKey(); err != nil {
		t.Fatal(err)
	}
	lock, held, err := tryLockFile(filepath.Join(stateDir, "refresh.token.lock"))
	if err != nil || !held {
		t.Fatalf("hold refresh lock: held=%t err=%v", held, err)
	}
	defer func() { _ = unlockFile(lock) }()

	m := config.Mining{
		ASBaseURL: as.srv.URL, ChainID: testChainID, SlotID: testSlotID,
		StateDir: stateDir, SpoolDir: filepath.Join(filepath.Dir(stateDir), "spool"),
	}
	client := doctorASClient(context.Background(), m)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	facts := gatherDoctorFacts(ctx, client, m)
	if facts.StandingErr == nil && facts.EpochErr == nil {
		t.Fatalf("doctor bypassed held refresh lock: %+v", facts)
	}
	if got := as.tokenCalls.Load(); got != 0 {
		t.Fatalf("doctor reached token endpoint while refresh lock was held: %d call(s)", got)
	}
}

func TestDoctorWithExistingAuthOnlyAllowsRefreshStateMutation(t *testing.T) {
	as := newFakeAS(t)
	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	spoolDir := filepath.Join(root, "spool")
	store, err := auth.OpenStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveMiningEnabled(true); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveRefreshToken("rt-doctor"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DPoPKey(); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkHealth(auth.HealthFlush, auth.HealthSubmissionFailed, "retained"); err != nil {
		t.Fatal(err)
	}
	seedSpoolRecord(t, spoolDir, testSlotID, 1042)
	before := snapshotFiles(t, stateDir, spoolDir)

	cfgPath := doctorConfig(t, as.srv.URL, stateDir, spoolDir)
	var out, stderr bytes.Buffer
	_ = cmdDoctor([]string{"-config", cfgPath}, &out, &stderr)
	after := snapshotFiles(t, stateDir, spoolDir)
	if got := as.tokenCalls.Load(); got != 1 {
		t.Fatalf("authenticated doctor refresh calls = %d, want 1", got)
	}
	for _, allowed := range []string{
		filepath.Join(stateDir, "refresh.token"),
		filepath.Join(stateDir, "refresh.token.lock"),
	} {
		delete(before, allowed)
		delete(after, allowed)
	}
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("doctor mutated files outside refresh-token rotation:\nbefore=%+v\nafter=%+v", before, after)
	}
	if !strings.Contains(out.String(), string(auth.HealthSubmissionFailed)) {
		t.Fatalf("authenticated doctor omitted retained health: %q", out.String())
	}
}

func TestDoctorNeverSpendsSearchOrProviderTraffic(t *testing.T) {
	var routerCalls atomic.Int64
	router := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		routerCalls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer router.Close()
	as := newFakeAS(t)
	root := t.TempDir()
	stateDir := filepath.Join(root, "missing-state")
	body := fmt.Sprintf(`[[provider]]
name = "search-router"
upstream = %q

[mining]
as_url = %q
chain_id = "twilight-1"
slot_id = 7
state_dir = %q
spool_dir = %q

[miner]
enabled = true
router_url = %q
`, router.URL, as.srv.URL, stateDir, filepath.Join(root, "spool"), router.URL)
	cfgPath := writeTOML(t, body)
	var out, stderr bytes.Buffer
	_ = cmdDoctor([]string{"-config", cfgPath}, &out, &stderr)
	if got := routerCalls.Load(); got != 0 {
		t.Fatalf("doctor issued %d router/provider request(s)", got)
	}
}

// The probe is the one write doctor performs, and the read-only promise
// now reads "one short-lived file in the intake directory, gone before
// doctor exits". This drives the real command against a real configured
// miner and asserts the directory is empty afterwards — the snapshot tests
// above cover the state directory, which the probe must still never touch.
func TestDoctorLeavesNoProbeFileInTheIntakeDirectory(t *testing.T) {
	as := newFakeAS(t)
	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	spoolDir := filepath.Join(root, "spool")
	intakeDir := filepath.Join(root, "miner", "intake")
	if err := os.MkdirAll(filepath.Join(root, "miner"), 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := auth.OpenStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveMiningEnabled(true); err != nil {
		t.Fatal(err)
	}
	cfgPath := writeTOML(t, "[mining]\n"+
		"as_url = "+quoteTOML(as.srv.URL)+"\n"+
		"chain_id = \"twilight-1\"\nslot_id = 7\n"+
		"state_dir = "+quoteTOML(stateDir)+"\n"+
		"spool_dir = "+quoteTOML(spoolDir)+"\n\n"+
		"[miner]\nenabled = true\n"+
		"router_url = \"https://router.fictional.test\"\n"+
		"intake_dir = "+quoteTOML(intakeDir)+"\n")

	var out, stderr bytes.Buffer
	_ = cmdDoctor([]string{"-config", cfgPath}, &out, &stderr)

	entries, err := os.ReadDir(intakeDir)
	if err != nil {
		t.Fatalf("the intake directory is not readable after doctor: %v", err)
	}
	if len(entries) != 0 {
		names := []string{}
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("doctor left %d file(s) in the intake directory: %v", len(entries), names)
	}
	if !strings.Contains(out.String(), "intake writable") {
		t.Errorf("the report does not carry the intake check:\n%s", out.String())
	}
	// And nothing was created above the intake directory.
	if _, err := os.Stat(filepath.Join(root, "miner", "flush.json")); !os.IsNotExist(err) {
		t.Errorf("doctor created miner state: %v", err)
	}
}
