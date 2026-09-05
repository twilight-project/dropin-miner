package observe

// §9.2 streaming cases, fixture-driven — all thirteen — plus FuzzSSEParser.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sseObserve drives the parser exactly as the observer loop would: feed in
// small chunks, honor early termination, finalize at EOF otherwise.
func sseObserve(t *testing.T, stream []byte, finalErr error) (*Observation, Outcome) {
	t.Helper()
	return sseObserveLimits(t, stream, finalErr, 1<<20, 100_000)
}

func sseObserveLimits(t *testing.T, stream []byte, finalErr error, maxEventBytes, maxEvents int64) (*Observation, Outcome) {
	t.Helper()
	p := newSSEParser(32, 256, maxEventBytes, maxEvents)
	obsv := &Observation{Streaming: true}
	const chunk = 7
	for i := 0; i < len(stream); i += chunk {
		p.consume(stream[i:min(i+chunk, len(stream))])
		if reason, aborted := p.abortReason(); aborted {
			return obsv, Abandoned(reason)
		}
		if p.terminated() {
			return obsv, p.finalize(false, finalErr, obsv)
		}
	}
	return obsv, p.finalize(true, finalErr, obsv)
}

func loadSSE(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "fixtures", "sse", name)) // #nosec G304 -- test-owned fixture names
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func mustTermination(t *testing.T, out Outcome, want Termination) {
	t.Helper()
	if got, ok := out.Termination(); !ok || got != want {
		t.Errorf("outcome %v, want complete:%s", out, want)
	}
}

// Case 1: early ID + [DONE].
func TestSSEEarlyIDAndDone(t *testing.T) {
	o, out := sseObserve(t, loadSSE(t, "early_id_done.sse"), nil)
	mustTermination(t, out, TerminationDone)
	if o.GenerationID != "gen-sse-early" || !o.SawDone || o.FinishReason != "stop" {
		t.Errorf("%+v", o)
	}
	if o.SawUsage {
		t.Error("no usage was sent")
	}
}

// Case 2: the final empty-choices usage chunk.
func TestSSEFinalUsageChunk(t *testing.T) {
	o, out := sseObserve(t, loadSSE(t, "final_usage_chunk.sse"), nil)
	mustTermination(t, out, TerminationDone)
	if !o.SawUsage || o.Usage == nil || o.Usage.TotalTokens != 14 || o.Usage.PromptTokens != 5 {
		t.Errorf("usage: %+v", o.Usage)
	}
	if o.FinishReason != "stop" || !o.SawFinishReason {
		t.Errorf("finish: %+v", o)
	}
}

// Case 3: no usage at all — a normal recorded outcome.
func TestSSENoUsage(t *testing.T) {
	o, out := sseObserve(t, loadSSE(t, "no_usage.sse"), nil)
	mustTermination(t, out, TerminationDone)
	if o.SawUsage || o.Usage != nil {
		t.Errorf("usage invented: %+v", o.Usage)
	}
}

// Case 4: a provider error event terminates with type/code only.
func TestSSEErrorEvent(t *testing.T) {
	o, out := sseObserve(t, loadSSE(t, "error_event.sse"), nil)
	mustTermination(t, out, TerminationUpstreamErrorEvent)
	if o.ErrorType != "synthetic_overloaded" || o.ErrorCode != "fixture_529" {
		t.Errorf("error fields: %+v", o)
	}
	if o.SawDone {
		t.Error("no [DONE] was sent")
	}
}

// Case 5: truncated mid-event → eof_without_done, degraded, partial event
// discarded.
func TestSSETruncatedMidEvent(t *testing.T) {
	o, out := sseObserve(t, loadSSE(t, "truncated_mid_event.sse"), nil)
	mustTermination(t, out, TerminationEOFWithoutDone)
	if !o.ParseDegraded {
		t.Error("truncation must degrade")
	}
	if o.GenerationID != "gen-sse-trunc" {
		t.Errorf("metadata from complete events must survive: %+v", o)
	}
}

// Case 6: one malformed JSON event degrades and resynchronizes; the stream
// stays good.
func TestSSEMalformedEventResync(t *testing.T) {
	o, out := sseObserve(t, loadSSE(t, "malformed_event.sse"), nil)
	mustTermination(t, out, TerminationDone)
	if !o.ParseDegraded {
		t.Error("malformed event must degrade")
	}
	if o.GenerationID != "gen-sse-malformed" || !o.SawUsage || o.Usage.TotalTokens != 3 {
		t.Errorf("resync failed: %+v", o)
	}
}

// Cases 7+8: an event just under max_event_bytes survives; just over
// degrades that event only.
func TestSSEEventSizeBoundary(t *testing.T) {
	pad := strings.Repeat("a", 800)
	under := "data: {\"id\":\"gen-under\",\"choices\":[{\"delta\":{\"content\":\"" + pad + "\"}}]}\n\ndata: [DONE]\n\n"
	o, out := sseObserveLimits(t, []byte(under), nil, 1024, 100_000)
	mustTermination(t, out, TerminationDone)
	if o.ParseDegraded || o.GenerationID != "gen-under" {
		t.Errorf("under-limit event: %+v", o)
	}

	bigPad := strings.Repeat("b", 2048)
	over := "data: {\"id\":\"gen-over-skipped\",\"choices\":[{\"delta\":{\"content\":\"" + bigPad + "\"}}]}\n\n" +
		"data: {\"id\":\"gen-over\",\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
	o, out = sseObserveLimits(t, []byte(over), nil, 1024, 100_000)
	mustTermination(t, out, TerminationDone)
	if !o.ParseDegraded {
		t.Error("over-limit event must degrade")
	}
	if o.GenerationID != "gen-over" {
		t.Errorf("later events must still be observed: %+v", o)
	}
}

// Case 9: conflicting id across chunks — first non-empty wins, conflict
// flagged, no overwrite.
func TestSSEConflictingID(t *testing.T) {
	o, out := sseObserve(t, loadSSE(t, "conflicting_id.sse"), nil)
	mustTermination(t, out, TerminationDone)
	if o.GenerationID != "gen-sse-one" || !o.MetadataConflict {
		t.Errorf("conflict handling: id=%q conflict=%v", o.GenerationID, o.MetadataConflict)
	}
}

// Case 10: conflicting model.
func TestSSEConflictingModel(t *testing.T) {
	o, out := sseObserve(t, loadSSE(t, "conflicting_model.sse"), nil)
	mustTermination(t, out, TerminationDone)
	if o.ResolvedModel != "synthetic/model-a" || !o.MetadataConflict {
		t.Errorf("conflict handling: model=%q conflict=%v", o.ResolvedModel, o.MetadataConflict)
	}
}

// Case 11: comment/keepalive lines interleaved (A-2).
func TestSSEKeepalivesIgnored(t *testing.T) {
	o, out := sseObserve(t, loadSSE(t, "keepalives.sse"), nil)
	mustTermination(t, out, TerminationDone)
	if o.GenerationID != "gen-sse-keep" || !o.SawUsage || o.Usage.TotalTokens != 4 || o.ParseDegraded {
		t.Errorf("keepalives disturbed observation: %+v", o)
	}
}

// Case 12: \r\n line endings.
func TestSSECRLFEndings(t *testing.T) {
	o, out := sseObserve(t, loadSSE(t, "crlf_endings.sse"), nil)
	mustTermination(t, out, TerminationDone)
	if o.GenerationID != "gen-sse-crlf" || !o.SawUsage || o.Usage.TotalTokens != 2 {
		t.Errorf("CRLF handling: %+v", o)
	}
}

// Case 13: multi-line data: fields join with \n and stream through the
// scanner without assembly.
func TestSSEMultilineData(t *testing.T) {
	o, out := sseObserve(t, loadSSE(t, "multiline_data.sse"), nil)
	mustTermination(t, out, TerminationDone)
	if o.GenerationID != "gen-sse-multi" || o.ResolvedModel != "synthetic/model-s" ||
		!o.SawUsage || o.Usage.TotalTokens != 7 || o.FinishReason != "stop" {
		t.Errorf("multi-line join: %+v", o)
	}
	if o.ParseDegraded {
		t.Error("well-formed multi-line event degraded")
	}
}

// event:, id:, retry: fields are ignored, their bytes skipped.
func TestSSENonDataFieldsIgnored(t *testing.T) {
	o, out := sseObserve(t, loadSSE(t, "ignored_fields.sse"), nil)
	mustTermination(t, out, TerminationDone)
	if o.GenerationID != "gen-sse-fields" || o.ParseDegraded {
		t.Errorf("non-data fields disturbed observation: %+v", o)
	}
}

// A stream that ends without [DONE] is eof_without_done — a real recorded
// outcome whose promotion is D-19's question, not ours.
func TestSSEEOFWithoutDone(t *testing.T) {
	stream := "data: {\"id\":\"gen-nodone\",\"choices\":[{\"delta\":{\"content\":\"x\"},\"finish_reason\":\"stop\"}]}\n\n"
	o, out := sseObserve(t, []byte(stream), nil)
	mustTermination(t, out, TerminationEOFWithoutDone)
	if o.SawDone {
		t.Error("SawDone without [DONE]")
	}
	if o.GenerationID != "gen-nodone" {
		t.Errorf("%+v", o)
	}
}

// A junk payload that is neither JSON nor [DONE] is skipped with
// ParseDegraded.
func TestSSEJunkPayloadSkipped(t *testing.T) {
	stream := "data: hello world\n\ndata: {\"id\":\"gen-after-junk\"}\n\ndata: [DONE]\n\n"
	o, out := sseObserve(t, []byte(stream), nil)
	mustTermination(t, out, TerminationDone)
	if !o.ParseDegraded || o.GenerationID != "gen-after-junk" {
		t.Errorf("junk payload handling: %+v", o)
	}
}

// max_events is a work limit: a pathological stream abandons.
func TestSSEMaxEventsAbandons(t *testing.T) {
	stream := strings.Repeat("data: {\"x\":1}\n\n", 20)
	_, out := sseObserveLimits(t, []byte(stream), nil, 1<<20, 10)
	if reason, ok := out.Reason(); !ok || reason != AbandonBodyTooLarge {
		t.Errorf("outcome: %v", out)
	}
}

// Client disconnect mid-stream classifies as client_disconnect with the
// metadata captured so far.
func TestSSEClientDisconnect(t *testing.T) {
	stream := "data: {\"id\":\"gen-sse-disc\",\"choices\":[{\"delta\":{\"content\":\"x\"},\"finish_reason\":null}]}\n\n"
	o, out := sseObserve(t, []byte(stream), context.Canceled)
	mustTermination(t, out, TerminationClientDisconnect)
	if o.GenerationID != "gen-sse-disc" {
		t.Errorf("%+v", o)
	}
}

func FuzzSSEParser(f *testing.F) {
	f.Add([]byte("data: {\"id\":\"gen-1\"}\n\ndata: [DONE]\n\n"))
	f.Add([]byte(": comment\ndata: [DONE]\n\n"))
	f.Add([]byte("data: {\"a\":\n\ndata: 1}\n\n"))
	f.Add([]byte("event: x\r\ndata: {\"usage\":{\"total_tokens\":1}}\r\n\r\n"))
	f.Add([]byte("data:{\"no-space\":true}\n\n"))
	f.Add([]byte("data: not json\n\n"))
	f.Add([]byte("\n\n\n\n"))
	f.Add([]byte("data: [DONE"))

	f.Fuzz(func(t *testing.T, data []byte) {
		p := newSSEParser(32, 256, 4096, 1000)
		obsv := &Observation{}
		step := 1 + len(data)%89
		for i := 0; i < len(data); i += step {
			p.consume(data[i:min(i+step, len(data))])
			if _, aborted := p.abortReason(); aborted {
				return
			}
			if p.terminated() {
				_ = p.finalize(false, nil, obsv)
				return
			}
		}
		_ = p.finalize(true, nil, obsv)
	})
}
