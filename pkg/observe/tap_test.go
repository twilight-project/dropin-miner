package observe

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// scriptedBody returns its chunks one Read at a time, then finalErr (io.EOF
// by default). finalWithLast makes the last chunk arrive together with the
// error — the n>0-with-io.EOF case.
type scriptedBody struct {
	chunks        [][]byte
	finalErr      error
	finalWithLast bool
	closed        bool
}

func (b *scriptedBody) Read(p []byte) (int, error) {
	if len(b.chunks) == 0 {
		if b.finalErr != nil {
			return 0, b.finalErr
		}
		return 0, io.EOF
	}
	chunk := b.chunks[0]
	n := copy(p, chunk)
	if n < len(chunk) {
		b.chunks[0] = chunk[n:] // keep the remainder for the next Read
		return n, nil
	}
	b.chunks = b.chunks[1:]
	if len(b.chunks) == 0 && b.finalWithLast {
		err := b.finalErr
		if err == nil {
			err = io.EOF
		}
		return n, err
	}
	return n, nil
}

func (b *scriptedBody) Close() error {
	b.closed = true
	return nil
}

// chanSink delivers observations to the test.
type chanSink struct{ ch chan *Observation }

func newChanSink() *chanSink { return &chanSink{ch: make(chan *Observation, 16)} }

func (s *chanSink) Offer(o *Observation) {
	select {
	case s.ch <- o:
	default:
	}
}

func (s *chanSink) wait(t *testing.T) *Observation {
	t.Helper()
	select {
	case o := <-s.ch:
		return o
	case <-time.After(5 * time.Second):
		t.Fatal("no observation arrived")
		return nil
	}
}

func testLimits() Limits {
	return Limits{
		MemoryBudgetBytes:    8 * testRing,
		RingBytes:            testRing,
		MaxObservedBodyBytes: 64 << 20,
		MaxEventBytes:        1 << 20,
		MaxEvents:            100_000,
		MaxDepth:             32,
		MaxJSONKeyBytes:      256,
	}
}

func jsonResponse(body io.ReadCloser) *http.Response {
	return &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       body,
	}
}

// driveThroughProxyLoop simulates what ReverseProxy does: install, copy the
// body to the client, close. Returns the delivered bytes.
func driveThroughProxyLoop(t *testing.T, inst *Installer, resp *http.Response) []byte {
	t.Helper()
	inst.Install(resp, true, ProfileChatCompletion, time.Now())
	var delivered bytes.Buffer
	_, err := io.Copy(&delivered, resp.Body)
	if err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("copy: %v", err)
	}
	_ = resp.Body.Close()
	return delivered.Bytes()
}

// driveWithBigReads reads through the tap with a buffer LARGER than one
// ring, so a single Read can hand back more bytes than the whole ring — the
// §9.5 case that must overrun the observation and still deliver every byte.
func driveWithBigReads(t *testing.T, inst *Installer, resp *http.Response) []byte {
	t.Helper()
	inst.Install(resp, true, ProfileChatCompletion, time.Now())
	buf := make([]byte, 2*testRing)
	var delivered []byte
	for {
		n, err := resp.Body.Read(buf)
		delivered = append(delivered, buf[:n]...)
		if err != nil {
			break
		}
	}
	_ = resp.Body.Close()
	return delivered
}

// §9.5 row 1: Read returning n>0 together with io.EOF — the final bytes are
// observed AND delivered; no truncation either way.
func TestFinalChunkWithEOF(t *testing.T) {
	sink := newChanSink()
	inst := NewInstaller("openrouter", testLimits(), sink, &Metrics{})

	doc := `{"id":"gen-tail","model":"m"}`
	body := &scriptedBody{chunks: [][]byte{[]byte(doc)}, finalWithLast: true}
	resp := jsonResponse(body)

	delivered := driveThroughProxyLoop(t, inst, resp)
	if string(delivered) != doc {
		t.Errorf("delivered bytes truncated: %q", delivered)
	}
	o := sink.wait(t)
	if o.GenerationID != "gen-tail" {
		t.Errorf("final chunk not observed: %+v", o)
	}
	if term, _ := o.Outcome.Termination(); term != TerminationDone {
		t.Errorf("outcome: %v", o.Outcome)
	}
	waitFreeRings(t, inst, 8)
}

func waitFreeRings(t *testing.T, inst *Installer, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if inst.FreeRings() == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("free rings never returned to %d (now %d)", want, inst.FreeRings())
}

// §9.5 row 6: a tap panic after the underlying read returned bytes — the
// client still receives those bytes (named-return preservation), the tap is
// disabled, observer_panics increments.
func TestTapPanicPreservesDeliveredBytes(t *testing.T) {
	m := &Metrics{}
	body := &scriptedBody{chunks: [][]byte{[]byte("payload-1"), []byte("payload-2")}}
	// A nil obs makes the first pin() panic — standing in for any bug in
	// tap-side code.
	tp := &tap{body: body, o: nil, metrics: m}
	tp.enabled.Store(true)

	buf := make([]byte, 64)
	n, err := tp.Read(buf)
	if n != len("payload-1") || err != nil {
		t.Fatalf("client bytes lost to the panic: n=%d err=%v", n, err)
	}
	if m.ObserverPanics.Load() != 1 {
		t.Errorf("observer_panics = %d", m.ObserverPanics.Load())
	}
	if tp.enabled.Load() {
		t.Error("tap not disabled after panic")
	}
	// Subsequent reads are pure passthrough.
	n, err = tp.Read(buf)
	if n != len("payload-2") || err != nil {
		t.Fatalf("passthrough after panic broken: n=%d err=%v", n, err)
	}
}

// §9.5 rows 8+9: a Read handing back more bytes than the whole ring — the
// observation is abandoned as observer_overrun and every client byte is
// still delivered.
func TestOverrunAbandonsButDelivers(t *testing.T) {
	sink := newChanSink()
	inst := NewInstaller("openrouter", testLimits(), sink, &Metrics{})

	big := bytes.Repeat([]byte("x"), testRing+512) // larger than one ring
	body := &scriptedBody{chunks: [][]byte{big}}
	resp := jsonResponse(body)

	delivered := driveWithBigReads(t, inst, resp)
	if len(delivered) != len(big) {
		t.Fatalf("client short-changed: %d of %d bytes", len(delivered), len(big))
	}
	o := sink.wait(t)
	if reason, _ := o.Outcome.Reason(); reason != AbandonObserverOverrun {
		t.Errorf("outcome: %v", o.Outcome)
	}
	waitFreeRings(t, inst, 8)
}

// The §10 Phase 2 exit proof: the copy path never blocks when the observer
// is stalled indefinitely. A consumer that never drains is exactly a
// stalled-forever observer; copying a body many times the ring size must
// complete promptly and byte-exactly.
func TestCopyPathNeverBlocksWithStalledObserver(t *testing.T) {
	pool := newRingPool(testRing, testRing)
	o := newObs(pool.tryGet(), pool)
	m := &Metrics{}

	const total = 10 << 20 // 10 MiB through a 4 KiB ring, nobody draining
	body := io.NopCloser(strings.NewReader(strings.Repeat("y", total)))
	tp := newTap(body, o, m)

	done := make(chan int64, 1)
	go func() {
		n, _ := io.Copy(io.Discard, tp)
		_ = tp.Close()
		done <- n
	}()
	select {
	case n := <-done:
		if n != total {
			t.Fatalf("copied %d of %d", n, total)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("copy path blocked behind a stalled observer")
	}
	if !o.overrun.Load() {
		t.Error("stalled observer should have overrun the ring")
	}
}

// §9.5 row 2, Phase 2 form: the observer exits early (overrun) while
// the producer keeps streaming — remaining reads are pure passthrough and
// the ring is released exactly once, at Close.
func TestEarlyObserverExitPassthrough(t *testing.T) {
	sink := newChanSink()
	inst := NewInstaller("openrouter", testLimits(), sink, &Metrics{})

	chunks := [][]byte{
		bytes.Repeat([]byte("a"), testRing+100), // overruns immediately
		bytes.Repeat([]byte("b"), 1000),
		bytes.Repeat([]byte("c"), 1000),
	}
	body := &scriptedBody{chunks: chunks}
	resp := jsonResponse(body)

	delivered := driveWithBigReads(t, inst, resp)
	want := testRing + 100 + 2000
	if len(delivered) != want {
		t.Fatalf("passthrough after early observer exit lost bytes: %d of %d", len(delivered), want)
	}
	o := sink.wait(t)
	if reason, _ := o.Outcome.Reason(); reason != AbandonObserverOverrun {
		t.Errorf("outcome: %v", o.Outcome)
	}
	waitFreeRings(t, inst, 8)
}

// §9.5 row 3: Body.Close before, during, and after EOF — done is idempotent
// and the ring is released exactly once.
func TestCloseIdempotent(t *testing.T) {
	sink := newChanSink()
	inst := NewInstaller("openrouter", testLimits(), sink, &Metrics{})

	body := &scriptedBody{chunks: [][]byte{[]byte(`{"id":"gen-close"}`)}}
	resp := jsonResponse(body)
	inst.Install(resp, true, ProfileChatCompletion, time.Now())

	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	_ = resp.Body.Close()
	_ = resp.Body.Close()

	sink.wait(t)
	waitFreeRings(t, inst, 8)
	if !body.closed {
		t.Error("underlying body never closed")
	}
}

// Budget exhausted: the response is proxied untouched with no tap installed
// and the skip is counted (§6.1).
func TestNoRingSkips(t *testing.T) {
	m := &Metrics{}
	limits := testLimits()
	limits.MemoryBudgetBytes = limits.RingBytes // exactly one ring
	inst := NewInstaller("openrouter", limits, newChanSink(), m)

	// Hold the only ring by starting an observation that never finishes.
	blockedBody := &scriptedBody{chunks: [][]byte{[]byte("{")}}
	heldResp := jsonResponse(blockedBody)
	inst.Install(heldResp, true, ProfileChatCompletion, time.Now())
	buf := make([]byte, 16)
	_, _ = heldResp.Body.Read(buf) // consume the chunk; body not closed yet

	doc := `{"id":"gen-unobserved"}`
	resp := jsonResponse(io.NopCloser(strings.NewReader(doc)))
	inst.Install(resp, true, ProfileChatCompletion, time.Now())
	if _, tapped := resp.Body.(*tap); tapped {
		t.Fatal("a tap was installed with no ring available")
	}
	delivered, _ := io.ReadAll(resp.Body)
	if string(delivered) != doc {
		t.Errorf("unobserved response altered: %q", delivered)
	}
	if m.SkippedNoRing.Load() != 1 {
		t.Errorf("skip counter: %d", m.SkippedNoRing.Load())
	}
	_ = heldResp.Body.Close()
}
