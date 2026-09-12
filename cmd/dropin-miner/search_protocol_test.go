package main

// The search protocol's three bounds: one finite deadline over the whole
// operation, a ceiling on every body the client reads, and a 2xx that is
// not a success until it has been shown to be one — plus the single
// compatibility retry, which is licensed by an exact machine code and by
// nothing else.
//
// No sleeps. The deadline guards use a transport that inspects the
// deadline each request actually carries, which is the thing being
// asserted; a sleep would only show that some wall-clock time passed.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/twilight-project/dropin-miner/pkg/auth"
)

// ── harness ─────────────────────────────────────────────────────────────

// recordingTransport answers each request from fn and keeps what it was
// asked, including the deadline the request context carried.
type recordingTransport struct {
	fn          func(i int, req *http.Request) (*http.Response, error)
	mu          sync.Mutex
	bodies      []string
	deadlines   []time.Time
	hadDeadline []bool
}

func (rt *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	body := ""
	if req.Body != nil {
		b, _ := io.ReadAll(req.Body)
		body = string(b)
	}
	dl, ok := req.Context().Deadline()
	rt.mu.Lock()
	n := len(rt.bodies)
	rt.bodies = append(rt.bodies, body)
	rt.deadlines = append(rt.deadlines, dl)
	rt.hadDeadline = append(rt.hadDeadline, ok)
	rt.mu.Unlock()
	return rt.fn(n, req)
}

func (rt *recordingTransport) calls() int {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return len(rt.bodies)
}

// useTransport installs a transport for the duration of one test. These
// tests do not run in parallel; the seam is a package var so production
// keeps net/http's default with no indirection.
func useTransport(t *testing.T, rt http.RoundTripper) {
	t.Helper()
	prev := searchTransport
	searchTransport = rt
	t.Cleanup(func() { searchTransport = prev })
}

func withSearchMaxBody(t *testing.T, n int64) {
	t.Helper()
	prev := searchMaxBody
	searchMaxBody = n
	t.Cleanup(func() { searchMaxBody = prev })
}

func fakeResponse(status int, body string, header http.Header) *http.Response {
	if header == nil {
		header = http.Header{}
	}
	return &http.Response{
		StatusCode:    status,
		Header:        header,
		Body:          io.NopCloser(strings.NewReader(body)),
		ContentLength: -1, // unknown: force the streaming max+1 check
	}
}

func traceCall() searchCall {
	return searchCall{
		Endpoint: "https://router.fictional.test/v1/search",
		Key:      "sr-fictional",
		Query:    "how do ports work",
		Trace:    &traceEnvelope{V: traceVersion, Harness: "cli", SessionID: "sess", CallID: "call"},
	}
}

func runPerformSearch(t *testing.T, timeout time.Duration, call searchCall) searchOutcome {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return performSearch(ctx, time.Now, call)
}

// ── §19 deadline ────────────────────────────────────────────────────────

func TestSearchRequestCarriesAFiniteDeadline(t *testing.T) {
	rt := &recordingTransport{fn: func(int, *http.Request) (*http.Response, error) {
		return fakeResponse(http.StatusOK, routerBody, nil), nil
	}}
	useTransport(t, rt)
	out := runPerformSearch(t, 30*time.Second, traceCall())
	if !out.ok() {
		t.Fatalf("fault %q err %v", out.Fault, out.Err)
	}
	if !rt.hadDeadline[0] {
		t.Fatal("the search request carried no deadline; the whole operation is unbounded")
	}
}

func TestTheTraceFallbackSharesTheFirstAttemptsAbsoluteDeadline(t *testing.T) {
	rt := &recordingTransport{fn: func(i int, _ *http.Request) (*http.Response, error) {
		if i == 0 {
			return fakeResponse(http.StatusBadRequest,
				`{"code":"trace_unsupported","error":"this deployment does not accept trace"}`, nil), nil
		}
		return fakeResponse(http.StatusOK, routerBody, nil), nil
	}}
	useTransport(t, rt)
	out := runPerformSearch(t, 30*time.Second, traceCall())
	if !out.ok() || out.Attempts != 2 || !out.Retried {
		t.Fatalf("attempts %d retried %v fault %q", out.Attempts, out.Retried, out.Fault)
	}
	if !rt.hadDeadline[0] || !rt.hadDeadline[1] {
		t.Fatal("an attempt carried no deadline")
	}
	// The same absolute instant, not a fresh budget. A fallback that got
	// its own timer would let one search take twice as long as promised.
	if !rt.deadlines[0].Equal(rt.deadlines[1]) {
		t.Errorf("the fallback got a new deadline: first %s, fallback %s (+%s)",
			rt.deadlines[0], rt.deadlines[1], rt.deadlines[1].Sub(rt.deadlines[0]))
	}
}

// blockingBody returns nothing until the request context ends, which is
// what a router that accepts a connection and then stalls looks like.
type blockingBody struct{ ctx context.Context }

func (b blockingBody) Read([]byte) (int, error) {
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}
func (b blockingBody) Close() error { return nil }

func TestABlockingResponseBodyEndsOnTheSearchDeadline(t *testing.T) {
	rt := &recordingTransport{fn: func(_ int, req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode:    http.StatusOK,
			Header:        http.Header{},
			Body:          blockingBody{ctx: req.Context()},
			ContentLength: -1,
		}, nil
	}}
	useTransport(t, rt)
	start := time.Now()
	out := runPerformSearch(t, 150*time.Millisecond, traceCall())
	if out.Fault != faultTimeout {
		t.Fatalf("a stalled body read gave fault %q, want %q (err %v)", out.Fault, faultTimeout, out.Err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("the deadline did not cover the body read: %s", elapsed)
	}
}

func TestCancellationIsNotReportedAsADeadline(t *testing.T) {
	rt := &recordingTransport{fn: func(_ int, req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode:    http.StatusOK,
			Header:        http.Header{},
			Body:          blockingBody{ctx: req.Context()},
			ContentLength: -1,
		}, nil
	}}
	useTransport(t, rt)
	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	defer cancel()
	start := time.Now()
	out := performSearch(ctx, time.Now, traceCall())
	if out.Fault != faultCanceled {
		t.Fatalf("an interrupted search gave fault %q, want %q", out.Fault, faultCanceled)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("cancellation did not return promptly: %s", elapsed)
	}
}

func TestANonPositiveTimeoutIsRefusedRatherThanUnbounded(t *testing.T) {
	_, cfg, root := newFakeRouter(t, nil)
	h := fixedSearchOps(root)
	for _, v := range []string{"0", "-1s"} {
		code, _, errOut := runSearch(t, h, map[string]string{"TOKENDROP_API_KEY": "k"}, "-config", cfg, "-timeout", v, "q")
		if code != exitUsage {
			t.Errorf("-timeout %s: exit %d, want %d", v, code, exitUsage)
		}
		if !strings.Contains(errOut, "must be positive") {
			t.Errorf("-timeout %s: %q", v, errOut)
		}
	}
}

func TestASearchTimeoutRecordsNoMiningEvidenceAndNoHealthFailure(t *testing.T) {
	rt := &recordingTransport{fn: func(_ int, req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK, Header: http.Header{},
			Body: blockingBody{ctx: req.Context()}, ContentLength: -1,
		}, nil
	}}
	useTransport(t, rt)
	_, cfg, root := newFakeRouter(t, nil)
	h := fixedSearchOps(root)
	code, out, _ := runSearch(t, h, map[string]string{"TOKENDROP_API_KEY": "k"},
		"-config", cfg, "-timeout", "150ms", "q")
	if code != exitTransport {
		t.Fatalf("exit %d, want %d", code, exitTransport)
	}
	if out != "" {
		t.Errorf("a timed-out search printed a body: %q", out)
	}
	if recs, _, _ := readIntake(filepath.Join(root, "intake")); len(recs) != 0 {
		t.Error("a timed-out search was recorded as mining evidence")
	}
	// A search that never completed is not a mining failure: nothing
	// mining-side ran, so nothing mining-side may be marked degraded.
	store, err := auth.OpenStoreExisting(filepath.Join(root, "state"))
	if err != nil {
		t.Fatal(err)
	}
	if recs, _ := store.HealthRecords(); len(recs) != 0 {
		t.Errorf("a router timeout was written as mining health: %+v", recs)
	}
	if len(h.flushes) != 0 {
		t.Error("a flush was started for a search that never completed")
	}
}

// ── §20 bounded and valid responses ─────────────────────────────────────

func bodyOfSize(t *testing.T, n int) string {
	t.Helper()
	const prefix, suffix = `{"request_id":"req-size","pad":"`, `"}`
	if n < len(prefix)+len(suffix) {
		t.Fatalf("cannot build a %d-byte object", n)
	}
	return prefix + strings.Repeat("x", n-len(prefix)-len(suffix)) + suffix
}

func TestRouterResponsesAreBoundedAndValidated(t *testing.T) {
	const max = 512
	exact := bodyOfSize(t, max)
	over := bodyOfSize(t, max+1)

	for _, tc := range []struct {
		name   string
		status int
		body   string
		header http.Header
		want   searchFault
		id     string
	}{
		{"exactly max bytes, valid", 200, exact, nil, faultNone, "req-size"},
		{"max + 1 bytes", 200, over, nil, faultOversized, ""},
		{"malformed JSON", 200, `{"request_id":`, nil, faultInvalidResponse, ""},
		{"trailing JSON value", 200, `{"request_id":"a"}{"request_id":"b"}`, nil, faultInvalidResponse, ""},
		{"trailing non-whitespace", 200, `{"request_id":"a"} oops`, nil, faultInvalidResponse, ""},
		{"empty 2xx", 200, ``, nil, faultInvalidResponse, ""},
		{"204 no body", 204, ``, nil, faultInvalidResponse, ""},
		{"root is not an object", 200, `[{"request_id":"a"}]`, nil, faultInvalidResponse, ""},
		{"root is null", 200, `null`, nil, faultInvalidResponse, ""},
		{"body request_id", 200, `{"request_id":"from-body"}`, nil, faultNone, "from-body"},
		{
			"header X-Request-Id only", 200, `{"candidates":[]}`,
			http.Header{"X-Request-Id": []string{"from-header"}}, faultNone, "from-header",
		},
		{
			"body wins over header", 200, `{"request_id":"from-body"}`,
			http.Header{"X-Request-Id": []string{"from-header"}}, faultNone, "from-body",
		},
		{"neither request id", 200, `{"candidates":[]}`, nil, faultMissingID, ""},
		{"blank request id", 200, `{"request_id":"   "}`, nil, faultMissingID, ""},
		{
			"unknown top-level fields", 200,
			`{"request_id":"unknown-ok","experiment":{"arm":"b"},"future":[1,2,3]}`, nil, faultNone, "unknown-ok",
		},
		{
			"unknown nested candidate fields", 200,
			`{"request_id":"nested-ok","candidates":[{"provider":"p","answer":"a","rerank_score":0.5}]}`,
			nil, faultNone, "nested-ok",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withSearchMaxBody(t, max)
			rt := &recordingTransport{fn: func(int, *http.Request) (*http.Response, error) {
				return fakeResponse(tc.status, tc.body, tc.header), nil
			}}
			useTransport(t, rt)
			out := runPerformSearch(t, 30*time.Second, searchCall{Endpoint: "https://r.test/v1/search", Key: "k", Query: "q"})
			if out.Fault != tc.want {
				t.Fatalf("fault %q, want %q (err %v)", out.Fault, tc.want, out.Err)
			}
			if tc.want == faultNone && out.Success.RequestID != tc.id {
				t.Errorf("request id %q, want %q", out.Success.RequestID, tc.id)
			}
		})
	}
}

func TestAnInterruptedBodyIsNeverASuccessOverThePrefix(t *testing.T) {
	rt := &recordingTransport{fn: func(int, *http.Request) (*http.Response, error) {
		// Content-Length promises more than the body delivers, which is
		// what a connection cut mid-response looks like.
		return &http.Response{
			StatusCode:    http.StatusOK,
			Header:        http.Header{},
			Body:          io.NopCloser(io.MultiReader(strings.NewReader(`{"request_id":"req-1"`), errorReader{})),
			ContentLength: 4096,
		}, nil
	}}
	useTransport(t, rt)
	out := runPerformSearch(t, 30*time.Second, searchCall{Endpoint: "https://r.test/v1/search", Key: "k", Query: "q"})
	if out.ok() {
		t.Fatal("an interrupted body was accepted as a successful search")
	}
	if out.Fault != faultInvalidResponse {
		t.Errorf("fault %q, want %q", out.Fault, faultInvalidResponse)
	}
}

type errorReader struct{}

func (errorReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestADeclaredOversizedContentLengthIsRefusedWithoutDraining(t *testing.T) {
	withSearchMaxBody(t, 512)
	drained := false
	rt := &recordingTransport{fn: func(int, *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode:    http.StatusOK,
			Header:        http.Header{},
			Body:          readMarker{&drained},
			ContentLength: 1 << 20,
		}, nil
	}}
	useTransport(t, rt)
	out := runPerformSearch(t, 30*time.Second, searchCall{Endpoint: "https://r.test/v1/search", Key: "k", Query: "q"})
	if out.Fault != faultOversized {
		t.Fatalf("fault %q, want %q", out.Fault, faultOversized)
	}
	if drained {
		t.Error("a body already declared oversized was read anyway")
	}
}

type readMarker struct{ read *bool }

func (m readMarker) Read(p []byte) (int, error) { *m.read = true; return 0, io.EOF }
func (m readMarker) Close() error               { return nil }

func TestAnInvalidSuccessRecordsNoMiningEvidence(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"malformed", `{"request_id":`},
		{"no request identity", `{"candidates":[]}`},
		{"trailing value", `{"request_id":"a"}{}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, cfg, root := newFakeRouter(t, func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(tc.body))
			})
			h := fixedSearchOps(root)
			code, out, errOut := runSearch(t, h, map[string]string{"TOKENDROP_API_KEY": "k"}, "-config", cfg, "q")
			if code != exitServerErr {
				t.Fatalf("exit %d, want %d (err %q)", code, exitServerErr, errOut)
			}
			if out != "" {
				t.Errorf("the malformed body was echoed to stdout: %q", out)
			}
			if recs, _, _ := readIntake(filepath.Join(root, "intake")); len(recs) != 0 {
				t.Error("an invalid 2xx was recorded as mining evidence")
			}
			if len(h.flushes) != 0 {
				t.Error("an invalid 2xx started a flush")
			}
		})
	}
}

// ── §21 the trace_unsupported compatibility retry ───────────────────────

// One POST unless the router says, in its own machine code, that it does
// not accept the field. The behavior this replaced inferred that from a
// bare 400 or 422, so an invalid query bought a second billable POST
// carrying the same query.
func TestOnlyAnExactTraceUnsupportedCodeBuysASecondPost(t *testing.T) {
	const flat = `{"code":"trace_unsupported","error":"not accepted"}`
	for _, tc := range []struct {
		name     string
		status   int
		body     string
		traced   bool
		wantPost int
	}{
		{"exact trace_unsupported", 400, flat, true, 2},
		{"exact trace_unsupported on 422", 422, flat, true, 2},
		{"ordinary 400", 400, `{"code":"invalid_query","error":"empty query"}`, true, 1},
		{"ordinary 422", 422, `{"code":"invalid_tier","error":"unknown tier"}`, true, 1},
		{"legacy unknown-field prose", 400, `{"error":"unknown field trace"}`, true, 1},
		{"401", 401, `{"code":"unauthorized","error":"bad key"}`, true, 1},
		{"403", 403, `{"code":"forbidden","error":"no access"}`, true, 1},
		{"404", 404, `{"code":"not_found","error":"no such route"}`, true, 1},
		{"429", 429, `{"code":"rate_limited","error":"slow down"}`, true, 1},
		{"500", 500, `{"code":"trace_unsupported","error":"not a client error"}`, true, 1},
		{"malformed error JSON", 400, `{"code":`, true, 1},
		{"trailing data after the envelope", 400, flat + `{}`, true, 1},
		{"prose containing the phrase", 400, `{"code":"invalid_query","error":"trace_unsupported is not why"}`, true, 1},
		{"AS-shaped nested envelope", 400, `{"error":{"code":"trace_unsupported","message":"wrong envelope"}}`, true, 1},
		{"code in a different case", 400, `{"code":"TRACE_UNSUPPORTED","error":"not exact"}`, true, 1},
		{"untraced request", 400, flat, false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt := &recordingTransport{fn: func(i int, _ *http.Request) (*http.Response, error) {
				if i == 0 {
					return fakeResponse(tc.status, tc.body, nil), nil
				}
				return fakeResponse(http.StatusOK, routerBody, nil), nil
			}}
			useTransport(t, rt)
			call := searchCall{Endpoint: "https://r.test/v1/search", Key: "k", Query: "q"}
			if tc.traced {
				call.Trace = &traceEnvelope{V: traceVersion, Harness: "cli", SessionID: "s", CallID: "c"}
			}
			out := runPerformSearch(t, 30*time.Second, call)
			if got := rt.calls(); got != tc.wantPost {
				t.Fatalf("%d POST(s), want %d", got, tc.wantPost)
			}
			if out.Attempts != tc.wantPost {
				t.Errorf("outcome recorded %d attempts, transport saw %d", out.Attempts, tc.wantPost)
			}
		})
	}
}

func TestTheTraceFallbackChangesOnlyTheTraceField(t *testing.T) {
	rt := &recordingTransport{fn: func(i int, _ *http.Request) (*http.Response, error) {
		if i == 0 {
			return fakeResponse(http.StatusBadRequest, `{"code":"trace_unsupported","error":"no"}`, nil), nil
		}
		return fakeResponse(http.StatusOK, routerBody, nil), nil
	}}
	useTransport(t, rt)
	call := traceCall()
	call.Tier = "fast"
	out := runPerformSearch(t, 30*time.Second, call)
	if !out.ok() {
		t.Fatalf("the fallback did not succeed: %q %v", out.Fault, out.Err)
	}
	first, second := decodeSentBody(t, rt.bodies[0]), decodeSentBody(t, rt.bodies[1])
	if _, ok := first["trace"]; !ok {
		t.Fatal("the first attempt carried no trace")
	}
	if _, ok := second["trace"]; ok {
		t.Error("the fallback still carried the trace")
	}
	delete(first, "trace")
	if fmt.Sprint(first) != fmt.Sprint(second) {
		t.Errorf("the fallback changed more than the trace:\n first (minus trace) %v\n second %v", first, second)
	}
	if second["query"] != call.Query || second["tier"] != call.Tier {
		t.Errorf("the fallback altered the search: %v", second)
	}
}

func decodeSentBody(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("sent body %q: %v", s, err)
	}
	return m
}

func TestSearchNeverIssuesMoreThanTwoPosts(t *testing.T) {
	rt := &recordingTransport{fn: func(int, *http.Request) (*http.Response, error) {
		// Always the code that licenses a retry: a recursive
		// implementation would never stop.
		return fakeResponse(http.StatusBadRequest, `{"code":"trace_unsupported","error":"always"}`, nil), nil
	}}
	useTransport(t, rt)
	out := runPerformSearch(t, 30*time.Second, traceCall())
	if got := rt.calls(); got != 2 {
		t.Fatalf("%d POST(s); the compatibility retry is not bounded at one", got)
	}
	if out.Fault != faultRouterStatus || out.HTTPStatus != http.StatusBadRequest {
		t.Errorf("a failed fallback should be classified normally: %q HTTP %d", out.Fault, out.HTTPStatus)
	}
}

// ── unit-level guards on the pieces ─────────────────────────────────────

func TestReadBoundedBodyRefusesOneByteOverTheCeiling(t *testing.T) {
	for _, tc := range []struct {
		n       int
		wantErr bool
	}{{0, false}, {8, false}, {16, false}, {17, true}, {64, true}} {
		raw, err := readBoundedBody(strings.NewReader(strings.Repeat("x", tc.n)), 16)
		switch {
		case tc.wantErr && !errors.Is(err, errBodyOversized):
			t.Errorf("%d bytes: err %v, want oversized", tc.n, err)
		case !tc.wantErr && err != nil:
			t.Errorf("%d bytes: %v", tc.n, err)
		case !tc.wantErr && len(raw) != tc.n:
			t.Errorf("%d bytes: read %d", tc.n, len(raw))
		}
	}
}

func TestRetryAfterIsParsedInBothFormsOrReportedUnknown(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		header string
		wantMS int64
		wantOK bool
	}{
		{"", 0, false},
		{"7", 7000, true},
		{"0", 0, true},
		{"-3", 0, false},
		{"soon", 0, false},
		{"Sat, 12 Sep 2026 10:00:30 GMT", 30000, true},
		{"Sat, 12 Sep 2026 09:59:00 GMT", 0, true}, // already past: zero, never negative
	} {
		h := http.Header{}
		if tc.header != "" {
			h.Set("Retry-After", tc.header)
		}
		ms, ok := retryAfterMS(h, now)
		if ms != tc.wantMS || ok != tc.wantOK {
			t.Errorf("Retry-After %q → (%d, %v), want (%d, %v)", tc.header, ms, ok, tc.wantMS, tc.wantOK)
		}
	}
}
