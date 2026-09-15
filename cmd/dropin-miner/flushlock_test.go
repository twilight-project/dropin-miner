package main

// The flush lock and stamp under a sandbox that denies writes to the miner
// root (#64), proven against the real permission condition: POSIX modes or a
// Windows deny-write ACE, applied in the test and checked before any flush
// runs. The one lock is shared by every generation of flush; a 0.2.9 flush is
// stood in for by a read-write flush paused on its lock, since the lock is
// the only part of it another flush can observe.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/twilight-project/dropin-miner/internal/selfupdate"
	"github.com/twilight-project/dropin-miner/pkg/auth"
	"github.com/twilight-project/dropin-miner/pkg/config"
	"github.com/twilight-project/dropin-miner/pkg/mining/spool"
)

// contractAS answers observation submissions per contract §59/§60: transport
// identity is client_record_id; evidence identity carries the target epoch
// the capability names, so one id under two epochs is OBSERVATION_CONFLICT.
type contractAS struct {
	mu          sync.Mutex
	accepted    map[string]string // client_record_id -> provider_event_id@epoch
	credited    map[string]int    // provider_event_id -> credited count
	conflicts   int
	submissions int
}

func installContractAS(t *testing.T, as *fakeAS) *contractAS {
	t.Helper()
	c := &contractAS{accepted: map[string]string{}, credited: map[string]int{}}
	next := as.srv.Config.Handler
	as.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/mining/observations" {
			next.ServeHTTP(w, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var obs struct {
			ClientRecordID  string `json:"client_record_id"`
			ProviderEventID string `json:"provider_event_id"`
		}
		_ = json.Unmarshal(body, &obs)
		epoch := strings.TrimPrefix(strings.TrimPrefix(r.Header.Get("Authorization"), "DPoP "), "capability-")
		evidence := obs.ProviderEventID + "@" + epoch
		c.mu.Lock()
		defer c.mu.Unlock()
		c.submissions++
		w.Header().Set("Content-Type", "application/json")
		prior, seen := c.accepted[obs.ClientRecordID]
		switch {
		case seen && prior == evidence:
			_ = json.NewEncoder(w).Encode(map[string]string{"submission_status": "ALREADY_ACCEPTED", "client_record_id": obs.ClientRecordID, "observation_id": "obsv-" + obs.ClientRecordID})
		case seen:
			c.conflicts++
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": "OBSERVATION_CONFLICT"}})
		default:
			c.accepted[obs.ClientRecordID] = evidence
			c.credited[obs.ProviderEventID]++
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{"submission_status": "ACCEPTED", "client_record_id": obs.ClientRecordID, "observation_id": "obsv-" + obs.ClientRecordID})
		}
	})
	return c
}

func (c *contractAS) totals(event string) (credited, conflicts, submissions int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.credited[event], c.conflicts, c.submissions
}

// flushLockFixture is a flush fixture whose AS follows the submission
// contract, with one recorded search waiting in intake.
type flushLockFixture struct {
	*flushFixture
	as     *fakeAS
	ledger *contractAS
	root   string // the miner root: the parent of intake, and of the config
	lock   string
	legacy string // the 0.2.9 stamp beside intake
	stamp  string // the stamp under the state directory
	state  string
}

const lockFixtureRequest = "req-flush-lock-1"

func newFlushLockFixture(t *testing.T) *flushLockFixture {
	t.Helper()
	as := newFakeAS(t)
	as.set(func(s *asState) { s.epoch, s.joinable, s.acceptSubmissions = 10, true, true })
	ledger := installContractAS(t, as)
	f := newFlushFixture(t, as)
	x := &flushLockFixture{
		flushFixture: f, as: as, ledger: ledger,
		root:   minerRoot(f.cfg.Miner),
		lock:   flushLockPath(f.cfg.Miner),
		legacy: legacyFlushStampPath(f.cfg.Miner),
		stamp:  flushStampPath(f.cfg.Mining),
		state:  f.cfg.Mining.StateDir,
	}
	if x.root != filepath.Dir(f.cfgPath) || filepath.Dir(x.stamp) != x.state {
		t.Fatalf("fixture layout: root %s, config %s, stamp %s", x.root, f.cfgPath, x.stamp)
	}
	// A real installation has these before any flush: setup creates them, and
	// under the sandbox nothing can create them in the miner root.
	for _, dir := range []string{f.cfg.Mining.SpoolDir, f.cfg.Miner.SessionsDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	seedIntakeRecord(t, f.cfg.Miner.IntakeDir, lockFixtureRequest)
	return x
}

func (x *flushLockFixture) intakeCount(t *testing.T) int {
	t.Helper()
	n, err := countIntakeJSON(x.cfg.Miner.IntakeDir)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func (x *flushLockFixture) flushHealth(t *testing.T) (auth.HealthRecord, bool) {
	t.Helper()
	store, err := auth.OpenStoreExisting(x.state)
	if err != nil {
		t.Fatal(err)
	}
	rec, ok, err := store.LoadHealth(auth.HealthFlush)
	if err != nil {
		t.Fatal(err)
	}
	return rec, ok
}

// recordFlushEvents installs flushTestHook for the test and returns what it
// saw, in order.
func recordFlushEvents(t *testing.T) func() []string {
	t.Helper()
	var mu sync.Mutex
	var events []string
	flushTestHook = func(_ context.Context, event string) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, event)
	}
	t.Cleanup(func() { flushTestHook = nil })
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), events...)
	}
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

func runFlushT(t *testing.T, cfg *config.Config, cfgPath string, force bool) (flushReport, int, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var stdout, stderr bytes.Buffer
	rep, code := runFlush(ctx, cfg, cfgPath, force, &stdout, &stderr)
	return rep, code, stdout.String() + stderr.String()
}

// proveWriteDeniedMinerRoot is the fixture's self-proof: the permission
// condition a sandboxed flush meets, established before a flush runs. With
// lock "" it skips the two checks on the lock file (for a lock that is absent,
// or held by another process, which on Windows also refuses a read handle).
func proveWriteDeniedMinerRoot(root, lock, state string) error {
	probe := filepath.Join(root, ".sandbox-probe")
	if err := os.WriteFile(probe, nil, 0o600); err == nil {
		_ = os.Remove(probe)
		return errors.New("a write to the miner root succeeded")
	} else if !errors.Is(err, fs.ErrPermission) {
		return fmt.Errorf("a write to the miner root failed, but not for permission: %w", err)
	}
	if lock != "" {
		if f, err := os.OpenFile(lock, os.O_RDWR, 0); err == nil { // #nosec G304 -- test-owned path
			_ = f.Close()
			return errors.New("a read-write open of the flush lock succeeded")
		} else if !errors.Is(err, fs.ErrPermission) {
			return fmt.Errorf("a read-write open of the flush lock failed, but not for permission: %w", err)
		}
		f, err := os.Open(lock) // #nosec G304 -- test-owned path
		if err != nil {
			return fmt.Errorf("a read-only open of the flush lock failed: %w", err)
		}
		_ = f.Close()
	}
	probe = filepath.Join(state, ".sandbox-probe")
	if err := os.WriteFile(probe, nil, 0o600); err != nil {
		return fmt.Errorf("a write to the state directory failed: %w", err)
	}
	return os.Remove(probe)
}

func requireSandboxEmulation(t *testing.T) {
	t.Helper()
	if reason := sandboxEmulationUnavailable(); reason != "" {
		skipPermissionTest(t, reason)
	}
}

// permissionTestTB is the part of testing.TB skipPermissionTest uses, so its
// own test can watch it decide without being stopped by it.
type permissionTestTB interface {
	Helper()
	Skip(args ...any)
	Fatalf(format string, args ...any)
}

// skipPermissionTest is the one way a permission test leaves when this
// environment cannot establish its condition (root on POSIX, an ACL Windows
// will not set, no chflags on macOS). Locally it skips with the reason. Under
// CI=true, which GitHub Actions sets, it fails instead: CI runs go test
// without -v, so a skip there would read as a pass, and a green job must mean
// the test ran.
func skipPermissionTest(t permissionTestTB, reason string) {
	t.Helper()
	if os.Getenv("CI") == "true" {
		t.Fatalf("a permission test cannot run on this CI runner, and CI does not let it skip: %s", reason)
		return
	}
	t.Skip(reason)
}

type recordingPermissionTB struct {
	skipped, failed bool
	message         string
}

func (r *recordingPermissionTB) Helper() {}
func (r *recordingPermissionTB) Skip(args ...any) {
	r.skipped, r.message = true, fmt.Sprint(args...)
}
func (r *recordingPermissionTB) Fatalf(format string, args ...any) {
	r.failed, r.message = true, fmt.Sprintf(format, args...)
}

func TestAPermissionTestFailsInsteadOfSkippingUnderCI(t *testing.T) {
	const reason = "running as root: file modes do not deny root"
	t.Setenv("CI", "true")
	var ci recordingPermissionTB
	skipPermissionTest(&ci, reason)
	if !ci.failed || ci.skipped || !strings.Contains(ci.message, reason) {
		t.Errorf("under CI=true: failed %v, skipped %v, message %q; want a failure naming the reason", ci.failed, ci.skipped, ci.message)
	}

	t.Setenv("CI", "")
	var local recordingPermissionTB
	skipPermissionTest(&local, reason)
	if local.failed || !local.skipped || local.message != reason {
		t.Errorf("outside CI: failed %v, skipped %v, message %q; want a skip with the reason", local.failed, local.skipped, local.message)
	}
}

func writeStampT(t *testing.T, path string, st flushStamp) {
	t.Helper()
	st.V = 1
	data, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// ── the sandbox ─────────────────────────────────────────────────────────

// TestSandboxedFlushTakesTheLockReadOnlyAndDelivers is #64's fix under the
// condition Codex's workspace-write sandbox creates: the miner root, the lock
// and the legacy stamp readable but not writable; state, intake and spool
// writable.
func TestSandboxedFlushTakesTheLockReadOnlyAndDelivers(t *testing.T) {
	requireSandboxEmulation(t)
	x := newFlushLockFixture(t)
	if _, err := ensureFlushLockFile(x.lock); err != nil {
		t.Fatal(err)
	}
	writeStampT(t, x.legacy, flushStamp{SlotID: testSlotID, TargetEpoch: 7, LastAS: time.Now().Add(-time.Hour)})
	legacyBefore, err := os.ReadFile(x.legacy)
	if err != nil {
		t.Fatal(err)
	}
	denyWritesKeepReads(t, x.root, x.lock, x.legacy)
	if err := proveWriteDeniedMinerRoot(x.root, x.lock, x.state); err != nil {
		t.Fatalf("the fixture does not emulate the sandbox: %v", err)
	}

	events := recordFlushEvents(t)
	rep, code, out := runFlushT(t, x.cfg, x.cfgPath, false)
	if code != exitOK {
		t.Fatalf("flush exit %d\n%s", code, out)
	}
	if !contains(events(), "locked read-only") {
		t.Fatalf("the flush did not take the lock read-only: events %v\n%s", events(), out)
	}
	if credited, _, _ := x.ledger.totals(lockFixtureRequest); rep.Delivered != 1 || credited != 1 {
		t.Fatalf("delivered %d, credited %d; want 1 and 1\n%s", rep.Delivered, credited, out)
	}
	if n := x.intakeCount(t); n != 0 {
		t.Errorf("%d record(s) still in intake", n)
	}
	if st := readFlushStamp(x.stamp); st.TargetEpoch != 10 || st.LastFlush.IsZero() {
		t.Errorf("new stamp %+v, want target 10 and a flush time", st)
	}
	if after, err := os.ReadFile(x.legacy); err != nil || !bytes.Equal(after, legacyBefore) {
		t.Errorf("the legacy stamp changed (err %v)", err)
	}
	if rec, ok := x.flushHealth(t); ok {
		t.Errorf("flush health %s %q after a clean sandboxed flush", rec.Reason, rec.Detail)
	}
}

// TestSandboxFixtureSelfProofRejectsAnUnrestrictedRoot proves the self-proof
// is a check and not a formality: on a root nothing restricts, it fails.
func TestSandboxFixtureSelfProofRejectsAnUnrestrictedRoot(t *testing.T) {
	root := t.TempDir()
	state := filepath.Join(root, "state")
	lock := filepath.Join(root, "flush.lock")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := proveWriteDeniedMinerRoot(root, lock, state); err == nil {
		t.Fatal("the self-proof accepted a miner root that denies nothing")
	}
	if err := proveWriteDeniedMinerRoot(root, "", state); err == nil {
		t.Fatal("the self-proof accepted a writable miner root when the lock checks were skipped")
	}
}

// ── exclusion across processes ──────────────────────────────────────────

const flushHolderEnv = "DROPIN_MINER_TEST_FLUSH_HOLDER_CONFIG"

// TestFlushLockHolderProcess is not a test on its own: it is the other process
// of TestFlushLockExcludesAcrossProcessesInEveryOpenMode. It runs one flush,
// prints how it holds the lock, and holds it until its stdin closes.
func TestFlushLockHolderProcess(t *testing.T) {
	cfgPath := os.Getenv(flushHolderEnv)
	if cfgPath == "" {
		t.Skip("helper process for the cross-process flush lock tests")
	}
	cfg, _, err := loadConfig(cfgPath, os.Getenv)
	if err != nil {
		fmt.Printf("HOLDER-ERROR %v\n", err)
		return
	}
	flushTestHook = func(_ context.Context, event string) {
		switch {
		case strings.HasPrefix(event, "locked "):
			fmt.Printf("HOLDING %s\n", strings.TrimPrefix(event, "locked "))
			_, _ = io.Copy(io.Discard, os.Stdin)
		case strings.HasPrefix(event, "busy "):
			fmt.Printf("BUSY %s\n", strings.TrimPrefix(event, "busy "))
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	_, code := runFlush(ctx, cfg, cfgPath, true, io.Discard, os.Stderr)
	fmt.Printf("EXIT %d\n", code)
}

type flushHolder struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	lines chan string
	out   *bytes.Buffer
}

// startFlushHolder starts a flush in another process and waits until it holds
// the lock, returning the open mode it reports.
func startFlushHolder(t *testing.T, cfgPath string) (*flushHolder, string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestFlushLockHolderProcess$", "-test.count=1") // #nosec G204 -- this test binary
	cmd.Env = append(os.Environ(), flushHolderEnv+"="+cfgPath)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var errOut bytes.Buffer
	cmd.Stderr = &errOut
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	h := &flushHolder{cmd: cmd, stdin: stdin, lines: make(chan string, 16), out: &errOut}
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			h.lines <- scanner.Text()
		}
		close(h.lines)
	}()
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	deadline := time.After(60 * time.Second)
	for {
		select {
		case line, ok := <-h.lines:
			if !ok {
				t.Fatalf("the holder process exited before holding the lock\n%s", errOut.String())
			}
			if mode, found := strings.CutPrefix(line, "HOLDING "); found {
				return h, mode
			}
			if strings.HasPrefix(line, "BUSY ") || strings.HasPrefix(line, "HOLDER-ERROR") {
				t.Fatalf("the holder process could not hold the lock: %s\n%s", line, errOut.String())
			}
		case <-deadline:
			t.Fatalf("the holder process never held the lock\n%s", errOut.String())
		}
	}
}

// release lets the holder finish its flush and returns its exit line.
func (h *flushHolder) release(t *testing.T) string {
	t.Helper()
	_ = h.stdin.Close()
	deadline := time.After(60 * time.Second)
	for {
		select {
		case line, ok := <-h.lines:
			if !ok {
				t.Fatalf("the holder process ended without reporting its exit\n%s", h.out.String())
			}
			if strings.HasPrefix(line, "EXIT ") {
				return line
			}
		case <-deadline:
			t.Fatalf("the holder process did not finish\n%s", h.out.String())
		}
	}
}

// TestFlushLockExcludesAcrossProcessesInEveryOpenMode holds the one flush lock
// in another process in each open mode and proves a flush here is busy in each
// mode. The read-write holder stands in for a 0.2.9 flush outside a sandbox;
// the read-only ones are sandboxed flushes. Permissions change between the two
// processes' opens, so each takes the mode it reports through its own real
// permission check.
func TestFlushLockExcludesAcrossProcessesInEveryOpenMode(t *testing.T) {
	requireSandboxEmulation(t)
	cases := []struct {
		name                string
		holder, contender   string
		restrictBeforeStart bool
	}{
		{"a read-write holder makes a read-only flush busy", "read-write", "read-only", false},
		{"a read-only holder makes a read-write flush busy", "read-only", "read-write", true},
		{"a read-only holder makes a read-only flush busy", "read-only", "read-only", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			x := newFlushLockFixture(t)
			if _, err := ensureFlushLockFile(x.lock); err != nil {
				t.Fatal(err)
			}
			var restore func()
			if c.restrictBeforeStart {
				restore = denyWritesKeepReads(t, x.root, x.lock)
				if err := proveWriteDeniedMinerRoot(x.root, x.lock, x.state); err != nil {
					t.Fatalf("the fixture does not emulate the sandbox: %v", err)
				}
			}
			holder, mode := startFlushHolder(t, x.cfgPath)
			if mode != c.holder {
				t.Fatalf("the holder took the lock %s, want %s", mode, c.holder)
			}
			switch {
			case c.contender == "read-write" && restore != nil:
				restore()
			case c.contender == "read-only" && restore == nil:
				denyWritesKeepReads(t, x.root, x.lock)
				if err := proveWriteDeniedMinerRoot(x.root, "", x.state); err != nil {
					t.Fatalf("the fixture does not emulate the sandbox: %v", err)
				}
			}

			events := recordFlushEvents(t)
			_, code, out := runFlushT(t, x.cfg, x.cfgPath, true)
			if code != exitOK || !strings.Contains(out, "another flush is running") {
				t.Fatalf("contending flush exit %d, want busy\n%s", code, out)
			}
			if got := events(); !contains(got, "busy "+c.contender) || contains(got, "locked "+c.contender) {
				t.Fatalf("contending flush events %v, want busy %s and never locked", got, c.contender)
			}
			if _, _, submissions := x.ledger.totals(lockFixtureRequest); submissions != 0 {
				t.Errorf("%d submission(s) while the lock was held elsewhere", submissions)
			}
			if exit := holder.release(t); exit != "EXIT 0" {
				t.Errorf("holder %s\n%s", exit, holder.out.String())
			}
		})
	}
}

// ── the overlap, made impossible ────────────────────────────────────────

type flushLabel struct{}

// TestAFlushPausedAfterReadingIntakeMakesASandboxedFlushBusy is the scenario
// that proved generation overlap damages records: one flush has read intake
// and not yet spooled; the AS's target moves on; a sandboxed flush starts. It
// is busy, so the record is spooled and delivered once, under one epoch.
func TestAFlushPausedAfterReadingIntakeMakesASandboxedFlushBusy(t *testing.T) {
	x := newFlushLockFixture(t)
	if _, err := ensureFlushLockFile(x.lock); err != nil {
		t.Fatal(err)
	}
	sandboxed := sandboxEmulationUnavailable() == ""

	reached, release := make(chan struct{}), make(chan struct{})
	var mu sync.Mutex
	var otherEvents []string
	flushTestHook = func(ctx context.Context, event string) {
		if ctx.Value(flushLabel{}) == "first" {
			if event == "intake read" {
				close(reached)
				<-release
			}
			return
		}
		mu.Lock()
		otherEvents = append(otherEvents, event)
		mu.Unlock()
	}
	t.Cleanup(func() { flushTestHook = nil })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var firstOut bytes.Buffer
	firstDone := make(chan int)
	go func() {
		_, code := runFlush(context.WithValue(ctx, flushLabel{}, "first"), x.cfg, x.cfgPath, true, &firstOut, &firstOut)
		firstDone <- code
	}()
	select {
	case <-reached:
	case <-ctx.Done():
		t.Fatal("the first flush never read intake")
	}

	x.as.set(func(s *asState) { s.epoch = 11 })
	var restore func()
	if sandboxed {
		restore = denyWritesKeepReads(t, x.root, x.lock)
		if err := proveWriteDeniedMinerRoot(x.root, "", x.state); err != nil {
			t.Fatalf("the fixture does not emulate the sandbox: %v", err)
		}
	}
	var otherOut bytes.Buffer
	rep, code := runFlush(ctx, x.cfg, x.cfgPath, true, &otherOut, &otherOut)
	if restore != nil {
		restore()
	}
	close(release)
	if first := <-firstDone; first != exitOK {
		t.Fatalf("the first flush exit %d\n%s", first, firstOut.String())
	}

	if code != exitOK || rep.Promoted != 0 || rep.Delivered != 0 || !strings.Contains(otherOut.String(), "another flush is running") {
		t.Fatalf("the second flush was not busy: exit %d, promoted %d, delivered %d\n%s", code, rep.Promoted, rep.Delivered, otherOut.String())
	}
	mu.Lock()
	wantBusy := "busy read-write"
	if sandboxed {
		wantBusy = "busy read-only"
	}
	if !contains(otherEvents, wantBusy) {
		t.Errorf("second flush events %v, want %s", otherEvents, wantBusy)
	}
	mu.Unlock()

	if _, later, out := runFlushT(t, x.cfg, x.cfgPath, true); later != exitOK {
		t.Fatalf("a later flush exit %d\n%s", later, out)
	}
	credited, conflicts, _ := x.ledger.totals(lockFixtureRequest)
	sp, err := spool.OpenExisting(x.cfg.Mining.SpoolDir)
	if err != nil {
		t.Fatal(err)
	}
	pending, _ := sp.Count()
	quarantined, _ := sp.CountQuarantined()
	if credited != 1 || conflicts != 0 || quarantined != 0 || pending != 0 {
		t.Errorf("credited %d, conflicts %d, quarantined %d, pending %d; want 1, 0, 0, 0\nfirst: %s", credited, conflicts, quarantined, pending, firstOut.String())
	}
	if n := x.intakeCount(t); n != 0 {
		t.Errorf("%d record(s) left in intake", n)
	}
	if rec, ok := x.flushHealth(t); ok {
		t.Errorf("flush health left as %s %q", rec.Reason, rec.Detail)
	}
}

// ── the lock file itself ────────────────────────────────────────────────

// TestAbsentUncreatableFlushLockStopsTheFlush: a read-only open cannot create
// the lock, so a sandboxed flush with no lock file does not run. It cannot
// hide an overlap: every flush that can write creates the file first.
func TestAbsentUncreatableFlushLockStopsTheFlush(t *testing.T) {
	requireSandboxEmulation(t)
	x := newFlushLockFixture(t)
	if lexists(x.lock) {
		t.Fatal("fixture: the lock must start absent")
	}
	denyWritesKeepReads(t, x.root)
	if err := proveWriteDeniedMinerRoot(x.root, "", x.state); err != nil {
		t.Fatalf("the fixture does not emulate the sandbox: %v", err)
	}

	events := recordFlushEvents(t)
	_, code, out := runFlushT(t, x.cfg, x.cfgPath, true)
	if code == exitOK {
		t.Fatalf("a flush with no lock file it could create exited 0\n%s", out)
	}
	for _, e := range events() {
		if strings.HasPrefix(e, "locked ") {
			t.Fatalf("the flush ran without a lock: events %v", events())
		}
	}
	if _, _, submissions := x.ledger.totals(lockFixtureRequest); submissions != 0 || x.intakeCount(t) != 1 {
		t.Errorf("submissions %d, intake %d; want 0 and 1", submissions, x.intakeCount(t))
	}
	rec, ok := x.flushHealth(t)
	if !ok || rec.Reason != auth.HealthFlushStateUnavailable || !strings.HasPrefix(rec.Detail, flushLockHealthPrefix) {
		t.Errorf("flush health %v %s %q, want %s naming the lock", ok, rec.Reason, rec.Detail, auth.HealthFlushStateUnavailable)
	}
}

func TestAbsentCreatableFlushLockIsCreatedAndTheFlushRuns(t *testing.T) {
	x := newFlushLockFixture(t)
	if lexists(x.lock) {
		t.Fatal("fixture: the lock must start absent")
	}
	events := recordFlushEvents(t)
	rep, code, out := runFlushT(t, x.cfg, x.cfgPath, true)
	if code != exitOK || rep.Delivered != 1 || !contains(events(), "locked read-write") {
		t.Fatalf("exit %d, delivered %d, events %v\n%s", code, rep.Delivered, events(), out)
	}
	if !lexists(x.lock) {
		t.Error("the flush did not leave its lock file behind for a sandboxed flush")
	}
}

// TestANonPermissionLockOpenErrorIsNotAFallback: the read-only path is for a
// permission denial only. A directory where the lock should be refuses a
// read-write open for another reason and must stop the flush, even though a
// read-only open of a directory (and a flock on it) would succeed on POSIX.
func TestANonPermissionLockOpenErrorIsNotAFallback(t *testing.T) {
	x := newFlushLockFixture(t)
	if err := os.Mkdir(x.lock, 0o700); err != nil {
		t.Fatal(err)
	}
	events := recordFlushEvents(t)
	_, code, out := runFlushT(t, x.cfg, x.cfgPath, true)
	if code == exitOK {
		t.Fatalf("exit 0 with a directory for a lock\n%s", out)
	}
	for _, e := range events() {
		if strings.HasPrefix(e, "locked ") {
			t.Fatalf("the flush took a lock on a directory: events %v", events())
		}
	}
	if _, _, submissions := x.ledger.totals(lockFixtureRequest); submissions != 0 {
		t.Errorf("%d submission(s)", submissions)
	}
	if rec, ok := x.flushHealth(t); !ok || rec.Reason != auth.HealthFlushStateUnavailable {
		t.Errorf("flush health %v %s, want %s", ok, rec.Reason, auth.HealthFlushStateUnavailable)
	}
}

func TestEnsureFlushLockFileCreatesOnlyWhatIsAbsent(t *testing.T) {
	dir := t.TempDir()
	lock := filepath.Join(dir, "flush.lock")
	if created, err := ensureFlushLockFile(lock); err != nil || !created || !lexists(lock) {
		t.Fatalf("absent: created %v, err %v, exists %v", created, err, lexists(lock))
	}
	if err := os.WriteFile(lock, []byte("kept"), 0o600); err != nil {
		t.Fatal(err)
	}
	if created, err := ensureFlushLockFile(lock); err != nil || created {
		t.Fatalf("present: created %v, err %v", created, err)
	}
	if data, _ := os.ReadFile(lock); string(data) != "kept" { // #nosec G304 -- the test's own t.TempDir()
		t.Error("an existing lock file was rewritten")
	}
	if created, err := ensureFlushLockFile(filepath.Join(dir, "missing", "flush.lock")); err != nil || created {
		t.Errorf("no directory: created %v, err %v; want nothing done", created, err)
	}
}

func TestSetupCreatesTheFlushLockAndItsDryRunSaysSo(t *testing.T) {
	s := newSetupSandbox(t)
	lock := filepath.Join(s.home, "flush.lock")
	code, out, errOut := s.run(nil, false, "-yes", "-no-agents", "-dry-run")
	if code != exitOK || !strings.Contains(out, "(dry run) would create "+lock) || lexists(lock) {
		t.Fatalf("dry run exit %d, exists %v\n%s\n%s", code, lexists(lock), out, errOut)
	}
	s.platform.claim("credits")
	if code, out, errOut := s.run(nil, false, "-yes", "-no-agents"); code != exitOK {
		t.Fatalf("setup exit %d\n%s\n%s", code, out, errOut)
	}
	if !lexists(lock) {
		t.Fatal("setup did not create the flush lock a sandboxed flush needs")
	}
}

func TestUpgradeAndRollbackCreateTheFlushLock(t *testing.T) {
	for _, args := range [][]string{nil, {"-rollback"}} {
		t.Run(fmt.Sprintf("upgrade %v", args), func(t *testing.T) {
			f := newUpgradeFixture(t, newLocalRelease(t, "0.3.1", map[string]string{"0.3.1": "0.3.1\n"}))
			f.write(selfupdate.PreviousPath(f.exe), "0.2.9")
			writeFileT(t, filepath.Join(f.home, setupConfigFile),
				"[miner]\nintake_dir = \""+filepath.ToSlash(filepath.Join(f.home, "intake"))+"\"\n")
			lock := filepath.Join(f.home, "flush.lock")
			if code, out, errOut := f.run(args...); code != exitOK {
				t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
			}
			if !lexists(lock) {
				t.Error("the upgrade transaction did not create the flush lock")
			}
		})
	}
}

func TestPurgeRemovesBothFlushStampsAndTheLock(t *testing.T) {
	s := installed(t)
	paths := []string{
		filepath.Join(s.home, "flush.json"),
		filepath.Join(s.home, "state", "flush.json"),
		filepath.Join(s.home, "flush.lock"),
	}
	for _, p := range paths[:2] {
		writeFileT(t, p, `{"v":1}`)
	}
	if !lexists(paths[2]) {
		t.Fatal("fixture: setup should have created the flush lock")
	}
	code, out, errOut := s.uninstall(t, tty(walletFixtureAddress(t)), true, &revokeRecorder{}, "-purge-state")
	if code != exitOK {
		t.Fatalf("purge exited %d\n%s\n%s", code, out, errOut)
	}
	for _, p := range paths {
		if lexists(p) {
			t.Errorf("purge left %s", p)
		}
	}
}

func TestConfiguredStatePathsNameBothFlushStamps(t *testing.T) {
	root := t.TempDir()
	cfg := &config.Config{
		Mining: config.Mining{StateDir: filepath.Join(root, "state")},
		Miner:  config.Miner{IntakeDir: filepath.Join(root, "intake")},
	}
	named := map[string]bool{}
	for _, c := range configuredStatePaths(cfg) {
		named[c.path] = true
	}
	for _, want := range []string{flushStampPath(cfg.Mining), legacyFlushStampPath(cfg.Miner), flushLockPath(cfg.Miner)} {
		if !named[want] {
			t.Errorf("configured state paths omit %s", want)
		}
	}
}

// ── the stamp ───────────────────────────────────────────────────────────

func TestLegacyStampIsReadOnceAndOnlyTheNewOneIsWritten(t *testing.T) {
	x := newFlushLockFixture(t)
	x.cfg.Miner.FlushInterval = time.Hour
	writeStampT(t, x.legacy, flushStamp{SlotID: testSlotID, TargetEpoch: 10, LastAS: time.Now()})
	legacyBefore, err := os.ReadFile(x.legacy)
	if err != nil {
		t.Fatal(err)
	}

	rep, code, out := runFlushT(t, x.cfg, x.cfgPath, false)
	if code != exitOK || rep.AskedAS || rep.Epoch != 10 || x.as.currentCalls.Load() != 0 {
		t.Fatalf("exit %d, asked %v, epoch %d, target calls %d: the fresh legacy stamp was not the starting value\n%s",
			code, rep.AskedAS, rep.Epoch, x.as.currentCalls.Load(), out)
	}
	if st := readFlushStamp(x.stamp); st.TargetEpoch != 10 || st.LastFlush.IsZero() {
		t.Fatalf("new stamp %+v", st)
	}
	if after, _ := os.ReadFile(x.legacy); !bytes.Equal(after, legacyBefore) {
		t.Error("the flush wrote the legacy stamp")
	}

	// Once the new stamp exists the legacy one is no longer read.
	writeStampT(t, x.legacy, flushStamp{SlotID: testSlotID, TargetEpoch: 99, LastAS: time.Now()})
	seedIntakeRecord(t, x.cfg.Miner.IntakeDir, "req-flush-lock-2")
	rep, code, out = runFlushT(t, x.cfg, x.cfgPath, false)
	if code != exitOK || rep.Epoch != 10 {
		t.Fatalf("exit %d, epoch %d; the legacy stamp was read again\n%s", code, rep.Epoch, out)
	}
}

// blockStamp makes the new stamp unwritable by putting a directory where it
// goes: the temp file is written, the rename over it fails.
func blockStamp(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(path, "occupied"), 0o700); err != nil {
		t.Fatal(err)
	}
}

func TestAStampThatCannotBeWrittenDoesNotStopDelivery(t *testing.T) {
	x := newFlushLockFixture(t)
	blockStamp(t, x.stamp)

	rep, code, out := runFlushT(t, x.cfg, x.cfgPath, true)
	if code != exitOK || rep.Delivered != 1 || x.intakeCount(t) != 0 {
		t.Fatalf("exit %d, delivered %d, intake %d; a stamp failure must not stop delivery\n%s", code, rep.Delivered, x.intakeCount(t), out)
	}
	rec, ok := x.flushHealth(t)
	if !ok || rec.Reason != auth.HealthFlushStateUnavailable || !strings.HasPrefix(rec.Detail, flushStampHealthPrefix) {
		t.Fatalf("flush health %v %s %q, want the stamp failure kept past the delivery", ok, rec.Reason, rec.Detail)
	}

	if err := os.RemoveAll(x.stamp); err != nil {
		t.Fatal(err)
	}
	if _, code, out := runFlushT(t, x.cfg, x.cfgPath, true); code != exitOK {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if rec, ok := x.flushHealth(t); ok {
		t.Errorf("a stamp written again left health %s %q", rec.Reason, rec.Detail)
	}
}

func TestTheNoTargetStampWriteGoesThroughTheCheckedHelper(t *testing.T) {
	x := newFlushLockFixture(t)
	x.as.set(func(s *asState) { s.epoch = -1 })
	blockStamp(t, x.stamp)

	if _, code, out := runFlushT(t, x.cfg, x.cfgPath, true); code != exitOK {
		t.Fatalf("exit %d\n%s", code, out)
	}
	rec, ok := x.flushHealth(t)
	if !ok || rec.Reason != auth.HealthFlushStateUnavailable || !strings.HasPrefix(rec.Detail, flushStampHealthPrefix) {
		t.Fatalf("flush health %v %s %q, want the no-target stamp failure recorded", ok, rec.Reason, rec.Detail)
	}
}

func TestALockTakenAgainClearsOnlyALockFailure(t *testing.T) {
	x := newFlushLockFixture(t)
	store, err := auth.OpenStoreExisting(x.state)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkHealth(auth.HealthFlush, auth.HealthFlushStateUnavailable, flushLockHealthPrefix+"earlier"); err != nil {
		t.Fatal(err)
	}
	x.as.set(func(s *asState) { s.epoch = -1 })
	if _, code, out := runFlushT(t, x.cfg, x.cfgPath, true); code != exitOK {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if rec, ok := x.flushHealth(t); ok {
		t.Errorf("a flush that took its lock left %s %q", rec.Reason, rec.Detail)
	}

	if err := store.MarkHealth(auth.HealthFlush, auth.HealthSubmissionFailed, "unrelated"); err != nil {
		t.Fatal(err)
	}
	if _, code, out := runFlushT(t, x.cfg, x.cfgPath, true); code != exitOK {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if rec, ok := x.flushHealth(t); !ok || rec.Reason != auth.HealthSubmissionFailed {
		t.Errorf("taking the lock cleared an unrelated record: %v %s", ok, rec.Reason)
	}
}

func TestDoctorReadsTheLegacyStampOnlyWhileTheNewOneIsAbsent(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	intakeDir := filepath.Join(root, "miner", "intake")
	for _, d := range []string{stateDir, intakeDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	store, err := auth.OpenStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveMiningEnabled(true); err != nil {
		t.Fatal(err)
	}
	m := config.Mining{StateDir: stateDir, SpoolDir: filepath.Join(root, "spool")}
	mn := config.Miner{Enabled: true, IntakeDir: intakeDir}
	gather := func() doctorFacts {
		return gatherDoctorFactsFor(t.Context(), &stubAS{docErr: errASDown}, m, mn, realIntakeProbeOps())
	}

	legacyAt := time.Now().Add(-2 * time.Hour).UTC().Truncate(time.Second)
	writeStampT(t, legacyFlushStampPath(mn), flushStamp{LastFlush: legacyAt})
	if f := gather(); !f.StampPresent || f.StampErr != nil || !f.Stamp.LastFlush.Equal(legacyAt) {
		t.Fatalf("legacy only: present %v err %v at %s, want %s", f.StampPresent, f.StampErr, f.Stamp.LastFlush, legacyAt)
	}
	newAt := legacyAt.Add(time.Hour)
	writeStampT(t, flushStampPath(m), flushStamp{LastFlush: newAt})
	if f := gather(); !f.StampPresent || !f.Stamp.LastFlush.Equal(newAt) {
		t.Fatalf("both: present %v at %s, want the new stamp %s", f.StampPresent, f.Stamp.LastFlush, newAt)
	}
	if err := os.WriteFile(flushStampPath(m), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if f := gather(); f.StampErr == nil {
		t.Fatalf("an unreadable new stamp was replaced by the legacy one: present %v at %s", f.StampPresent, f.Stamp.LastFlush)
	}
}

func TestFlushStateUnavailableRendersInStatusAndDoctor(t *testing.T) {
	cfgPath, root, store := statusFixture(t)
	if err := store.SaveMiningEnabled(true); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkHealth(auth.HealthFlush, auth.HealthFlushStateUnavailable, flushLockHealthPrefix+"permission denied"); err != nil {
		t.Fatalf("the store refused the reason: %v", err)
	}
	reason := string(auth.HealthFlushStateUnavailable)

	var text, textErr bytes.Buffer
	if code := statusMain([]string{"-config", cfgPath}, &text, &textErr, noEnv); code != exitOK || !strings.Contains(text.String(), reason) {
		t.Errorf("status exit %d does not name %s:\n%s%s", code, reason, text.String(), textErr.String())
	}
	var jsonOut, jsonErr bytes.Buffer
	if code := statusMain([]string{"-config", cfgPath, "-json"}, &jsonOut, &jsonErr, noEnv); code != exitOK || !strings.Contains(jsonOut.String(), `"`+reason+`"`) {
		t.Errorf("status -json exit %d does not carry %s:\n%s", code, reason, jsonOut.String())
	}

	f := gatherDoctorFactsFor(t.Context(), &stubAS{docErr: errASDown},
		config.Mining{StateDir: filepath.Join(root, "state"), SpoolDir: filepath.Join(root, "spool")},
		config.Miner{}, realIntakeProbeOps())
	checks := assembleDoctor(f)
	var doc bytes.Buffer
	printDoctor(&doc, checks, f)
	if !strings.Contains(doc.String(), reason) {
		t.Errorf("doctor does not name %s:\n%s", reason, doc.String())
	}
	var docJSON bytes.Buffer
	emitMachine(&docJSON, doctorEnvelope(f, checks, doctorExit(checks)))
	if !strings.Contains(docJSON.String(), `"`+reason+`"`) {
		t.Errorf("doctor -json does not carry %s:\n%s", reason, docJSON.String())
	}
}
