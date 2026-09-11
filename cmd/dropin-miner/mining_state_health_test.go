package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/twilight-project/dropin-miner/pkg/auth"
	"github.com/twilight-project/dropin-miner/pkg/config"
)

func TestMiningASConfiguredDependsOnlyOnASURL(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mining config.Mining
		want   bool
	}{
		{name: "enabled without AS", mining: config.Mining{Enabled: true}, want: false},
		{name: "disabled with AS", mining: config.Mining{Enabled: false, ASBaseURL: "https://as.example"}, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := miningASConfigured(tc.mining); got != tc.want {
				t.Fatalf("miningASConfigured(%+v) = %t, want %t", tc.mining, got, tc.want)
			}
		})
	}
}

func TestStatusRendersDecisionAndASConfigurationIndependently(t *testing.T) {
	for _, tc := range []struct {
		name       string
		decision   auth.MiningDecisionState
		configured bool
		wantState  string
		wantAS     string
	}{
		{name: "ON with AS", decision: auth.MiningEnabled, configured: true, wantState: "ON", wantAS: "as:      configured"},
		{name: "OFF with AS", decision: auth.MiningDisabled, configured: true, wantState: "OFF", wantAS: "as:      configured"},
		{name: "undecided without AS", decision: auth.MiningUndecided, wantState: "NOT DECIDED", wantAS: "no authorization server configured"},
		{name: "degraded with AS", decision: auth.MiningDegraded, configured: true, wantState: "DEGRADED", wantAS: "as:      configured"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			stateDir := filepath.Join(root, "state")
			body := fmt.Sprintf("[mining]\nstate_dir = %q\n", stateDir)
			if tc.configured {
				body += "as_url = \"https://as.example\"\nchain_id = \"twilight-1\"\nslot_id = 7\n"
			}
			cfgPath := writeTOML(t, body)
			if tc.decision != auth.MiningUndecided {
				store, err := auth.OpenStore(stateDir)
				if err != nil {
					t.Fatal(err)
				}
				switch tc.decision {
				case auth.MiningEnabled:
					err = store.SaveMiningEnabled(true)
				case auth.MiningDisabled:
					err = store.SaveMiningEnabled(false)
				case auth.MiningDegraded:
					err = os.WriteFile(filepath.Join(stateDir, "mining_decision.json"), []byte("{not-json"), 0o600)
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			var out, stderr bytes.Buffer
			_ = printAgentIdentityStatus([]string{"-config", cfgPath}, &out, &stderr, noEnv)
			if !strings.Contains(out.String(), tc.wantState) || !strings.Contains(out.String(), tc.wantAS) {
				t.Fatalf("status output = %q, want state %q and AS %q", out.String(), tc.wantState, tc.wantAS)
			}
		})
	}
}

func TestSearchUndecidedIsSilentAndDoesNotCapture(t *testing.T) {
	_, cfg, root := newFakeRouter(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Request-Id", "undecided-request")
		_, _ = w.Write([]byte(routerBody))
	})
	if err := os.Remove(filepath.Join(root, "state", "mining_decision.json")); err != nil {
		t.Fatal(err)
	}
	h := fixedSearchOps(root)
	code, _, stderr := runSearch(t, h, map[string]string{"TOKENDROP_API_KEY": "k"}, "-config", cfg, "q")
	if code != exitOK || stderr != "" {
		t.Fatalf("undecided search = exit %d stderr %q", code, stderr)
	}
	if recs, _, err := readIntake(filepath.Join(root, "intake")); err != nil || len(recs) != 0 {
		t.Fatalf("undecided search captured intake: records=%v err=%v", recs, err)
	}
	if len(h.flushes) != 0 {
		t.Fatalf("undecided search spawned flush: %v", h.flushes)
	}
}

func TestSearchDecisionInspectionFailureFailsClosedButRouterWins(t *testing.T) {
	_, cfg, root := newFakeRouter(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Request-Id", "degraded-request")
		_, _ = w.Write([]byte(routerBody))
	})
	stateDir := filepath.Join(root, "state")
	if err := os.RemoveAll(stateDir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stateDir, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := fixedSearchOps(root)
	code, out, stderr := runSearch(t, h, map[string]string{"TOKENDROP_API_KEY": "k"}, "-config", cfg, "q")
	if code != exitOK || out != routerBody {
		t.Fatalf("degraded search changed router result: exit=%d out=%q", code, out)
	}
	if !strings.Contains(stderr, "cannot be safely trusted") {
		t.Fatalf("degraded search did not explain the fail-closed state: %q", stderr)
	}
	if len(h.flushes) != 0 {
		t.Fatalf("degraded search spawned flush: %v", h.flushes)
	}
	if _, err := os.Stat(filepath.Join(root, "intake")); !os.IsNotExist(err) {
		t.Fatalf("degraded search created intake: %v", err)
	}
}

func TestSearchCaptureHealthMarksAndLaterClearsOnlyCapture(t *testing.T) {
	_, cfg, root := newFakeRouter(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Request-Id", "capture-request")
		_, _ = w.Write([]byte(routerBody))
	})
	intakeDir := filepath.Join(root, "intake")
	if err := os.RemoveAll(intakeDir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(intakeDir, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := fixedSearchOps(root)
	code, _, stderr := runSearch(t, h, map[string]string{"TOKENDROP_API_KEY": "k"}, "-config", cfg, "q")
	if code != exitOK || !strings.Contains(stderr, "could not record") {
		t.Fatalf("capture failure = exit %d stderr %q", code, stderr)
	}
	store, err := auth.OpenStoreExisting(filepath.Join(root, "state"))
	if err != nil {
		t.Fatal(err)
	}
	if rec, ok, err := store.LoadHealth(auth.HealthCapture); err != nil || !ok || rec.Reason != auth.HealthIntakeUnwritable {
		t.Fatalf("capture health = %+v ok=%t err=%v", rec, ok, err)
	}
	if err := store.MarkHealth(auth.HealthDecision, auth.HealthDecisionUnreadable, "decision history"); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkHealth(auth.HealthFlush, auth.HealthSubmissionFailed, "flush history"); err != nil {
		t.Fatal(err)
	}

	if err := os.Remove(intakeDir); err != nil {
		t.Fatal(err)
	}
	code, _, stderr = runSearch(t, h, map[string]string{"TOKENDROP_API_KEY": "k"}, "-config", cfg, "q")
	if code != exitOK || stderr != "" {
		t.Fatalf("recovered capture = exit %d stderr %q", code, stderr)
	}
	if _, ok, err := store.LoadHealth(auth.HealthCapture); err != nil || ok {
		t.Fatalf("capture health was not cleared after a real write: ok=%t err=%v", ok, err)
	}
	if _, ok, err := store.LoadHealth(auth.HealthDecision); err != nil || !ok {
		t.Fatalf("capture recovery erased decision health: ok=%t err=%v", ok, err)
	}
	if _, ok, err := store.LoadHealth(auth.HealthFlush); err != nil || !ok {
		t.Fatalf("capture recovery erased flush health: ok=%t err=%v", ok, err)
	}
}

func TestSearchFlushSpawnFailurePersistsFlushHealth(t *testing.T) {
	_, cfg, root := newFakeRouter(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Request-Id", "spawn-request")
		_, _ = w.Write([]byte(routerBody))
	})
	h := fixedSearchOps(root)
	h.ops.spawnFlush = func(string) error { return os.ErrPermission }
	code, _, stderr := runSearch(t, h, map[string]string{"TOKENDROP_API_KEY": "k"}, "-config", cfg, "q")
	if code != exitOK || !strings.Contains(stderr, "could not start mining flush") {
		t.Fatalf("spawn failure = exit %d stderr %q", code, stderr)
	}
	store, err := auth.OpenStoreExisting(filepath.Join(root, "state"))
	if err != nil {
		t.Fatal(err)
	}
	rec, ok, err := store.LoadHealth(auth.HealthFlush)
	if err != nil || !ok || rec.Reason != auth.HealthFlushSpawnFailed {
		t.Fatalf("flush spawn health = %+v ok=%t err=%v", rec, ok, err)
	}
}

func TestSearchMalformedDecisionPersistsDecisionHealthAndFailsOpen(t *testing.T) {
	_, cfg, root := newFakeRouter(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Request-Id", "malformed-decision")
		_, _ = w.Write([]byte(routerBody))
	})
	stateDir := filepath.Join(root, "state")
	if err := os.WriteFile(filepath.Join(stateDir, "mining_decision.json"), []byte("{not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := fixedSearchOps(root)
	code, out, stderr := runSearch(t, h, map[string]string{"TOKENDROP_API_KEY": "k"}, "-config", cfg, "q")
	if code != exitOK || out != routerBody || !strings.Contains(stderr, "cannot be safely trusted") {
		t.Fatalf("malformed-decision search = exit %d out=%q stderr=%q", code, out, stderr)
	}
	store, err := auth.OpenStoreExisting(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if rec, ok, err := store.LoadHealth(auth.HealthDecision); err != nil || !ok || rec.Reason != auth.HealthDecisionUnreadable {
		t.Fatalf("decision health = %+v ok=%t err=%v", rec, ok, err)
	}
	if recs, _, err := readIntake(filepath.Join(root, "intake")); err != nil || len(recs) != 0 {
		t.Fatalf("malformed decision captured intake: records=%v err=%v", recs, err)
	}
}

func TestSearchHealthPersistenceFailureCannotReplaceRouterSuccess(t *testing.T) {
	_, cfg, root := newFakeRouter(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Request-Id", "health-write-failure")
		_, _ = w.Write([]byte(routerBody))
	})
	stateDir := filepath.Join(root, "state")
	if err := os.WriteFile(filepath.Join(stateDir, "mining_decision.json"), []byte("{not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(stateDir, "health_decision.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	h := fixedSearchOps(root)
	code, out, stderr := runSearch(t, h, map[string]string{"TOKENDROP_API_KEY": "k"}, "-config", cfg, "q")
	if code != exitOK || out != routerBody || !strings.Contains(stderr, "cannot be safely trusted") {
		t.Fatalf("health-write-failure search = exit %d out=%q stderr=%q", code, out, stderr)
	}
	if len(h.flushes) != 0 {
		t.Fatalf("degraded search spawned flush after health failure: %v", h.flushes)
	}
}

func TestSearchPersistedOnOverridesConfigEnabledFalse(t *testing.T) {
	_, cfg, root := newFakeRouter(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Request-Id", "config-disagreement")
		_, _ = w.Write([]byte(routerBody))
	})
	raw, err := os.ReadFile(cfg) // #nosec G304 -- cfg is the test fixture's fixed config path
	if err != nil {
		t.Fatal(err)
	}
	body := strings.Replace(string(raw), "[mining]\nenabled = true", "[mining]\nenabled = false", 1)
	if body == string(raw) {
		t.Fatal("test fixture did not contain the mining config enabled key")
	}
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil { // #nosec G703 -- cfg is test-owned
		t.Fatal(err)
	}

	h := fixedSearchOps(root)
	code, out, stderr := runSearch(t, h, map[string]string{"TOKENDROP_API_KEY": "k"}, "-config", cfg, "q")
	if code != exitOK || out != routerBody || stderr != "" {
		t.Fatalf("persisted ON with config false = exit %d out=%q stderr=%q", code, out, stderr)
	}
	if recs, _, err := readIntake(filepath.Join(root, "intake")); err != nil || len(recs) != 1 {
		t.Fatalf("persisted ON did not capture with config false: records=%v err=%v", recs, err)
	}
}

// This is the production lifecycle that the runtime decision must survive:
// the config has AS identity but no [mining] enabled key, the interactive
// connect decision writes ON, connect registers/enrolls, then search and the
// real flush consume that persisted decision. The flush AS is a local stub
// with the normal mining endpoints; no decision file is written by setup.
func TestInteractiveConnectDecisionAuthorizesSearchAndFlushWithoutConfigEnabled(t *testing.T) {
	withShortConnectTimings(t)
	platform := newStubPlatform(t)
	onboardingAS := newStubAS(t)
	cfgPath, stateDir := connectConfig(t, platform.srv.URL, onboardingAS.srv.URL)
	raw, err := os.ReadFile(cfgPath) // #nosec G304 -- cfgPath is this test's writeTOML output
	if err != nil {
		t.Fatal(err)
	}
	body := strings.Replace(string(raw), "\nenabled = true\nas_url", "\nas_url", 1)
	if body == string(raw) || strings.Contains(body, "[mining]\nenabled = true") {
		t.Fatal("test lifecycle config still contains [mining] enabled = true")
	}
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil { // #nosec G703 -- cfgPath is test-owned
		t.Fatal(err)
	}

	cfg, _, err := loadConfig(cfgPath, noEnv)
	if err != nil {
		t.Fatal(err)
	}
	store, err := auth.OpenStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	addr := "twilight1k5stzqa2sgvfgx9u04cv93pek3gcmm9h5t9hkn"
	stdin := bytes.NewBufferString("yes\n" + addr + "\n")
	outcome, code := decideRegistrationOutcome(stdin, bufio.NewReader(stdin), &bytes.Buffer{}, &bytes.Buffer{}, noEnv, cfg, store, true)
	if code != exitOK || !outcome.enabled || outcome.payoutAddress != addr {
		t.Fatalf("interactive connect decision = %+v code=%d", outcome, code)
	}
	decision := store.ReadMiningDecision()
	if decision.State != auth.MiningEnabled {
		t.Fatalf("interactive decision = %+v, want ON", decision)
	}

	if code, _, errOut := runConnect(t, cfgPath, nil); code != exitOK {
		t.Fatalf("register connect exited %d: %s", code, errOut)
	}
	platform.claim("credits", "mining")
	if code, out, errOut := runConnect(t, cfgPath, nil); code != exitOK || !strings.Contains(out, "enrolled for mining") {
		t.Fatalf("enrollment connect exited %d: stdout=%q stderr=%q", code, out, errOut)
	}
	if _, ok, err := store.LoadRefreshToken(); err != nil || !ok {
		t.Fatalf("enrollment did not leave refresh authorization: ok=%t err=%v", ok, err)
	}

	miningAS := newFakeAS(t)
	miningAS.set(func(s *asState) {
		s.epoch, s.joinable, s.acceptSubmissions = 1042, true, true
	})
	router, _, _ := newFakeRouter(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Request-Id", "lifecycle-request")
		_, _ = w.Write([]byte(routerBody))
	})
	final, err := os.ReadFile(cfgPath) // #nosec G304 -- cfgPath is this test's writeTOML output
	if err != nil {
		t.Fatal(err)
	}
	finalBody := strings.Replace(string(final), onboardingAS.srv.URL, miningAS.srv.URL, 1)
	finalBody += fmt.Sprintf("\n[miner]\nenabled = true\nrouter_url = %q\n", router.srv.URL)
	if err := os.WriteFile(cfgPath, []byte(finalBody), 0o600); err != nil { // #nosec G703 -- cfgPath is test-owned
		t.Fatal(err)
	}

	h := fixedSearchOps(filepath.Dir(stateDir))
	code, out, stderr := runSearch(t, h, map[string]string{"TOKENDROP_API_KEY": "k"}, "-config", cfgPath, "q")
	if code != exitOK || out != routerBody || stderr != "" {
		t.Fatalf("lifecycle search = exit %d out=%q stderr=%q", code, out, stderr)
	}
	if recs, _, err := readIntake(filepath.Join(filepath.Dir(stateDir), "intake")); err != nil || len(recs) != 1 {
		t.Fatalf("lifecycle intake = %v err=%v", recs, err)
	}

	finalCfg, _, err := loadConfig(cfgPath, noEnv)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var flushOut, flushErr bytes.Buffer
	rep, code := runFlush(ctx, finalCfg, cfgPath, true, &flushOut, &flushErr)
	if code != exitOK || rep.Delivered != 1 {
		t.Fatalf("lifecycle flush = report=%+v code=%d stdout=%q stderr=%q", rep, code, flushOut.String(), flushErr.String())
	}
	if got := miningAS.submissionCalls.Load(); got != 1 {
		t.Fatalf("stub AS submission calls = %d, want 1", got)
	}
}

func TestFlushHealthTracksSubmissionBacklogAndRelevantRecovery(t *testing.T) {
	as := newFakeAS(t)
	f := newFlushFixture(t, as)
	seedSpoolRecord(t, f.cfg.Mining.SpoolDir, testSlotID, 1042)
	store, err := auth.OpenStoreExisting(f.cfg.Mining.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkHealth(auth.HealthCapture, auth.HealthIntakeUnwritable, "capture remains broken"); err != nil {
		t.Fatal(err)
	}

	run := func() flushReport {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		var out, stderr bytes.Buffer
		rep, code := runFlush(ctx, f.cfg, f.cfgPath, true, &out, &stderr)
		if code != exitOK {
			t.Fatalf("flush exited %d: stdout=%q stderr=%q", code, out.String(), stderr.String())
		}
		return rep
	}

	if rep := run(); rep.Pending != 1 {
		t.Fatalf("first failed flush report = %+v", rep)
	}
	if rec, ok, err := store.LoadHealth(auth.HealthFlush); err != nil || !ok || rec.Reason != auth.HealthSubmissionFailed {
		t.Fatalf("first flush health = %+v ok=%t err=%v", rec, ok, err)
	}
	if rep := run(); rep.Pending != 1 {
		t.Fatalf("backlogged flush report = %+v", rep)
	}
	if rec, ok, err := store.LoadHealth(auth.HealthFlush); err != nil || !ok || rec.Reason != auth.HealthSpoolBacklog {
		t.Fatalf("backlog health = %+v ok=%t err=%v", rec, ok, err)
	}

	as.set(func(s *asState) { s.acceptSubmissions = true })
	if rep := run(); rep.Delivered != 1 || rep.Pending != 0 {
		t.Fatalf("recovered flush report = %+v", rep)
	}
	if _, ok, err := store.LoadHealth(auth.HealthFlush); err != nil || ok {
		t.Fatalf("flush health survived accepted delivery: ok=%t err=%v", ok, err)
	}
	if rec, ok, err := store.LoadHealth(auth.HealthCapture); err != nil || !ok || rec.Reason != auth.HealthIntakeUnwritable {
		t.Fatalf("flush recovery erased capture health: %+v ok=%t err=%v", rec, ok, err)
	}
}

func TestFlushAuthFailureMarksOnlyFlushHealth(t *testing.T) {
	as := newFakeAS(t)
	f := newFlushFixture(t, as)
	store, err := auth.OpenStoreExisting(f.cfg.Mining.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteRefreshToken(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var out, stderr bytes.Buffer
	_, code := runFlush(ctx, f.cfg, f.cfgPath, true, &out, &stderr)
	if code == exitOK {
		t.Fatalf("flush with no authorization unexpectedly succeeded: stdout=%q stderr=%q", out.String(), stderr.String())
	}
	if rec, ok, err := store.LoadHealth(auth.HealthFlush); err != nil || !ok || rec.Reason != auth.HealthAuthUnavailable {
		t.Fatalf("auth failure health = %+v ok=%t err=%v", rec, ok, err)
	}
}

func TestFlushNormalInactiveStatesDoNotCreateHealth(t *testing.T) {
	for _, tc := range []struct {
		name     string
		decision *bool
	}{
		{name: "OFF", decision: func() *bool { v := false; return &v }()},
		{name: "undecided"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			as := newFakeAS(t)
			f := newFlushFixture(t, as)
			store, err := auth.OpenStoreExisting(f.cfg.Mining.StateDir)
			if err != nil {
				t.Fatal(err)
			}
			if tc.decision == nil {
				if err := os.Remove(filepath.Join(f.cfg.Mining.StateDir, "mining_decision.json")); err != nil {
					t.Fatal(err)
				}
			} else if err := store.SaveMiningEnabled(*tc.decision); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			var out, stderr bytes.Buffer
			if _, code := runFlush(ctx, f.cfg, f.cfgPath, true, &out, &stderr); code != exitOK {
				t.Fatalf("inactive flush exited %d: stdout=%q stderr=%q", code, out.String(), stderr.String())
			}
			if records, err := store.HealthRecords(); err != nil || len(records) != 0 {
				t.Fatalf("inactive state created degradation: records=%+v err=%v", records, err)
			}
		})
	}
}

func TestFlushNormalOperationalNoOpsDoNotCreateHealth(t *testing.T) {
	t.Run("no target and empty queue", func(t *testing.T) {
		as := newFakeAS(t)
		f := newFlushFixture(t, as)
		runHealthyNoOpFlush(t, f)
	})
	t.Run("already joined and empty queue", func(t *testing.T) {
		as := newFakeAS(t)
		as.set(func(s *asState) {
			s.epoch, s.joinable, s.joinStatus = 1042, false, auth.JoinAlreadyAccepted
		})
		f := newFlushFixture(t, as)
		runHealthyNoOpFlush(t, f)
	})
	t.Run("another flush owns the lock", func(t *testing.T) {
		as := newFakeAS(t)
		f := newFlushFixture(t, as)
		if err := os.MkdirAll(minerRoot(f.cfg.Miner), 0o700); err != nil {
			t.Fatal(err)
		}
		lock, held, err := tryLockFile(flushLockPath(f.cfg.Miner))
		if err != nil || !held {
			t.Fatalf("hold flush lock: held=%t err=%v", held, err)
		}
		defer func() { _ = unlockFile(lock) }()
		runHealthyNoOpFlush(t, f)
	})
}

func runHealthyNoOpFlush(t *testing.T, f *flushFixture) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var out, stderr bytes.Buffer
	if _, code := runFlush(ctx, f.cfg, f.cfgPath, true, &out, &stderr); code != exitOK {
		t.Fatalf("flush no-op exited %d: stdout=%q stderr=%q", code, out.String(), stderr.String())
	}
	store, err := auth.OpenStoreExisting(f.cfg.Mining.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	if records, err := store.HealthRecords(); err != nil || len(records) != 0 {
		t.Fatalf("healthy no-op created degradation: records=%+v err=%v", records, err)
	}
}
