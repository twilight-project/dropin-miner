package selfupdate

// A candidate that has not answered is not a bad candidate (#95).
//
// Every timeout here is a real one: the runner blocks until the context the
// package handed it expires, so the package's own detection is what decides.
// The budget is shortened through the tests-only seam so that costs
// milliseconds; CandidateTimeout itself stays frozen (checksum_test.go).

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

const retryTestBudget = 250 * time.Millisecond

// silentThen never answers its first `silent` calls — each blocks until its
// deadline — and hands every later call to then. calls counts all of them.
type silentThen struct {
	silent int32
	then   CommandRunner
	calls  atomic.Int32
}

func (r *silentThen) Run(ctx context.Context, path string, args, env []string) ([]byte, []byte, error) {
	if n := r.calls.Add(1); n <= r.silent {
		<-ctx.Done()
		return nil, nil, ctx.Err()
	}
	return r.then.Run(ctx, path, args, env)
}

func TestACandidateThatTimesOutOnceIsAskedOnceMoreAndAccepted(t *testing.T) {
	r := &silentThen{silent: 1, then: outputs("dropin-miner 0.3.0\n", "")}
	if err := validateCandidate(context.Background(), r, "/candidate", mustVersion(t, "0.3.0"), retryTestBudget); err != nil {
		t.Fatalf("a candidate that answered on the second attempt was refused: %v", err)
	}
	if got := r.calls.Load(); got != 2 {
		t.Errorf("%d version calls, want exactly 2", got)
	}
}

func TestTwoTimeoutsAreATimeoutAndThereIsNoThirdAttempt(t *testing.T) {
	// It WOULD answer on a third call. Bounded means it is never asked.
	r := &silentThen{silent: 2, then: outputs("dropin-miner 0.3.0\n", "")}
	err := validateCandidate(context.Background(), r, "/candidate", mustVersion(t, "0.3.0"), retryTestBudget)
	if !errors.Is(err, ErrCandidateTimeout) {
		t.Fatalf("two timeouts reported as %v, want ErrCandidateTimeout", err)
	}
	if got := r.calls.Load(); got != 2 {
		t.Errorf("%d version calls, want exactly 2: one retry, not a loop", got)
	}
}

// Evidence is refused on the first call. Each runner answers correctly on its
// SECOND call, so a retry would not merely waste time: it would accept a
// binary that had just shown itself to be wrong.
func TestAnythingTheCandidateActuallySaidIsRefusedWithOneCall(t *testing.T) {
	good := outputs("dropin-miner 0.3.0\n", "")
	for name, first := range map[string]CommandRunner{
		"the wrong version":       outputs("dropin-miner 0.2.9\n", ""),
		"a byte on stderr":        outputs("dropin-miner 0.3.0\n", "!"),
		"a malformed line":        outputs("dropin-miner 0.3.0", ""),
		"a non-canonical version": outputs("dropin-miner v0.3.0\n", ""),
		"nothing at all":          outputs("", ""),
		"a failure to start": runnerFunc(func(context.Context, string, []string, []string) ([]byte, []byte, error) {
			return nil, nil, errors.New("exec format error")
		}),
		// A runner that SAYS deadline exceeded while the budget has not
		// passed is a failed command, not a timeout: the package believes
		// its own clock, not the error's text or type.
		"an error that only claims to be a deadline": runnerFunc(func(context.Context, string, []string, []string) ([]byte, []byte, error) {
			return nil, nil, context.DeadlineExceeded
		}),
	} {
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int32
			r := runnerFunc(func(ctx context.Context, path string, args, env []string) ([]byte, []byte, error) {
				if calls.Add(1) == 1 {
					return first.Run(ctx, path, args, env)
				}
				return good.Run(ctx, path, args, env)
			})
			err := validateCandidate(context.Background(), r, "/candidate", mustVersion(t, "0.3.0"), time.Hour)
			if err == nil {
				t.Fatal("accepted: the candidate was asked again and its second answer was believed")
			}
			if errors.Is(err, ErrCandidateTimeout) {
				t.Errorf("evidence was reported as a timeout: %v", err)
			}
			if got := calls.Load(); got != 1 {
				t.Errorf("%d version calls, want exactly 1", got)
			}
		})
	}
}

// When the operation's own deadline has passed there is no second attempt:
// it could not run, and the failure is the operation's, not a slow start.
func TestNoSecondAttemptOnceTheOperationItselfIsOutOfTime(t *testing.T) {
	parent, cancel := context.WithTimeout(context.Background(), retryTestBudget)
	defer cancel()
	r := &silentThen{silent: 5, then: outputs("dropin-miner 0.3.0\n", "")}
	if err := validateCandidate(parent, r, "/candidate", mustVersion(t, "0.3.0"), time.Hour); err == nil {
		t.Fatal("accepted a candidate that never answered")
	}
	if got := r.calls.Load(); got != 1 {
		t.Errorf("%d version calls under an expired operation, want 1", got)
	}
}

// Before anything is replaced: two timeouts are retryable, not an invalid
// release, the staged candidate is gone and the installed binary untouched.
func TestPrepareReportsAnUnansweringCandidateAsRetryableNotAsABadRelease(t *testing.T) {
	exe := installedBinary(t)
	r := &silentThen{silent: 2, then: markerRunner()}
	_, err := Updater{Source: release030(t, "0.3.0\n"), Runner: r, GOOS: "linux", GOARCH: "amd64", candidateBudget: retryTestBudget}.
		Prepare(context.Background(), exe, "0.2.8", nil)
	if kind := KindOf(err); kind != KindReplacementFailed || !kind.Retryable() {
		t.Fatalf("kind %q (retryable=%v) for %v; want a retryable kind, never %q or %q", kind, kind.Retryable(), err, KindCandidateInvalid, KindReleaseInvalid)
	}
	if got := r.calls.Load(); got != 2 {
		t.Errorf("%d version calls, want 2", got)
	}
	if names := dirNames(t, filepath.Dir(exe)); len(names) != 1 {
		t.Errorf("the staged candidate was left behind: %v", names)
	}

	// One timeout, then the right answer: prepared as if nothing happened.
	r = &silentThen{silent: 1, then: markerRunner()}
	p, err := Updater{Source: release030(t, "0.3.0\n"), Runner: r, GOOS: "linux", GOARCH: "amd64", candidateBudget: retryTestBudget}.
		Prepare(context.Background(), exe, "0.2.8", nil)
	if err != nil || p.To.String() != "0.3.0" || r.calls.Load() != 2 {
		t.Fatalf("prepared %+v, err %v, calls %d", p, err, r.calls.Load())
	}
	p.Discard()

	// A candidate that answers with the wrong version is still a bad
	// candidate, on one call.
	var calls atomic.Int32
	counting := runnerFunc(func(ctx context.Context, path string, args, env []string) ([]byte, []byte, error) {
		calls.Add(1)
		return markerRunner().Run(ctx, path, args, env)
	})
	_, err = Updater{Source: release030(t, "0.2.9\n"), Runner: counting, GOOS: "linux", GOARCH: "amd64", candidateBudget: retryTestBudget}.
		Prepare(context.Background(), exe, "0.2.8", nil)
	if KindOf(err) != KindCandidateInvalid || calls.Load() != 1 {
		t.Errorf("a wrong version: kind %q after %d calls, want %q after 1", KindOf(err), calls.Load(), KindCandidateInvalid)
	}
}

// After the swap, on both platforms' sequences: two timeouts put the old
// binary back, leave .previous alone and say retry; one timeout then an
// answer installs.
func TestAnInstalledBinaryThatDoesNotAnswerIsRolledBackAndRetryable(t *testing.T) {
	for name, windows := range strategies() {
		t.Run(name, func(t *testing.T) {
			x := newTxn(t, "0.2.8", "0.2.7", "0.3.0")
			r := &silentThen{silent: 2, then: markerRunner()}
			ops := x.ops("", nil)
			ops.validate = replaceOpsWithin(r, retryTestBudget).validate
			err := replaceWith(context.Background(), windows, x.exe, x.can, mustVersion(t, "0.3.0"), ops)
			if kind := KindOf(err); kind != KindReplacementFailed || !kind.Retryable() {
				t.Fatalf("kind %q for %v, want %q", kind, err, KindReplacementFailed)
			}
			if !errors.Is(err, ErrCandidateTimeout) {
				t.Errorf("the cause is no longer recognizable as a timeout: %v", err)
			}
			if x.version(x.exe) != "0.2.8" || x.version(x.prev) != "0.2.7" {
				t.Errorf("installed %s, previous %s; want the old binary back and .previous untouched", x.version(x.exe), x.version(x.prev))
			}
			if got := r.calls.Load(); got != 2 {
				t.Errorf("%d version calls, want 2", got)
			}

			x = newTxn(t, "0.2.8", "0.2.7", "0.3.0")
			r = &silentThen{silent: 1, then: markerRunner()}
			ops = x.ops("", nil)
			ops.validate = replaceOpsWithin(r, retryTestBudget).validate
			if err := replaceWith(context.Background(), windows, x.exe, x.can, mustVersion(t, "0.3.0"), ops); err != nil {
				t.Fatalf("a slow first answer failed the upgrade: %v", err)
			}
			if x.version(x.exe) != "0.3.0" || x.version(x.prev) != "0.2.8" || r.calls.Load() != 2 {
				t.Errorf("installed %s, previous %s, calls %d", x.version(x.exe), x.version(x.prev), r.calls.Load())
			}
		})
	}
}

// Rollback asks .previous' copy the same question, and an unanswering copy
// is not an invalid .previous.
func TestRollbackReportsAnUnansweringPreviousAsRetryable(t *testing.T) {
	x := newTxn(t, "0.3.1", "0.3.0", "")
	ops := x.ops("", nil)
	windows := false
	r := &silentThen{silent: 2, then: markerRunner()}
	_, err := Updater{Source: forbiddenSource{t}, Runner: r, ops: &ops, windows: &windows, candidateBudget: retryTestBudget}.
		Rollback(context.Background(), x.exe, "0.3.1")
	if KindOf(err) != KindReplacementFailed {
		t.Fatalf("kind %q for %v, want %q, not %q", KindOf(err), err, KindReplacementFailed, KindPreviousInvalid)
	}
	if x.version(x.exe) != "0.3.1" || x.version(x.prev) != "0.3.0" || len(x.names()) != 2 {
		t.Errorf("rollback changed something: %v", x.names())
	}
}
