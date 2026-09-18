package selfupdate

// The move-aside waits a moment for a passing holder, and for nothing else
// (#78). This file is the POLICY, driven on every OS through the sequence's
// own injectable operations: how many attempts, how long, for which errors,
// and what is left when they run out. Which real errors count as a passing
// holder is Windows' own answer and is proven on Windows, in
// rename_windows_test.go.

import (
	"context"
	"errors"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var errHeld = errors.New("injected: another process holds the file")

// asideProbe counts what the move-aside did.
type asideProbe struct {
	attempts int
	pauses   []time.Duration
}

func (p *asideProbe) waited() time.Duration {
	var total time.Duration
	for _, d := range p.pauses {
		total += d
	}
	return total
}

// heldOps are the fixture's real operations with the move-aside — the rename
// of the installed binary to its displaced name, and no other rename —
// failing with err for its first `failures` attempts. Only errHeld is
// transient, so any other injected error is one the sequence must not wait on.
func heldOps(x *txn, failures int, err error, probe *asideProbe) replaceOps {
	ops := x.ops("", nil)
	rename := ops.renameNew
	ops.renameNew = func(from, to string) error {
		if from == x.exe && strings.Contains(filepath.Base(to), "displaced") {
			probe.attempts++
			if probe.attempts <= failures {
				return err
			}
		}
		return rename(from, to)
	}
	ops.transient = func(e error) bool { return errors.Is(e, errHeld) }
	ops.pause = func(d time.Duration) { probe.pauses = append(probe.pauses, d) }
	return ops
}

func TestAMoveAsideHeldForAMomentIsRetriedAndTheUpgradeGoesThrough(t *testing.T) {
	for _, failures := range []int{1, moveAsideAttempts - 1} {
		x := newTxn(t, "0.3.0", "0.2.9", "0.3.1")
		var probe asideProbe
		err := replaceWith(context.Background(), true, x.exe, x.can, mustVersion(t, "0.3.1"), heldOps(x, failures, errHeld, &probe))
		if err != nil {
			t.Fatalf("held for %d attempts: the upgrade failed: %v", failures, err)
		}
		if probe.attempts != failures+1 || len(probe.pauses) != failures {
			t.Errorf("held for %d attempts: %d attempts and %d pauses, want %d and %d", failures, probe.attempts, len(probe.pauses), failures+1, failures)
		}
		if x.version(x.exe) != "0.3.1" || x.version(x.prev) != "0.3.0" {
			t.Errorf("installed %s, previous %s; want 0.3.1 and the displaced 0.3.0", x.version(x.exe), x.version(x.prev))
		}
	}
}

// Bounded: a holder that never lets go gets a few attempts over about a
// second, and then the failure this step always reported, with nothing changed.
func TestAMoveAsideHeldForGoodFailsAsItAlwaysDidAfterAboutASecond(t *testing.T) {
	x := newTxn(t, "0.3.0", "0.2.9", "0.3.1")
	exeSum, prevSum := x.sum(x.exe), x.sum(x.prev)
	var probe asideProbe
	err := replaceWith(context.Background(), true, x.exe, x.can, mustVersion(t, "0.3.1"), heldOps(x, 1<<30, errHeld, &probe))

	if KindOf(err) != KindReplacementFailed || !errors.Is(err, errHeld) || !strings.Contains(err.Error(), "move the installed binary aside") {
		t.Fatalf("got %v; want the existing replacement_failed for the move-aside, carrying the last error", err)
	}
	if probe.attempts != moveAsideAttempts || len(probe.pauses) != moveAsideAttempts-1 {
		t.Errorf("%d attempts and %d pauses, want %d and %d: never a pause after the last attempt", probe.attempts, len(probe.pauses), moveAsideAttempts, moveAsideAttempts-1)
	}
	if w := probe.waited(); w < 500*time.Millisecond || w > 2*time.Second {
		t.Errorf("waited %s in all; the bound is about a second", w)
	}
	if x.sum(x.exe) != exeSum || x.sum(x.prev) != prevSum {
		t.Error("a move-aside that never happened changed the installed binary or .previous")
	}
	for _, n := range x.names() {
		if strings.Contains(n, "displaced") {
			t.Errorf("a transaction file was left: %v", x.names())
		}
	}
}

// Never for anything else. A missing file will not appear and an existing
// target will not vanish: one attempt, no wait, the same failure as before.
func TestAMoveAsideThatFailedForAnyOtherReasonIsNotRetried(t *testing.T) {
	for name, cause := range map[string]error{
		"a missing file":      fs.ErrNotExist,
		"an existing target":  fs.ErrExist,
		"a permission error":  fs.ErrPermission,
		"an unclassified one": errInjected,
	} {
		t.Run(name, func(t *testing.T) {
			x := newTxn(t, "0.3.0", "0.2.9", "0.3.1")
			var probe asideProbe
			err := replaceWith(context.Background(), true, x.exe, x.can, mustVersion(t, "0.3.1"), heldOps(x, 1<<30, cause, &probe))
			if KindOf(err) != KindReplacementFailed || !errors.Is(err, cause) {
				t.Fatalf("got %v", err)
			}
			if probe.attempts != 1 || len(probe.pauses) != 0 {
				t.Errorf("%d attempts and %d pauses, want 1 and 0", probe.attempts, len(probe.pauses))
			}
		})
	}
}

// An operation that has been canceled or has run out of time stops asking.
func TestAMoveAsideIsNotRetriedOnceTheOperationIsOver(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	x := newTxn(t, "0.3.0", "0.2.9", "0.3.1")
	var probe asideProbe
	if err := replaceWith(ctx, true, x.exe, x.can, mustVersion(t, "0.3.1"), heldOps(x, 1<<30, errHeld, &probe)); err == nil {
		t.Fatal("succeeded against a holder that never let go")
	}
	if probe.attempts != 1 || len(probe.pauses) != 0 {
		t.Errorf("%d attempts and %d pauses under a finished operation, want 1 and 0", probe.attempts, len(probe.pauses))
	}
}

// A directory this user cannot write never reaches the move-aside: reserving
// the displaced name creates and removes a file there first. That is why an
// access-denied AT the move-aside can be read as a passing holder.
func TestADirectoryThatCannotBeWrittenFailsBeforeTheMoveAside(t *testing.T) {
	x := newTxn(t, "0.3.0", "0.2.9", "0.3.1")
	var probe asideProbe
	ops := heldOps(x, 0, nil, &probe)
	ops.reserve = func(string) (string, error) { return "", fs.ErrPermission }
	err := replaceWith(context.Background(), true, x.exe, x.can, mustVersion(t, "0.3.1"), ops)
	if KindOf(err) != KindReplacementFailed || !strings.Contains(err.Error(), "reserve a name") {
		t.Fatalf("got %v", err)
	}
	if probe.attempts != 0 || len(probe.pauses) != 0 {
		t.Errorf("%d move-aside attempts and %d pauses, want none of either", probe.attempts, len(probe.pauses))
	}
}

// POSIX is untouched: its sequence has no move-aside, and nothing in it waits,
// even when the same "held" error turns up at every step it does have.
func TestThePOSIXSequenceNeverWaits(t *testing.T) {
	for _, step := range []string{"snapshot", "install", "sync-installed", "validate", "commit"} {
		x := newTxn(t, "0.3.0", "0.2.9", "0.3.1")
		var probe asideProbe
		ops := x.ops(step, errHeld)
		ops.transient = func(e error) bool { return errors.Is(e, errHeld) }
		ops.pause = func(d time.Duration) { probe.pauses = append(probe.pauses, d) }
		if err := replaceWith(context.Background(), false, x.exe, x.can, mustVersion(t, "0.3.1"), ops); err == nil {
			t.Fatalf("%s: the injected failure did not happen", step)
		}
		if len(probe.pauses) != 0 {
			t.Errorf("%s: the POSIX sequence paused %d times", step, len(probe.pauses))
		}
	}
}

// The production operations carry both halves, so the sequence never calls a
// nil function on a participant's machine.
func TestTheRealOperationsCanClassifyAndCanWait(t *testing.T) {
	ops := defaultReplaceOps(nil)
	if ops.transient == nil || ops.pause == nil {
		t.Fatal("the real operations are missing the move-aside's classifier or its pause")
	}
	if ops.transient(nil) || ops.transient(fs.ErrNotExist) || ops.transient(errInjected) {
		t.Error("an error that is not a held file was classified as transient")
	}
}
