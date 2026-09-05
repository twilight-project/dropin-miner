package scope

import (
	"sync"
	"testing"
	"time"
)

func TestHolderSnapshotSemantics(t *testing.T) {
	var h Holder
	// Absent participation is a valid, non-failing state.
	if snap := h.Snapshot(); snap != nil {
		t.Fatalf("fresh holder must snapshot nil, got %+v", snap)
	}
	if (*Context)(nil).Valid(time.Now()) {
		t.Fatal("nil context must not be valid")
	}

	now := time.Now()
	ctx := &Context{SlotID: 7, TargetEpoch: 1042, Capability: "cap", ExpiresAt: now.Add(time.Minute)}
	h.Set(ctx)
	snap := h.Snapshot()
	if snap != ctx || !snap.Valid(now) {
		t.Fatalf("snapshot mismatch: %+v", snap)
	}
	if snap.Valid(now.Add(2 * time.Minute)) {
		t.Fatal("expired context reported valid")
	}
	if (&Context{ExpiresAt: now.Add(time.Minute)}).Valid(now) {
		t.Fatal("context without a capability reported valid")
	}
	h.Clear()
	if h.Snapshot() != nil {
		t.Fatal("Clear did not drop participation")
	}
}

// The request path reads this holder on every request: concurrent
// snapshots and updates must be race-free (run under -race in CI).
func TestHolderConcurrentAccess(t *testing.T) {
	var (
		h  Holder
		wg sync.WaitGroup
	)
	stop := make(chan struct{})
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = h.Snapshot().Valid(time.Now())
				}
			}
		}()
	}
	for i := 0; i < 200; i++ {
		h.Set(&Context{SlotID: 7, TargetEpoch: uint64(i), Capability: "c", ExpiresAt: time.Now().Add(time.Minute)})
		h.Clear()
	}
	close(stop)
	wg.Wait()
}
