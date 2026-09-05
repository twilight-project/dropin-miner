package observe

// bodyParser is what the observer drains the ring into. Three
// implementations: the non-streaming JSON parser (Phase 2), the SSE parser
// (Phase 3), and the search-router profile of the JSON parser — all feeding
// the same structural scanner underneath (§6.3).
type bodyParser interface {
	// consume scans one drained chunk. Never blocks, never accumulates.
	consume(b []byte)
	// abortReason reports a mid-stream fatal condition (depth, event
	// budget) that abandons the observation.
	abortReason() (AbandonReason, bool)
	// terminated reports that the parser saw a terminal event ([DONE], a
	// provider error event) and the observation can finalize before EOF.
	terminated() bool
	// finalize fills obsv's metadata fields and classifies the outcome.
	// atEOF is false when terminated() ended the observation early.
	finalize(atEOF bool, finalErr error, obsv *Observation) Outcome
}

// jsonParser is the non-streaming path: the body is one JSON document,
// scanned incrementally and finalized at EOF (§6.6).
type jsonParser struct {
	scan *scanner
}

func newJSONParser(maxDepth, maxKeyBytes int) *jsonParser {
	return &jsonParser{scan: newScanner(maxDepth, maxKeyBytes)}
}

func (p *jsonParser) consume(b []byte) { p.scan.feed(b) }

func (p *jsonParser) abortReason() (AbandonReason, bool) {
	if r := p.scan.result(); r.Abandoned == AbandonMaxDepthExceeded {
		return AbandonMaxDepthExceeded, true
	}
	return "", false
}

func (p *jsonParser) terminated() bool { return false }

func (p *jsonParser) finalize(atEOF bool, finalErr error, obsv *Observation) Outcome {
	if atEOF {
		p.scan.finish()
	}
	r := p.scan.result()
	obsv.GenerationID = r.ID
	obsv.ResolvedModel = r.Model
	obsv.FinishReason = r.FinishReason
	obsv.ErrorType = r.ErrType
	obsv.ErrorCode = r.ErrCode
	obsv.SawFinishReason = r.SawFinishReason
	obsv.ParseDegraded = r.Degraded
	if r.SawUsage {
		obsv.SawUsage = true
		u := r.Usage
		obsv.Usage = &u
	}

	switch {
	case isClientDisconnect(finalErr):
		// The generation may well be real and billable — which is precisely
		// why the proxy classifies the termination and stops; promotion is
		// D-19 (§6.5).
		return Complete(TerminationClientDisconnect)
	case r.Abandoned != "":
		return Abandoned(r.Abandoned)
	case !r.Complete:
		return Abandoned(AbandonMalformedBody)
	case r.SawError:
		return Complete(TerminationUpstreamErrorEvent)
	default:
		obsv.SawDone = true // the non-streaming equivalent: a complete body
		return Complete(TerminationDone)
	}
}

// searchParser is the search router's profile of the non-streaming path
// (POST /v1/search). It reuses jsonParser's mechanics unchanged — same
// scanner, same bounds, same abort rules — and differs in exactly two
// places: the whitelist the scanner walks, and how a finished scan maps
// onto an Observation. Anything that could stall or grow the observer is in
// the shared half, so it stays reviewed once.
type searchParser struct {
	jsonParser
	// headerID is the response's X-Request-Id, already screened, read on
	// the request goroutine and passed by value: once Install returns, the
	// response belongs to the copy loop and this goroutine must not touch
	// it.
	headerID string
}

func newSearchParser(maxDepth, maxKeyBytes int, headerID string) *searchParser {
	return &searchParser{
		jsonParser: jsonParser{scan: newScannerFor(searchWhitelist, maxDepth, maxKeyBytes)},
		headerID:   headerID,
	}
}

func (p *searchParser) finalize(atEOF bool, finalErr error, obsv *Observation) Outcome {
	if atEOF {
		p.scan.finish()
	}
	r := p.scan.result()

	// The header is preferred over the body's request_id: it is the same
	// value by the API's own contract, it is already in hand before a
	// single body byte is read, and it survives a body the observer had to
	// stop reading. A body that disagrees is not overridden silently —
	// disagreement between two sources for one field is exactly what
	// MetadataConflict records.
	id := p.headerID
	switch {
	case id == "":
		id = r.ID
	case r.ID != "" && r.ID != id:
		obsv.MetadataConflict = true
	}
	obsv.GenerationID = id
	// The provider that actually answered, resolved through chosen. Empty
	// when the router chose nothing (chosen == -1), which is a real outcome
	// and not a parse failure.
	obsv.ResolvedModel = r.ChosenProvider
	obsv.ParseDegraded = r.Degraded
	// Usage stays nil, and the token counters stay zero. This API prices in
	// micro-dollars and reports no token count anywhere, so there is
	// nothing provider-reported to put in a token field; deriving one would
	// be the local estimation the proxy is forbidden to do. cost_micros is
	// left on the wire rather than converted, because a locally computed
	// cost is not a provider-reported cost.

	switch {
	case isClientDisconnect(finalErr):
		// The search may well have run and been billed — which is why the
		// proxy classifies the termination and stops; promotion is D-19.
		return Complete(TerminationClientDisconnect)
	case r.Abandoned != "":
		return Abandoned(r.Abandoned)
	case !r.Complete:
		return Abandoned(AbandonMalformedBody)
	default:
		obsv.SawDone = true // the non-streaming equivalent: a complete body
		return Complete(TerminationDone)
	}
}
