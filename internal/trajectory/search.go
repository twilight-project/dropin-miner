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

// ResultShape names an envelope shape that decides what can be read out of a
// result, where the shape itself is the answer rather than a failure.
type ResultShape string

// ShapeMergedView is the envelope a search asking for "view":"merged"
// returns: a merged list of pages across providers, and no per-provider
// candidates at all — the one view that omits the key the citations live
// under (`search_machine.go`'s `Candidates` pointer, omitted for that view).
//
// A search in this shape is ANCHORED: the request id is still at the top of
// the envelope. What is unavailable is its citations, and that is a fact
// about the shape rather than a search that offered none. The merged list is
// deliberately NOT read as citations: it is a different list, deduplicated
// across providers, so a position in it does not mean what a position in a
// provider's candidate list means, and a label carrying one would name the
// wrong page.
//
// It is named here so it is counted rather than parsed into silence. Nothing
// refuses it — it is a documented shape of a contract this client owns — but
// a corpus losing its labels must say so out loud. Client side: issue #126.
const ShapeMergedView ResultShape = "merged_view"

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
	// Shape is set when the result's envelope shape decides what could be
	// read out of it. Empty is the ordinary case.
	Shape ResultShape
	// Citations are the pages the result offered, by position. A position is
	// an index into a result the router already holds under the request id,
	// so an outcome label can name a page without carrying anything of it.
	// The URL is kept only under Options.KeepContent, to match against what
	// the turn did next, and is never emitted.
	Citations []Citation

	// queries are the queries the command line shows, one per invocation
	// where it shows one. They are what the model wrote: held in memory to
	// tell a changed query from a repeated one, emitted only behind a gate.
	queries []string
}

// Citation is one page a search result offered.
type Citation struct {
	Candidate, Index int
	URL              string
}

// Anchored reports whether path 1 tied this search to a router request.
func (s *Search) Anchored() bool { return len(s.RequestIDs) > 0 }

// searchResultDoc covers both documents a search can print: this client's
// machine envelope (request_id at the top and again under result) and the
// router's own JSON under -format json (request_id at the top). It is decoded
// permissively and only for these fields — the answer text is never bound.
type searchResultDoc struct {
	OK         *bool             `json:"ok"`
	RequestID  string            `json:"request_id"`
	Candidates []resultCandidate `json:"candidates"`
	// Merged is bound as empty structs on purpose: the merged list's pages
	// carry a url, a title and a snippet, and this field exists only to know
	// whether the list is there and how long it is. Decoding into struct{}
	// discards every field, so no merged page's content can land anywhere.
	Merged []struct{} `json:"merged"`
	Result *struct {
		RequestID  string            `json:"request_id"`
		Candidates []resultCandidate `json:"candidates"`
		Merged     []struct{}        `json:"merged"`
	} `json:"result"`
}

// resultCandidate binds a candidate's citations and nothing else of it: the
// answer and the snippets have no field here to land in.
type resultCandidate struct {
	Citations []struct {
		URL string `json:"url"`
	} `json:"citations"`
}

type scrape struct {
	citations []Citation
	ids       []string
	shape     ResultShape
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
		candidates := doc.Candidates
		if doc.Result != nil && len(doc.Result.Candidates) > 0 {
			candidates = doc.Result.Candidates
		}
		merged := len(doc.Merged) > 0
		if doc.Result != nil && len(doc.Result.Merged) > 0 {
			merged = true
		}
		// A merged list and no candidates is the merged view. Both keys
		// present is the ordinary envelope, which carries merged pages beside
		// the per-provider candidates the citations are read from.
		if out.docs == 1 && merged && len(candidates) == 0 {
			out.shape = ShapeMergedView
		}
		// Positions are only meaningful against one result, so a call that
		// printed several documents offers the first one's citations.
		if out.docs == 1 {
			for ci, c := range candidates {
				for k, cit := range c.Citations {
					out.citations = append(out.citations, Citation{Candidate: ci, Index: k, URL: cit.URL})
				}
			}
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
func (s *Search) settle(text string, isError, denied, keepContent bool, line int) {
	s.HasResult, s.ResultLine = true, line
	sc := scrapeRequestIDs(text)
	s.RequestIDs = sc.ids
	s.Citations = sc.citations
	s.Shape = sc.shape
	if !keepContent {
		for i := range s.Citations {
			s.Citations[i].URL = ""
		}
	}
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
