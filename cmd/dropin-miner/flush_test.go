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

// WP4b (design f0ddb69 §2.3/§5.5): observations spooled for an epoch this
// installation lost to another installation of the same participant are
// dropped quietly ONLY once that epoch's capability deadline has passed —
// not the instant the conflict is recorded, in case it was transient.
func TestConflictedObservationsDropOnlyAfterTheDeadline(t *testing.T) {
	as := newFakeAS(t)
	as.set(func(s *asState) { s.epoch = -1 }) // nothing open this run; only the housekeeping step matters
	f := newFlushFixture(t, as)
	store, err := auth.OpenStore(f.cfg.Mining.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	seedSpoolRecord(t, f.cfg.Mining.SpoolDir, testSlotID, 1042)

	t.Run("before the deadline: still queued", func(t *testing.T) {
		if err := store.SaveEpochCapabilitySuccess(testSlotID, 1042, time.Now().Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		if err := store.SaveEpochConflict(testSlotID, 1042); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		var stdout, stderr bytes.Buffer
		if _, code := runFlush(ctx, f.cfg, f.cfgPath, true, &stdout, &stderr); code != exitOK {
			t.Fatalf("runFlush exit code %d\nstderr: %s", code, stderr.String())
		}
		if n := spoolCount(t, f.cfg.Mining.SpoolDir); n != 1 {
			t.Fatalf("spool count = %d, want 1 (still within the deadline, must not be dropped)", n)
		}
		if strings.Contains(stdout.String(), "dropped") {
			t.Fatalf("dropped an observation before its deadline:\n%s", stdout.String())
		}
	})

	t.Run("after the deadline: dropped quietly", func(t *testing.T) {
		if err := store.SaveEpochCapabilitySuccess(testSlotID, 1042, time.Now().Add(-time.Minute)); err != nil {
			t.Fatal(err)
		}
		if err := store.SaveEpochConflict(testSlotID, 1042); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		var stdout, stderr bytes.Buffer
		if _, code := runFlush(ctx, f.cfg, f.cfgPath, true, &stdout, &stderr); code != exitOK {
			t.Fatalf("runFlush exit code %d\nstderr: %s", code, stderr.String())
		}
		if n := spoolCount(t, f.cfg.Mining.SpoolDir); n != 0 {
			t.Fatalf("spool count = %d, want 0 (past the deadline, must be dropped)", n)
		}
		if !strings.Contains(stdout.String(), "dropped 1 observation") {
			t.Fatalf("expected an informational stdout line naming the drop, got:\n%s", stdout.String())
		}
		if strings.Contains(stderr.String(), "dropped") {
			t.Fatalf("dropping a conflicted, expired epoch's observations must be quiet, not error-level: %q", stderr.String())
		}
		if _, ok, err := store.LoadEpochParticipation(); err != nil || ok {
			t.Fatalf("epoch participation record should be cleared once acted on: ok=%v err=%v", ok, err)
		}
	})
}

// A pure join-time ENROLLMENT_CONFLICT — this installation never
// successfully held the epoch, so there is no deadline to compare
// against — must not speculatively drop the observations. They stay
// queued; a human or a later flush is the only thing that resolves them.
func TestConflictedObservationsWithNoKnownDeadlineAreNotDroppedSpeculatively(t *testing.T) {
	as := newFakeAS(t)
	as.set(func(s *asState) { s.epoch = -1 })
	f := newFlushFixture(t, as)
	store, err := auth.OpenStore(f.cfg.Mining.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	seedSpoolRecord(t, f.cfg.Mining.SpoolDir, testSlotID, 1042)
	if err := store.SaveEpochConflict(testSlotID, 1042); err != nil { // no prior SaveEpochCapabilitySuccess
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var stdout, stderr bytes.Buffer
	if _, code := runFlush(ctx, f.cfg, f.cfgPath, true, &stdout, &stderr); code != exitOK {
		t.Fatalf("runFlush exit code %d\nstderr: %s", code, stderr.String())
	}
	if n := spoolCount(t, f.cfg.Mining.SpoolDir); n != 1 {
		t.Fatalf("spool count = %d, want 1 (no known deadline; must not drop speculatively)", n)
	}
	rec, ok, err := store.LoadEpochParticipation()
	if err != nil || !ok || !rec.Conflict {
		t.Fatalf("the conflict record itself should survive untouched: rec=%+v ok=%v err=%v", rec, ok, err)
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
