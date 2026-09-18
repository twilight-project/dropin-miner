package trajectory

import (
	"encoding/json"
	"strings"
)

// LossReason says why a search could not be tied to a router request by
// reading its own result (anchoring path 1). Each is decided from structure;
// none is inferred from prose.
type LossReason string

const (
	// LossNoResult: the call has no result in its turn — the turn was
	// interrupted, superseded or still open before the tool returned.
	LossNoResult LossReason = "no_result"
	// LossDeniedByHost: the host refused to run the call (a permission rule,
	// the participant's rejection, the auto-mode classifier). No search ran.
	LossDeniedByHost LossReason = "denied_by_host"
	// LossSearchFailed: the tool reported an error, or this client's envelope
	// says ok=false and carries no request id.
	LossSearchFailed LossReason = "search_failed"
	// LossHumanFormat: the call asked for the human rendering, which prints
	// no request id. The anchor was never in the transcript to lose.
	LossHumanFormat LossReason = "human_format"
	// LossOutputElsewhere: the search's output was piped into another command
	// or redirected to a file, so the model — and the transcript — saw that
	// command's output instead. The id went where the output went.
	LossOutputElsewhere LossReason = "output_elsewhere"
	// LossResultTruncated: the result starts as a JSON document and does not
	// parse to the end — the host cut it.
	LossResultTruncated LossReason = "result_truncated"
	// LossNoRequestID: a JSON document parsed and no request id is in it.
	LossNoRequestID LossReason = "no_request_id"
	// LossResultNotJSON: JSON was asked for and something else came back.
	LossResultNotJSON LossReason = "result_not_json"
)

// Search is one tool call that runs this client's search, and what its
// result gave back. One call may run the search more than once (a chained
// command line), so both counts are kept.
type Search struct {
	ToolUseID   string
	Tool        string
	Invocations []SearchInvocation
	CallLine    int
	HasResult   bool
	ResultLine  int
	// RequestIDs are the router request ids read from the result, in order.
	RequestIDs []string
	// Loss is set exactly when RequestIDs is empty.
	Loss LossReason
}

// Anchored reports whether path 1 tied this search to a router request.
func (s *Search) Anchored() bool { return len(s.RequestIDs) > 0 }

// searchResultDoc covers both documents a search can print: this client's
// machine envelope (request_id at the top and again under result) and the
// router's own JSON under -format json (request_id at the top). It is decoded
// permissively and only for these fields — the answer text is never bound.
type searchResultDoc struct {
	OK        *bool  `json:"ok"`
	RequestID string `json:"request_id"`
	Result    *struct {
		RequestID string `json:"request_id"`
	} `json:"result"`
}

type scrape struct {
	ids       []string
	docs      int  // JSON documents that parsed
	notOK     bool // some parsed document said ok=false
	jsonStart bool // the text begins like a JSON document
}

// scrapeRequestIDs reads the text a search returned as a sequence of JSON
// documents and collects their request ids. The text is already unescaped —
// it came out of the transcript's own JSON decoding — which is the step a
// pattern match over the raw file skips, and why such a match finds nothing:
// on disk the result is a JSON document inside a JSON string.
func scrapeRequestIDs(text string) scrape {
	// Windows PowerShell 5.1 puts a byte-order mark in front of what it writes.
	text = strings.TrimSpace(strings.TrimPrefix(text, string(rune(0xFEFF))))
	var out scrape
	if !strings.HasPrefix(text, "{") {
		return out
	}
	out.jsonStart = true
	dec := json.NewDecoder(strings.NewReader(text))
	for {
		var doc searchResultDoc
		if err := dec.Decode(&doc); err != nil {
			break
		}
		out.docs++
		if doc.OK != nil && !*doc.OK {
			out.notOK = true
		}
		id := doc.RequestID
		if id == "" && doc.Result != nil {
			id = doc.Result.RequestID
		}
		if validRequestID(id) {
			out.ids = append(out.ids, id)
		}
	}
	return out
}

// validRequestID keeps an id only if it is shaped like one: a short token.
// Anything else in that field is not evidence of a router request.
func validRequestID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.', r == ':':
		default:
			return false
		}
	}
	return true
}

// settle records a result against the search and decides its loss reason.
func (s *Search) settle(text string, isError, denied bool, line int) {
	s.HasResult, s.ResultLine = true, line
	sc := scrapeRequestIDs(text)
	s.RequestIDs = sc.ids
	if len(sc.ids) > 0 {
		s.Loss = ""
		return
	}
	expectsJSON, elsewhere := false, false
	for _, inv := range s.Invocations {
		expectsJSON = expectsJSON || inv.ExpectsJSON()
		elsewhere = elsewhere || inv.OutputElsewhere
	}
	switch {
	case denied:
		s.Loss = LossDeniedByHost
	case isError || sc.notOK:
		s.Loss = LossSearchFailed
	case elsewhere:
		s.Loss = LossOutputElsewhere
	case !expectsJSON:
		s.Loss = LossHumanFormat
	case sc.jsonStart && sc.docs == 0:
		// It opens as a JSON document and not one document parses to its end.
		s.Loss = LossResultTruncated
	case sc.docs > 0:
		s.Loss = LossNoRequestID
	default:
		s.Loss = LossResultNotJSON
	}
}
