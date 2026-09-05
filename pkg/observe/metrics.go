package observe

import (
	"fmt"
	"io"
	"sync/atomic"
)

// SkipReason labels observations_skipped_total: responses that never got an
// Observation object at all.
type SkipReason string

const (
	SkipNoRing            SkipReason = "no_ring"
	SkipUpstreamStatus    SkipReason = "upstream_status"
	SkipNonMeterableRoute SkipReason = "non_meterable_route"
)

// Metrics holds the observation counters for /metricsz. Every label set is
// closed at compile time — nothing caller-influenced (§11).
type Metrics struct {
	Started atomic.Uint64

	CompletedDone           atomic.Uint64
	CompletedEOFWithoutDone atomic.Uint64
	CompletedClientDisc     atomic.Uint64
	CompletedUpstreamError  atomic.Uint64

	SkippedNoRing         atomic.Uint64
	SkippedUpstreamStatus atomic.Uint64
	SkippedNonMeterable   atomic.Uint64

	AbandonedOverrun   atomic.Uint64
	AbandonedTooLarge  atomic.Uint64
	AbandonedEncoded   atomic.Uint64
	AbandonedMaxDepth  atomic.Uint64
	AbandonedMalformed atomic.Uint64

	Degraded       atomic.Uint64
	ObserverPanics atomic.Uint64
}

func (m *Metrics) CountSkip(r SkipReason) {
	switch r {
	case SkipNoRing:
		m.SkippedNoRing.Add(1)
	case SkipUpstreamStatus:
		m.SkippedUpstreamStatus.Add(1)
	case SkipNonMeterableRoute:
		m.SkippedNonMeterable.Add(1)
	}
}

func (m *Metrics) CountOutcome(o Outcome) {
	if t, ok := o.Termination(); ok {
		switch t {
		case TerminationDone:
			m.CompletedDone.Add(1)
		case TerminationEOFWithoutDone:
			m.CompletedEOFWithoutDone.Add(1)
		case TerminationClientDisconnect:
			m.CompletedClientDisc.Add(1)
		case TerminationUpstreamErrorEvent:
			m.CompletedUpstreamError.Add(1)
		}
		return
	}
	reason, _ := o.Reason()
	switch reason {
	case AbandonObserverOverrun:
		m.AbandonedOverrun.Add(1)
	case AbandonBodyTooLarge:
		m.AbandonedTooLarge.Add(1)
	case AbandonEncodedBody:
		m.AbandonedEncoded.Add(1)
	case AbandonMaxDepthExceeded:
		m.AbandonedMaxDepth.Add(1)
	case AbandonMalformedBody:
		m.AbandonedMalformed.Add(1)
	}
}

// WriteMetrics renders the plain-text metric lines; satisfies diag's
// structural MetricSource without an import edge.
func (m *Metrics) WriteMetrics(w io.Writer) {
	fmt.Fprintf(w, "observations_started_total %d\n", m.Started.Load())
	fmt.Fprintf(w, "observations_completed_total{termination=%q} %d\n", TerminationDone, m.CompletedDone.Load())
	fmt.Fprintf(w, "observations_completed_total{termination=%q} %d\n", TerminationEOFWithoutDone, m.CompletedEOFWithoutDone.Load())
	fmt.Fprintf(w, "observations_completed_total{termination=%q} %d\n", TerminationClientDisconnect, m.CompletedClientDisc.Load())
	fmt.Fprintf(w, "observations_completed_total{termination=%q} %d\n", TerminationUpstreamErrorEvent, m.CompletedUpstreamError.Load())
	fmt.Fprintf(w, "observations_skipped_total{reason=%q} %d\n", SkipNoRing, m.SkippedNoRing.Load())
	fmt.Fprintf(w, "observations_skipped_total{reason=%q} %d\n", SkipUpstreamStatus, m.SkippedUpstreamStatus.Load())
	fmt.Fprintf(w, "observations_skipped_total{reason=%q} %d\n", SkipNonMeterableRoute, m.SkippedNonMeterable.Load())
	fmt.Fprintf(w, "observations_abandoned_total{reason=%q} %d\n", AbandonObserverOverrun, m.AbandonedOverrun.Load())
	fmt.Fprintf(w, "observations_abandoned_total{reason=%q} %d\n", AbandonBodyTooLarge, m.AbandonedTooLarge.Load())
	fmt.Fprintf(w, "observations_abandoned_total{reason=%q} %d\n", AbandonEncodedBody, m.AbandonedEncoded.Load())
	fmt.Fprintf(w, "observations_abandoned_total{reason=%q} %d\n", AbandonMaxDepthExceeded, m.AbandonedMaxDepth.Load())
	fmt.Fprintf(w, "observations_abandoned_total{reason=%q} %d\n", AbandonMalformedBody, m.AbandonedMalformed.Load())
	fmt.Fprintf(w, "observations_degraded_total %d\n", m.Degraded.Load())
	fmt.Fprintf(w, "observer_panics_total %d\n", m.ObserverPanics.Load())
}
