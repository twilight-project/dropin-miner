package observe

import (
	"context"
	"errors"
	"mime"
	"net/http"
	"time"
)

// Profile names the metadata layout of a meterable response body. The route
// table decides it; the observer never sniffs it. The proxy already
// committed to what a route returns when it admitted the route, and letting
// a header the upstream controls pick the parser would hand that choice
// back to the upstream.
type Profile uint8

const (
	// ProfileChatCompletion is the OpenAI-compatible completion envelope,
	// JSON or SSE. The zero value, so an unset profile reads as the shape
	// the observer shipped with.
	ProfileChatCompletion Profile = iota
	// ProfileSearchRouter is the search router's single JSON response.
	ProfileSearchRouter
)

// Limits carries the observation bounds (from config, passed as plain values
// so this package imports nothing).
type Limits struct {
	MemoryBudgetBytes    int64
	RingBytes            int64
	MaxObservedBodyBytes int64
	MaxEventBytes        int64
	MaxEvents            int64
	MaxDepth             int
	MaxJSONKeyBytes      int
}

// Installer owns the ring budget and spawns observers. Install is the ONLY
// entry point and it returns nothing: ModifyResponse stays a two-line
// closure that cannot produce the 502 path (§3.4).
type Installer struct {
	provider string
	limits   Limits
	pool     *ringPool
	sink     Sink
	metrics  *Metrics

	// observeStreams is the Phase 3 dispatch branch — and the Phase 3
	// rollback: flipping it to false restores Phase 2 behavior exactly
	// (§10 Phase 3).
	observeStreams bool
}

// NewInstaller preallocates the whole ring budget up front: the derived ring
// count is the concurrency limit, and exhaustion is a counted signal, not an
// allocation.
func NewInstaller(provider string, limits Limits, sink Sink, metrics *Metrics) *Installer {
	return &Installer{
		provider:       provider,
		limits:         limits,
		pool:           newRingPool(limits.MemoryBudgetBytes, limits.RingBytes),
		sink:           sink,
		metrics:        metrics,
		observeStreams: true,
	}
}

// FreeRings reports the available ring count (diagnostics and tests).
func (i *Installer) FreeRings() int { return i.pool.free() }

// Install observes resp if it is observable, wrapping resp.Body in the tap
// and spawning the observer. It ALWAYS leaves the response deliverable —
// every refusal is a counter, never an error. Meterability is checked first:
// a non-meterable route gets no tap, no ring, no Observation, regardless of
// what the response looks like (§6.2, T30). profile comes from the same
// route-table row as meterable and is ignored when it is false.
func (i *Installer) Install(resp *http.Response, meterable bool, profile Profile, startedAt time.Time) {
	if !meterable {
		i.metrics.CountSkip(SkipNonMeterableRoute)
		return
	}
	// Non-2xx: no Observation object is created at all — a drop reason on a
	// counter, not a Termination on an Observation that does not exist
	// (§6.2). There is nothing to promote.
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		i.metrics.CountSkip(SkipUpstreamStatus)
		return
	}

	// The framing is read from the RESPONSE Content-Type, never the request
	// body — the proxy never parses the request (§6.2).
	streaming := isEventStream(resp.Header.Get("Content-Type"))
	if profile == ProfileSearchRouter {
		// The search route's documented response is one JSON document.
		// The route table already said so, so a Content-Type the upstream
		// controls does not get to re-decide it; a body that turns out not
		// to be that shape abandons as malformed_body rather than being
		// guessed at.
		streaming = false
	}
	if streaming && !i.observeStreams {
		return
	}

	obsv := &Observation{
		Provider:   i.provider,
		Profile:    profile,
		StatusCode: resp.StatusCode,
		StartedAt:  startedAt,
		Streaming:  streaming,
	}

	// A body the observer cannot read without decompressing terminates the
	// observation as encoded_body; nothing here decompresses (§5.5).
	if enc := resp.Header.Get("Content-Encoding"); enc != "" && enc != "identity" {
		i.metrics.Started.Add(1)
		i.finalize(obsv, Abandoned(AbandonEncodedBody))
		return
	}

	buf := i.pool.tryGet()
	if buf == nil {
		// Budget fully leased: the response is proxied with zero
		// observation overhead. Ring availability IS the concurrency limit.
		i.metrics.CountSkip(SkipNoRing)
		return
	}

	o := newObs(buf, i.pool)
	t := newTap(resp.Body, o, i.metrics)
	resp.Body = t
	i.metrics.Started.Add(1)

	var parser bodyParser
	switch {
	case profile == ProfileSearchRouter:
		// requestIDHeader is read HERE, on the request goroutine: after
		// Install returns, resp belongs to the copy loop and the observer
		// goroutine must not touch it.
		parser = newSearchParser(i.limits.MaxDepth, i.limits.MaxJSONKeyBytes, requestIDHeader(resp.Header))
	case streaming:
		parser = newSSEParser(i.limits.MaxDepth, i.limits.MaxJSONKeyBytes, i.limits.MaxEventBytes, i.limits.MaxEvents)
	default:
		parser = newJSONParser(i.limits.MaxDepth, i.limits.MaxJSONKeyBytes)
	}

	started := false
	defer func() {
		if !started {
			// The installer's never-started path: drop the consumer
			// reference so the ring cannot be stranded.
			o.setConsumerDone()
			o.tryRelease()
		}
	}()
	go i.runObserver(o, obsv, parser)
	started = true
}

func isEventStream(contentType string) bool {
	mt, _, err := mime.ParseMediaType(contentType)
	return err == nil && mt == "text/event-stream"
}

// requestIDHeader takes the search router's X-Request-Id, subject to the
// same screen a captured body field gets: bounded length, printable ASCII
// without whitespace, no credential shape. A header is upstream input, and
// arriving in a header rather than a body earns it nothing. The bound is the
// scanner's own id cap, not the submission format's — this package does not
// know the wire contract, and a value the AS would reject is rejected there.
func requestIDHeader(h http.Header) string {
	v := h.Get("X-Request-Id")
	if v == "" || len(v) > capIDBytes {
		return ""
	}
	b := []byte(v)
	if !printableASCIINoWS(b) || looksLikeCredential(b) {
		return ""
	}
	return v
}

// runObserver drains the ring through the parser and finalizes at EOF or at
// a terminal event. One goroutine per observed response, bounded by ring
// availability — the ring budget is the concurrency limit, there is no
// separate semaphore (§3.4).
func (i *Installer) runObserver(o *obs, obsv *Observation, parser bodyParser) {
	defer func() {
		if r := recover(); r != nil {
			i.metrics.ObserverPanics.Add(1)
		}
		// Every exit path, including panic: consumer done, then the one
		// release site.
		o.setConsumerDone()
		o.tryRelease()
	}()

	var scanned int64
	for {
		if o.overrun.Load() {
			i.finalize(obsv, Abandoned(AbandonObserverOverrun))
			return
		}
		progressed := o.drainInto(func(b []byte) {
			scanned += int64(len(b))
			parser.consume(b)
		})
		if scanned > i.limits.MaxObservedBodyBytes {
			// A bound on bytes SCANNED, not stored: the client receives the
			// entire response regardless; only the observation is lost
			// (§6.6).
			i.finalize(obsv, Abandoned(AbandonBodyTooLarge))
			return
		}
		if reason, aborted := parser.abortReason(); aborted {
			i.finalize(obsv, Abandoned(reason))
			return
		}
		if parser.terminated() {
			// [DONE] or a provider error event: the observation is over
			// even though the producer may still be copying (§6.4).
			// o.finalErr is NOT read here: the producer may be writing it
			// concurrently (the happens-before edge exists only through
			// done), and a read error after a terminal event could not
			// change the classification anyway.
			i.finalize(obsv, parser.finalize(false, nil, obsv))
			return
		}
		if !progressed {
			if o.done.Load() && o.tail.Load() == o.head.Load() {
				i.finalize(obsv, parser.finalize(true, o.finalErr, obsv))
				return
			}
			<-o.wakeCh
		}
	}
}

func (i *Installer) finalize(obsv *Observation, outcome Outcome) {
	obsv.Outcome = outcome
	obsv.FinishedAt = time.Now()
	if obsv.ParseDegraded {
		i.metrics.Degraded.Add(1)
	}
	i.metrics.CountOutcome(outcome)
	i.sink.Offer(obsv)
}

// isClientDisconnect: context cancellation surfacing as a read error on the
// tap is the client leaving, not the provider finishing (§6.5).
func isClientDisconnect(err error) bool {
	return err != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded))
}
