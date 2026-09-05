package observe

import "sync/atomic"

// The ADR-0002 mechanism: one fixed ring per observation, leased from a
// global preallocated budget, released in exactly one place. Three
// channel-based designs each broke on a different buffer-ownership window;
// this design stops transferring ownership. The reader writes into the ring;
// the observer reads out of it; neither ever hands a buffer to the other.
//
// Only the byte slab is pooled. All metadata lives on the per-observation
// obs struct, which is ordinary GC-managed memory and never recycled — so
// there is no such thing as "writing to released metadata".

// ringPool is the global budget: a buffered channel of preallocated slabs.
// Exhaustion IS the signal (a sync.Pool can neither bound memory nor signal
// exhaustion), and ring availability IS the concurrency limit — derived, not
// configured.
type ringPool struct {
	slabs chan []byte
}

func newRingPool(budgetBytes, ringBytes int64) *ringPool {
	count := int(budgetBytes / ringBytes)
	p := &ringPool{slabs: make(chan []byte, count)}
	for i := 0; i < count; i++ {
		p.slabs <- make([]byte, ringBytes)
	}
	return p
}

// tryGet returns a slab or nil; it never blocks and never allocates.
func (p *ringPool) tryGet() []byte {
	select {
	case b := <-p.slabs:
		return b
	default:
		return nil
	}
}

// put zeroes the slab and returns it to the budget. Zeroing on release means
// no observation can ever read another request's bytes out of a recycled
// ring.
func (p *ringPool) put(b []byte) {
	clear(b)
	p.slabs <- b
}

// free reports available rings (tests and diagnostics).
func (p *ringPool) free() int { return len(p.slabs) }

// obs.state packs the lifetime bits and the active-reader count into one
// atomic word: closed | consumerDone | released | readerCount.
const (
	stateClosed       = uint64(1) << 0
	stateConsumerDone = uint64(1) << 1
	stateReleased     = uint64(1) << 2
	readerCountShift  = 3
	readerOne         = uint64(1) << readerCountShift
)

// obs owns one leased ring for the whole life of one observation.
//
// The lifetime protocol is deliberately asymmetric (ADR-0002): the producer
// pins around every buf access, so a paused Read can never overlap a
// release; the consumer never pins — it must keep draining after Close — and
// its safety comes from the release predicate requiring consumerDone, which
// only the consumer sets, as the last thing it does after its final buf
// read. Release needs BOTH facts, and each side owns the one it can vouch
// for.
type obs struct {
	buf   []byte        // THE pooled resource; nil after release (stale use panics loudly)
	head  atomic.Uint64 // written only by the tap
	tail  atomic.Uint64 // written only by the observer
	done  atomic.Bool   // tap → observer terminal signal; not pooled, always safe
	state atomic.Uint64

	overrun  atomic.Bool   // tap → observer: ring free space was exhausted
	finalErr error         // written by the tap before done.Store; read after done.Load
	wakeCh   chan struct{} // 1-slot wake signal

	pool *ringPool
}

func newObs(buf []byte, pool *ringPool) *obs {
	return &obs{buf: buf, pool: pool, wakeCh: make(chan struct{}, 1)}
}

// pin is for the PRODUCER only: refuse once closed, else count a reader.
func (o *obs) pin() bool {
	for {
		s := o.state.Load()
		if s&stateClosed != 0 {
			return false
		}
		if o.state.CompareAndSwap(s, s+readerOne) {
			return true
		}
	}
}

func (o *obs) unpin() {
	o.state.Add(^(readerOne) + 1) // atomic subtract of one reader
	o.tryRelease()
}

func (o *obs) setClosed() {
	o.orBit(stateClosed)
	o.tryRelease()
}

// setConsumerDone is called by the observer as the LAST thing it does after
// its final buf read (via its defer), then tryRelease.
func (o *obs) setConsumerDone() {
	o.orBit(stateConsumerDone)
}

func (o *obs) orBit(bit uint64) {
	for {
		s := o.state.Load()
		if s&bit != 0 {
			return
		}
		if o.state.CompareAndSwap(s, s|bit) {
			return
		}
	}
}

// tryRelease is the ONLY site that returns buf to the pool. Four paths reach
// it (unpin, Close, the observer's defer, the installer's never-started
// path) and exactly one CAS can win, so double-release is impossible by
// construction rather than by inspection. The predicate is
// closed && readers == 0 && consumerDone && !released — all three facts, not
// two: producer quiescence and consumer completion are different conditions
// and both are required.
func (o *obs) tryRelease() {
	for {
		s := o.state.Load()
		if s&stateReleased != 0 {
			return
		}
		if s&stateClosed == 0 || s&stateConsumerDone == 0 || s>>readerCountShift != 0 {
			return
		}
		if o.state.CompareAndSwap(s, s|stateReleased) {
			buf := o.buf
			// Nilling the field matters: the protocol already stops
			// legitimate readers, but leaving o pointing at a slab now
			// leased to a DIFFERENT observation is a stale alias. A nil
			// panic is loud; quietly reading another request's bytes is not.
			o.buf = nil
			o.pool.put(buf)
			return
		}
	}
}

// write copies p into the ring. The caller must hold a pin. It is the single
// producer: two atomic loads, a copy (wrapping the physical boundary), one
// atomic store. head is stored only after the bytes are copied, so the
// observer can never read a slot the writer has not finished filling.
// Returns false — without writing — when free space is insufficient: the
// observer fell behind and this observation is done (degrade, never block).
func (o *obs) write(p []byte) bool {
	h, t := o.head.Load(), o.tail.Load()
	size := uint64(len(o.buf))
	if uint64(len(p)) > size-(h-t) {
		return false
	}
	off := h % size
	n := copy(o.buf[off:], p)
	if n < len(p) {
		copy(o.buf, p[n:])
	}
	o.head.Store(h + uint64(len(p)))
	return true
}

// drainInto feeds every readable byte to scan (in at most two wrapped
// chunks), then publishes the advanced tail. tail is stored only after the
// bytes are consumed, so the writer can never overwrite bytes the observer
// has not read. Returns false when there was nothing to read.
func (o *obs) drainInto(scan func([]byte)) bool {
	h, t := o.head.Load(), o.tail.Load()
	if h == t {
		return false
	}
	size := uint64(len(o.buf))
	off := t % size
	n := h - t
	first := min(n, size-off)
	scan(o.buf[off : off+first])
	if n > first {
		scan(o.buf[:n-first])
	}
	o.tail.Store(h)
	return true
}

// wake is a non-blocking send on the 1-slot signal channel.
func (o *obs) wake() {
	select {
	case o.wakeCh <- struct{}{}:
	default:
	}
}
