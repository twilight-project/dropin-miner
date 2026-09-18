package trajectory

import (
	"encoding/json"
	"strings"
)

// Level 2's outcome labels, in the router's existing feedback vocabulary so
// that this level needs no new router contract. Each is decided from what the
// turn did after a search, and a label that cannot be decided is absent,
// never guessed.
//
//	result.fetched      a later tool call in the turn names, as its "url"
//	                    parameter, a page the search offered. Exact match
//	                    after dropping a fragment and a trailing slash. High:
//	                    the host recorded the call.
//	result.cited        the model's later prose contains that page's URL.
//	                    High for presence. Its absence is NOT a rejection: a
//	                    model may use a page and not print its address.
//	query.reformulated  the next search in the same turn carries a different
//	                    query, compared case- and space-insensitively. Medium:
//	                    a changed query may be the next sub-question, not a
//	                    second attempt at the same one. Absent when either
//	                    query is not visible on its command line.
//	session.abandoned   the turn ended interrupted (high) or superseded
//	                    (medium) and the model wrote no prose after its last
//	                    search. A turn still open at the end of its file is
//	                    left unlabelled: the session may simply be running.
//
// Two of the vocabulary's types are never emitted, and the findings say so.
// result.rejected has no structural signal — not fetching and not citing a
// page is silence, not rejection. answer.accepted would need the
// participant's judgement, which a transcript does not hold; the next prompt
// arriving proves only that they were still there.
const (
	LabelFetched      = "result.fetched"
	LabelCited        = "result.cited"
	LabelReformulated = "query.reformulated"
	LabelAbandoned    = "session.abandoned"
)

const (
	ConfidenceHigh   = "high"
	ConfidenceMedium = "medium"
)

// Label is one outcome. It carries positions and ids, never a URL, a title or
// a word of prose: Candidate and Citation index into the result the router
// already holds under RequestID.
type Label struct {
	Type       string `json:"type"`
	Search     int    `json:"search"` // ordinal of the search within its turn
	RequestID  string `json:"request_id,omitempty"`
	Candidate  *int   `json:"candidate,omitempty"`
	Citation   *int   `json:"citation,omitempty"`
	Confidence string `json:"confidence"`
}

// labelsOf derives a turn's labels. It needs the content the reader keeps
// under Options.KeepContent; without it there is nothing to match against and
// it returns none.
func labelsOf(t *Turn) []Label {
	var labels []Label
	ordinal := map[*Search]int{}
	for i, s := range t.Searches {
		ordinal[s] = i
	}
	requestID := func(s *Search) string {
		if len(s.RequestIDs) > 0 {
			return s.RequestIDs[0]
		}
		return ""
	}

	var offered []*Search // searches whose results have arrived, in order
	seen := map[[3]int]map[string]bool{}
	mark := func(s *Search, c Citation, typ string) {
		key := [3]int{ordinal[s], c.Candidate, c.Index}
		if seen[key] == nil {
			seen[key] = map[string]bool{}
		}
		if seen[key][typ] {
			return
		}
		seen[key][typ] = true
		candidate, citation := c.Candidate, c.Index
		labels = append(labels, Label{
			Type: typ, Search: ordinal[s], RequestID: requestID(s),
			Candidate: &candidate, Citation: &citation, Confidence: ConfidenceHigh,
		})
	}
	lastSearchEvent, proseAfterLastSearch := -1, false
	for i, ev := range t.Events {
		switch ev.Kind {
		case KindSearchCall:
			lastSearchEvent, proseAfterLastSearch = i, false
		case KindSearchResult:
			offered = append(offered, ev.Search)
		case KindToolCall:
			if url := urlParameter(ev.Input); url != "" {
				for _, s := range offered {
					for _, c := range s.Citations {
						if c.URL != "" && normalizeURL(c.URL) == normalizeURL(url) {
							mark(s, c, LabelFetched)
						}
					}
				}
			}
		case KindAssistantText:
			if lastSearchEvent >= 0 {
				proseAfterLastSearch = true
			}
			for _, s := range offered {
				for _, c := range s.Citations {
					if u := normalizeURL(c.URL); u != "" && strings.Contains(ev.Text, u) {
						mark(s, c, LabelCited)
					}
				}
			}
		}
	}

	for i := 1; i < len(t.Searches); i++ {
		prev, next := t.Searches[i-1], t.Searches[i]
		if len(prev.queries) != 1 || len(next.queries) != 1 {
			continue // not visible, or a chained call: undecidable
		}
		if normalizeQuery(prev.queries[0]) != normalizeQuery(next.queries[0]) {
			labels = append(labels, Label{Type: LabelReformulated, Search: i, RequestID: requestID(next), Confidence: ConfidenceMedium})
		}
	}

	if len(t.Searches) > 0 && !proseAfterLastSearch {
		last := len(t.Searches) - 1
		switch t.End {
		case EndInterrupted:
			labels = append(labels, Label{Type: LabelAbandoned, Search: last, RequestID: requestID(t.Searches[last]), Confidence: ConfidenceHigh})
		case EndSuperseded:
			labels = append(labels, Label{Type: LabelAbandoned, Search: last, RequestID: requestID(t.Searches[last]), Confidence: ConfidenceMedium})
		}
	}
	return labels
}

// urlParameter reads a tool call's "url" parameter, the shape of the host's
// page-fetching tool and of the fetch tools shaped like it.
func urlParameter(input json.RawMessage) string {
	var in struct {
		URL string `json:"url"`
	}
	if len(input) == 0 || json.Unmarshal(input, &in) != nil {
		return ""
	}
	return in.URL
}

func normalizeURL(u string) string {
	if k := strings.IndexByte(u, '#'); k >= 0 {
		u = u[:k]
	}
	return strings.TrimRight(strings.TrimSpace(u), "/")
}

func normalizeQuery(q string) string {
	return strings.Join(strings.Fields(strings.ToLower(q)), " ")
}
