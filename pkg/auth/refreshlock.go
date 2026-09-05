package auth

// Cross-process custody of the refresh authorization.
//
// Rotation makes a refresh a read-modify-write over one file: load the
// stored token, spend it at the AS, persist the successor. Two kinds of
// process share that file — the mining daemon, which renews its
// capability on a one-minute tick, and every CLI command that talks to
// the AS, each of which refreshes first. If both read the same token and
// both spend it, the AS sees the second presentation as reuse, and
// STRICT reuse policy answers reuse by revoking the whole token family:
// the installation is dead until the participant re-enrolls. The window
// is milliseconds wide, which is exactly why it surfaces in front of a
// user rather than in a test run.
//
// The remedy is an advisory lock held across the WHOLE cycle, the
// network call included. Releasing before the AS answers only narrows
// the window; it does not close it.
//
// The lock is taken on a sibling lockfile, never on refresh.token
// itself. SaveRefreshToken installs the successor by renaming a temp
// file over it, and a lock taken on the token file stays attached to the
// replaced inode while the next arrival locks the new one — two holders
// believing they have exclusion, which is worse than no lock at all.

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"
)

// refreshLockFile is the sibling this package locks. It is created on
// first use, never written to, and never removed: an empty file is the
// whole mechanism, and deleting it would race a waiter that already
// holds a descriptor on it.
const refreshLockFile = "refresh.token.lock"

const (
	// refreshLockTimeout bounds the wait. A holder that is stuck on an
	// unresponsive AS must not wedge every other process indefinitely,
	// and 30s matches the timeout every HTTP client in this package
	// already carries — so a waiter gives up at roughly the moment the
	// holder's own request would have.
	refreshLockTimeout = 30 * time.Second
	// The acquire loop polls, because neither platform primitive offers
	// a blocking wait with a deadline. It starts fast, since the common
	// contention is two processes microseconds apart, and backs off so a
	// genuinely long wait costs a handful of syscalls rather than
	// thousands.
	refreshLockFirstPoll = time.Millisecond
	refreshLockMaxPoll   = 50 * time.Millisecond
)

// ErrRefreshLockBusy is returned when the wait expires with the lock
// still held elsewhere. It is a distinct error because the caller's
// situation is distinct: nothing is wrong with this installation's
// custody, another process is simply mid-refresh, and the right response
// is to try again rather than to re-enroll.
var ErrRefreshLockBusy = errors.New("auth: another process holds the refresh-token lock")

// lockRefreshToken takes the cross-process refresh lock, waiting at most
// refreshLockTimeout. The returned function releases it and is safe to
// defer immediately.
func (s *Store) lockRefreshToken(ctx context.Context) (release func(), err error) {
	return s.lockRefreshTokenFor(ctx, refreshLockTimeout)
}

// lockRefreshTokenFor is lockRefreshToken with the bound as an argument,
// so the expiry path can be proven in a test without waiting half a
// minute for it.
func (s *Store) lockRefreshTokenFor(ctx context.Context, timeout time.Duration) (func(), error) {
	path := filepath.Join(s.dir, refreshLockFile)
	deadline := time.Now().Add(timeout)
	poll := refreshLockFirstPoll
	for {
		f, held, err := tryLockRefreshFile(path)
		if err != nil {
			return nil, err
		}
		if held {
			// The release error is dropped deliberately: it can only be
			// a failure to close a descriptor on an empty file, the
			// kernel releases the lock when the process exits either
			// way, and there is nothing a caller mid-refresh could do
			// about it that would not be worse than continuing.
			return func() { _ = unlockRefreshFile(f) }, nil
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, fmt.Errorf("%w: gave up after %s waiting on %s; the daemon refreshes every minute, so retry — if it persists, the holder is stuck on an unresponsive AS", ErrRefreshLockBusy, timeout, path)
		}
		wait := poll
		if wait > remaining {
			wait = remaining
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, fmt.Errorf("auth: waiting for the refresh-token lock: %w", ctx.Err())
		case <-timer.C:
		}
		if poll < refreshLockMaxPoll {
			poll *= 2
		}
	}
}
