package observe

import (
	"io"
	"sync/atomic"
)

// tap wraps resp.Body. Its Read returns exactly the (n, err) of the wrapped
// body — it never manufactures an error, and the copy work is two atomic
// loads, one copy into the observation's own ring, and one atomic store: no
// lock, no channel send that could fill, no acquire that could fail midway.
type tap struct {
	body io.ReadCloser
	o    *obs
	// enabled is atomic: degrade, the terminal paths, and Close all disable
	// the tap while a read may be in flight — a plain bool there is a data
	// race -race finds immediately.
	enabled atomic.Bool
	metrics *Metrics
}

func newTap(body io.ReadCloser, o *obs, m *Metrics) *tap {
	t := &tap{body: body, o: o, metrics: m}
	t.enabled.Store(true)
	return t
}

// Read: (n, err) are NAMED returns assigned from the underlying read before
// any tap work runs. With unnamed returns, a recover() after the body had
// already produced bytes would hand the caller (0, nil) — truncating the
// client's response to keep an observation safe, the invariant exactly
// inverted. A panic in tap work disables the tap for the rest of the
// response and copying continues.
func (t *tap) Read(p []byte) (n int, err error) {
	n, err = t.body.Read(p)
	defer func() {
		if r := recover(); r != nil {
			t.enabled.Store(false)
			t.metrics.ObserverPanics.Add(1)
		}
	}()
	if t.enabled.Load() && n > 0 && t.o.pin() {
		func() {
			defer t.o.unpin() // deferred immediately: a panic still unpins
			if !t.o.write(p[:n]) {
				// The observer is behind; this observation is done. The
				// client loses nothing — (n, err) are returned verbatim
				// regardless; only observation bytes are lost.
				t.o.overrun.Store(true)
				t.enabled.Store(false)
			}
		}()
	}
	if err != nil {
		t.o.finalErr = err // safe unpinned: o is not pooled, only buf is
		t.o.done.Store(true)
	}
	t.o.wake()
	return n, err
}

// Close frees nothing directly: it marks the producer side closed and lets
// tryRelease decide, exactly once, across all four exit paths.
func (t *tap) Close() error {
	t.enabled.Store(false)
	t.o.done.Store(true)
	t.o.wake()
	t.o.setClosed()
	return t.body.Close()
}
