package observe

// Scanner unit tests: §9.2's non-streaming cases plus the §6.3 hardening
// list — every behavior encoding/json would have gotten wrong.

import (
	"strings"
	"testing"
)

func scanDoc(t *testing.T, doc string) scanResult {
	t.Helper()
	s := newScanner(32, 256)
	// Feed in awkward chunk sizes to exercise incremental state.
	for i := 0; i < len(doc); i += 7 {
		end := min(i+7, len(doc))
		s.feed([]byte(doc[i:end]))
	}
	s.finish()
	return s.result()
}

func TestFullMetadataCapture(t *testing.T) {
	r := scanDoc(t, `{
		"id": "gen-abc123",
		"object": "chat.completion",
		"model": "anthropic/claude-sonnet",
		"choices": [{"index": 0, "message": {"role": "assistant", "content": "hello world"}, "finish_reason": "stop"}],
		"usage": {"prompt_tokens": 9, "completion_tokens": 12, "total_tokens": 21}
	}`)
	if r.ID != "gen-abc123" || r.Model != "anthropic/claude-sonnet" || r.FinishReason != "stop" {
		t.Errorf("capture: %+v", r)
	}
	if !r.SawUsage || r.Usage.PromptTokens != 9 || r.Usage.CompletionTokens != 12 || r.Usage.TotalTokens != 21 {
		t.Errorf("usage: %+v", r.Usage)
	}
	if !r.Complete || r.Degraded || r.Abandoned != "" {
		t.Errorf("flags: %+v", r)
	}
}

func TestMissingUsageIsNormal(t *testing.T) {
	r := scanDoc(t, `{"id":"gen-1","model":"m","choices":[{"finish_reason":"stop"}]}`)
	if r.SawUsage || r.Degraded || !r.Complete {
		t.Errorf("absent usage must be a normal recorded outcome: %+v", r)
	}
}

func TestErrorBody(t *testing.T) {
	r := scanDoc(t, `{"error":{"type":"rate_limit_error","code":"slow_down","message":"try later: full prompt echoed here"}}`)
	if !r.SawError || r.ErrType != "rate_limit_error" || r.ErrCode != "slow_down" {
		t.Errorf("error capture: %+v", r)
	}
}

// The §6.3 path-awareness proof: a content STRING containing a usage-shaped
// fragment, and a nested OBJECT at the wrong path, must capture nothing.
func TestPathAwareness(t *testing.T) {
	r := scanDoc(t, `{
		"choices": [{"message": {"content": "ignore this: \"usage\":{\"total_tokens\":999}"}}],
		"metadata": {"usage": {"total_tokens": 888}},
		"usage": {"total_tokens": 5}
	}`)
	if r.Usage.TotalTokens != 5 {
		t.Errorf("path-awareness failed: %+v", r.Usage)
	}
	// And an id nested in the wrong place is not the id.
	r = scanDoc(t, `{"choices":[{"id":"not-the-id"}],"id":"gen-right"}`)
	if r.ID != "gen-right" {
		t.Errorf("nested id captured: %q", r.ID)
	}
}

// Key matching is exact and case-sensitive: USAGE must not populate usage.
func TestCaseSensitiveKeys(t *testing.T) {
	r := scanDoc(t, `{"USAGE":{"total_tokens":999},"Usage":{"total_tokens":998},"usage":{"total_tokens":7}}`)
	if r.Usage.TotalTokens != 7 {
		t.Errorf("case-insensitive key matched: %+v", r.Usage)
	}
	r = scanDoc(t, `{"ID":"nope","id":"gen-1"}`)
	if r.ID != "gen-1" {
		t.Errorf("ID matched id: %q", r.ID)
	}
}

// Duplicate keys: first non-empty wins for id/model/finish_reason.
func TestDuplicateStringsFirstWins(t *testing.T) {
	r := scanDoc(t, `{"id":"gen-first","id":"gen-second","model":"m1","model":"m2"}`)
	if r.ID != "gen-first" || r.Model != "m1" {
		t.Errorf("duplicate policy: id=%q model=%q", r.ID, r.Model)
	}
}

// Duplicate numeric fields: last wins PER FIELD, so a partial second usage
// object cannot zero a field the first one set.
func TestDuplicateUsageLastWinsPerField(t *testing.T) {
	r := scanDoc(t, `{"usage":{"prompt_tokens":10,"completion_tokens":20,"total_tokens":30},"usage":{"completion_tokens":21}}`)
	if r.Usage.PromptTokens != 10 || r.Usage.CompletionTokens != 21 || r.Usage.TotalTokens != 30 {
		t.Errorf("per-field last-wins: %+v", r.Usage)
	}
}

// Escaped keys are decoded then compared exactly: {"id": …} matches id
// (§6.3).
func TestEscapedKeyDecoded(t *testing.T) {
	r := scanDoc(t, `{"\u0069d":"gen-escaped"}`)
	if r.ID != "gen-escaped" {
		t.Errorf("escaped key not decoded: %+v", r)
	}
	// Escapes assemble the exact key: USAGE decodes to USAGE, which
	// must NOT match usage — decoding never case-folds.
	r = scanDoc(t, `{"\u0055SAGE":{"total_tokens":9}}`)
	if r.Usage.TotalTokens != 0 {
		t.Errorf("decoded escape case-folded: %+v", r.Usage)
	}
}

// Integer hygiene: overflow and non-integers degrade, never coerce.
func TestIntegerBounds(t *testing.T) {
	// 21 digits: over the digit bound.
	r := scanDoc(t, `{"usage":{"prompt_tokens":123456789012345678901}}`)
	if !r.Degraded || r.Usage.PromptTokens != 0 {
		t.Errorf("digit bound: %+v", r)
	}
	// Fraction: not an unsigned integer.
	r = scanDoc(t, `{"usage":{"prompt_tokens":1.5}}`)
	if !r.Degraded || r.Usage.PromptTokens != 0 {
		t.Errorf("fraction: %+v", r)
	}
	// Negative.
	r = scanDoc(t, `{"usage":{"prompt_tokens":-3}}`)
	if !r.Degraded || r.Usage.PromptTokens != 0 {
		t.Errorf("negative: %+v", r)
	}
	// uint64 max is fine (20 digits).
	r = scanDoc(t, `{"usage":{"prompt_tokens":18446744073709551615}}`)
	if r.Degraded || r.Usage.PromptTokens != 18446744073709551615 {
		t.Errorf("uint64 max: %+v", r)
	}
}

// Nesting past max_depth abandons the observation.
func TestMaxDepthAbandons(t *testing.T) {
	doc := strings.Repeat(`{"a":`, 40) + `1` + strings.Repeat("}", 40)
	r := scanDoc(t, doc)
	if r.Abandoned != AbandonMaxDepthExceeded {
		t.Errorf("depth: %+v", r)
	}
}

// Per-field caps: an over-cap value is dropped and degraded, never
// truncated-and-kept.
func TestOverCapValueDropped(t *testing.T) {
	long := strings.Repeat("x", 600)
	r := scanDoc(t, `{"id":"`+long+`"}`)
	if r.ID != "" || !r.Degraded {
		t.Errorf("over-cap id kept: %q degraded=%v", r.ID, r.Degraded)
	}
}

// Charset validation: whitespace and control characters have no place in a
// generation ID.
func TestCharsetValidation(t *testing.T) {
	r := scanDoc(t, `{"id":"gen 123"}`)
	if r.ID != "" || !r.Degraded {
		t.Errorf("id with whitespace kept: %+v", r)
	}
	r = scanDoc(t, `{"id":"gen\t123"}`)
	if r.ID != "" || !r.Degraded {
		t.Errorf("id with control kept: %+v", r)
	}
	r = scanDoc(t, `{"id":"gen-café"}`)
	if r.ID != "" || !r.Degraded {
		t.Errorf("non-ASCII id kept: %+v", r)
	}
}

// Credential screening: a provider has no legitimate reason to return an
// API key in a metadata field.
func TestCredentialRejected(t *testing.T) {
	key := "sk-or-v1-" + strings.Repeat("0", 8)
	r := scanDoc(t, `{"id":"`+key+`"}`)
	if r.ID != "" || !r.CredentialRejected {
		t.Errorf("credential-shaped id kept: %+v", r)
	}
}

// Invalid escapes degrade rather than passing through raw.
func TestInvalidEscapeDegrades(t *testing.T) {
	r := scanDoc(t, `{"id":"gen\q123"}`)
	if r.ID != "" || !r.Degraded {
		t.Errorf("invalid escape: %+v", r)
	}
}

// Unterminated string at EOF: incomplete value → abandoned.
func TestUnterminatedStringAbandons(t *testing.T) {
	r := scanDoc(t, `{"id":"gen-unterminated`)
	if r.Abandoned != AbandonMalformedBody || r.Complete {
		t.Errorf("unterminated: %+v", r)
	}
}

// Escapes inside skipped content must not confuse structure.
func TestEscapesInSkippedContent(t *testing.T) {
	r := scanDoc(t, `{"choices":[{"message":{"content":"quote \" brace } bracket ] \\ end"}}],"id":"gen-1"}`)
	if r.ID != "gen-1" || !r.Complete {
		t.Errorf("skipped-content escapes: %+v", r)
	}
}

// Top-level framing: BOM skipped; trailing content and a second top-level
// value abandon.
func TestTopLevelFraming(t *testing.T) {
	r := scanDoc(t, "\xEF\xBB\xBF"+`{"id":"gen-bom"}`)
	if r.ID != "gen-bom" || !r.Complete {
		t.Errorf("BOM: %+v", r)
	}
	r = scanDoc(t, `{"id":"gen-1"} trailing`)
	if r.Abandoned != AbandonMalformedBody {
		t.Errorf("trailing: %+v", r)
	}
	r = scanDoc(t, `{"id":"gen-1"}{"id":"gen-2"}`)
	if r.Abandoned != AbandonMalformedBody {
		t.Errorf("second value: %+v", r)
	}
	r = scanDoc(t, `{"id":"gen-1"`)
	if r.Abandoned != AbandonMalformedBody {
		t.Errorf("truncated: %+v", r)
	}
}

// Malformed JSON: garbage inside the document abandons.
func TestMalformedJSON(t *testing.T) {
	for _, doc := range []string{
		`{"id" "gen-1"}`,   // missing colon
		`{"id":}`,          // missing value
		`{id:"gen-1"}`,     // unquoted key
		`{"id":"a",}` + "", // trailing comma before }
		`[1 2]`,            // missing comma
		`{"id":truthy}`,    // bad literal
	} {
		r := scanDoc(t, doc)
		if r.Abandoned != AbandonMalformedBody {
			t.Errorf("%q: want malformed_body, got %+v", doc, r)
		}
	}
}

// Unknown and extension fields are structurally consumed and ignored.
func TestUnknownFieldsIgnored(t *testing.T) {
	r := scanDoc(t, `{"x_extension":{"deep":[1,2,{"a":null}]},"id":"gen-1","flags":[true,false,null],"n":1.5e10}`)
	if r.ID != "gen-1" || !r.Complete || r.Degraded {
		t.Errorf("unknown fields: %+v", r)
	}
}

// A key longer than max_json_key_bytes stops being retained and its value is
// skipped as an unknown subtree — but scanning continues.
func TestOversizedKeySkipped(t *testing.T) {
	bigKey := strings.Repeat("k", 300)
	r := scanDoc(t, `{"`+bigKey+`":{"usage":{"total_tokens":999}},"id":"gen-1"}`)
	if r.ID != "gen-1" {
		t.Errorf("scan did not continue past oversized key: %+v", r)
	}
	if r.Usage.TotalTokens != 0 {
		t.Errorf("value under oversized key was captured: %+v", r.Usage)
	}
}

// Non-integer top-level values complete cleanly (scanner correctness).
func TestScalarTopLevel(t *testing.T) {
	for _, doc := range []string{`42`, `"str"`, `true`, `null`, `[1,2,3]`} {
		r := scanDoc(t, doc)
		if !r.Complete || r.Abandoned != "" {
			t.Errorf("%q: %+v", doc, r)
		}
	}
}

// Empty containers are valid JSON — the canonical final usage chunk carries
// "choices":[] (§2.1 of the proxy spec), which is how this case was found.
func TestEmptyContainers(t *testing.T) {
	for _, doc := range []string{`{}`, `[]`, `[[]]`, `{"a":[]}`, `{"a":{}}`, `[{},[]]`} {
		r := scanDoc(t, doc)
		if !r.Complete || r.Abandoned != "" || r.Degraded {
			t.Errorf("%q: %+v", doc, r)
		}
	}
	r := scanDoc(t, `{"id":"gen-empty","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`)
	if r.ID != "gen-empty" || r.Usage.TotalTokens != 3 {
		t.Errorf("empty-choices usage chunk: %+v", r)
	}
	// A trailing comma is still malformed: [1,] and {"a":1,} must not pass.
	for _, doc := range []string{`[1,]`, `{"a":1,}`} {
		if r := scanDoc(t, doc); r.Abandoned != AbandonMalformedBody {
			t.Errorf("%q accepted: %+v", doc, r)
		}
	}
}
