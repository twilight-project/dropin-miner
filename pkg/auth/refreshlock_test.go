package auth

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// rotatingAS is the AS half of the concurrency test: it issues a fresh
// successor on every refresh and remembers which tokens it has already
// redeemed, because a second presentation of a redeemed token is
// precisely what a STRICT AS answers with family revocation.
type rotatingAS struct {
	// delay is served after the successor has been issued, widening the
	// client's load-to-persist window. It makes the race the lock exists
	// to prevent reliably reachable instead of vanishingly rare — the
	// test is stricter with it, not more forgiving.
	delay time.Duration

	mu        sync.Mutex
	spent     map[string]bool
	issued    int
	redeemed  int
	reuse     int
	malformed int
	// expiresIn is the access-token lifetime this AS advertises. Zero
	// means it declines to say, which the client treats as uncacheable —
	// the setting that isolates the LOCK's behavior from the cache's.
	expiresIn int
}

func (a *rotatingAS) handle(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		a.mu.Lock()
		a.malformed++
		a.mu.Unlock()
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	presented := r.PostFormValue("refresh_token")

	a.mu.Lock()
	if a.spent[presented] {
		a.reuse++
	}
	a.spent[presented] = true
	a.redeemed++
	a.issued++
	successor := fmt.Sprintf("rt-%d", a.issued)
	a.mu.Unlock()

	time.Sleep(a.delay)
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(tokenJSONExpiring("at-"+successor, successor, a.expiresIn)))
}

// counts is a consistent snapshot for assertions.
func (a *rotatingAS) counts() (redeemed, reuse, issued, malformed int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.redeemed, a.reuse, a.issued, a.malformed
}

// Concurrent Refresh callers must never present the same refresh token
// twice: the AS treats the second presentation as reuse, and STRICT
// policy revokes the whole family, which kills the installation.
//
// These callers are goroutines, so this leg proves the lock serializes
// within one process; TestRefreshLockCrossesProcesses proves the half
// that actually matters in production, where the contenders are the
// mining daemon and a CLI command. One mechanism covers both because a
// flock belongs to the open file description, not to the process.
func TestRefreshNeverSpendsATokenTwice(t *testing.T) {
	const callers = 8

	// expiresIn: 0 — this AS declines to say how long its access tokens
	// last, so the client never caches one and every caller reaches the
	// token endpoint. That is what keeps this test about the LOCK: with a
	// cacheable token, seven of eight callers would be served from memory
	// and the serialization property would go unexercised while the test
	// still passed.
	as := &rotatingAS{spent: map[string]bool{}, delay: 5 * time.Millisecond, expiresIn: 0}
	f := newFakeAS(t)
	f.token = as.handle
	c := newClient(t, f)
	if err := c.store.SaveRefreshToken("rt-0"); err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := c.Refresh(context.Background())
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent refresh failed: %v", err)
		}
	}

	redeemed, reuse, issued, malformed := as.counts()
	if reuse != 0 {
		t.Errorf("the AS saw %d reuse presentations; STRICT policy would have revoked the family", reuse)
	}
	// A run in which nothing reached the AS, or in which only one caller
	// did, proves nothing about serialization however green it looks.
	// This guard is the one that catches a degenerate caller count; the
	// next catches a caller that never got there.
	if redeemed <= 1 {
		t.Fatalf("the AS redeemed %d refresh tokens; nothing was concurrent", redeemed)
	}
	if redeemed != callers {
		t.Fatalf("the AS redeemed %d refresh tokens, want one per caller (%d)", redeemed, callers)
	}
	if malformed != 0 {
		t.Errorf("%d token requests were unparseable", malformed)
	}
	// The last successor issued is the one on disk: a caller that
	// persisted while holding a stale token would have written back an
	// earlier one.
	stored, ok, err := c.store.LoadRefreshToken()
	if err != nil || !ok {
		t.Fatalf("no refresh token after %d refreshes: ok=%v err=%v", redeemed, ok, err)
	}
	if want := fmt.Sprintf("rt-%d", issued); stored != want {
		t.Errorf("stored refresh token = %q, want the last successor issued (%q)", stored, want)
	}
}

// refreshLockChildEnv carries the state directory to the child half of
// the cross-process test. Its presence is also what tells the child test
// it is the child.
const refreshLockChildEnv = "TOKENDROP_TEST_REFRESH_LOCK_DIR"

// TestRefreshLockChildProcess is the second process in
// TestRefreshLockCrossesProcesses. Re-entering the test binary is the
// cheapest way to get one, so this is a test function that skips itself
// unless the parent asked for it by name.
func TestRefreshLockChildProcess(t *testing.T) {
	root := os.Getenv(refreshLockChildEnv)
	if root == "" {
		t.Skip("child half of TestRefreshLockCrossesProcesses; not run on its own")
	}
	s, err := OpenStore(filepath.Join(root, "state"))
	if err != nil {
		t.Fatal(err)
	}
	// Announced before the attempt, so the parent can tell "still
	// starting up" apart from "blocked on the lock".
	if err := os.WriteFile(filepath.Join(root, "child.started"), []byte("started"), 0o600); err != nil { // #nosec G703 -- test-owned scratch root
		t.Fatal(err)
	}
	release, err := s.lockRefreshTokenFor(context.Background(), 30*time.Second)
	if err != nil {
		t.Fatalf("child never acquired the lock: %v", err)
	}
	defer release()
	if err := os.WriteFile(filepath.Join(root, "child.acquired"), []byte("acquired"), 0o600); err != nil { // #nosec G703 -- test-owned scratch root
		t.Fatal(err)
	}
}

// The lock is a cross-process one or it is nothing: the two contenders
// in production are separate processes. A second process must block
// while this one holds the lock, and must then acquire once it is
// released.
func TestRefreshLockCrossesProcesses(t *testing.T) {
	root := t.TempDir()
	s, err := OpenStore(filepath.Join(root, "state"))
	if err != nil {
		t.Fatal(err)
	}
	release, err := s.lockRefreshTokenFor(context.Background(), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	releaseOnce := func() { once.Do(release) }
	t.Cleanup(releaseOnce)

	// The child writes to a file rather than to a buffer: os/exec would
	// otherwise copy into the buffer from its own goroutine while the
	// assertions below read it, which is a data race the moment a
	// diagnostic needs the output.
	logPath := filepath.Join(root, "child.log")
	childLog, err := os.OpenFile(logPath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600) // #nosec G304 -- test-owned temp path
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = childLog.Close() })

	cmd := exec.Command(os.Args[0], "-test.run=^TestRefreshLockChildProcess$", "-test.timeout=2m") // #nosec G204 G702 -- re-runs this test binary
	cmd.Env = append(os.Environ(), refreshLockChildEnv+"="+root)
	cmd.Stdout, cmd.Stderr = childLog, childLog
	if err := cmd.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	acquired := filepath.Join(root, "child.acquired")
	waitForFile(t, filepath.Join(root, "child.started"), done, logPath)
	// The child is past process startup and inside its acquire loop.
	// Anything it does now it does against a held lock.
	time.Sleep(200 * time.Millisecond)
	if _, err := os.Stat(acquired); err == nil {
		t.Fatalf("a second process took the lock while this one held it%s", childOutput(logPath))
	}

	releaseOnce()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("child exited %v%s", err, childOutput(logPath))
		}
	case <-time.After(45 * time.Second):
		t.Fatalf("child never finished after the lock was released%s", childOutput(logPath))
	}
	if _, err := os.Stat(acquired); err != nil {
		t.Fatalf("child finished without acquiring the lock (%v)%s", err, childOutput(logPath))
	}
}

// waitForFile polls for path, failing if the child dies first — a dead
// child would otherwise turn into a bare timeout with no explanation.
func waitForFile(t *testing.T, path string, done <-chan error, logPath string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		select {
		case err := <-done:
			t.Fatalf("child exited before reaching the lock (%v)%s", err, childOutput(logPath))
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("child never wrote %s%s", filepath.Base(path), childOutput(logPath))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// childOutput renders whatever the child has written so far, for a
// failure message.
func childOutput(logPath string) string {
	raw, err := os.ReadFile(logPath) // #nosec G304 -- test-owned temp path
	if err != nil {
		return fmt.Sprintf("\n(child output unreadable: %v)", err)
	}
	return "\nchild output:\n" + string(raw)
}

// A holder stuck on an unresponsive AS must not wedge everyone else
// forever: the wait is bounded and expiry names the situation.
func TestRefreshLockGivesUpRatherThanHanging(t *testing.T) {
	s, dir := newStore(t)
	release, err := s.lockRefreshTokenFor(context.Background(), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	// The lock is on a sibling file. Locking refresh.token itself would
	// follow the inode that SaveRefreshToken renames away.
	if _, err := os.Stat(filepath.Join(dir, refreshLockFile)); err != nil {
		t.Fatalf("lockfile %s absent after acquiring: %v", refreshLockFile, err)
	}
	if _, err := os.Stat(filepath.Join(dir, refreshTokenFile)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("acquiring the lock touched %s (%v); it must lock a sibling", refreshTokenFile, err)
	}

	const bound = 50 * time.Millisecond
	started := time.Now()
	second, err := s.lockRefreshTokenFor(context.Background(), bound)
	elapsed := time.Since(started)
	if err == nil {
		second()
		t.Fatal("a second acquisition succeeded while the lock was held")
	}
	if !errors.Is(err, ErrRefreshLockBusy) {
		t.Fatalf("expiry returned %v, want an error wrapping ErrRefreshLockBusy", err)
	}
	if elapsed < bound {
		t.Errorf("gave up after %s, before the %s it was given", elapsed, bound)
	}
	// The deadline governs the wait, not the poll schedule. That the
	// call returned at all is the proof it does not hang.
	if elapsed > 2*time.Second {
		t.Errorf("waited %s for a %s bound", elapsed, bound)
	}

	// A canceled caller stops waiting immediately rather than sitting
	// out a bound it no longer cares about.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	third, err := s.lockRefreshTokenFor(ctx, time.Minute)
	if err == nil {
		third()
		t.Fatal("a canceled caller acquired the held lock")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("canceled wait returned %v, want context.Canceled", err)
	}
}

// Release must work whether or not the refresh succeeded: a failed
// refresh that kept the lock would wedge the next caller until the
// timeout, every time.
func TestRefreshReleasesLockAfterFailure(t *testing.T) {
	f := newFakeAS(t)
	f.token = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
	}
	c := newClient(t, f)
	if err := c.store.SaveRefreshToken("rt-doomed"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Refresh(context.Background()); err == nil {
		t.Fatal("invalid_grant did not fail the refresh")
	}
	release, err := c.store.lockRefreshTokenFor(context.Background(), 250*time.Millisecond)
	if err != nil {
		t.Fatalf("lock still held after a failed refresh: %v", err)
	}
	release()
}

// The paths that establish a FIRST refresh authorization take the lock too.
//
// They are not the read-modify-write Refresh is, so they cannot cause reuse.
// What they can do is race a daemon tick that is rotating: both write
// atomically, one wins, and if enrollment loses, the participant has been told
// they are enrolled while the file holds a token from the family they were
// replacing. If that family is dead — usually why somebody re-enrolls — they
// hold a dead token and were shown no error.
//
// Asserted by holding the lock and showing each path blocks rather than
// writing through it. Enumerated rather than sampled, so a fourth enrollment
// path has to be added here to pass.
func TestEnrollmentPathsPersistUnderTheLock(t *testing.T) {
	for _, tc := range []struct {
		name string
		call func(context.Context, *OAuthClient) error
	}{
		{"assertion grant", func(ctx context.Context, c *OAuthClient) error {
			_, err := c.RedeemEnrollmentAssertion(ctx, "the.assertion.jwt")
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			as := newAssertAS(t)
			as.token = func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(boundTokenJSON("rt-enrolled")))
			}
			oc, store := as.client(t)

			// Somebody else holds the lock for longer than the caller will
			// wait, so a path that takes it must give up rather than write.
			release, err := store.lockRefreshTokenFor(context.Background(), time.Second)
			if err != nil {
				t.Fatalf("seed the lock: %v", err)
			}
			defer release()

			ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
			defer cancel()
			if err := tc.call(ctx, oc); err == nil {
				t.Fatal("the path wrote a refresh authorization while the lock was held elsewhere")
			}
			if _, ok, _ := store.LoadRefreshToken(); ok {
				t.Fatal("a refused enrollment stored a refresh authorization anyway")
			}
		})
	}
}

// The access-token cache: concurrent callers spend ONE rotation, not eight.
//
// This is the whole point of the cache, and the number it moves is the one
// that matters. The AS rotates on every refresh under a STRICT no-grace
// policy, so each rotation is an opportunity to lose the successor to a
// dropped response and kill the credential permanently. The Slot 3 family
// that died had 917 rotations in a day, at one a minute, to mint tokens the
// AS says are good for fifteen.
func TestConcurrentRefreshSpendsOneRotation(t *testing.T) {
	const callers = 8

	as := &rotatingAS{spent: map[string]bool{}, delay: 5 * time.Millisecond, expiresIn: 900}
	f := newFakeAS(t)
	f.token = as.handle
	c := newClient(t, f)
	if err := c.store.SaveRefreshToken("rt-0"); err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	tokens := make([]string, callers)
	errs := make([]error, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			tok, err := c.Refresh(context.Background())
			errs[i] = err
			if tok != nil {
				tokens[i] = tok.AccessToken
			}
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("caller %d: %v", i, err)
		}
	}
	redeemed, reuse, _, _ := as.counts()
	if reuse != 0 {
		t.Errorf("the AS saw %d reuse presentations", reuse)
	}
	// One, not eight. The second check inside the lock is what makes this
	// 1 rather than 8: without it every caller that missed the cache and
	// then WAITED on the lock would rotate a token the winner had already
	// made valid.
	if redeemed != 1 {
		t.Fatalf("%d callers spent %d rotations, want 1 — the cache is not being consulted, or not under the lock", callers, redeemed)
	}
	for i, got := range tokens {
		if got != tokens[0] {
			t.Fatalf("caller %d got access token %q, caller 0 got %q; one rotation must serve them all", i, got, tokens[0])
		}
	}
}

// An AS that does not say when its access token expires is never cached.
//
// Zero is not "forever". Serving a token of unknown lifetime out of memory
// would keep offering it after the AS had stopped honoring it, and the
// client cannot tell the difference until something fails.
func TestAccessTokenWithNoStatedExpiryIsNotCached(t *testing.T) {
	as := &rotatingAS{spent: map[string]bool{}, expiresIn: 0}
	f := newFakeAS(t)
	f.token = as.handle
	c := newClient(t, f)
	if err := c.store.SaveRefreshToken("rt-0"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := c.Refresh(context.Background()); err != nil {
			t.Fatalf("refresh %d: %v", i, err)
		}
	}
	if redeemed, _, _, _ := as.counts(); redeemed != 3 {
		t.Fatalf("%d rotations for 3 refreshes of an unexpiring-unknown token, want 3", redeemed)
	}
}

// A token expiring inside the margin is not served from the cache.
//
// The margin exists so a token cannot be handed out valid and arrive
// expired. A test that only used long lifetimes would never touch it.
func TestAccessTokenInsideTheExpiryMarginIsNotServed(t *testing.T) {
	as := &rotatingAS{spent: map[string]bool{}, expiresIn: 20} // < accessTokenMargin
	f := newFakeAS(t)
	f.token = as.handle
	c := newClient(t, f)
	if err := c.store.SaveRefreshToken("rt-0"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := c.Refresh(context.Background()); err != nil {
			t.Fatalf("refresh %d: %v", i, err)
		}
	}
	if redeemed, _, _, _ := as.counts(); redeemed != 3 {
		t.Fatalf("%d rotations, want 3 — a token expiring within %s of now was served from the cache", redeemed, accessTokenMargin)
	}
}

// A 401 drops the cached token, so the retry carries a different one.
//
// Before the cache every mining-plane call rotated, which is why the Slot 3
// run's 401 "missing scope mining:join" cleared on an immediate retry. A
// cache that kept re-offering the rejected token would convert that
// transient into an outage lasting the token's whole nominal lifetime.
func TestRejectedAccessTokenIsDroppedFromTheCache(t *testing.T) {
	as := &rotatingAS{spent: map[string]bool{}, expiresIn: 900}
	f := newFakeAS(t)
	f.token = as.handle
	c := newClient(t, f)
	if err := c.store.SaveRefreshToken("rt-0"); err != nil {
		t.Fatal(err)
	}

	first, err := c.Refresh(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// Cached: a second call must not reach the AS.
	if _, err := c.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if redeemed, _, _, _ := as.counts(); redeemed != 1 {
		t.Fatalf("%d rotations before any rejection, want 1", redeemed)
	}

	c.InvalidateAccessToken()

	second, err := c.Refresh(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if redeemed, _, _, _ := as.counts(); redeemed != 2 {
		t.Fatalf("%d rotations after a rejection, want 2 — the rejected token was served again", redeemed)
	}
	if second.AccessToken == first.AccessToken {
		t.Fatalf("the retry carried the rejected access token %q", first.AccessToken)
	}
}
