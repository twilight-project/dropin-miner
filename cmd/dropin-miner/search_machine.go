package main

// `dropin-miner search --stdin`: the stable machine path.
//
// The query arrives as JSON on stdin and never as shell text, which is the
// point of the mode — an agent composing a command line has to escape the
// query correctly every time, and a query is exactly the kind of string
// that contains quotes, backticks, semicolons and newlines. Here it is one
// JSON string, decoded once, forwarded byte-for-byte.
//
// The answer is one envelope. An agent decides what to do next from ok,
// retryable and action, never by reading a message, and never by matching
// on the router's prose.

import (
	"encoding/json"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/twilight-project/dropin-miner/pkg/auth"
)

const (
	machineRequestVersion = 1
	// machineStdinMax bounds the request before any JSON decoding. Read
	// max+1, for the same reason the router's response is: a truncating
	// read would hand the decoder a prefix and call the result a request.
	machineStdinMax = 1 << 20
)

// Stable client codes for a request this client refused to send. All of
// them are exit 2 / usage_error / fix_input: the caller has something to
// correct, and no amount of retrying corrects it.
const (
	codeInputTooLarge      = "input_too_large"
	codeInvalidUTF8        = "invalid_utf8"
	codeInvalidJSON        = "invalid_json"
	codeUnsupportedVersion = "unsupported_version"
	codeUnknownField       = "unknown_field"
	codeMissingQuery       = "missing_query"
	codeEmptyQuery         = "empty_query"
	codeUnexpectedArgument = "unexpected_argument"
	codeInvalidFlags       = "invalid_flags"
)

// inputError is a local refusal with a stable machine code. It is a type
// rather than a string so the classifier reads the code off the value.
type inputError struct {
	Code string
	Msg  string
}

func (e *inputError) Error() string { return e.Msg }

func inputErrorf(code, msg string) *inputError { return &inputError{Code: code, Msg: msg} }

// machineSearchRequest is the v1 request, after validation.
type machineSearchRequest struct {
	query string
	tier  string
}

// wireSearchRequest is the decoded shape. Pointers distinguish an absent
// field from a present empty one, which is the difference between
// missing_query and empty_query.
type wireSearchRequest struct {
	Version *int    `json:"version"`
	Query   *string `json:"query"`
	Tier    *string `json:"tier"`
}

// decodeMachineSearchRequest reads exactly one v1 request from r.
//
// Strict, unlike the router's response, and deliberately so: this is the
// client's own contract with its caller, not someone else's product API.
// Unknown fields are refused, which is also what keeps a caller from
// supplying trace or lineage — those are local host state, decided here,
// and a request that could set them would let the caller forge the
// trajectory a search is attributed to.
func decodeMachineSearchRequest(r io.Reader) (machineSearchRequest, error) {
	raw, err := io.ReadAll(io.LimitReader(r, machineStdinMax+1))
	if err != nil {
		return machineSearchRequest{}, inputErrorf(codeInvalidJSON, "the request could not be read from stdin")
	}
	if int64(len(raw)) > machineStdinMax {
		// Oversized input is a usage error, not a partially parsed request.
		return machineSearchRequest{}, inputErrorf(codeInputTooLarge,
			"the request is larger than the 1 MiB limit")
	}
	if !utf8.Valid(raw) {
		return machineSearchRequest{}, inputErrorf(codeInvalidUTF8, "the request is not valid UTF-8")
	}
	if err := singleJSONObject(raw); err != nil {
		return machineSearchRequest{}, inputErrorf(codeInvalidJSON,
			"the request must be exactly one JSON object with nothing after it")
	}

	// The version is read with a permissive decode first, so a future
	// version is told its version is unsupported rather than being
	// lectured about the new fields that came with it.
	var probe struct {
		Version *int `json:"version"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil || probe.Version == nil {
		return machineSearchRequest{}, inputErrorf(codeUnsupportedVersion,
			"the request needs \"version\": 1")
	}
	if *probe.Version != machineRequestVersion {
		return machineSearchRequest{}, inputErrorf(codeUnsupportedVersion,
			"this client speaks search request version 1")
	}

	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	var wire wireSearchRequest
	if err := dec.Decode(&wire); err != nil {
		return machineSearchRequest{}, inputErrorf(codeUnknownField,
			"the request carries a field this version does not accept")
	}
	if wire.Query == nil {
		return machineSearchRequest{}, inputErrorf(codeMissingQuery, "the request needs a \"query\"")
	}
	// Trimming decides emptiness and nothing else. The query that goes to
	// the router is the caller's bytes, unaltered — leading whitespace,
	// shell metacharacters, combining marks and all.
	if strings.TrimSpace(*wire.Query) == "" {
		return machineSearchRequest{}, inputErrorf(codeEmptyQuery, "the query is empty")
	}
	req := machineSearchRequest{query: *wire.Query}
	if wire.Tier != nil {
		req.tier = *wire.Tier
	}
	return req, nil
}

// ── the envelope ────────────────────────────────────────────────────────

type searchEnvelope struct {
	machineHeader
	HTTPStatus   int            `json:"http_status,omitempty"`
	RequestID    string         `json:"request_id,omitempty"`
	RetryAfterMS *int64         `json:"retry_after_ms,omitempty"`
	Result       *machineResult `json:"result,omitempty"`
	Mining       *machineMining `json:"mining,omitempty"`
	Error        *machineError  `json:"error,omitempty"`
}

// machineResult is the client-owned view of a search: the fields this
// client understands, not a passthrough of whatever the router sent. The
// raw router JSON stays available on the human -format json path for
// callers that deliberately want it.
//
// The query is not echoed. The caller sent it; repeating it back costs
// bytes and puts the one piece of model-influenceable text in this
// envelope somewhere it serves no purpose.
type machineResult struct {
	RequestID  string             `json:"request_id"`
	Chosen     int                `json:"chosen"`
	SessionID  string             `json:"session_id,omitempty"`
	LatencyMS  int64              `json:"latency_ms,omitempty"`
	Candidates []machineCandidate `json:"candidates"`
}

type machineCandidate struct {
	Provider  string            `json:"provider,omitempty"`
	Kind      string            `json:"kind,omitempty"`
	Status    string            `json:"status,omitempty"`
	Chosen    bool              `json:"chosen"`
	Answer    string            `json:"answer,omitempty"`
	Error     string            `json:"error,omitempty"`
	Citations []machineCitation `json:"citations,omitempty"`
}

type machineCitation struct {
	URL     string `json:"url,omitempty"`
	Title   string `json:"title,omitempty"`
	Snippet string `json:"snippet,omitempty"`
}

// machineResultOf is data, not presentation. Answers and snippets are
// carried as the router sent them: JSON already makes control bytes
// syntactically inert, and rewriting a provider's answer to make it
// prettier would corrupt the thing the caller asked for. The terminal
// sanitizing belongs to the human renderer, where a control byte would
// actually do something.
func machineResultOf(s routerSuccess) *machineResult {
	r := s.Response
	out := &machineResult{
		RequestID:  s.RequestID,
		Chosen:     r.Chosen,
		LatencyMS:  r.Usage.LatencyMS,
		Candidates: make([]machineCandidate, 0, len(r.Candidates)),
	}
	if r.Session != nil {
		out.SessionID = r.Session.ID
	}
	for i, c := range r.Candidates {
		mc := machineCandidate{
			Provider: c.Provider,
			Kind:     c.Kind,
			Status:   c.Status,
			Chosen:   i == r.Chosen,
			Answer:   c.Answer,
			Error:    c.Error,
		}
		for _, cit := range c.Citations {
			// A conversion, not a field-by-field copy, on purpose: if the
			// router type ever grows a field, this stops compiling and
			// someone has to decide whether the machine contract should
			// carry it. A literal would silently drop it instead.
			mc.Citations = append(mc.Citations, machineCitation(cit))
		}
		out.Candidates = append(out.Candidates, mc)
	}
	return out
}

// machineMining is the economic half of the answer, and it is separate
// from ok on purpose: a successful search with mining disabled, undecided
// or degraded is still a successful search. Nothing here is called
// "earned" — the AS reconciles observations against the provider, and this
// client never claims a reward on its own say-so.
type machineMining struct {
	// State is the persisted runtime decision, in its own canonical
	// spelling: enabled, disabled, undecided or degraded.
	State string `json:"state"`
	// Configured is [miner] enabled — whether intake exists at all.
	Configured bool `json:"configured"`
	// Recorded says whether THIS search's observation was written. False
	// with State "enabled" means capture failed; Health says why.
	Recorded bool            `json:"recorded"`
	Detail   string          `json:"detail,omitempty"`
	Health   []machineHealth `json:"health,omitempty"`
}

type machineHealth struct {
	Component string `json:"component"`
	Reason    string `json:"reason"`
	Detail    string `json:"detail,omitempty"`
}

func healthOf(records []auth.HealthRecord) []machineHealth {
	if len(records) == 0 {
		return nil
	}
	out := make([]machineHealth, 0, len(records))
	for _, r := range records {
		out = append(out, machineHealth{
			Component: string(r.Component),
			Reason:    string(r.Reason),
			Detail:    boundMessage(r.Detail),
		})
	}
	return out
}

// ── classification ──────────────────────────────────────────────────────

// searchClassification is the recovery advice for one outcome. It is
// computed from the fault value and the HTTP status, and from nothing
// else — in particular, never from the text of an error or of the
// router's "error" field.
type searchClassification struct {
	ExitCode  int
	Code      string
	Retryable bool
	Action    string
}

func classifySearch(out searchOutcome) searchClassification {
	switch out.Fault {
	case faultNone:
		return searchClassification{exitOK, "ok", false, actionNone}
	case faultNoCredential:
		return searchClassification{exitClientErr, string(faultNoCredential), false, actionConnect}
	case faultTimeout:
		return searchClassification{exitTransport, string(faultTimeout), true, actionRetry}
	case faultCanceled:
		// A person stopped this. Telling an agent to retry it would
		// undo the interruption.
		return searchClassification{exitTransport, string(faultCanceled), false, actionNone}
	case faultTransport:
		return searchClassification{exitTransport, "transport_failed", true, actionRetry}
	case faultOversized, faultInvalidResponse, faultMissingID:
		// A 2xx the client could not use. Not a safe automatic replay
		// signal: the request may well have been served and billed, and
		// repeating it would not make the answer parseable.
		return searchClassification{exitServerErr, string(out.Fault), false, actionReport}
	}
	return classifyRouterStatus(out)
}

func classifyRouterStatus(out searchOutcome) searchClassification {
	c := searchClassification{Code: routerCodeFor(out)}
	switch {
	case out.HTTPStatus >= 500:
		c.ExitCode, c.Retryable, c.Action = exitServerErr, true, actionRetry
	case out.HTTPStatus == 401:
		// 401 means this key is not accepted. It does NOT mean "never
		// registered" — a registered installation with an expired or
		// rotated key gets exactly this, and sending it to connect would
		// start a second registration for one participant. connect is for
		// the registration/claim workflow; this is a credential.
		c.ExitCode, c.Retryable, c.Action = exitClientErr, false, actionLogin
	case out.HTTPStatus == 403:
		c.ExitCode, c.Retryable, c.Action = exitClientErr, false, actionCheckAccess
	case out.HTTPStatus == 429:
		c.ExitCode, c.Retryable, c.Action = exitClientErr, true, actionRetry
	case requestFixable(out.HTTPStatus):
		c.ExitCode, c.Retryable, c.Action = exitClientErr, false, actionFixInput
	default:
		// Every other 4xx. Not retryable and not the caller's input
		// either — a 404 or a 409 from the search endpoint is a
		// deployment or state problem a caller cannot edit its way out
		// of, and saying fix_input would send it round a loop.
		c.ExitCode, c.Retryable, c.Action = exitClientErr, false, actionReport
	}
	return c
}

// requestFixable lists the statuses that name something the SEARCH CALLER
// can actually change: the request it composed.
//
// 400, 413 and 422 are about the query, the tier, or how much of them
// there is — a caller can send a different one. The rest of the 4xx range
// is not. A 405, 406, 411, 414, 415 or 431 is about the method, the Accept
// header, the framing, the endpoint URI or the headers, and every one of
// those is chosen by this client, not by its caller. Telling an agent to
// fix_input for them sends it round a loop editing a query that was never
// the problem, so they report instead.
func requestFixable(status int) bool {
	switch status {
	case 400, 413, 422:
		return true
	default:
		return false
	}
}

// routerCodeFor prefers the search host's own machine code, so an agent
// sees a stable identifier the router owns. It is accepted only when it
// looks like a code: an arbitrary human sentence in that field must not
// become one, which is the same rule that keeps prose out of action.
func routerCodeFor(out searchOutcome) string {
	if out.HasRouterErr {
		if code, ok := machineCodeLike(out.RouterErr.Code); ok {
			return code
		}
	}
	switch {
	case out.HTTPStatus >= 500:
		return "router_error"
	case out.HTTPStatus == 401:
		return "unauthorized"
	case out.HTTPStatus == 403:
		return "forbidden"
	case out.HTTPStatus == 429:
		return "rate_limited"
	default:
		return "router_rejected"
	}
}

const routerCodeCap = 64

func machineCodeLike(s string) (string, bool) {
	if s == "" || len(s) > routerCodeCap {
		return "", false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-', r == '.':
		default:
			return "", false
		}
	}
	return s, true
}

// ── assembling the answer ───────────────────────────────────────────────

// searchEnvelopeOf builds the whole answer from the structured outcome.
// It reads the normalized request id off the outcome rather than the raw
// body, so the identity in the envelope is the identity the observation
// was recorded against.
func searchEnvelopeOf(out searchOutcome, mining *machineMining) (searchEnvelope, int) {
	c := classifySearch(out)
	env := searchEnvelope{
		machineHeader: newMachineHeader("search", c.ExitCode, c.Code, c.Retryable, c.Action),
		HTTPStatus:    out.HTTPStatus,
		Mining:        mining,
	}
	if out.HasRetryAfter && (c.Retryable || out.HTTPStatus == 429) {
		ms := out.RetryAfterMS
		env.RetryAfterMS = &ms
	}
	if out.ok() {
		env.RequestID = out.Success.RequestID
		env.Result = machineResultOf(out.Success)
		return env, c.ExitCode
	}
	if out.HasRouterErr {
		env.Error = routerMessage(out.RouterErr.Error)
	}
	if env.Error == nil {
		env.Error = clientMessage(out.Err)
	}
	return env, c.ExitCode
}

// failSearchInput answers a request this client refused to send. It never
// reaches the router, so there is no HTTP status and no mining state to
// report.
func failSearchInput(stdout io.Writer, err *inputError) int {
	env := searchEnvelope{
		machineHeader: newMachineHeader("search", exitUsage, err.Code, false, actionFixInput),
		Error:         clientMessage(err),
	}
	emitMachine(stdout, env)
	return exitUsage
}

// failSearchSetup answers a local failure that stopped the search before
// any request: an unreadable config, no router, no credential. The exit
// code is the one this command has always used for that state; only the
// explanation is new.
func failSearchSetup(stdout io.Writer, exitCode int, code, action string, err error) int {
	env := searchEnvelope{
		machineHeader: newMachineHeader("search", exitCode, code, false, action),
		Error:         clientMessage(err),
	}
	emitMachine(stdout, env)
	return exitCode
}
