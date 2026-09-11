package main

// runFlush against a scripted AS and a router that must never be dialed.
//
// flush.go's own doc comment promises: "the one thing a flush never does
// is touch the router — it talks to the AS only." Nothing drove runFlush
// end to end before this file existed; the AS-facing pieces (driver,
// promote, collector) each had their own tests, but the assembled pass —
// the thing that actually ships the promise — did not.
//
// The router recorder counts CONNECTIONS, not completed requests: the
// wrong-observable hazard (a probe that opens a socket and never
// completes a request still "reached out") means counting handler
// invocations would pass even if something dialed the router and the
// dial merely failed to finish.

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/twilight-project/dropin-miner/pkg/auth"
	"github.com/twilight-project/dropin-miner/pkg/config"
	"github.com/twilight-project/dropin-miner/pkg/mining/spool"
)

// routerRecorder stands in for the search router. Any TCP connection to
// it — whether or not a request ever completes on it — is a violation of
// "a flush talks to the AS only".
type routerRecorder struct {
	srv   *httptest.Server
	conns atomic.Int64
}

func newRouterRecorder(t *testing.T) *routerRecorder {
	t.Helper()
	r := &routerRecorder{}
	r.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	r.srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			r.conns.Add(1)
		}
	}
	r.srv.Start()
	t.Cleanup(r.srv.Close)
	return r
}

// flushFixture wires a real config file, an enrolled state directory, and
// a scripted AS the same way `flush` itself will see them at runtime:
// through miningClients -> config.Load, not through pkg/auth's
// constructors directly (that shortcut is what newDriverFixture in
// mining_test.go takes; it proves the AS wiring, not the command layer
// runFlush assembles on top of it).
type flushFixture struct {
	cfg     *config.Config
	cfgPath string
	router  *routerRecorder
}

func newFlushFixture(t *testing.T, as *fakeAS) *flushFixture {
	t.Helper()
	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// The enrolled state a real installation would have after `enroll`:
	// a refresh authorization already in custody.
	store, err := auth.OpenStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveRefreshToken("rt-0"); err != nil {
		t.Fatal(err)
	}
	// miningActive's own default (an absent decision reads as stopped)
	// only applies to a state dir nothing has ever decided anything
	// about; this fixture models an already-enrolled installation, which
	// now always means connect asked the mining question at some point.
	if err := store.SaveMiningEnabled(true); err != nil {
		t.Fatal(err)
	}
	router := newRouterRecorder(t)
	doc := fmt.Sprintf(`
[mining]
enabled = true
as_url = %q
chain_id = %q
slot_id = %d
state_dir = %q
spool_dir = %q

[miner]
enabled = true
router_url = %q
intake_dir = %q
sessions_dir = %q
flush_interval = "1s"
`, as.srv.URL, testChainID, testSlotID, stateDir, filepath.Join(root, "spool"),
		router.srv.URL, filepath.Join(root, "intake"), filepath.Join(root, "sessions"))
	cfgPath := filepath.Join(root, "tokendrop.toml")
	if err := os.WriteFile(cfgPath, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := loadConfig(cfgPath, os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	return &flushFixture{cfg: cfg, cfgPath: cfgPath, router: router}
}

// TestFlushNeverDialsTheRouterEvenOnFailure drives the real runFlush —
// join succeeding, and the AS refusing the one call that path depends on
// — and checks the one thing that must be true regardless: the router
// recorder sees zero connections either way.
func TestFlushNeverDialsTheRouterEvenOnFailure(t *testing.T) {
	cases := []struct {
		name string
		set  func(*asState)
	}{
		{"a joinable open epoch, join succeeds", func(s *asState) { s.epoch, s.joinable = 1042, true }},
		{"the AS refusing current-target", func(s *asState) { s.targetStatus = 500 }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			as := newFakeAS(t)
			as.set(c.set)
			f := newFlushFixture(t, as)

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			var stdout, stderr bytes.Buffer
			_, code := runFlush(ctx, f.cfg, f.cfgPath, true, &stdout, &stderr)
			if code != exitOK {
				t.Fatalf("runFlush exit %d\nstdout: %s\nstderr: %s", code, stdout.String(), stderr.String())
			}
			if n := f.router.conns.Load(); n != 0 {
				t.Fatalf("flush opened %d connection(s) to the router; a flush must talk to the AS only", n)
			}
		})
	}
}

func seedIntakeRecord(t *testing.T, intakeDir, requestID string) {
	t.Helper()
	now := time.Now().UTC()
	if _, err := writeIntake(intakeDir, intakeRecord{
		RequestID:      requestID,
		StatusCode:     http.StatusOK,
		StartedAt:      now,
		FinishedAt:     now,
		ChosenProvider: "fictional",
	}); err != nil {
		t.Fatal(err)
	}
}

func TestFlushCurrentTargetFailurePersistsHealthAndUsesDeliverOnly(t *testing.T) {
	as := newFakeAS(t)
	as.set(func(s *asState) { s.targetStatus = http.StatusServiceUnavailable })
	f := newFlushFixture(t, as)
	seedIntakeRecord(t, f.cfg.Miner.IntakeDir, "current-target-failure")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var stdout, stderr bytes.Buffer
	_, code := runFlush(ctx, f.cfg, f.cfgPath, true, &stdout, &stderr)
	if code != exitOK {
		t.Fatalf("runFlush exit %d\nstdout: %s\nstderr: %s", code, stdout.String(), stderr.String())
	}
	if records, _, err := readIntake(f.cfg.Miner.IntakeDir); err != nil || len(records) != 1 {
		t.Fatalf("current-target failure changed intake: records=%d err=%v", len(records), err)
	}
	if got := spoolCount(t, f.cfg.Mining.SpoolDir); got != 0 {
		t.Fatalf("current-target failure promoted %d observation(s)", got)
	}
	store, err := auth.OpenStoreExisting(f.cfg.Mining.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	record, ok, err := store.LoadHealth(auth.HealthFlush)
	if err != nil || !ok {
		t.Fatalf("flush health missing after current-target failure: ok=%v err=%v", ok, err)
	}
	if record.Component != auth.HealthFlush || record.Reason != auth.HealthSubmissionFailed {
		t.Fatalf("current-target health = %+v, want flush/submission_failed", record)
	}
	if !strings.HasPrefix(record.Detail, currentTargetHealthPrefix) {
		t.Fatalf("current-target detail = %q, want prefix %q", record.Detail, currentTargetHealthPrefix)
	}
	if !strings.Contains(stderr.String(), "no target epoch this run") {
		t.Fatalf("deliverOnly behavior was not preserved: stderr=%q", stderr.String())
	}
}

func TestFlushCurrentTargetAuthFailureUsesAuthStateHealth(t *testing.T) {
	as := newFakeAS(t)
	as.set(func(s *asState) { s.tokenStatus = http.StatusUnauthorized })
	f := newFlushFixture(t, as)
	seedIntakeRecord(t, f.cfg.Miner.IntakeDir, "current-target-auth-failure")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var stdout, stderr bytes.Buffer
	_, code := runFlush(ctx, f.cfg, f.cfgPath, true, &stdout, &stderr)
	if code != exitOK {
		t.Fatalf("runFlush exit %d\nstdout: %s\nstderr: %s", code, stdout.String(), stderr.String())
	}
	store, err := auth.OpenStoreExisting(f.cfg.Mining.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	record, ok, err := store.LoadHealth(auth.HealthFlush)
	if err != nil || !ok {
		t.Fatalf("flush health missing after auth current-target failure: ok=%v err=%v", ok, err)
	}
	if record.Component != auth.HealthFlush || record.Reason != auth.HealthAuthUnavailable {
		t.Fatalf("auth current-target health = %+v, want flush/auth_state_unavailable", record)
	}
	if !strings.HasPrefix(record.Detail, currentTargetHealthPrefix) {
		t.Fatalf("auth current-target detail = %q, want prefix %q", record.Detail, currentTargetHealthPrefix)
	}
}

func TestSuccessfulCurrentTargetClearsOnlyTargetResolutionHealth(t *testing.T) {
	as := newFakeAS(t)
	as.set(func(s *asState) { s.targetStatus = http.StatusServiceUnavailable })
	f := newFlushFixture(t, as)
	seedIntakeRecord(t, f.cfg.Miner.IntakeDir, "current-target-recovery")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var stdout, stderr bytes.Buffer
	if _, code := runFlush(ctx, f.cfg, f.cfgPath, true, &stdout, &stderr); code != exitOK {
		t.Fatalf("failed-target flush exit %d: %s", code, stderr.String())
	}
	store, err := auth.OpenStoreExisting(f.cfg.Mining.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.LoadHealth(auth.HealthFlush); err != nil || !ok {
		t.Fatalf("target-resolution health was not recorded: ok=%v err=%v", ok, err)
	}

	as.set(func(s *asState) {
		s.targetStatus, s.epoch, s.joinable, s.acceptSubmissions = 0, 1042, false, true
	})
	stdout.Reset()
	stderr.Reset()
	if _, code := runFlush(ctx, f.cfg, f.cfgPath, true, &stdout, &stderr); code != exitOK {
		t.Fatalf("recovery flush exit %d: %s", code, stderr.String())
	}
	if _, ok, err := store.LoadHealth(auth.HealthFlush); err != nil || ok {
		t.Fatalf("target-resolution health survived successful resolution: ok=%v err=%v", ok, err)
	}

	if err := store.MarkHealth(auth.HealthFlush, auth.HealthFlushSpawnFailed, "spawn remains broken"); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	if _, code := runFlush(ctx, f.cfg, f.cfgPath, true, &stdout, &stderr); code != exitOK {
		t.Fatalf("unrelated-health flush exit %d: %s", code, stderr.String())
	}
	record, ok, err := store.LoadHealth(auth.HealthFlush)
	if err != nil || !ok || record.Reason != auth.HealthFlushSpawnFailed {
		t.Fatalf("successful target lookup cleared unrelated health: record=%+v ok=%v err=%v", record, ok, err)
	}
}

func TestFlushNoOpenTargetIsNormalAndKeepsIntake(t *testing.T) {
	as := newFakeAS(t)
	f := newFlushFixture(t, as)
	seedIntakeRecord(t, f.cfg.Miner.IntakeDir, "no-open-target")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var stdout, stderr bytes.Buffer
	_, code := runFlush(ctx, f.cfg, f.cfgPath, true, &stdout, &stderr)
	if code != exitOK {
		t.Fatalf("runFlush exit %d\nstdout: %s\nstderr: %s", code, stdout.String(), stderr.String())
	}
	store, err := auth.OpenStoreExisting(f.cfg.Mining.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.LoadHealth(auth.HealthFlush); err != nil || ok {
		t.Fatalf("no-open-target created flush health: ok=%v err=%v", ok, err)
	}
	if records, _, err := readIntake(f.cfg.Miner.IntakeDir); err != nil || len(records) != 1 {
		t.Fatalf("no-open-target changed intake: records=%d err=%v", len(records), err)
	}
	if got := spoolCount(t, f.cfg.Mining.SpoolDir); got != 0 {
		t.Fatalf("no-open-target promoted %d observation(s)", got)
	}
	if !strings.Contains(stderr.String(), "no target epoch this run") {
		t.Fatalf("deliverOnly behavior missing for no-open-target: %q", stderr.String())
	}
}

func TestFlushMissingCurrentTargetEndpointIsNormal(t *testing.T) {
	as := newFakeAS(t)
	as.set(func(s *asState) { s.advertise, s.epoch = false, 1042 })
	f := newFlushFixture(t, as)
	seedIntakeRecord(t, f.cfg.Miner.IntakeDir, "missing-current-target-endpoint")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var stdout, stderr bytes.Buffer
	_, code := runFlush(ctx, f.cfg, f.cfgPath, true, &stdout, &stderr)
	if code != exitOK {
		t.Fatalf("runFlush exit %d\nstdout: %s\nstderr: %s", code, stdout.String(), stderr.String())
	}
	store, err := auth.OpenStoreExisting(f.cfg.Mining.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.LoadHealth(auth.HealthFlush); err != nil || ok {
		t.Fatalf("missing endpoint created flush health: ok=%v err=%v", ok, err)
	}
	if records, _, err := readIntake(f.cfg.Miner.IntakeDir); err != nil || len(records) != 1 {
		t.Fatalf("missing endpoint changed intake: records=%d err=%v", len(records), err)
	}
	if !strings.Contains(stderr.String(), "no current-target endpoint") {
		t.Fatalf("existing endpoint warning disappeared: %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "no target epoch this run") {
		t.Fatalf("deliverOnly behavior missing for missing endpoint: %q", stderr.String())
	}
}

// seedSpoolRecord writes one durable spool record under (slotID, epoch),
// the way promoteIntake would have on some earlier flush.
func seedSpoolRecord(t *testing.T, spoolDir string, slotID, epoch uint64) {
	t.Helper()
	sp, err := spool.Open(spoolDir)
	if err != nil {
		t.Fatal(err)
	}
	id, err := spool.NewClientRecordID()
	if err != nil {
		t.Fatal(err)
	}
	rec := &spool.Record{ClientRecordID: id, SlotID: slotID, TargetEpoch: epoch, Observation: []byte(`{"client_record_id":"` + id + `"}`)}
	if err := sp.Write(rec); err != nil {
		t.Fatal(err)
	}
}

func spoolCount(t *testing.T, spoolDir string) int {
	t.Helper()
	sp, err := spool.Open(spoolDir)
	if err != nil {
		t.Fatal(err)
	}
	n, err := sp.Count()
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// WP2-review defect 1 / WP4b (design aba1245 §2.3/§5.5): observations
// spooled for an epoch this installation lost to another installation of
// the same participant are dropped quietly, state-based — once the AS's
// current target has moved past the conflicted epoch, not before, and NOT
// gated on any capability deadline (the common case — a pure join-time
// ENROLLMENT_CONFLICT — never has one, since this installation never held
// the epoch at all). This is the case an earlier, deadline-gated version
// of this mechanism never dropped.
func TestConflictedObservationsDropOnceTargetAdvancesPastThem(t *testing.T) {
	as := newFakeAS(t)
	as.set(func(s *asState) { s.epoch = -1 }) // nothing open yet; only the housekeeping step matters
	f := newFlushFixture(t, as)
	store, err := auth.OpenStore(f.cfg.Mining.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	seedSpoolRecord(t, f.cfg.Mining.SpoolDir, testSlotID, 1042)
	// A pure join-time conflict: this installation never held 1042, so
	// there is no capability deadline for it anywhere.
	if err := store.SaveEpochConflict(testSlotID, 1042); err != nil {
		t.Fatal(err)
	}

	t.Run("target still at or before the conflicted epoch: still queued", func(t *testing.T) {
		as.set(func(s *asState) { s.epoch, s.joinable = 1042, false }) // AS still offers 1042, not joinable by us
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		var stdout, stderr bytes.Buffer
		if _, code := runFlush(ctx, f.cfg, f.cfgPath, true, &stdout, &stderr); code != exitOK {
			t.Fatalf("runFlush exit code %d\nstderr: %s", code, stderr.String())
		}
		if n := spoolCount(t, f.cfg.Mining.SpoolDir); n != 1 {
			t.Fatalf("spool count = %d, want 1 (target has not moved past 1042 yet)", n)
		}
		if strings.Contains(stdout.String(), "dropped") {
			t.Fatalf("dropped an observation before the target advanced:\n%s", stdout.String())
		}
		conflicts, cerr := store.EpochConflicts()
		if cerr != nil || len(conflicts) != 1 {
			t.Fatalf("EpochConflicts() = %+v err=%v, want the conflict still on file", conflicts, cerr)
		}
	})

	t.Run("target moved past the conflicted epoch: dropped quietly", func(t *testing.T) {
		as.set(func(s *asState) { s.epoch, s.joinable = 1043, true }) // the AS has moved on
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		var stdout, stderr bytes.Buffer
		if _, code := runFlush(ctx, f.cfg, f.cfgPath, true, &stdout, &stderr); code != exitOK {
			t.Fatalf("runFlush exit code %d\nstderr: %s", code, stderr.String())
		}
		if n := spoolCount(t, f.cfg.Mining.SpoolDir); n != 0 {
			t.Fatalf("spool count = %d, want 0 (target moved to 1043, past the conflicted 1042)", n)
		}
		if !strings.Contains(stdout.String(), "dropped 1 observation") {
			t.Fatalf("expected an informational stdout line naming the drop, got:\n%s", stdout.String())
		}
		if strings.Contains(stderr.String(), "dropped") {
			t.Fatalf("dropping a conflicted epoch's observations must be quiet, not error-level: %q", stderr.String())
		}
		conflicts, cerr := store.EpochConflicts()
		if cerr != nil || len(conflicts) != 0 {
			t.Fatalf("EpochConflicts() = %+v err=%v, want it cleared once acted on", conflicts, cerr)
		}
	})
}

// A second conflicted epoch must not clobber the first — the defect a
// single-record EpochParticipation had: recording epoch 1042 as a
// conflict, then later 1043, used to silently forget 1042 forever.
func TestConflictedObservationsSecondConflictDoesNotClobberTheFirst(t *testing.T) {
	as := newFakeAS(t)
	as.set(func(s *asState) { s.epoch = -1 })
	f := newFlushFixture(t, as)
	store, err := auth.OpenStore(f.cfg.Mining.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	seedSpoolRecord(t, f.cfg.Mining.SpoolDir, testSlotID, 1042)
	seedSpoolRecord(t, f.cfg.Mining.SpoolDir, testSlotID, 1043)
	if err := store.SaveEpochConflict(testSlotID, 1042); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveEpochConflict(testSlotID, 1043); err != nil {
		t.Fatal(err)
	}
	conflicts, err := store.EpochConflicts()
	if err != nil || len(conflicts) != 2 {
		t.Fatalf("EpochConflicts() = %+v err=%v, want both 1042 and 1043 on file", conflicts, err)
	}

	as.set(func(s *asState) { s.epoch, s.joinable = 1044, true }) // moved past both
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var stdout, stderr bytes.Buffer
	if _, code := runFlush(ctx, f.cfg, f.cfgPath, true, &stdout, &stderr); code != exitOK {
		t.Fatalf("runFlush exit code %d\nstderr: %s", code, stderr.String())
	}
	if n := spoolCount(t, f.cfg.Mining.SpoolDir); n != 0 {
		t.Fatalf("spool count = %d, want 0 (both 1042 and 1043 are now behind the target)", n)
	}
	if !strings.Contains(stdout.String(), "dropped 2 observation") {
		t.Fatalf("expected an informational stdout line naming both drops, got:\n%s", stdout.String())
	}
}

// invariant 1, both conflict checkpoints: a flush that meets either
// refusal never fails, and never touches the router.
func TestFlushNeverFailsASearchOnEitherConflict(t *testing.T) {
	cases := []struct {
		name string
		set  func(*asState)
	}{
		{"ENROLLMENT_CONFLICT on join", func(s *asState) { s.epoch, s.joinable, s.joinRefusalCode = 1042, true, "ENROLLMENT_CONFLICT" }},
		{"PROXY_BINDING_MISMATCH on exchange", func(s *asState) {
			s.epoch, s.joinable, s.capabilityTwilightError = 1042, true, "PROXY_BINDING_MISMATCH"
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			as := newFakeAS(t)
			as.set(c.set)
			f := newFlushFixture(t, as)

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			var stdout, stderr bytes.Buffer
			_, code := runFlush(ctx, f.cfg, f.cfgPath, true, &stdout, &stderr)
			if code != exitOK {
				t.Fatalf("runFlush exit %d — a conflict must never fail a flush\nstdout: %s\nstderr: %s", code, stdout.String(), stderr.String())
			}
			if n := f.router.conns.Load(); n != 0 {
				t.Fatalf("flush opened %d connection(s) to the router on a conflict", n)
			}
		})
	}
}
