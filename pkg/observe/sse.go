package observe

import "bytes"

// The SSE framer (§6.4): an incremental line framer, NOT bufio.Scanner —
// its 64 KiB token cap would silently truncate a large event, and raising
// Buffer() only moves the cliff. The framer finds line boundaries and field
// prefixes; it does not accumulate event payloads. data: bytes stream
// straight into the structural scanner, which is reset at each event
// boundary, so an event carrying a megabyte of completion text is scanned a
// chunk at a time and never assembled anywhere.
//
// Per the SSE spec an event may carry several data: lines whose values join
// with a single \n, and the event fires at the blank line. The framer feeds
// a literal \n between continuation lines — insignificant whitespace between
// JSON tokens — so a multi-line payload streams through correctly without a
// buffer. [DONE] is by construction a single-line event.

const classifyBytes = 16 // the only payload bytes ever held

var doneMarker = []byte("[DONE]")

type sseState uint8

const (
	sseLineStart sseState = iota
	sseFieldName
	sseDataStart // just after "data:", one optional leading space
	sseDataBytes
	sseSkipLine // comments, non-data fields, skipped data
)

type sseFramer struct {
	scan          *scanner
	maxEventBytes int64
	maxEvents     int64

	state    sseState
	lineLen  int64
	fieldBuf [8]byte
	fieldLen int

	// Per-event state.
	eventHasData bool
	eventJSON    bool  // classified as a JSON payload; scanner active
	eventSkipped bool  // classified junk or over-budget; discarding
	eventBytes   int64 // data payload bytes in this event (work limit)
	classify     [classifyBytes]byte
	classifyLen  int
	classifying  bool // first data line, first bytes: decision pending
	continuation bool // a previous data line exists in this event

	events    int64
	prevCR    bool
	doneSeen  bool
	errorSeen bool
	abort     AbandonReason

	merge   streamMerge
	oneByte [1]byte
}

// streamMerge applies the §6.4 metadata rules across events: id/model first
// non-empty wins with MetadataConflict on disagreement; usage last-non-nil
// wins; finish_reason first-non-nil wins.
type streamMerge struct {
	id, model        string
	conflict         bool
	usage            *Usage
	finishReason     string
	errType, errCode string
	sawUsage         bool
	sawFinishReason  bool
	degraded         bool
}

func (m *streamMerge) apply(r scanResult) {
	if m.id == "" {
		m.id = r.ID
	} else if r.ID != "" && r.ID != m.id {
		m.conflict = true
	}
	if m.model == "" {
		m.model = r.Model
	} else if r.Model != "" && r.Model != m.model {
		m.conflict = true
	}
	if r.SawUsage {
		u := r.Usage
		m.usage = &u
		m.sawUsage = true
	}
	if m.finishReason == "" && r.SawFinishReason {
		m.finishReason = r.FinishReason
	}
	if r.SawFinishReason {
		m.sawFinishReason = true
	}
	if r.ErrType != "" && m.errType == "" {
		m.errType = r.ErrType
	}
	if r.ErrCode != "" && m.errCode == "" {
		m.errCode = r.ErrCode
	}
	if r.Degraded {
		m.degraded = true
	}
}

func newSSEFramer(maxDepth, maxKeyBytes int, maxEventBytes, maxEvents int64) *sseFramer {
	return &sseFramer{
		scan:          newScanner(maxDepth, maxKeyBytes),
		maxEventBytes: maxEventBytes,
		maxEvents:     maxEvents,
	}
}

func (f *sseFramer) consume(b []byte) {
	for i := 0; i < len(b); i++ {
		if f.abort != "" || f.doneSeen || f.errorSeen {
			return
		}
		c := b[i]
		switch {
		case f.prevCR && c == '\n':
			f.prevCR = false // the LF of a CRLF; line already ended at CR
		case c == '\r':
			f.endLine()
			f.prevCR = true
		case c == '\n':
			f.endLine()
			f.prevCR = false
		default:
			f.prevCR = false
			f.lineLen++
			f.stepChar(c)
		}
	}
}

func (f *sseFramer) stepChar(c byte) {
	switch f.state {
	case sseLineStart:
		if c == ':' {
			// Comment/keepalive line: ignored, bytes skipped (A-2).
			f.state = sseSkipLine
			return
		}
		f.fieldLen = 0
		f.state = sseFieldName
		f.stepChar(c)
	case sseFieldName:
		if c == ':' {
			if string(f.fieldBuf[:f.fieldLen]) == "data" {
				f.state = sseDataStart
			} else {
				// event:, id:, retry:, anything else — bytes skipped.
				f.state = sseSkipLine
			}
			return
		}
		if f.fieldLen < len(f.fieldBuf) {
			f.fieldBuf[f.fieldLen] = c
			f.fieldLen++
		} else {
			f.state = sseSkipLine // longer than any field we handle
		}
	case sseDataStart:
		f.beginDataLine()
		f.state = sseDataBytes
		if c == ' ' {
			return // one optional leading space is stripped
		}
		f.stepChar(c)
	case sseDataBytes:
		f.dataByte(c)
	case sseSkipLine:
	}
}

// beginDataLine runs once per data: line, before its first payload byte.
func (f *sseFramer) beginDataLine() {
	f.eventHasData = true
	if f.eventJSON && f.continuation {
		// The join rule: continuation values are separated by a single \n,
		// fed through rather than assembled.
		f.oneByte[0] = '\n'
		f.scan.feed(f.oneByte[:])
	}
	if !f.eventJSON && !f.eventSkipped && !f.continuation {
		f.classifying = true
		f.classifyLen = 0
	}
	f.continuation = true
}

func (f *sseFramer) dataByte(c byte) {
	f.eventBytes++
	if f.eventBytes > f.maxEventBytes {
		// A work limit, not a retention limit: degrade this one event and
		// skip its remaining bytes (§6.4).
		if !f.eventSkipped {
			f.eventSkipped = true
			f.merge.degraded = true
			f.classifying = false
		}
		return
	}
	if f.eventSkipped {
		return
	}
	if f.classifying {
		if f.classifyLen == 0 && c == '{' {
			// A JSON payload: start a scanner pass.
			f.classifying = false
			f.eventJSON = true
			f.scan.reset()
			f.feedScanner(c)
			return
		}
		if f.classifyLen < classifyBytes {
			f.classify[f.classifyLen] = c
			f.classifyLen++
			return
		}
		// Longer than the classification window and not JSON: skipped.
		f.classifying = false
		f.eventSkipped = true
		f.merge.degraded = true
		return
	}
	if f.eventJSON {
		f.feedScanner(c)
	}
}

func (f *sseFramer) feedScanner(c byte) {
	f.oneByte[0] = c
	f.scan.feed(f.oneByte[:])
}

func (f *sseFramer) endLine() {
	blank := f.lineLen == 0
	if f.state == sseDataBytes && f.classifying {
		// A short non-JSON payload line: [DONE] terminates; anything else
		// is skipped with ParseDegraded (§6.4).
		f.classifying = false
		if bytes.Equal(f.classify[:f.classifyLen], doneMarker) {
			f.doneSeen = true
		} else {
			f.eventSkipped = true
			f.merge.degraded = true
		}
	}
	f.lineLen = 0
	f.state = sseLineStart
	if blank {
		f.finishEvent()
	}
}

// finishEvent runs at the blank-line delimiter.
func (f *sseFramer) finishEvent() {
	if !f.eventHasData {
		return // consecutive blank lines dispatch nothing
	}
	f.events++
	if f.events > f.maxEvents {
		f.abort = AbandonBodyTooLarge // a pathological stream (§6.4)
		return
	}
	if f.eventJSON {
		f.scan.finish()
		r := f.scan.result()
		switch {
		case r.Abandoned == AbandonMaxDepthExceeded:
			f.abort = AbandonMaxDepthExceeded
		case r.Abandoned != "" || !r.Complete:
			// A structurally invalid payload aborts THIS event's scanner
			// pass and resynchronizes at the next blank line; one bad event
			// does not destroy an otherwise good observation (§6.4).
			f.merge.degraded = true
		default:
			f.merge.apply(r)
			if r.SawError {
				f.errorSeen = true
			}
		}
	}
	f.eventHasData = false
	f.eventJSON = false
	f.eventSkipped = false
	f.eventBytes = 0
	f.continuation = false
}

// sseParser adapts the framer to the observer loop.
type sseParser struct {
	f *sseFramer
}

func newSSEParser(maxDepth, maxKeyBytes int, maxEventBytes, maxEvents int64) *sseParser {
	return &sseParser{f: newSSEFramer(maxDepth, maxKeyBytes, maxEventBytes, maxEvents)}
}

func (p *sseParser) consume(b []byte) { p.f.consume(b) }

func (p *sseParser) abortReason() (AbandonReason, bool) {
	if p.f.abort != "" {
		return p.f.abort, true
	}
	return "", false
}

func (p *sseParser) terminated() bool { return p.f.doneSeen || p.f.errorSeen }

func (p *sseParser) finalize(atEOF bool, finalErr error, obsv *Observation) Outcome {
	f := p.f
	if atEOF && f.eventHasData {
		// Truncated mid-event: the partial event's captures are discarded
		// and the observation is degraded, not poisoned.
		f.merge.degraded = true
	}
	m := &f.merge
	obsv.GenerationID = m.id
	obsv.ResolvedModel = m.model
	obsv.FinishReason = m.finishReason
	obsv.ErrorType = m.errType
	obsv.ErrorCode = m.errCode
	obsv.SawDone = f.doneSeen
	obsv.SawUsage = m.sawUsage
	obsv.SawFinishReason = m.sawFinishReason
	obsv.ParseDegraded = m.degraded
	obsv.MetadataConflict = m.conflict
	if m.usage != nil {
		u := *m.usage
		obsv.Usage = &u
	}

	switch {
	case f.doneSeen:
		return Complete(TerminationDone)
	case f.errorSeen:
		return Complete(TerminationUpstreamErrorEvent)
	case isClientDisconnect(finalErr):
		return Complete(TerminationClientDisconnect)
	default:
		// A stream ending at EOF without [DONE]: a real, recorded outcome.
		// Whether it is ever promoted is D-19, and no rule exists here.
		return Complete(TerminationEOFWithoutDone)
	}
}
