package main

// The search host's wire protocol, as much of it as this client requires.
//
// A search is a bounded operation: one finite deadline covers the whole of
// it, every body the client inspects is read against a ceiling, and a 2xx
// is not a success until it has been shown to be one. Those three rules
// are here rather than inline in search.go because each of them is a
// guarantee a test has to be able to drive on its own.
//
// The asymmetry with AGENTS.md invariant 6 is deliberate and unchanged: the
// router's *response* is decoded permissively, because we do not own the
// router's product API and an unknown field is not an error. What is
// enforced is only what the client itself must have to do its job — one
// JSON object, and a request identity to record the observation against.

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// defaultSearchTimeout is the whole-operation budget for one search: not
// per connection, not per attempt, and emphatically not per retry. A
// search with no deadline at all is the state this replaced — an agent
// that shells out to a hung router waits forever, and the one thing a
// tool invoked by an agent must never do is never return.
const defaultSearchTimeout = 60 * time.Second

// searchMaxBody is the ceiling on any router body the client reads. It is
// a var so a test can lower it and exercise the boundary without moving
// 32 MiB through a loopback server; production never assigns it.
var searchMaxBody int64 = 32 << 20

// traceUnsupportedCode is the ONE router answer that licenses a second
// POST. Status alone never does — see searchTraceUnsupported.
const traceUnsupportedCode = "trace_unsupported"

// searchTransport is a package-local seam: nil in production, so the
// search client uses net/http's default transport. A test sets it to
// inspect the deadline each request carries, which a loopback server
// cannot show and a sleep could only approximate.
var searchTransport http.RoundTripper

// Why a body the client read is not usable. These are values rather than
// message text because the machine envelope classifies from them.
var (
	errBodyOversized = errors.New("the router's response exceeds the response ceiling")
	errBodyNotJSON   = errors.New("the router's response is not a single JSON object")
	errBodyTrailing  = errors.New("the router's response carries more than one JSON value")
	errNoRequestID   = errors.New("the router's response carries no request identity")
)

// readBoundedBody reads up to max bytes and refuses anything longer.
//
// The max+1 read is the whole point: io.LimitReader(body, max) cannot tell
// a body that is exactly max bytes from one that was cut off at max, so a
// client using it parses a truncated prefix and calls the result a
// success. Reading one byte past the ceiling makes "too long" observable.
func readBoundedBody(r io.Reader, max int64) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		// An interrupted body — a short read against a longer declared
		// Content-Length — arrives here as io.ErrUnexpectedEOF and is a
		// failure, never a success over the prefix that did arrive.
		return nil, err
	}
	if int64(len(raw)) > max {
		return nil, errBodyOversized
	}
	return raw, nil
}

// singleJSONObject reports whether raw is exactly one JSON value, that
// value is an object, and nothing but whitespace follows it.
//
// Trailing data is refused rather than ignored. encoding/json's Unmarshal
// would accept `{"a":1}` and stop, leaving whatever followed unread; a
// response that carries a second value is one this client does not
// understand, and quietly reading the first half of it is how a framing
// bug becomes a wrong answer.
func singleJSONObject(raw []byte) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	var v json.RawMessage
	if err := dec.Decode(&v); err != nil {
		return errBodyNotJSON
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errBodyTrailing
	}
	if t := bytes.TrimSpace(v); len(t) == 0 || t[0] != '{' {
		return errBodyNotJSON
	}
	return nil
}

// routerSuccess is a 2xx that has been shown to satisfy the contract.
type routerSuccess struct {
	Response  routerResponse
	RequestID string
	Raw       []byte
}

// decodeRouterSuccess turns a 2xx body into a success, or says why it is
// not one.
//
// The body's own request_id wins over the header: the header is the
// transport's label for the exchange, the body is the search host's label
// for the search, and when both are present it is the latter the AS
// reconciles against. The header stands in only when the body has no id
// at all, which is the one case §7 has it cover.
//
// Unknown fields stay legal. DisallowUnknownFields here would make every
// router feature addition a client outage, which is exactly the coupling
// invariant 6 exists to prevent.
func decodeRouterSuccess(raw []byte, headerRequestID string) (routerSuccess, error) {
	if err := singleJSONObject(raw); err != nil {
		return routerSuccess{}, err
	}
	var parsed routerResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return routerSuccess{}, errBodyNotJSON
	}
	id := strings.TrimSpace(parsed.RequestID)
	if id == "" {
		id = strings.TrimSpace(headerRequestID)
	}
	if id == "" {
		// No identity means nothing to record the observation against and
		// nothing to quote back to support. Inventing one would put a
		// fabricated id into mining evidence.
		return routerSuccess{}, errNoRequestID
	}
	// Carry the effective identity in the decoded value too, so everything
	// downstream — rendering, intake, the machine envelope — reads one
	// field and cannot disagree about which id this search had.
	parsed.RequestID = id
	return routerSuccess{Response: parsed, RequestID: id, Raw: raw}, nil
}

// searchHostError is the search host's FLAT error envelope:
//
//	{"code":"trace_unsupported","error":"trace is not accepted here"}
//
// It is not the AS's nested {"error":{"code":…,"message":…}} envelope, and
// the two must not be conflated: a decoder that accepted both would let an
// AS-shaped body supply a search-host code. Anything that is not this
// exact shape simply yields ok=false, and the caller falls back to the
// HTTP status.
type searchHostError struct {
	Code  string `json:"code"`
	Error string `json:"error"`
}

func decodeSearchHostError(raw []byte) (searchHostError, bool) {
	if err := singleJSONObject(raw); err != nil {
		return searchHostError{}, false
	}
	var e searchHostError
	if err := json.Unmarshal(raw, &e); err != nil {
		return searchHostError{}, false
	}
	return e, true
}

// searchTraceUnsupported reports whether this response is the search
// host's explicit statement that it does not accept a trace.
//
// The code is mandatory. The behavior this replaced inferred trace
// incompatibility from a bare 400 or 422, which meant an invalid query, an
// unknown tier, or any other ordinary rejection bought the participant a
// second billable POST carrying the same query. Prose is not consulted
// either: a router that merely mentions the phrase in its "error" string
// has not said the field is unsupported.
func searchTraceUnsupported(status int, raw []byte, bodyErr error) bool {
	if bodyErr != nil {
		return false
	}
	if status != http.StatusBadRequest && status != http.StatusUnprocessableEntity {
		return false
	}
	e, ok := decodeSearchHostError(raw)
	return ok && e.Code == traceUnsupportedCode
}

// retryAfterMS reads RFC 7231's Retry-After in either form. An unparsable
// or negative value is reported as unknown rather than guessed at, and a
// date already in the past becomes zero rather than a negative wait.
func retryAfterMS(h http.Header, now time.Time) (int64, bool) {
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs < 0 {
			return 0, false
		}
		return int64(secs) * 1000, true
	}
	if t, err := http.ParseTime(v); err == nil {
		d := t.Sub(now)
		if d < 0 {
			d = 0
		}
		return d.Milliseconds(), true
	}
	return 0, false
}
