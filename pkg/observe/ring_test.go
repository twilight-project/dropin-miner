package observe

// The §9.5 tap-lifecycle table: the cases where a bounded lossy tap goes
// wrong are all lifecycle cases, so each gets a named test. Run under -race
// in CI.

import (
	"sync"
	"testing"
)

const testRing = 4096

func TestPoolLeaseReleaseZeroed(t *testing.T) {
	pool := newRingPool(4*testRing, testRing)
	if pool.free() != 4 {
		t.Fatalf("free: %d", pool.free())
	}
	buf := pool.tryGet()
	buf[0], buf[1] = 0xAA, 0xBB
	pool.put(buf)
	if pool.free() != 4 {
		t.Fatalf("free after put: %d", pool.free())
	}
	// Zeroed on release: no observation can read another request's bytes.
	again := pool.tryGet()
	if again[0] != 0 || again[1] != 0 {
		t.Error("ring not zeroed on release")
	}
}

func TestPoolExhaustionIsSignal(t *testing.T) {
	pool := newRingPool(2*testRing, testRing)
	a, b := pool.tryGet(), pool.tryGet()
	if a == nil || b == nil {
		t.Fatal("expected two rings")
	}
	if pool.tryGet() != nil {
		t.Error("exhausted pool must return nil, not block or allocate")
	}
}

// A write that crosses the physical ring boundary but fits in free space
// wraps correctly with no corruption — a different outcome from overrun,
// which is why they are separate tests.
func TestWriteWrapsPhysicalBoundary(t *testing.T) {
	pool := newRingPool(testRing, testRing)
	o := newObs(pool.tryGet(), pool)

	first := make([]byte, 3000)
	for i := range first {
		first[i] = byte(i % 251)
	}
	if !o.write(first) {
		t.Fatal("first write refused")
	}
	var drained []byte
	o.drainInto(func(b []byte) { drained = append(drained, b...) })

	second := make([]byte, 3000)
	for i := range second {
		second[i] = byte((i + 7) % 251)
	}
	if !o.write(second) { // 3000+3000 > 4096 physically, but free space fits
		t.Fatal("wrapping write refused")
	}
	drained = drained[:0]
	o.drainInto(func(b []byte) { drained = append(drained, b...) })
	if len(drained) != 3000 {
		t.Fatalf("drained %d bytes", len(drained))
	}
	for i := range drained {
		if drained[i] != byte((i+7)%251) {
			t.Fatalf("corruption at %d: %d", i, drained[i])
		}
	}
}

// Overrun: a write larger than free space is refused without writing.
func TestWriteOverrunRefused(t *testing.T) {
	pool := newRingPool(testRing, testRing)
	o := newObs(pool.tryGet(), pool)
	if o.write(make([]byte, testRing+1)) {
		t.Error("oversized write accepted")
	}
	if o.head.Load() != 0 {
		t.Error("refused write advanced head")
	}
}

// The release predicate needs all three facts. Exactly one of the four
// paths that reach tryRelease can win.
func TestSingleRelease(t *testing.T) {
	pool := newRingPool(2*testRing, testRing)
	o := newObs(pool.tryGet(), pool)

	// Consumer done alone: no release (not closed).
	o.setConsumerDone()
	o.tryRelease()
	if pool.free() != 1 {
		t.Fatal("released before closed")
	}
	// Closed too: released, exactly once, idempotently.
	o.setClosed()
	if pool.free() != 2 {
		t.Fatal("not released when closed && consumerDone && readers==0")
	}
	o.tryRelease()
	o.setClosed()
	o.tryRelease()
	if pool.free() != 2 {
		t.Fatal("double release")
	}
	if o.buf != nil {
		t.Error("buf must be nil after release: stale aliases must panic loudly")
	}
}

// A pinned producer blocks release; unpinning releases.
func TestPinBlocksRelease(t *testing.T) {
	pool := newRingPool(testRing, testRing)
	o := newObs(pool.tryGet(), pool)

	if !o.pin() {
		t.Fatal("pin refused before close")
	}
	o.setConsumerDone()
	o.setClosed()
	if pool.free() != 0 {
		t.Fatal("released under a live pin")
	}
	// pin() must refuse after closed.
	if o.pin() {
		t.Fatal("pin succeeded after close")
	}
	o.unpin()
	if pool.free() != 1 {
		t.Fatal("unpin did not trigger release")
	}
}

// The installer's never-started path: consumer reference dropped without an
// observer ever existing; Close alone then releases the ring.
func TestNeverStartedObserverReleases(t *testing.T) {
	pool := newRingPool(testRing, testRing)
	o := newObs(pool.tryGet(), pool)
	o.setConsumerDone()
	o.tryRelease()
	if pool.free() != 0 {
		t.Fatal("released while producer still open")
	}
	o.setClosed()
	if pool.free() != 1 {
		t.Fatal("never-started path leaked the ring")
	}
}

// The interleaving that broke all three channel-based designs: Close racing
// an in-flight Read. The assertion is direct — the free-ring count after
// equals the count before — repeated to exercise schedules, and run under
// -race in CI.
func TestCloseRacesRead(t *testing.T) {
	const rings = 8
	pool := newRingPool(rings*testRing, testRing)
	m := &Metrics{}

	for i := 0; i < 500; i++ {
		o := newObs(pool.tryGet(), pool)
		body := &scriptedBody{chunks: [][]byte{make([]byte, 100), make([]byte, 100)}}
		tp := newTap(body, o, m)

		var wg sync.WaitGroup
		wg.Add(3)
		go func() { // producer: two reads through the tap
			defer wg.Done()
			buf := make([]byte, 256)
			_, _ = tp.Read(buf)
			_, _ = tp.Read(buf)
		}()
		go func() { // closer, racing the reads
			defer wg.Done()
			_ = tp.Close()
		}()
		go func() { // consumer: drain until done, then quit
			defer wg.Done()
			for {
				o.drainInto(func([]byte) {})
				if o.done.Load() && o.tail.Load() == o.head.Load() {
					break
				}
			}
			o.setConsumerDone()
			o.tryRelease()
		}()
		wg.Wait()

		if free := pool.free(); free != rings {
			t.Fatalf("iteration %d: free rings %d, want %d — a ring was stranded or double-released", i, free, rings)
		}
	}
}
