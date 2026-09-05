package observe

import (
	"unicode/utf8"
)

// The structural scanner (ADR-0004): fed bytes as they arrive, never
// accumulating the payload. It tracks JSON structure only — a depth stack,
// one bounded key buffer, string/escape state — and captures values only at
// an explicit whitelist of paths, each with its own cap. Content is scanned,
// never stored: bytes outside the whitelist are consumed for their
// structural meaning and immediately discarded. encoding/json is not usable
// here: it needs the whole document, and for a completion the document IS
// the completion text (§6.3); it also brings case-insensitive key matching,
// last-wins duplicates, float64 coercion, and no depth limit.
//
// Peak retention is the scanner's own state: the depth stack, one key
// buffer, at most one whitelisted capture, and the fixed candidate-provider
// table below — a few KiB regardless of response size. Do not "simplify"
// this back to encoding/json; that reintroduces a CRITICAL retention
// finding.

// Per-field capture caps (§6.3). Exceeding a cap discards the value and sets
// degraded — never truncate-and-keep, so a multi-megabyte id is a dropped
// field rather than a retention channel.
const (
	capIDBytes           = 512
	capModelBytes        = 512
	capFinishReasonBytes = 128
	capErrTypeBytes      = 128
	capErrCodeBytes      = 64
	capProviderBytes     = 64
	capIntDigits         = 20
	maxCaptureBytes      = 512 // the largest cap; sizes the shared value buffer
)

// maxCandidateSlots bounds the search profile's candidate-provider table.
// `chosen` names the winning candidate by POSITION, and the two fields may
// arrive in either key order, so the positions seen so far have to be held
// until the document ends. Eight slots of 64 bytes is 512 bytes — the same
// order as the single shared value buffer, fixed at construction: a response
// with more candidates than this loses the table's tail, never memory.
const maxCandidateSlots = 8

// noChosenIndex is `chosen: -1`, the router's way of saying it picked no
// candidate — and also the scanner's "no chosen field was seen". Both mean
// the same thing to a reader: there is no resolved provider.
const noChosenIndex = -1

// maxChosenIndex clamps an absurd chosen value instead of overflowing int.
// Anything this large is past the table regardless.
const maxChosenIndex = 1<<31 - 1

type captureKind uint8

const (
	capNone captureKind = iota
	capID
	capModel
	capFinishReason
	capErrType
	capErrCode
	capPromptTokens
	capCompletionTokens
	capTotalTokens
	capChosen
	capCandidateProvider
)

func (k captureKind) stringCap() int {
	switch k {
	case capID:
		return capIDBytes
	case capModel:
		return capModelBytes
	case capFinishReason:
		return capFinishReasonBytes
	case capErrType:
		return capErrTypeBytes
	case capErrCode:
		return capErrCodeBytes
	case capCandidateProvider:
		return capProviderBytes
	default:
		return 0
	}
}

func (k captureKind) isInt() bool {
	return k == capPromptTokens || k == capCompletionTokens || k == capTotalTokens || k == capChosen
}

// pathNode is a node in the static whitelist tree. A nil node means "unknown
// subtree": structure is still tracked, nothing is captured.
type pathNode struct {
	children map[string]*pathNode
	anyIndex *pathNode
	capture  captureKind
	isError  bool // the $.error node: its presence classifies the outcome
}

// The whitelist. Streaming and non-streaming carry the finish reason at
// different subtrees ($.choices[*].delta vs .message) but finish_reason sits
// on the choice object itself in both, so one entry covers both framings;
// delta, message, tool_calls, and every content-bearing subtree simply have
// no entry. $.error.message is deliberately absent: provider error messages
// routinely echo request content.
var whitelist = &pathNode{children: map[string]*pathNode{
	"id":    {capture: capID},
	"model": {capture: capModel},
	"usage": {children: map[string]*pathNode{
		"prompt_tokens":     {capture: capPromptTokens},
		"completion_tokens": {capture: capCompletionTokens},
		"total_tokens":      {capture: capTotalTokens},
	}},
	"choices": {anyIndex: &pathNode{children: map[string]*pathNode{
		"finish_reason": {capture: capFinishReason},
	}}},
	"error": {isError: true, children: map[string]*pathNode{
		"type": {capture: capErrType},
		"code": {capture: capErrCode},
	}},
}}

// searchWhitelist is the search router's profile: a different response
// shape, the same retention posture. The generation identity is request_id,
// and the resolved provider is candidates[chosen].provider — a positional
// reference, which is why array frames carry an index.
//
// What is deliberately absent is the point of the table. candidates[*] has
// exactly one entry, provider: `answer` is the completion content this proxy
// exists not to retain, and `citations` are derived from it. `usage` has no
// entry either — this API prices in micro-dollars, not tokens, and there is
// no token count anywhere in the response to report. Feeding cost_micros
// into a token counter would be an invented number, and a locally derived
// cost is exactly what §52 forbids: provider-reported values or nothing.
var searchWhitelist = &pathNode{children: map[string]*pathNode{
	"request_id": {capture: capID},
	"chosen":     {capture: capChosen},
	"candidates": {anyIndex: &pathNode{children: map[string]*pathNode{
		"provider": {capture: capCandidateProvider},
	}}},
}}

type scanState uint8

const (
	stStart          scanState = iota // very first bytes: optional BOM, then a value
	stValue                           // expecting a value
	stObjectKeyOrEnd                  // after { : a key or }
	stObjectKey                       // after , in an object: a key
	stColon                           // after a key: expect :
	stObjectNext                      // after a member value: , or }
	stArrayFirst                      // after [ : a value or ] (empty array)
	stArrayNext                       // after an element value: , or ]
	stKeyString                       // inside a key string
	stValueString                     // inside a value string
	stNumber                          // inside a number
	stLiteral                         // inside true/false/null
	stTopDone                         // top-level value complete; whitespace only
	stDead                            // abandoned; consume and ignore
)

type frame struct {
	isArray bool
	node    *pathNode
	// index is the position of the element being scanned, for arrays whose
	// profile resolves a value by position (candidates[chosen]).
	index int
}

// scanResult is what one document scan yields.
type scanResult struct {
	ID, Model, FinishReason, ErrType, ErrCode string
	Usage                                     Usage
	SawUsage                                  bool
	SawFinishReason                           bool
	SawError                                  bool
	Degraded                                  bool
	CredentialRejected                        bool
	Abandoned                                 AbandonReason // "" = not abandoned
	Complete                                  bool          // top-level value complete at finish

	// Chosen is the search profile's chosen index; noChosenIndex both when
	// the router chose nothing and when no such field exists.
	Chosen int
	// ChosenProvider is candidates[Chosen].provider, resolved at finish.
	ChosenProvider string
}

type scanner struct {
	// root is the profile: which whitelist this scanner walks. Everything
	// else — bounds, buffers, structural rules — is shape-independent,
	// because the response layout may vary but the retention posture may
	// not.
	root        *pathNode
	maxDepth    int
	maxKeyBytes int

	state    scanState
	stack    []frame // preallocated to maxDepth; len tracked by depth
	depth    int
	inString struct {
		escape      bool
		unicodeLeft int  // hex digits still expected for \uXXXX
		unicodeVal  rune // accumulated \uXXXX value (≤ 0xFFFF)
		key         bool // capturing a key vs a value
	}
	litRemaining []byte // bytes still expected after a literal's first byte

	keyBuf      []byte // the ONE bounded key buffer (§6.3)
	keyLen      int
	keyOverflow bool
	keyInvalid  bool // invalid escape or invalid UTF-8 in the key

	pending *pathNode // resolved whitelist node for the value about to start

	valKind     captureKind
	valBuf      []byte // shared capture buffer, sized to the largest cap
	valLen      int
	valOverflow bool
	valInvalid  bool // invalid escape / invalid UTF-8 sentinel

	intVal     uint64
	intDigits  int
	intInvalid bool
	intNeg     bool // a leading '-' on a field whose profile allows one

	// candidates is the fixed positional table; candidatesOver records that
	// a candidate fell off its end, so a lost chosen row can be told apart
	// from one that was never there.
	candidates     [maxCandidateSlots]string
	candidatesOver bool

	res scanResult
}

// newScanner builds a scanner over the default chat-completion profile.
func newScanner(maxDepth, maxKeyBytes int) *scanner {
	return newScannerFor(whitelist, maxDepth, maxKeyBytes)
}

// newScannerFor builds a scanner over a specific whitelist root. Adding a
// profile is adding a table, never a second parser: the structural rules,
// the caps, and the abandonment behavior are the reviewed part and stay
// shared.
func newScannerFor(root *pathNode, maxDepth, maxKeyBytes int) *scanner {
	s := &scanner{
		root:        root,
		maxDepth:    maxDepth,
		maxKeyBytes: maxKeyBytes,
		stack:       make([]frame, maxDepth+1),
		keyBuf:      make([]byte, maxKeyBytes),
		valBuf:      make([]byte, maxCaptureBytes),
	}
	s.reset()
	return s
}

// reset prepares the scanner for a fresh document. The fixed buffers are
// reused; nothing from the previous document survives.
func (s *scanner) reset() {
	s.state = stStart
	s.depth = 0
	s.inString = struct {
		escape      bool
		unicodeLeft int
		unicodeVal  rune
		key         bool
	}{}
	s.litRemaining = nil
	s.keyLen, s.keyOverflow, s.keyInvalid = 0, false, false
	s.pending = nil
	s.valKind = capNone
	s.valLen, s.valOverflow, s.valInvalid = 0, false, false
	s.intVal, s.intDigits, s.intInvalid, s.intNeg = 0, 0, false, false
	for i := range s.candidates {
		s.candidates[i] = ""
	}
	s.candidatesOver = false
	s.res = scanResult{Chosen: noChosenIndex}
}

func (s *scanner) abandon(r AbandonReason) {
	if s.res.Abandoned == "" {
		s.res.Abandoned = r
	}
	s.state = stDead
}

func (s *scanner) degrade() { s.res.Degraded = true }

// feed scans one chunk. It never blocks and never allocates on the data
// path.
func (s *scanner) feed(b []byte) {
	for i := 0; i < len(b); i++ {
		if s.state == stDead {
			return
		}
		s.step(b[i])
	}
}

// finish declares end of input.
func (s *scanner) finish() {
	// Resolve first: the positional lookup needs the whole document, and it
	// is meaningful even on the paths below that abandon — an abandoned
	// result is simply never promoted, so the resolution is discarded with
	// it rather than being a second thing to remember.
	s.resolveChosen()
	switch s.state {
	case stTopDone:
		s.res.Complete = true
	case stNumber:
		// A top-level bare number can only complete at EOF.
		s.endNumber()
		if s.state == stTopDone {
			s.res.Complete = true
		} else {
			s.abandon(AbandonMalformedBody)
		}
	case stDead:
	default:
		// Incomplete value at EOF: abandoned rather than tolerated (§6.3).
		s.abandon(AbandonMalformedBody)
	}
}

func (s *scanner) result() scanResult { return s.res }

// resolveChosen turns `chosen` into the winning candidate's provider name.
// A -1 (the router picked nothing) leaves it empty, which is a recorded
// fact rather than a failure — as is a chosen index the document never
// filled in.
func (s *scanner) resolveChosen() {
	i := s.res.Chosen
	switch {
	case i < 0:
		// Nothing chosen: no resolved provider, and nothing was lost.
	case i >= maxCandidateSlots:
		if s.candidatesOver {
			// The chosen row is the part that fell off the fixed table —
			// a dropped field, which is what degraded means.
			s.degrade()
		}
	default:
		s.res.ChosenProvider = s.candidates[i]
	}
}

// putCandidateProvider files a candidate's provider by its position, since
// `chosen` refers to it by index and the two fields may arrive in either
// key order.
func (s *scanner) putCandidateProvider(value string) {
	i := s.enclosingArrayIndex()
	if i < 0 || i >= maxCandidateSlots {
		s.candidatesOver = true
		return
	}
	if s.candidates[i] == "" {
		s.candidates[i] = value // first non-empty wins, as for id/model
	}
}

// enclosingArrayIndex is the position of the innermost enclosing array
// element, or -1 when the value is not inside an array at all.
func (s *scanner) enclosingArrayIndex() int {
	for d := s.depth - 1; d >= 0; d-- {
		if s.stack[d].isArray {
			return s.stack[d].index
		}
	}
	return -1
}

func isWS(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

func (s *scanner) step(c byte) {
	switch s.state {
	case stStart:
		// Skip a UTF-8 BOM bytewise (EF BB BF), then behave as stValue.
		switch c {
		case 0xEF, 0xBB, 0xBF:
			return
		default:
			s.state = stValue
			s.step(c)
		}
	case stValue:
		s.stepValue(c)
	case stObjectKeyOrEnd:
		switch {
		case isWS(c):
		case c == '"':
			s.startKey()
		case c == '}':
			s.pop()
		default:
			s.abandon(AbandonMalformedBody)
		}
	case stObjectKey:
		switch {
		case isWS(c):
		case c == '"':
			s.startKey()
		default:
			s.abandon(AbandonMalformedBody)
		}
	case stColon:
		switch {
		case isWS(c):
		case c == ':':
			// The child node was already resolved when the key string
			// ended; the value start will consume it via takePending.
			s.state = stValue
		default:
			s.abandon(AbandonMalformedBody)
		}
	case stObjectNext:
		switch {
		case isWS(c):
		case c == ',':
			s.state = stObjectKey
		case c == '}':
			s.pop()
		default:
			s.abandon(AbandonMalformedBody)
		}
	case stArrayFirst:
		switch {
		case isWS(c):
		case c == ']':
			s.pop() // empty array
		default:
			s.state = stValue
			s.step(c)
		}
	case stArrayNext:
		switch {
		case isWS(c):
		case c == ',':
			s.advanceArrayIndex()
			s.state = stValue
		case c == ']':
			s.pop()
		default:
			s.abandon(AbandonMalformedBody)
		}
	case stKeyString, stValueString:
		s.stepString(c)
	case stNumber:
		s.stepNumber(c)
	case stLiteral:
		if len(s.litRemaining) > 0 && c == s.litRemaining[0] {
			s.litRemaining = s.litRemaining[1:]
			if len(s.litRemaining) == 0 {
				s.endScalar()
			}
			return
		}
		s.abandon(AbandonMalformedBody)
	case stTopDone:
		if !isWS(c) {
			// Trailing non-whitespace, or a second top-level value (§6.3).
			s.abandon(AbandonMalformedBody)
		}
	case stDead:
	}
}

var (
	litTrue  = []byte("true")
	litFalse = []byte("false")
	litNull  = []byte("null")
)

func (s *scanner) stepValue(c byte) {
	switch c {
	case ' ', '\t', '\n', '\r':
	case '{':
		s.push(false)
	case '[':
		s.push(true)
	case '"':
		s.startValueString()
	case 't':
		s.litRemaining = litTrue[1:]
		s.state = stLiteral
	case 'f':
		s.litRemaining = litFalse[1:]
		s.state = stLiteral
	case 'n':
		s.litRemaining = litNull[1:]
		s.state = stLiteral
	default:
		if c == '-' || (c >= '0' && c <= '9') {
			s.startNumber(c)
			return
		}
		s.abandon(AbandonMalformedBody)
	}
}

// push descends into an object or array. The whitelist node for the new
// frame is whatever the pending key/index resolved to — nil for unknown
// subtrees, whose structure is tracked and whose content is discarded.
func (s *scanner) push(isArray bool) {
	if s.depth >= s.maxDepth {
		// Nesting past max_depth abandons the observation (§6.3,
		// master_plan §8.4).
		s.abandon(AbandonMaxDepthExceeded)
		return
	}
	node := s.takePending()
	if node != nil && node.isError {
		s.res.SawError = true
	}
	s.stack[s.depth] = frame{isArray: isArray, node: node}
	s.depth++
	if isArray {
		// Elements of a whitelisted array resolve through anyIndex; the
		// first position may also be the closing ] of an empty array.
		s.state = stArrayFirst
	} else {
		s.state = stObjectKeyOrEnd
	}
}

func (s *scanner) pop() {
	s.depth--
	if s.depth == 0 {
		s.state = stTopDone
		return
	}
	if s.stack[s.depth-1].isArray {
		s.state = stArrayNext
	} else {
		s.state = stObjectNext
	}
}

// current returns the innermost frame, or nil at top level.
func (s *scanner) current() *frame {
	if s.depth == 0 {
		return nil
	}
	return &s.stack[s.depth-1]
}

// takePending consumes the node resolved for the value that is starting:
// the key's child in an object, anyIndex in an array, the whitelist root at
// top level.
func (s *scanner) takePending() *pathNode {
	if f := s.current(); f != nil {
		if f.isArray {
			if f.node != nil {
				return f.node.anyIndex
			}
			return nil
		}
		node := s.pending
		s.pending = nil
		return node
	}
	// Top level: the document root for this scanner's profile.
	return s.root
}

// advanceArrayIndex counts elements in the innermost array. It is real state
// now rather than the no-op it was when every whitelisted array matched by
// anyIndex alone: the search profile resolves candidates[chosen] by
// position.
func (s *scanner) advanceArrayIndex() {
	if f := s.current(); f != nil && f.isArray {
		f.index++
	}
}

// startKey begins scanning an object key into the single bounded key buffer.
func (s *scanner) startKey() {
	s.keyLen, s.keyOverflow, s.keyInvalid = 0, false, false
	s.inString.escape = false
	s.inString.unicodeLeft = 0
	s.inString.key = true
	s.state = stKeyString
}

// startValueString begins a value string; capture is armed only when the
// resolved node captures a string kind.
func (s *scanner) startValueString() {
	node := s.takePending()
	s.valKind = capNone
	if node != nil && node.capture != capNone && !node.capture.isInt() {
		s.valKind = node.capture
	}
	s.valLen, s.valOverflow, s.valInvalid = 0, false, false
	s.inString.escape = false
	s.inString.unicodeLeft = 0
	s.inString.key = false
	s.state = stValueString
}

func (s *scanner) startNumber(c byte) {
	node := s.takePending()
	s.valKind = capNone
	if node != nil && node.capture.isInt() {
		s.valKind = node.capture
	}
	s.intVal, s.intDigits, s.intInvalid, s.intNeg = 0, 0, false, false
	s.state = stNumber
	if c == '-' && s.valKind == capChosen {
		// chosen is the one signed field in any profile: -1 is the router
		// saying it picked nothing, a value to record rather than degrade.
		// Every other counter keeps the unsigned rule, where a leading '-'
		// is still an invalid integer.
		s.intNeg = true
		return
	}
	s.stepNumber(c)
}

// stepString handles one byte inside a string (key or value), decoding
// escapes into the bounded target buffer when capturing and merely tracking
// state when skipping. Escaped keys are decoded then compared exactly
// (§6.3): {"id": …} matches id.
func (s *scanner) stepString(c byte) {
	in := &s.inString
	if in.unicodeLeft > 0 {
		v, ok := hexVal(c)
		if !ok {
			s.markStringInvalid()
			in.unicodeLeft = 0
			return
		}
		in.unicodeVal = in.unicodeVal<<4 | rune(v)
		in.unicodeLeft--
		if in.unicodeLeft == 0 {
			r := in.unicodeVal
			if r >= 0xD800 && r <= 0xDFFF {
				// Surrogate halves cannot appear in the whitelisted metadata
				// this scanner captures; a sentinel fails validation later
				// without any attempt to normalize (§6.3: never U+FFFD).
				s.markStringInvalid()
				return
			}
			var enc [4]byte
			n := utf8.EncodeRune(enc[:], r)
			s.appendString(enc[:n])
		}
		return
	}
	if in.escape {
		in.escape = false
		switch c {
		case '"':
			s.appendString([]byte{'"'})
		case '\\':
			s.appendString([]byte{'\\'})
		case '/':
			s.appendString([]byte{'/'})
		case 'b':
			s.appendString([]byte{'\b'})
		case 'f':
			s.appendString([]byte{'\f'})
		case 'n':
			s.appendString([]byte{'\n'})
		case 'r':
			s.appendString([]byte{'\r'})
		case 't':
			s.appendString([]byte{'\t'})
		case 'u':
			in.unicodeLeft = 4
			in.unicodeVal = 0
		default:
			// An invalid escape degrades rather than being passed through
			// raw — two readers must not disagree about what the key was.
			s.markStringInvalid()
			s.degrade()
		}
		return
	}
	switch c {
	case '\\':
		in.escape = true
	case '"':
		s.endString()
	default:
		s.appendString([]byte{c})
	}
}

func (s *scanner) markStringInvalid() {
	if s.inString.key {
		s.keyInvalid = true
	} else {
		s.valInvalid = true
	}
}

func (s *scanner) appendString(b []byte) {
	if s.inString.key {
		for _, c := range b {
			if s.keyLen >= s.maxKeyBytes {
				// Past the cap the key stops being retained and its value
				// is treated as an unknown subtree to skip (§6.3).
				s.keyOverflow = true
				return
			}
			s.keyBuf[s.keyLen] = c
			s.keyLen++
		}
		return
	}
	if s.valKind == capNone {
		return // skipped string: bytes discarded, structure only
	}
	limit := s.valKind.stringCap()
	for _, c := range b {
		if s.valLen >= limit {
			s.valOverflow = true
			return
		}
		s.valBuf[s.valLen] = c
		s.valLen++
	}
}

func (s *scanner) endString() {
	if s.inString.key {
		// Resolve the child node for the value that will follow the colon.
		f := s.current()
		s.pending = nil
		if f != nil && !f.isArray && f.node != nil && !s.keyOverflow && !s.keyInvalid {
			s.pending = f.node.children[string(s.keyBuf[:s.keyLen])]
		}
		if s.keyInvalid {
			s.degrade()
		}
		s.state = stColon
		return
	}
	s.commitStringCapture()
	s.endScalar()
}

func (s *scanner) commitStringCapture() {
	kind := s.valKind
	s.valKind = capNone
	if kind == capNone {
		return
	}
	switch {
	case s.valOverflow, s.valInvalid:
		s.degrade()
		return
	case !utf8.Valid(s.valBuf[:s.valLen]):
		// Invalid UTF-8 drops the field — never normalized to U+FFFD, which
		// would silently change a generation ID into a different plausible
		// string (§6.3).
		s.degrade()
		return
	case !printableASCIINoWS(s.valBuf[:s.valLen]):
		// Charset validation: real generation IDs, model slugs, and error
		// codes are printable ASCII without whitespace; prose is not (§6.3).
		s.degrade()
		return
	case looksLikeCredential(s.valBuf[:s.valLen]):
		// A provider has no legitimate reason to return an API key in a
		// metadata field (§6.3).
		s.res.CredentialRejected = true
		s.degrade()
		return
	}
	value := string(s.valBuf[:s.valLen])
	switch kind {
	// First non-empty wins for id/model/finish_reason; a duplicate never
	// overwrites (§6.3 stated policy, not a library default).
	case capID:
		if s.res.ID == "" {
			s.res.ID = value
		}
	case capModel:
		if s.res.Model == "" {
			s.res.Model = value
		}
	case capFinishReason:
		if s.res.FinishReason == "" {
			s.res.FinishReason = value
		}
		s.res.SawFinishReason = true
	case capErrType:
		if s.res.ErrType == "" {
			s.res.ErrType = value
		}
	case capErrCode:
		if s.res.ErrCode == "" {
			s.res.ErrCode = value
		}
	case capCandidateProvider:
		s.putCandidateProvider(value)
	}
}

func (s *scanner) stepNumber(c byte) {
	switch {
	case c >= '0' && c <= '9':
		if s.intDigits >= capIntDigits {
			s.intInvalid = true // digit-count bound: never coerced (§6.3)
			return
		}
		v := s.intVal*10 + uint64(c-'0')
		if v < s.intVal {
			s.intInvalid = true // uint64 overflow
			return
		}
		s.intVal = v
		s.intDigits++
	case c == '-' || c == '+' || c == '.' || c == 'e' || c == 'E':
		// Structurally part of a JSON number, but not an unsigned integer:
		// a whitelisted counter with a fraction or sign degrades, never
		// coerces (§6.3).
		s.intInvalid = true
	default:
		// The number ended at a delimiter; reprocess c in the outer state.
		s.endNumber()
		if s.state != stDead {
			s.step(c)
		}
	}
}

func (s *scanner) endNumber() {
	if s.valKind.isInt() {
		if s.intInvalid || s.intDigits == 0 {
			s.degrade()
		} else {
			// Last-wins PER FIELD for the usage counters: the final chunk
			// carries the authoritative usage, and a partial second usage
			// object cannot zero a field the first one set (§6.3).
			switch s.valKind {
			case capPromptTokens:
				s.res.Usage.PromptTokens = s.intVal
				s.res.SawUsage = true
			case capCompletionTokens:
				s.res.Usage.CompletionTokens = s.intVal
				s.res.SawUsage = true
			case capTotalTokens:
				s.res.Usage.TotalTokens = s.intVal
				s.res.SawUsage = true
			case capChosen:
				switch {
				case s.intNeg:
					s.res.Chosen = noChosenIndex
				case s.intVal > maxChosenIndex:
					s.res.Chosen = maxChosenIndex // clamped; past the table either way
				default:
					s.res.Chosen = int(s.intVal)
				}
			}
		}
	} else if s.intInvalid && s.valKind != capNone {
		s.degrade()
	}
	s.valKind = capNone
	s.endScalar()
}

// endScalar returns to the enclosing container state (or completes the
// document).
func (s *scanner) endScalar() {
	f := s.current()
	switch {
	case f == nil:
		s.state = stTopDone
	case f.isArray:
		s.state = stArrayNext
	default:
		s.state = stObjectNext
	}
}

func hexVal(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

func printableASCIINoWS(b []byte) bool {
	for _, c := range b {
		if c < 0x21 || c > 0x7E {
			return false
		}
	}
	return true
}

// looksLikeCredential implements the same sk-[a-z]+- screen as the log
// redactor, without a regexp on the hot path.
func looksLikeCredential(b []byte) bool {
	for i := 0; i+3 < len(b); i++ {
		if b[i] != 's' || b[i+1] != 'k' || b[i+2] != '-' {
			continue
		}
		j := i + 3
		for j < len(b) && b[j] >= 'a' && b[j] <= 'z' {
			j++
		}
		if j > i+3 && j < len(b) && b[j] == '-' {
			return true
		}
	}
	return false
}
